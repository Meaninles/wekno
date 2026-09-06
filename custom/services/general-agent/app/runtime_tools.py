"""One tool catalog and validator for platform and SDK execution."""
import asyncio
import contextvars
import json
import uuid

from jsonschema import Draft202012Validator

from .workspace import Workspace
from .working_context import ToolResultStore

tool_call_id = contextvars.ContextVar("tool_call_id", default="")
tool_call_name = contextvars.ContextVar("tool_call_name", default="")


class RuntimeTools:
    def __init__(self, payload, artifacts):
        from .runner import effective_weknora_tool_specs
        self.payload = payload
        self.entries = {}
        self.sdk_bindings = {}
        self.sdk_executions = {}
        self.workspace = Workspace(artifacts, self.callback)
        self.result_store = ToolResultStore(artifacts.run_dir)
        from .original_sources import OriginalSources
        self.originals = OriginalSources(payload, artifacts.run_dir)
        if self.originals.sources:
            self.entries["materialize_file"] = (
                "Download and verify an authorized original file into this run's workspace on demand. Returns its local path, size and SHA256. Use file_id from original_file_sources; repeated calls reuse the verified local file.",
                {"type": "object", "properties": {"file_id": {"type": "string", "minLength": 1}}, "required": ["file_id"], "additionalProperties": False},
                self.originals.materialize, "workspace")
        for spec in effective_weknora_tool_specs(payload):
            async def handler(args, name=spec.name):
                if name == "inspect_image":
                    return await self.workspace.inspect_image(args)
                return await self.callback(name, args)
            parameters = spec.parameters
            if spec.name == "inspect_image":
                from .image_inspection import LOCAL_IMAGE_SCHEMA
                parameters = LOCAL_IMAGE_SCHEMA
            self.entries[spec.name] = (spec.description, parameters, handler, "platform")
        for name, description, parameters, handler in self.workspace.tools():
            if not payload.enable_artifacts and name not in {"read_file", "list_files"}:
                continue
            if name in self.entries:
                raise ValueError(f"duplicate runtime tool: {name}")
            self.entries[name] = (description, parameters, handler, "workspace")

    async def callback(self, name, args):
        from .runner import call_tool_callback
        # Nested inspections have their own execution identity, while direct
        # model tool calls preserve the ID emitted by the model protocol.
        return await asyncio.to_thread(call_tool_callback, self.payload, name, args,
            (tool_call_id.get() if tool_call_name.get()==name else "") or str(uuid.uuid4()))

    def definitions(self):
        return [{"type":"function", "function":{"name":name,"description":desc,"parameters":params}}
            for name,(desc,params,_,_) in self.entries.items()]

    async def execute(self, name, args, call_id):
        entry = self.entries.get(name)
        if entry is None:
            return {"success":False,"error":f"tool {name!r} is not in the current catalog"}
        errors = sorted(Draft202012Validator(entry[1]).iter_errors(args),key=lambda x:str(x.path))
        if errors:
            return {"success":False,"error":"invalid arguments", "fields":[
                {"path":"/"+"/".join(str(p) for p in err.path),"error":err.message} for err in errors]}
        token=tool_call_id.set(call_id)
        name_token=tool_call_name.set(name)
        try:
            result = await entry[2](args)
            return result
        except Exception as exc:
            return {"success":False,"error":str(exc)}
        finally:
            tool_call_id.reset(token)
            tool_call_name.reset(name_token)

    async def sdk_before_tool(self, event, tool_use_id, _context):
        from .protocol_capture import capture
        capture(self.payload.run_id,"sdk_tool_hook",event)
        name = event["tool_name"].removeprefix("mcp__weknora__")
        call_id = event.get("tool_use_id") or tool_use_id
        if name not in self.entries or not call_id:
            raise ValueError("SDK tool event has no catalog/identity binding")
        args = dict(event["tool_input"])
        args.pop("__weknora_transport_binding", None)
        binding = str(uuid.uuid4())
        self.sdk_bindings[binding] = (name, call_id, args)
        return {"hookSpecificOutput": {"hookEventName": "PreToolUse",
            "updatedInput": {**args, "__weknora_transport_binding": binding}}}

    async def sdk_execute(self, name, arguments):
        args = dict(arguments)
        token = args.pop("__weknora_transport_binding", "")
        bound = self.sdk_bindings.get(token)
        if bound is None or bound[0] != name or bound[2] != args:
            return {"success": False, "error": "SDK execution identity was not supplied by its tool hook"}
        from .protocol_capture import capture
        capture(self.payload.run_id,"mcp_execution",{"tool_name":name,"tool_call_id":bound[1],"arguments":args})
        # Replayed MCP transport requests share one execution, including two
        # simultaneous deliveries. Identical arguments in distinct model calls
        # remain distinct; there is no name/argument heuristic correlation.
        if bound[1] not in self.sdk_executions:
            self.sdk_executions[bound[1]] = asyncio.create_task(self.execute(name, args, bound[1]))
        result = await self.sdk_executions[bound[1]]
        capture(self.payload.run_id,"mcp_result",{"tool_call_id":bound[1],"result":result})
        return result

    def sdk_server(self):
        from mcp.server import Server
        from mcp.types import Tool, CallToolResult
        from .runner import mcp_tool_result, mcp_text
        server = Server("weknora", version="2.0.0")
        @server.list_tools()
        async def list_tools():
            return [Tool(name=name, description=entry[0], inputSchema=entry[1]) for name,entry in self.entries.items()]
        # Strip the hook-owned transport envelope before the shared validator.
        # The catalog seen by the model stays identical to the platform catalog.
        @server.call_tool(validate_input=False)
        async def call_tool(name, arguments):
            result = await self.sdk_execute(name, arguments)
            origin = self.entries.get(name,(None,None,None,"workspace"))[3]
            projected = mcp_tool_result(result,self.payload.query) if origin=="platform" else mcp_text(result,is_error=result.get("success") is False)
            for block in projected["content"]:
                if block["type"] == "text":
                    block["text"] = self.result_store.view(block["text"], name)
            return CallToolResult.model_validate({"content": projected["content"], "isError": projected.get("is_error",False)})
        return {"type":"sdk", "name":"weknora", "instance":server}

    def model_result(self, result, tool_name=""):
        # Both adapters use the same compact evidence projection. Binary images
        # travel in the typed message field, never inside a JSON text prompt.
        from .runner import mcp_tool_result
        if "output" in result or "source_references" in result:
            projected=mcp_tool_result(result)
            text = "\n".join(block["text"] for block in projected["content"] if block["type"]=="text")
        else:
            text = json.dumps(result,ensure_ascii=False)
        return self.result_store.view(text, tool_name)
