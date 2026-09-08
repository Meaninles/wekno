"""SDK workspace/tools on an isolated, resource-bounded container backend.

Only BackendBase's three I/O primitives are implemented here. Reasoning,
tool execution selection, context offloading and skills remain SDK features.
"""
from __future__ import annotations

import asyncio
import base64
import contextlib
import hashlib
import io
import json
import mimetypes
import os
import posixpath
import tarfile
import time
import uuid
from importlib import resources
from contextlib import asynccontextmanager
from pathlib import PurePosixPath

import aiodocker
from agentscope.message import TextBlock
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.skill import Skill
from agentscope.tool import BackendBase, ExecResult, Glob, ToolBase, ToolChunk
from agentscope.workspace import WorkspaceBase

from .tools import invocation_id


def safe_path(path: str) -> str:
    result = posixpath.normpath(posixpath.join("/workspace", path))
    if result != "/workspace" and not result.startswith("/workspace/"):
        raise ValueError("Path is outside this run's workspace")
    return result


class SandboxBackend(BackendBase):
    def __init__(self, payload, control):
        self.payload, self.control = payload, control
        self.docker = None
        self.container = None
        self.lock = asyncio.Lock()
        self.key = hashlib.sha256(payload.run_id.encode()).hexdigest()[:32]

    async def initialize(self):
        async with self.lock:
            if self.container is not None:
                return
            await self.control.post("runs/heartbeat")
            self.docker = aiodocker.Docker()
            # A new owner replaces the old process boundary before mounting
            # the run volume. Different runs never share a writable volume.
            old = await self.docker.containers.list(all=True, filters=json.dumps({"label":["weknora.agent.workspace="+self.key]}))
            for container in old:
                info = await container.show()
                epoch = int(info["Config"]["Labels"]["weknora.agent.epoch"])
                if epoch > self.payload.owner_epoch:
                    raise asyncio.CancelledError("Newer workspace owner exists")
                await container.delete(force=True)
            labels={"weknora.agent.workspace":self.key,"weknora.agent.epoch":str(self.payload.owner_epoch),
                    "weknora.agent.run":self.payload.run_id,"app.kubernetes.io/managed-by":"weknora-agent-runtime"}
            for prefix in ("agent-workspace-", "agent-receipts-"):
                await self.docker.volumes.create({"Name":prefix+self.key,"Labels":labels})
            config = {
                "Image":os.environ["AGENT_WORKSPACE_IMAGE"], "Cmd":["sleep","infinity"],
                "WorkingDir":"/workspace", "Env":["HOME=/workspace","PYTHONUNBUFFERED=1"],
                "Labels":labels,
                "HostConfig":{"NetworkMode":"none","ReadonlyRootfs":True,"CapDrop":["ALL"],
                              "CapAdd":["SETUID","SETGID","KILL"],"SecurityOpt":["no-new-privileges:true"],
                              "Memory":int(os.environ.get("AGENT_WORKSPACE_MEMORY_BYTES",str(2*1024**3))),
                              "NanoCpus":2_000_000_000,"PidsLimit":128,
                              "Tmpfs":{"/tmp":"rw,noexec,nosuid,size=512m,mode=1777"},
                              "Mounts":[{"Type":"volume","Source":"agent-workspace-"+self.key,"Target":"/workspace"},
                                        {"Type":"volume","Source":"agent-receipts-"+self.key,"Target":"/control"}]},
            }
            await self.control.post("runs/heartbeat")
            try:
                self.container = await self.docker.containers.create(config=config,name=f"agent-workspace-{self.key}")
            except aiodocker.exceptions.DockerError as exc:
                if exc.status == 409:
                    raise asyncio.CancelledError("Workspace provisioning is owned by another worker") from exc
                raise
            await self.container.start()
            await self.control.post("runs/heartbeat")

    async def close(self):
        if self.container:
            await self.container.delete(force=True)
            self.container = None
        if self.docker:
            try:
                with contextlib.suppress(Exception):
                    status = await self.control.post("runs/status")
                    if status.get("status") in ("completed", "failed", "cancelled", "incomplete"):
                        for prefix in ("agent-workspace-", "agent-receipts-"):
                            with contextlib.suppress(aiodocker.exceptions.DockerError):
                                volume = await self.docker.volumes.get(prefix+self.key)
                                await volume.delete()
            finally:
                await self.docker.close()
                self.docker = None

    async def exec_shell(self, command, *, cwd=None, timeout=None):
        await self.initialize()
        await self.control.post("runs/heartbeat")
        params={"command":command,"cwd":safe_path(cwd or "/workspace"),"timeout":timeout or 300,
                "epoch":self.payload.owner_epoch}
        call = invocation_id.get()
        identity_body={key:value for key,value in params.items() if key!="epoch"}
        params["id"] = (call or uuid.uuid4().hex)+":"+hashlib.sha256(json.dumps(identity_body,sort_keys=True).encode()).hexdigest()
        encoded=base64.b64encode(json.dumps(params).encode()).decode()
        operation = await self.container.exec(["python","/opt/agent/job.py",encoded],stdout=True,stderr=True)
        parts={1:bytearray(),2:bytearray()}
        try:
            async with operation.start() as stream:
                while (chunk:=await stream.read_out()) is not None:
                    parts[chunk.stream].extend(chunk.data)
                    if sum(len(v) for v in parts.values())>4*1024**2:
                        raise RuntimeError("Workspace transport output exceeds limit")
            info=await operation.inspect()
            if info.get("ExitCode")!=0:
                raise RuntimeError(parts[2].decode(errors="replace")[-2000:])
            result=json.loads(parts[1])
            return ExecResult(exit_code=result["exit_code"],stdout=base64.b64decode(result["stdout"]),stderr=base64.b64decode(result["stderr"]))
        except asyncio.CancelledError:
            await self.close()
            raise

    async def read_file(self,path):
        await self.initialize()
        path=safe_path(path)
        operation=await self.container.exec(["python","/opt/agent/files.py","read",path],
                                             user="1000:1000", stdout=True, stderr=True)
        content, errors = bytearray(), bytearray()
        try:
            async with operation.start() as stream:
                while (chunk := await stream.read_out()) is not None:
                    if chunk.stream == 1:
                        content.extend(chunk.data)
                        if len(content)>128*1024**2:
                            raise ValueError("Workspace file exceeds 128 MiB")
                    else:
                        errors.extend(chunk.data[:max(0,4096-len(errors))])
            if (await operation.inspect()).get("ExitCode") != 0:
                raise OSError(errors.decode(errors="replace"))
            return bytes(content)
        except asyncio.CancelledError:
            await self.close()
            raise

    async def write_file(self,path,data):
        await self.write_files([(path,data)])

    async def write_files(self,files):
        await self.initialize()
        # Stream into the unprivileged helper. Docker archive extraction does
        # not support tmpfs on a read-only root and would use root privileges.
        payload=io.BytesIO()
        with tarfile.open(fileobj=payload,mode="w") as bundle:
            for path,data in files:
                if len(data)>128*1024**2:
                    raise ValueError("Workspace file exceeds 128 MiB")
                entry=tarfile.TarInfo(safe_path(path).removeprefix("/workspace/"))
                entry.size=len(data);entry.mode=0o600
                bundle.addfile(entry,io.BytesIO(data))
        operation=await self.container.exec(["python","/opt/agent/files.py","write-stream"],
                                             user="1000:1000", stdin=True, stdout=True, stderr=True)
        errors=bytearray()
        try:
            async with operation.start() as stream:
                payload.seek(0)
                while chunk := payload.read(65536):
                    await stream.write_in(chunk)
                while (chunk := await stream.read_out()) is not None:
                    errors.extend(chunk.data[:max(0,4096-len(errors))])
            if (await operation.inspect()).get("ExitCode") != 0:
                raise OSError(errors.decode(errors="replace"))
        except asyncio.CancelledError:
            await self.close()
            raise


