"""A single deterministic delivery contract inside the SDK reply lifecycle."""
from __future__ import annotations

import json
import time
import asyncio

from agentscope.event import ReplyEndEvent
from agentscope.message import Msg, TextBlock
from agentscope.middleware import MiddlewareBase
from agentscope.types import ReplyFinishedReason

from .contracts import RunResult
from .control import Control
from .state import Lifecycle, final_decision
from .budget import BudgetExhausted


class DeliveryError(RuntimeError):
    pass


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
            candidate = next((msg for msg in reversed(agent.state.context) if msg.role == "assistant"), None)
            candidate = final_decision(candidate)
            if item.finished_reason == ReplyFinishedReason.EXCEED_MAX_ITERS and candidate is None:
                raise BudgetExhausted("Iteration budget exhausted without a final answer")
            structured = agent.state.reply_context.structured_output
            answer = (json.dumps(structured, ensure_ascii=False) if structured is not None else
                      "\n".join(block.text for block in candidate.content if isinstance(block, TextBlock))
                      if candidate is not None else "")
            result = RunResult(run_id=self.control.payload.run_id, answer=answer,
                               timings={"runtime_ms": (time.monotonic() - self.started) * 1000})
            validation = await self.control.validate(result.model_dump(mode="json"))
            violations = validation.get("violations") or []
            if violations:
                if agent.state.cur_iter >= self.control.payload.runtime_config.max_iterations:
                    raise BudgetExhausted("Iteration budget exhausted without a validated final answer: " + json.dumps(violations))
                signature = json.dumps(violations, ensure_ascii=False, sort_keys=True)
                attempts = agent.state.middle_context.setdefault("delivery_violations", [])
                if signature in attempts or len(attempts) >= 2:
                    raise DeliveryError("Delivery requirements could not be satisfied: " + signature)
                attempts.append(signature)
                agent.state.context.append(Msg(name="delivery", role="user", content=[TextBlock(
                    text="Internal delivery validation feedback, not a user message. Revise the final answer to resolve these errors. Do not quote this feedback, blame the user, describe internal source handles, or add an apology about validation: " + signature)]))
                await self.lifecycle.save(agent, "delivery_feedback")
                # Suppress the end event: the SDK continues this same reply.
                continue
            result = RunResult.model_validate(validation.get("result") or result.model_dump())
            await self.lifecycle.save(agent, "before_commit")
            if self.events:
                await self.events.flush()
            committed = await self.control.commit(result.model_dump(mode="json"))
            self.result = RunResult.model_validate(committed.get("result") or result.model_dump())
            yield item
