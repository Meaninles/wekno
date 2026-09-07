"""Kubernetes transport for the same SDK workspace and execution receipts.

Only Pods in a dedicated workspace namespace are manageable by this worker.
PVCs outlive worker/Pod replacement; terminal runs release them after delivery.
"""
from __future__ import annotations

import asyncio
import contextlib
import json
import os
from types import SimpleNamespace

from kubernetes_asyncio import client, config
from kubernetes_asyncio.client.exceptions import ApiException
from kubernetes_asyncio.stream import WsApiClient

from .workspace import SandboxBackend


class PodExec:
    def __init__(self, container, command, user, stdin):
        self.container, self.command, self.stdin = container, command, stdin
        if user:
            self.command = ["python", "-c", "import os,sys; os.setgroups([]); os.setgid(1000); os.setuid(1000); os.execvp(sys.argv[1],sys.argv[1:])", *command]
        self.exit_code = None

    def start(self):
        return self

    async def __aenter__(self):
        self.transport = WsApiClient(self.container.api.api_client.configuration)
        try:
            self.socket = await client.CoreV1Api(self.transport).connect_get_namespaced_pod_exec(
                self.container.name, self.container.namespace, command=self.command, container="workspace",
                stdout=True, stderr=True, stdin=self.stdin, tty=False, _preload_content=False)
        except BaseException:
            await self.transport.close()
            raise
        return self

    async def write_in(self, data):
        await self.socket.send_bytes(b"\0" + data)

    async def read_out(self):
        async for message in self.socket:
            if message.type not in (1, 2):
                break
            raw = message.data if isinstance(message.data, bytes) else message.data.encode()
            if not raw:
                continue
            channel, data = raw[0], raw[1:]
            if channel in (1, 2):
                return SimpleNamespace(stream=channel, data=data)
            if channel == 3:
                status = json.loads(data)
                self.exit_code = 0 if status.get("status") == "Success" else 1
                for cause in status.get("details", {}).get("causes", []):
                    if cause.get("reason") == "ExitCode":
                        self.exit_code = int(cause["message"])
        if self.exit_code is None:
            raise ConnectionError("Workspace execution stream closed without exit status")
        return None

    async def inspect(self):
        return {"ExitCode": self.exit_code}

    async def __aexit__(self, *_):
        await self.socket.close()
        await self.transport.close()


class PodContainer:
    def __init__(self, api, namespace, name, uid):
        self.api, self.namespace, self.name, self.uid = api, namespace, name, uid

    async def exec(self, command, *, user=None, stdin=False, **_):
        return PodExec(self, command, user, stdin)

    async def delete(self, **_):
        try:
            await self.api.delete_namespaced_pod(self.name, self.namespace, body={
                "gracePeriodSeconds": 3, "preconditions": {"uid": self.uid}})
        except ApiException as exc:
            if exc.status != 404:
                raise
        # Waiting for disappearance prevents overlapping writable owners.
        async with asyncio.timeout(90):
            while True:
                try:
                    pod = await self.api.read_namespaced_pod(self.name, self.namespace)
                    if pod.metadata.uid != self.uid:
                        return
                except ApiException as exc:
                    if exc.status == 404:
                        return
                    raise
                await asyncio.sleep(.25)


def pod_spec(payload, key):
    labels = {"weknora.agent.workspace": key, "weknora.agent.epoch": str(payload.owner_epoch),
              "weknora.agent.run": payload.run_id, "app.kubernetes.io/managed-by": "weknora-agent-runtime"}
    volumes = [{"name": name, "persistentVolumeClaim": {"claimName": prefix+key}}
               for name, prefix in (("workspace", "agent-workspace-"), ("receipts", "agent-receipts-"))]
    mounts = [{"name":"workspace", "mountPath":"/workspace"}, {"name":"receipts", "mountPath":"/control"}]
    return {"apiVersion":"v1", "kind":"Pod", "metadata":{"name":"agent-workspace-"+key, "labels":labels}, "spec":{
        "automountServiceAccountToken":False, "restartPolicy":"Never", "terminationGracePeriodSeconds":3,
        "activeDeadlineSeconds":int(os.environ.get("AGENT_WORKSPACE_MAX_SECONDS", "7200")),
        "securityContext":{"seccompProfile":{"type":"RuntimeDefault"}},
        "imagePullSecrets":[{"name":name} for name in os.environ.get("AGENT_WORKSPACE_IMAGE_PULL_SECRETS", "").split(",") if name],
        "nodeSelector":json.loads(os.environ.get("AGENT_WORKSPACE_NODE_SELECTOR", "{}")),
        "initContainers":[{"name":"permissions", "image":os.environ["AGENT_WORKSPACE_IMAGE"],
            "command":["sh", "-c", "chown 1000:1000 /workspace && chmod 700 /workspace /control"], "volumeMounts":mounts,
            "securityContext":{"runAsUser":0,"allowPrivilegeEscalation":False,"readOnlyRootFilesystem":True,
                               "capabilities":{"drop":["ALL"],"add":["CHOWN","FOWNER"]}}}],
        "containers":[{"name":"workspace", "image":os.environ["AGENT_WORKSPACE_IMAGE"], "imagePullPolicy":"IfNotPresent",
            "command":["sleep","infinity"], "workingDir":"/workspace", "env":[{"name":"HOME","value":"/workspace"}],
            "securityContext":{"runAsUser":0,"allowPrivilegeEscalation":False,"readOnlyRootFilesystem":True,
                               "capabilities":{"drop":["ALL"],"add":["SETUID","SETGID","KILL"]}},
            "resources":{"requests":{"cpu":"100m","memory":"256Mi"},"limits":{"cpu":"2","memory":"2Gi","ephemeral-storage":"1Gi"}},
            "volumeMounts":mounts+[{"name":"tmp","mountPath":"/tmp"}]}],
        "volumes":volumes+[{"name":"tmp","emptyDir":{"medium":"Memory","sizeLimit":"512Mi"}}]}}


