"""One budget for the SDK lifecycle, including its finalization decision."""
from agentscope.message import Msg, TextBlock
from agentscope.middleware import MiddlewareBase
from agentscope.tool import ToolChoice
from .control import control_cause
from .models import finalizing_call
from .context_projection import transient_messages


class BudgetExhausted(RuntimeError):
    pass


class IterationBudget(MiddlewareBase):
    def __init__(self, limit: int, control):
        self.limit = limit
        self.control = control

    async def on_acting(self, agent, input_kwargs, next_handler):
        if agent.state.cur_iter >= self.limit - 1 or agent.state.middle_context.get("budget_finalizing"):
            raise BudgetExhausted("Iteration budget reserved for final answer; further tool calls are not allowed")
        async for item in next_handler(**input_kwargs):
            yield item

    async def on_model_call(self, agent, input_kwargs, next_handler):
        remaining = self.limit - agent.state.cur_iter
        if remaining <= 0:
            raise BudgetExhausted("Iteration budget exhausted without a validated final answer")
        instruction = (
            f"Runtime iteration budget: maximum={self.limit}; "
            f"completed={agent.state.cur_iter}; remaining={remaining} including this decision. "
            "This is a ceiling, not a target. Finish immediately when evidence is sufficient. "
            "Reserve the last decision for the final answer. Preserve accuracy and citations; "
            "state unresolved questions honestly. Do not describe this internal budget to the user. "
            "Before spending another decision, distinguish required unfinished work from optional refinement. "
            "Complete requested functionality and required checks first; once these pass, deliver. "
            "Repeat a check only after a relevant change or an unresolved failure. "
        )
        budget = await self.control.budget()
        tokens = budget["remaining_tokens"]
        requests = budget["remaining_requests"]
        seconds = budget["remaining_seconds"]
        reserve = budget["final_reserve"]
        instruction += (f"Shared token budget: maximum={budget['max_tokens']}, remaining={tokens}; "
                        f"model requests: maximum={budget['max_requests']}, remaining={requests}; "
                        f"time remaining={seconds}s; final-answer reserve={reserve} tokens. "
                        "Auxiliary model calls consume this same budget. ")
        output_limit = self.control.payload.runtime_config.max_completion_tokens or 8192
        instruction += (f"Single-response output limit={output_limit} tokens, shared by text and all tool arguments. "
                        "Keep each tool call complete within this limit. Build long files through smaller incremental "
                        "writes or edits, preserving all required content. ")
        final = remaining == 1 or tokens <= 2 * reserve or requests <= 1 or seconds <= 30 or agent.state.middle_context.get("budget_finalizing", False)
        if remaining <= 3 or tokens <= 4 * reserve or requests <= 3 or seconds <= 120:
            instruction += "Begin finalization now. Avoid new investigations; finish essential work and save deliverables before the final decision. "
        if final:
            agent.state.middle_context["budget_finalizing"] = True
            instruction += "FINAL DECISION: return the final answer now, without tools. Do not claim unfinished work is complete."
            input_kwargs = {**input_kwargs, "tool_choice": ToolChoice(mode="none"), "tools": []}
        call_kwargs = {
            **input_kwargs,
            "messages": transient_messages(input_kwargs["messages"], Msg(name="runtime_budget", role="system", content=[TextBlock(text=instruction)])),
        }
        token = finalizing_call.set(final)
        try:
            try:
                return await next_handler(**call_kwargs)
            except Exception as exc:
                cause = control_cause(exc)
                if cause is None:
                    raise
                if cause.code == "finalization_required" and not final:
                    # Admission rejected before any provider generation. The same
                    # SDK decision can use its protected allowance without tools.
                    finalizing_call.set(True)
                    agent.state.middle_context["budget_finalizing"] = True
                    call_kwargs.update(tool_choice=ToolChoice(mode="none"), tools=[])
                    budget_index = next(i for i, msg in enumerate(call_kwargs["messages"]) if msg.name == "runtime_budget")
                    call_kwargs["messages"][budget_index] = Msg(name="runtime_budget", role="system", content=[TextBlock(
                        text=instruction + " FINAL DECISION: answer now with available results, without tools. State unfinished work honestly.")])
                    try:
                        return await next_handler(**call_kwargs)
                    except Exception as final_exc:
                        raise control_cause(final_exc) or final_exc
                raise cause
        finally:
            finalizing_call.reset(token)
