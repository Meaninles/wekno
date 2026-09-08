"""A single deterministic delivery contract inside the SDK reply lifecycle."""
from __future__ import annotations

import time
import asyncio

from agentscope.event import ReplyEndEvent
from agentscope.middleware import MiddlewareBase
from agentscope.types import ReplyFinishedReason

from .contracts import RunResult
from .control import Control
from .state import Lifecycle
from .budget import BudgetExhausted
from .answer import Answer, DeliveryError


class Delivery(MiddlewareBase):
    def __init__(self, control: Control, lifecycle: Lifecycle):
        self.control = control
        self.lifecycle = lifecycle
        self.started = time.monotonic()
        self.result: RunResult | None = None
        self.events = None

    async def on_reply(self, agent, input_kwargs, next_handler):
        async for item in next_handler(**input_kwargs):
            if not isinstance(item, ReplyEndEvent):
                yield item
                continue
            if item.finished_reason == ReplyFinishedReason.INTERRUPTED:
                # SDK emits the interruption end event from its finally block.
                # Preserve cancellation instead of replacing it with a failure;
                # a worker shutdown must leave the durable run recoverable.
                raise asyncio.CancelledError("SDK reply interrupted")
            if item.finished_reason not in (ReplyFinishedReason.COMPLETED, ReplyFinishedReason.EXCEED_MAX_ITERS):
                # Never convert exceed-limit/error/interrupt into a success.
                raise DeliveryError(f"Run did not complete: {item.finished_reason}")
            structured = agent.state.reply_context.structured_output
            if item.finished_reason == ReplyFinishedReason.EXCEED_MAX_ITERS and structured is None:
                raise BudgetExhausted("Iteration budget exhausted without a final answer")
            try:
                answer = Answer.model_validate(structured)
            except ValueError as exc:
                raise DeliveryError("Invalid final answer structure or empty response") from exc
            result = RunResult(run_id=self.control.payload.run_id, answer=answer.answer, citations=answer.citations,
                               timings={"runtime_ms": (time.monotonic() - self.started) * 1000})
            await self.lifecycle.save(agent, "before_commit")
            self.result = result
            yield item