class KubernetesBackend(SandboxBackend):
    def __init__(self, payload, control):
        super().__init__(payload, control)
        self.namespace = os.environ["AGENT_WORKSPACE_NAMESPACE"]
        self.api = self.api_client = None

    async def initialize(self):
        async with self.lock:
            if self.container:
                return
            await self.control.post("runs/heartbeat")
            config.load_incluster_config()
            self.api_client = client.ApiClient()
            self.api = client.CoreV1Api(self.api_client)
            name = "agent-workspace-"+self.key
            try:
                old = await self.api.read_namespaced_pod(name, self.namespace)
                if int(old.metadata.labels["weknora.agent.epoch"]) > self.payload.owner_epoch:
                    raise asyncio.CancelledError("Newer workspace owner exists")
                await PodContainer(self.api,self.namespace,name,old.metadata.uid).delete()
            except ApiException as exc:
                if exc.status != 404:
                    raise
            spec = pod_spec(self.payload, self.key)
            for prefix in ("agent-workspace-", "agent-receipts-"):
                storage = {"accessModes":["ReadWriteOnce"],"resources":{"requests":{"storage":os.environ.get("AGENT_WORKSPACE_STORAGE_SIZE", "4Gi")}}}
                if os.environ.get("AGENT_WORKSPACE_STORAGE_CLASS"):
                    storage["storageClassName"] = os.environ["AGENT_WORKSPACE_STORAGE_CLASS"]
                try:
                    await self.api.create_namespaced_persistent_volume_claim(self.namespace, {
                        "apiVersion":"v1","kind":"PersistentVolumeClaim",
                        "metadata":{"name":prefix+self.key,"labels":spec["metadata"]["labels"]},"spec":storage})
                except ApiException as exc:
                    if exc.status != 409:
                        raise
            await self.control.post("runs/heartbeat")
            try:
                pod = await self.api.create_namespaced_pod(self.namespace, spec)
            except ApiException as exc:
                if exc.status == 409:
                    raise asyncio.CancelledError("Workspace provisioning is owned by another worker") from exc
                raise
            self.container = PodContainer(self.api,self.namespace,name,pod.metadata.uid)
            async with asyncio.timeout(180):
                while True:
                    pod = await self.api.read_namespaced_pod(name,self.namespace)
                    if pod.metadata.uid != self.container.uid:
                        raise asyncio.CancelledError("Workspace ownership changed")
                    if pod.status.phase == "Running" and pod.status.container_statuses and all(c.ready for c in pod.status.container_statuses):
                        break
                    if pod.status.phase in ("Failed","Succeeded"):
                        raise RuntimeError("Workspace terminated during startup")
                    await asyncio.sleep(.5)
            await self.control.post("runs/heartbeat")

    async def close(self):
        try:
            if self.container:
                await self.container.delete()
                self.container = None
            if self.api:
                with contextlib.suppress(Exception):
                    status = await self.control.post("runs/status")
                    if status.get("status") in ("completed","failed","cancelled","incomplete"):
                        for prefix in ("agent-workspace-", "agent-receipts-"):
                            await self.api.delete_namespaced_persistent_volume_claim(prefix+self.key,self.namespace)
        finally:
            if self.api_client:
                await self.api_client.close()
                self.api_client = None
