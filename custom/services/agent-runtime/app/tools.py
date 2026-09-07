"""One JSON-schema bridge for all authorized business tools."""
from __future__ import annotations

import base64
import contextvars
import json
from typing import Any

from agentscope.message import Base64Source, DataBlock, TextBlock, ToolResultState, URLSource
from agentscope.permission import PermissionBehavior, PermissionDecision
from agentscope.tool import ToolBase, ToolChunk
from jsonschema import Draft202012Validator

from .contracts import RuntimeToolSpec
from .control import Control, ControlUnavailable


invocation_id: contextvars.ContextVar[str] = contextvars.ContextVar("invocation_id", default="")


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
    def __init__(self, spec: RuntimeToolSpec, control: Control):
        super().__init__()
        self.name = spec.name
        self.description = spec.description
        self.input_schema = spec.parameters
        self.is_read_only = spec.is_read_only
        self.is_concurrency_safe = spec.is_concurrency_safe
        self.control = control
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
            result = await self.control.tool(self.name, arguments, call_id)
        except ControlUnavailable as exc:
            import asyncio
            raise asyncio.CancelledError("Recover pending business tool from its receipt") from exc
        text = str(result.get("output") or "")
        if not text:
            text = json.dumps(result.get("data") or {}, ensure_ascii=False)
        if result.get("error"):
            text += "\nTool error: " + str(result["error"])
        if result.get("citation_output_contract"):
            text += "\n" + str(result["citation_output_contract"])
        blocks = [TextBlock(text=text)]
        blocks.extend(media_block(value) for value in result.get("images") or [])
        return ToolChunk(content=blocks,
                         state=ToolResultState.SUCCESS if result.get("success") else ToolResultState.ERROR,
                         metadata={"receipt": result.get("receipt"),
                                   "source_references": result.get("source_references") or []})
