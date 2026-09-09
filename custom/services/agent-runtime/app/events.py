"""Bounded ordered event batches; critical records use explicit barriers."""
from __future__ import annotations

import asyncio

from agentscope.event import ModelCallStartEvent, ModelCallEndEvent, TextBlockDeltaEvent, ThinkingBlockDeltaEvent, ToolCallStartEvent, ToolCallDeltaEvent
from agentscope.message import Msg

from .contracts import RunEvent
from .control import Control
from .answer import ANSWER_TOOL, AnswerStream, DeliveryError


class Events:
    def __init__(self, control: Control):
        self.control = control
        self.pending: list[dict] = []
        self.pending_bytes = 0
        self.seq = int((control.payload.checkpoint or {}).get("event_seq", 0))
        self.revision = int((control.payload.checkpoint or {}).get("revision", 0))
        self.lock = asyncio.Lock()
        self.first_text = True
        self.answer_streams = {}
        self.decision_tools = []
        self.thought_blocks = set()
        self.text_blocks = {}

    async def sdk(self, item) -> None:
        if isinstance(item, Msg):
            return
        if isinstance(item, ModelCallStartEvent):
            await self.close_thoughts()
            self.revision += 1
            self.answer_streams.clear()
            self.decision_tools.clear()
            self.text_blocks.clear()
            await self.push(RunEvent(type="process_status", id="model", content="正在思考"), flush=True)
        if isinstance(item, TextBlockDeltaEvent):
            # Hold ordinary text until the decision proves it is a business-tool
            # preamble. Final candidates and invalid decisions stay private.
            self.text_blocks[item.block_id] = (self.text_blocks.get(item.block_id, "") + item.delta)[:12000]
            return
        if isinstance(item, ModelCallEndEvent):
            await self.close_thoughts()
            if self.decision_tools and ANSWER_TOOL not in self.decision_tools:
                for block, text in self.text_blocks.items():
                    if text.strip():
                        await self.push(RunEvent(type="commentary", id=block, content=text, done=True))
            self.text_blocks.clear()
        if isinstance(item, ToolCallStartEvent):
            self.decision_tools.append(item.tool_call_name)
            if ANSWER_TOOL in self.decision_tools and len(self.decision_tools) != 1:
                raise DeliveryError("Final answer must be submitted separately from business actions")
            if item.tool_call_name == ANSWER_TOOL:
                limit = self.control.payload.runtime_config.max_completion_tokens or 8192
                self.answer_streams[item.tool_call_id] = AnswerStream(limit * 16)
        final = self.answer_streams.get(getattr(item, "tool_call_id", ""))
        if final is not None and isinstance(item, ToolCallDeltaEvent):
            final.feed(item.delta)
            # The complete candidate is filtered by the backend before publishing.
            return
        elif final is not None:
            return
        elif isinstance(item, ThinkingBlockDeltaEvent):
            self.thought_blocks.add(item.block_id)
            event = RunEvent(type="thought_delta", content=item.delta, revision=self.revision, id=item.block_id)
        else:
            event = RunEvent(type="sdk", id=item.id, revision=self.revision,
                             data={"event_type":item.type})
        first = event.type == "answer_delta" and self.first_text
        if first:
            self.first_text = False
        await self.push(event, flush=first)

    async def close_thoughts(self):
        for block in sorted(self.thought_blocks):
            await self.push(RunEvent(type="thought_delta", id=block, done=True))
        self.thought_blocks.clear()

    async def push(self, event: RunEvent, *, flush: bool = False) -> None:
        async with self.lock:
            self.seq += 1
            event.seq = self.seq
            if not event.revision:
                event.revision = self.revision
            self.pending.append(event.model_dump(mode="json"))
            self.pending_bytes += len(event.model_dump_json().encode())
            if flush or self.pending_bytes >= 4096:
                await self._flush()

    async def _flush(self) -> None:
        if self.pending:
            await self.control.events(self.pending)
            self.pending = []
            self.pending_bytes = 0

    async def flush(self) -> None:
        async with self.lock:
            await self._flush()

    async def pump(self) -> None:
        while True:
            await asyncio.sleep(.05)
            await self.flush()
