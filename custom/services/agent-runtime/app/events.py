"""Bounded ordered event batches; critical records use explicit barriers."""
from __future__ import annotations

import asyncio

from agentscope.event import ModelCallStartEvent, TextBlockDeltaEvent, ThinkingBlockDeltaEvent
from agentscope.message import Msg

from .contracts import RunEvent
from .control import Control


class Events:
    def __init__(self, control: Control):
        self.control = control
        self.pending: list[dict] = []
        self.pending_bytes = 0
        self.seq = int((control.payload.checkpoint or {}).get("event_seq", 0))
        self.revision = int((control.payload.checkpoint or {}).get("revision", 0))
        self.lock = asyncio.Lock()
        self.first_text = True

    async def sdk(self, item) -> None:
        if isinstance(item, Msg):
            return
        if isinstance(item, ModelCallStartEvent):
            self.revision += 1
        if isinstance(item, TextBlockDeltaEvent):
            event = RunEvent(type="answer_delta", content=item.delta,
                             message_id=item.reply_id, revision=self.revision,
                             id=item.id, data={"candidate": True})
        elif isinstance(item, ThinkingBlockDeltaEvent):
            event = RunEvent(type="thought_delta", content=item.delta, revision=self.revision, id=item.id)
        else:
            event = RunEvent(type="sdk", id=item.id, revision=self.revision,
                             data={"event_type":item.type})
        first = isinstance(item, TextBlockDeltaEvent) and self.first_text
        if first:
            self.first_text = False
        await self.push(event, flush=first)

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
