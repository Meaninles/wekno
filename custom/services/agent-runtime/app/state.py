"""Persist SDK state only at consistent boundaries; never run a second loop."""
from __future__ import annotations

import asyncio
import contextlib
import json
import time
import httpx
from typing import Any

from agentscope.middleware import MiddlewareBase
from agentscope.event import ModelCallEndEvent, ReplyStartEvent
from agentscope.message import TextBlock, ToolCallBlock, ToolResultBlock
from agentscope.tool import ToolResponse
from agentscope.state import AgentState

from .control import Control, control_cause
from .contracts import RunEvent
from .models import ProviderStreamInterrupted
from .tools import invocation_id
from .answer import ANSWER_TOOL, validate_decision

SDK_VERSION = "2.0.7.post1"


def model_interrupted(error):
    if control_cause(error) is not None:
        return False
    seen = set()
    while error is not None and id(error) not in seen:
        seen.add(id(error))
        if isinstance(error, (httpx.NetworkError, httpx.TimeoutException,
                              httpx.RemoteProtocolError, ProviderStreamInterrupted)):
            return True
        error = error.__cause__
    return False


def normalize_failed_tool_arguments(state):
    """Keep failed calls replayable through strict provider chat templates.

    Raw malformed arguments belong to failure metadata. The historical call
    keeps its identity and failed result, with an empty JSON object on the wire.
    This never repairs or executes a tool, or changes successful tool inputs.
    """
    failures = {block.id: block for message in state.context for block in message.content
                if isinstance(block, ToolResultBlock) and str(block.state) == "error"}
    for message in state.context:
        for block in message.content:
            if not isinstance(block, ToolCallBlock) or block.id not in failures:
                continue
            try:
                valid = isinstance(json.loads(block.input), dict)
            except (ValueError, TypeError):
                valid = False
            if not valid:
                failures[block.id].metadata["invalid_arguments"] = block.input
                block.input = "{}"


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
        self.workspace = None
        self.checkpoint_seq = (control.payload.checkpoint or {}).get("checkpoint_seq", 0)

    async def on_reply(self, agent, input_kwargs, next_handler):
        # reply_stream(None) starts a new SDK reply by default. A durable
        # resume must retain its reply identity, iteration count and result.
        resumed = agent.state.reply_context.model_copy(deep=True) if self.control.payload.checkpoint else None
        async for item in next_handler(**input_kwargs):
            if isinstance(item, ReplyStartEvent) and resumed is not None:
                agent.state.reply_context = resumed
                item.reply_id = resumed.reply_id
            yield item

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
        # At entry the previous complete tool batch has been merged. At exit
        # the new model decision is saved by the SDK, before tools execute.
        normalize_failed_tool_arguments(agent.state)
        await self.save(agent, "before_reasoning")
        while True:
            completed = False
            try:
                # SDK does not save response blocks or execute tools until
                # the whole decision completes. Each new attempt emits its
                # own ModelCallStart/revision and acquires fresh model budget.
                async for item in next_handler(**input_kwargs):
                    completed |= isinstance(item, ModelCallEndEvent)
                    yield item
                break
            except Exception as exc:
                retries = agent.state.middle_context.get("model_stream_retries", 0)
                if completed or retries >= 2 or not model_interrupted(exc):
                    raise
                # Persist the retry allowance at the existing safe boundary,
                # so worker recovery cannot restart an unlimited retry loop.
                agent.state.middle_context["model_stream_retries"] = retries + 1
                await self.save(agent, "before_reasoning")
                await asyncio.sleep(.5 * (retries + 1))
        agent.state.middle_context.pop("model_stream_retries", None)
        if asyncio.current_task().cancelling():
            raise asyncio.CancelledError("Model decision interrupted")
        validate_decision(agent.state.context[-1] if agent.state.context else None)
        await self.save(agent, "model_decision")

    async def on_acting(self, agent, input_kwargs, next_handler):
        call = input_kwargs["tool_call"]
        if call.name == ANSWER_TOOL:
            validate_decision(agent.state.context[-1])
            async for item in next_handler(**input_kwargs):
                yield item
            return
        token = invocation_id.set(call.id)
        local = call.name not in {tool.name for tool in self.control.payload.tools}
        started = time.monotonic()
        try:
            try:
                arguments = json.loads(call.input) if isinstance(call.input, str) else call.input
                if not isinstance(arguments, dict):
                    arguments = None
            except (ValueError, TypeError):
                arguments = None

            async def responses():
                if arguments is None:
                    # Malformed model output is a recoverable tool error, not
                    # a lifecycle failure. Do not guess or repair mutation args.
                    yield ToolResponse(id=call.id, state="error", content=[TextBlock(text=
                        "Tool arguments must be a complete JSON object. No tool was executed. "
                        "Submit a complete, smaller tool call within the remaining budget.")])
                    return
                if local and self.workspace is not None:
                    try:
                        await self.workspace.prepare_tool(call.name, arguments)
                    except (OSError, ValueError, httpx.HTTPError):
                        yield ToolResponse(id=call.id, state="error", content=[TextBlock(text=
                            "Required workspace input could not be prepared or failed its integrity check. "
                            "No tool was executed. Use other available sources or report the missing input.")])
                        return
                async for response in next_handler(**input_kwargs):
                    yield response

            if local:
                await self.events.push(RunEvent(type="local_tool_start", id=call.id, data={
                    "tool_call_id": call.id, "tool_name": call.name,
                    "arguments": arguments or {}}), flush=True)
            async for item in responses():
                if local and isinstance(item, ToolResponse):
                    output = "\n".join(block.text for block in item.content if isinstance(block, TextBlock))[:50000]
                    await self.events.push(RunEvent(type="local_tool_result", id=call.id, data={
                        "tool_call_id": call.id, "tool_name": call.name, "output": output,
                        "success": str(item.state) == "success", "duration": int((time.monotonic()-started)*1000)}), flush=True)
                yield item
        finally:
            if local and self.workspace is not None:
                self.workspace.tool_finished(call.name)
            # SDK cancellation can finalize an async generator in a separate
            # context. Its context dies with that task; never mask the error.
            with contextlib.suppress(ValueError):
                invocation_id.reset(token)
