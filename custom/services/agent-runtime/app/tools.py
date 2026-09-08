"""One JSON-schema bridge for all authorized business tools."""
from __future__ import annotations

import base64
import asyncio
import contextvars
import hashlib
import json
from typing import Any

from agentscope.message import Base64Source, DataBlock, TextBlock, ToolResultState, URLSource
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.tool import ToolBase, ToolChunk
from jsonschema import Draft202012Validator

from .contracts import RuntimeToolSpec
from .control import Control, ControlUnavailable


invocation_id: contextvars.ContextVar[str] = contextvars.ContextVar("invocation_id", default="")


def data_shape(value, depth=0):
    """Bounded structural description; never infer complete search coverage."""
    if isinstance(value, dict):
        if depth >= 3:
            return {"type": "object", "keys": list(value)[:24]}
        return {key: data_shape(item, depth + 1) for key, item in list(value.items())[:24]}
    if isinstance(value, list):
        return {"type": "array", "count": len(value),
                "first_item": data_shape(value[0], depth + 1) if value and depth < 3 else None}
    if value is None:
        return "null"
    return type(value).__name__


def citation_catalog(references):
    # Citation identity is part of the source-data protocol, not optional
    # metadata: provider formatters do not send ToolChunk.metadata to models.
    return [{key: ref[key] for key in ("id", "cite_exactly", "chunk_id", "title") if key in ref}
            for ref in references if isinstance(ref, dict)]


def media_block(value: str) -> DataBlock:
    if value.startswith("data:"):
        header, encoded = value.split(",", 1)
        if not header.endswith(";base64"):
            raise ValueError("Only base64 data URLs are supported")
        base64.b64decode(encoded, validate=True)
        return DataBlock(source=Base64Source(media_type=header[5:-7], data=encoded))
    if not value.startswith(("https://", "http://")):
        raise ValueError("Unsupported media reference")
    return DataBlock(source=URLSource(url=value, media_type="image/*"))


class BusinessTool(ToolBase):
    def __init__(self, spec: RuntimeToolSpec, control: Control, data_backend=None, concurrency=None):
        super().__init__()
        self.name = spec.name
        self.description = spec.description
        self.input_schema = spec.parameters
        self.is_read_only = spec.is_read_only
        self.is_concurrency_safe = spec.is_concurrency_safe
        self.control = control
        self.data_backend = data_backend
        self.concurrency = concurrency or asyncio.Semaphore(4)
        self.validator = Draft202012Validator(self.input_schema)

    async def check_permissions(self, tool_input, context):
        # The server-authored catalog grants the capability. Live resource
        # authorization and transactional approvals remain in its Go tool.
        return PermissionDecision(PermissionBehavior.ALLOW, "Authorized business tool")

    async def call(self, **arguments: Any) -> ToolChunk:
        self.validator.validate(arguments)
        call_id = invocation_id.get()
        if not call_id:
            raise RuntimeError("SDK tool call identity is missing")
        try:
            async with self.concurrency:
                result = await self.control.tool(self.name, arguments, call_id)
        except ControlUnavailable as exc:
            import asyncio
            raise asyncio.CancelledError("Recover pending business tool from its receipt") from exc
        text = str(result.get("output") or "")
        if not text:
            text = json.dumps(result.get("data") or {}, ensure_ascii=False)
        if result.get("error"):
            text += "\nTool error: " + str(result["error"])
        data_file = None
        if self.data_backend is not None and result.get("success"):
            source = json.dumps({"tool": self.name, **result}, ensure_ascii=False).encode("utf-8")
            if len(source) >= 8192:
                # Authorized source data goes directly to the same run's
                # workspace, so exports need not retranscribe it through model
                # output. The model receives a locator and bounded preview;
                # the complete evidence remains available in the run workspace.
                identity = hashlib.sha256(call_id.encode()).hexdigest()[:24]
                data_file = f"/workspace/source-data/{identity}.json"
                await self.data_backend.write_file(data_file, source)
                manifest = {"path": data_file, "sha256": hashlib.sha256(source).hexdigest(),
                            "bytes": len(source), "complete_tool_response": True,
                            "data_json_pointer": "/data", "data_shape": data_shape(result.get("data"))}
                text = ("Source file: " + json.dumps(manifest, ensure_ascii=False) + "\n"
                        "UTF-8 JSON: data holds structured values; output holds tool text; source_references holds citations. "
                        "Complete response does not imply full search coverage; honor pagination in data. "
                        "Load in code: source=json.load(open(path)); data=source['data']. "
                        "Record fields live inside data, not at the JSON root.\n"
                        "Current citation handles (match these to the inspected source records): " +
                        json.dumps(citation_catalog(result.get("source_references") or []), ensure_ascii=False) +
                        "\nThis is a partial preview:\n" + text[:1600])
        if result.get("citation_output_contract"):
            text = str(result["citation_output_contract"]) + "\n" + text
        blocks = [TextBlock(text=text)]
        blocks.extend(media_block(value) for value in result.get("images") or [])
        return ToolChunk(content=blocks,
                         state=ToolResultState.SUCCESS if result.get("success") else ToolResultState.ERROR,
                         metadata={"receipt": result.get("receipt"),
                                   "source_data_file": data_file,
                                   "source_references": result.get("source_references") or []})