class WorkspaceGlob(Glob):
    """Install the SDK's own helper in the remote backend on first use."""
    def __init__(self, backend):
        super().__init__(backend=backend, glob_helper_path="/workspace/.runtime/glob_helper.py")
        self.backend = backend
        self.installed = False

    async def call(self, pattern, path=None):
        if not self.installed:
            helper = resources.files("agentscope.tool._builtin._scripts").joinpath("_glob_helper.py").read_bytes()
            await self.backend.write_file("/workspace/.runtime/glob_helper.py", helper)
            self.installed = True
        return await super().call(pattern=pattern, path=path)


class RuntimeWorkspace(WorkspaceBase):
    def __init__(self,payload,control):
        super().__init__(workspace_id=payload.run_id)
        self.workdir="/workspace"
        backend = os.environ.get("AGENT_WORKSPACE_BACKEND", "docker")
        if backend == "kubernetes":
            from .kubernetes_workspace import KubernetesBackend
            self._backend=KubernetesBackend(payload,control)
        elif backend == "docker":
            self._backend=SandboxBackend(payload,control)
        else:
            raise ValueError("AGENT_WORKSPACE_BACKEND must be docker or kubernetes")
        self.tools=[]
        self.skills=[]
        self.offloader=self
        self.payload,self.control=payload,control
        self.pending_files = {}
        self.prepare_lock = asyncio.Lock()
        self.output_revision = 0
        self.output_directory_ready = False

    async def initialize(self):
        # Provision lazily for plain QA: no container startup on its critical
        # path unless a workspace operation or SDK offload is actually needed.
        self.is_alive=True
        self.tools=await self.list_tools()
        self.tools=[WorkspaceGlob(self.get_backend()) if tool.name == "Glob" else tool for tool in self.tools]
        from .file_context import WorkspaceEdit, WorkspaceWrite
        file_tools = {"Write": WorkspaceWrite, "Edit": WorkspaceEdit}
        self.tools=[file_tools[tool.name](backend=self.get_backend()) if tool.name in file_tools else tool for tool in self.tools]
        if not self.payload.enable_artifacts:
            self.tools=[tool for tool in self.tools if tool.is_read_only]
        instructions=[]
        image_inputs=[]
        for spec in self.payload.original_input_files:
            path=safe_path("inputs/"+spec.id+"/"+PurePosixPath(spec.file_name.replace("\\","/")).name)
            self.pending_files[path] = spec
            instructions.append({"input_id":spec.id,"path":path,"role":spec.role,"sha256":spec.sha256,
                                 "knowledge_id":spec.knowledge_id,"knowledge_base_id":spec.knowledge_base_id})
            media_type=mimetypes.guess_type(spec.file_name)[0] or "application/octet-stream"
            # Current user images are necessary evidence, unlike unused files.
            if media_type.startswith("image/") and spec.source == "weknora_chat_image_original":
                content = await self.input_bytes(spec)
                image_inputs.append("data:"+media_type+";base64,"+base64.b64encode(content).decode())
                self.pending_files[path] = content
        if image_inputs:
            self.payload.image_urls=list(dict.fromkeys([*self.payload.image_urls,*image_inputs]))
        for index,spec in enumerate(self.payload.document_template_context.files):
            path=safe_path(f"templates/{index}/"+PurePosixPath(spec.file_name.replace("\\","/")).name)
            self.pending_files[path] = base64.b64decode(spec.content_base64,validate=True)
            instructions.append({"template_variable":spec.variable,"role":spec.role,"path":path})
        for spec in self.payload.professional_skills:
            directory=safe_path("skills/"+spec.name)
            markdown=""
            for item in spec.files:
                path=safe_path(posixpath.join(directory,item.path))
                if not path.startswith(directory+"/"):raise ValueError("Skill package path escapes its directory")
                content=base64.b64decode(item.content_base64,validate=True)
                self.pending_files[path] = content
                if item.path=="SKILL.md":markdown=content.decode()
            if not markdown:raise ValueError("Professional skill has no SKILL.md")
            self.skills.append(Skill(name=spec.name,description=spec.description,dir=directory,markdown=markdown,updated_at=time.time()))
        # Lightweight instructions selected for the current request are facts
        # about capabilities, not a second dynamic prompt/router model.
        prompt={"workspace":"/workspace","files":instructions,"visible_context":self.payload.visible_context,
                "lightweight_skills":[s.model_dump() for s in self.payload.lightweight_skills]}
        self.payload.system_prompt += "\n\n"+json.dumps(prompt,ensure_ascii=False)
        self.payload.system_prompt += "\nUse available evidence to answer directly. Call a tool only when necessary to resolve missing information or perform an action within your capabilities. Independent reads may run together."
        if not self.payload.enable_artifacts:
            self.payload.system_prompt += "\nWorkspace capability: read supplied input files as source material."
        self.payload.system_prompt += "\nGround factual claims in the sources actually inspected. A search with no relevant result establishes only that this search found no evidence; it does not establish that a rule or document does not exist. Distinguish explicit provisions from your interpretation. For findings from original files, reuse an existing matching citation handle. If none is available, use list_knowledge_chunks with the file's knowledge_id to obtain the relevant citable excerpt; do not invent source handles or cite a fragment that does not support the finding. Internal conversation metadata is not part of the user-facing answer."
        if self.payload.enable_artifacts:
            self.payload.system_prompt += "\nThe runtime creates /workspace/outputs before workspace tools run. Save finished deliverables there directly. Keep scripts, previews and temporary files elsewhere."
            self.payload.system_prompt += ("\nWorkspace capabilities are already provisioned: Python with openpyxl, xlsxwriter, pandas, python-docx, python-pptx, PyMuPDF, Pillow and matplotlib; Node with pptxgenjs; LibreOffice, pandoc and PDF utilities with CJK fonts. Do not probe or install these dependencies. Combine creation and meaningful validation in one script when their inputs are known. For spreadsheets, verify formulas and recalculate with LibreOffice before publishing if computed values are needed. Use /workspace/outputs for deliverables.")

    async def input_bytes(self, spec):
        content=bytearray()
        async with self.control.client.stream("GET",spec.download_url,timeout=120) as response:
            response.raise_for_status()
            async for chunk in response.aiter_bytes():
                content.extend(chunk)
                if len(content)>128*1024**2:
                    raise ValueError("Original input exceeds 128 MiB")
        content=bytes(content)
        if len(content)!=spec.file_size or hashlib.sha256(content).hexdigest()!=spec.sha256:
            raise ValueError("Original input integrity verification failed")
        return content

    async def prepare_tool(self, name, arguments):
        tool = next((tool for tool in self.tools if tool.name == name), None)
        if tool is None:
            return
        async with self.prepare_lock:
            if self.payload.enable_artifacts and not self.output_directory_ready:
                result = await self.get_backend().exec_shell('mkdir -p /workspace/outputs', timeout=15)
                if result.exit_code:
                    raise OSError('Output directory could not be prepared')
                self.output_directory_ready = True
            if not tool.is_read_only:
                from .artifact_delivery import capture_baseline
                await capture_baseline(self.payload, self.control, self)
            # A shell can access arbitrary files. Path-based tools only need
            # their selected file/subtree. No intent model or keyword router.
            path = arguments.get("file_path") or arguments.get("path")
            target = safe_path(path) if isinstance(path, str) else None
            paths = [p for p in self.pending_files if target is None or p == target or p.startswith(target.rstrip("/")+"/")]
            for path in paths:
                value = self.pending_files[path]
                # On recovery a run may already have edited a supplied file.
                # Never replace that current file with its original contents.
                try:
                    await self.get_backend().read_file(path)
                except OSError:
                    content = value if isinstance(value, bytes) else await self.input_bytes(value)
                    await self.get_backend().write_file(path, content)
                self.pending_files.pop(path)

    def tool_finished(self, name):
        tool = next((tool for tool in self.tools if tool.name == name), None)
        if tool is not None and not tool.is_read_only:
            self.output_revision += 1

    async def close(self):
        await self.get_backend().close()

    async def get_instructions(self):
        return "Each run has an isolated workspace at /workspace. Use /workspace/outputs for deliverables."



@asynccontextmanager
async def workspace_for(payload,control):
    workspace=RuntimeWorkspace(payload,control)
    try:
        if payload.finalization is None:
            await workspace.initialize()
        yield workspace
    finally:
        await workspace.close()
