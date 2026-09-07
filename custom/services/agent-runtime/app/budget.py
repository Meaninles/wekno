"""One budget for the SDK lifecycle, including its finalization decision."""
from agentscope.message import Msg, TextBlock
from agentscope.middleware import MiddlewareBase
from agentscope.tool import ToolChoice


class BudgetExhausted(RuntimeError):
    pass


class IterationBudget(MiddlewareBase):
    def __init__(self, limit: int):
        self.limit = limit

    async def on_acting(self, agent, input_kwargs, next_handler):
        if agent.state.cur_iter >= self.limit - 1:
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
        )
        if remaining <= 3:
            instruction += "Begin finalization now. Avoid new investigations; finish essential verification and publish required files before the final decision. "
        if remaining == 1:
            instruction += "FINAL DECISION: return the final answer now, without tools. Do not claim unfinished work is complete."
            input_kwargs = {**input_kwargs, "tool_choice": ToolChoice(mode="none"), "tools": []}
        return await next_handler(**{
            **input_kwargs,
            "messages": [Msg(name="runtime_budget", role="system", content=[TextBlock(text=instruction)]), *input_kwargs["messages"]],
        })
