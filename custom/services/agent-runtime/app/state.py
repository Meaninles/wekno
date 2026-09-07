"""Persist SDK state only at consistent boundaries; never run a second loop."""
from __future__ import annotations

import asyncio
import contextlib
import json
import time
from typing import Any

from agentscope.middleware import MiddlewareBase
from agentscope.message import TextBlock, ToolCallBlock, ToolResultBlock
from agentscope.tool import ToolResponse
from agentscope.state import AgentState

from .control import Control
from .contracts import RunEvent
from .tools import invocation_id

SDK_VERSION = "2.0.7.post1"


def final_decision(message):
    """SDK merges a reply's decisions and tool results into one message."""
    if message is None or message.role != "assistant":
        return None
    start = max((i + 1 for i, block in enumerate(message.content) if isinstance(block, ToolResultBlock)), default=0)
    content = message.content[start:]
    if any(isinstance(block, ToolCallBlock) for block in content) or not any(isinstance(block, TextBlock) for block in content):
        return None
    return message.model_copy(update={"content": content})


def restore(checkpoint: dict[str, Any] | None, run_id: str) -> AgentState:
    if checkpoint is None:
        return AgentState(session_id=run_id)
    if checkpoint.get("sdk_version") != SDK_VERSION or checkpoint.get("run_id") != run_id:
        raise ValueError("Run checkpoint version or identity does not match")
    return AgentState.model_validate(checkpoint["agent"])


class Lifecycle(MiddlewareBase):
    def __init__(self, control: Control):
        self.control = control
        self.lock = asyncio.Lock()
        self.events = None
        self.resume_boundary = (control.payload.checkpoint or {}).get("boundary")
        self.checkpoint_seq = (control.payload.checkpoint or {}).get("checkpoint_seq", 0)

    async def save(self, agent, boundary: str) -> None:
        async with self.lock:
            if self.events:
                await self.events.flush()
            self.checkpoint_seq += 1
            await self.control.checkpoint({"sdk_version": SDK_VERSION,
                                           "checkpoint_seq": self.checkpoint_seq,
                                           "boundary": boundary,
                                           "run_id": self.control.payload.run_id,
                                           "event_seq": self.events.seq if self.events else 0,
                                           "revision": self.events.revision if self.events else 0,
                                           "agent": agent.state.model_dump(mode="json")}, boundary=boundary)

    async def on_reasoning(self, agent, input_kwargs, next_handler):
        boundary, self.resume_boundary = self.resume_boundary, None
        if boundary in ("model_decision", "before_commit"):
            candidate = agent.state.context[-1] if agent.state.context else None
            candidate = final_decision(candidate)
            if candidate is not None:
                # The provider already completed this decision before the
                # process died. Let the SDK deliver it without buying it again.
                yield candidate
                return
        # At entry the previous complete tool batch has been merged. At exit
        # the new model decision is saved by the SDK, before tools execute.
        await self.save(agent, "before_reasoning")
        async for item in next_handler(**input_kwargs):
            yield item
        await self.save(agent, "model_decision")

    async def on_acting(self, agent, input_kwargs, next_handler):
        call = input_kwargs["tool_call"]
        token = invocation_id.set(call.id)
        local = call.name not in {tool.name for tool in self.control.payload.tools}
        started = time.monotonic()
        try:
            if local:
                await self.events.push(RunEvent(type="local_tool_start", id=call.id, data={
                    "tool_call_id": call.id, "tool_name": call.name,
                    "arguments": json.loads(call.input) if isinstance(call.input, str) else call.input}), flush=True)
            async for item in next_handler(**input_kwargs):
                if local and isinstance(item, ToolResponse):
                    output = "\n".join(block.text for block in item.content if isinstance(block, TextBlock))[:50000]
                    await self.events.push(RunEvent(type="local_tool_result", id=call.id, data={
                        "tool_call_id": call.id, "tool_name": call.name, "output": output,
                        "success": str(item.state) == "success", "duration": int((time.monotonic()-started)*1000)}), flush=True)
                yield item
        finally:
            # SDK cancellation can finalize an async generator in a separate
            # context. Its context dies with that task; never mask the error.
            with contextlib.suppress(ValueError):
                invocation_id.reset(token)
