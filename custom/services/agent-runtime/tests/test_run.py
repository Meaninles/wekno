import asyncio
import copy
import time

import pytest
from agentscope.credential import OpenAICredential
from agentscope.formatter import OpenAIChatFormatter
from agentscope.message import TextBlock, ToolCallBlock
from agentscope.model import ChatModelBase, ChatResponse, ChatUsage
from pydantic import BaseModel

from app.contracts import RunRequest
from app.delivery import DeliveryError
from app.budget import BudgetExhausted
from app.run import execute


class Parameters(BaseModel):
    max_tokens: int = 4096


class ScriptedModel(ChatModelBase):
    def __init__(self, responses):
        super().__init__(OpenAICredential(api_key="fixture"), "fixture", Parameters(),
                         max_retries=0, context_size=128000)
        self.responses = iter(responses)
        self.requests = []
        self.formatter = OpenAIChatFormatter()

    async def _call_api(self, model_name, messages, **kwargs):
        self.requests.append(copy.deepcopy(messages))
        content = next(self.responses)
        if isinstance(content, asyncio.Event):
            await content.wait()
            content = [TextBlock(text="late answer")]
        return ChatResponse(content=content, is_last=True, usage=ChatUsage(input_tokens=25, output_tokens=10, time=.01))


class MemoryControl:
    def __init__(self, payload):
        self.payload = payload
        self.snapshots = []
        self.emitted = []
        self.calls = []
        self.committed = None
        self.reject_once = False

    async def checkpoint(self, state, *, boundary):
        self.snapshots.append((boundary, copy.deepcopy(state)))

    async def events(self, events):
        assert self.committed is None, "events cannot be written after terminal commit"
        self.emitted.extend(copy.deepcopy(events))

    async def tool(self, name, arguments, call_id):
        # A durable decision containing this identity must precede execution.
        assert any(call_id in str(snapshot) for _, snapshot in self.snapshots)
        self.calls.append((name, arguments, call_id))
        return {"success": True, "output": "The verified value is 42."}

    async def validate(self, result):
        if self.reject_once:
            self.reject_once = False
            return {"violations": ["Unknown source ID"]}
        return {"result": result}

    async def commit(self, result):
        assert result["answer"].strip()
        assert self.committed is None
        self.committed = copy.deepcopy(result)
        return {"result": result}

    async def post(self, path, **kwargs):
        assert path == "runs/prefetch"
        return {"output": "The verified value is 42."}


def request(**updates):
    return RunRequest(run_id="run-1", owner_epoch=1, session_id="session-1", request_id="request-1",
                      assistant_message_id="assistant-1", query="What is the value?",
                      deadline_unix=time.time() + 30, llm={"model_name": "fixture"},
                      tool_callback_url="http://fixture/internal/tools/call", **updates)


@pytest.mark.asyncio
async def test_plain_answer_is_committed_once_by_same_harness():
    payload = request()
    control = MemoryControl(payload)
    model = ScriptedModel([[TextBlock(text="Hello")]])
    result = await execute(payload, control, model)
    assert result.answer == control.committed["answer"] == "Hello"
    assert len(model.requests) == 1
    assert any(e["type"] == "answer_delta" and e["content"] == "Hello" for e in control.emitted)


@pytest.mark.asyncio
@pytest.mark.parametrize("limit", [15, 50])
async def test_budget_is_visible_each_round_and_last_valid_answer_commits(limit):
    payload = request(runtime_config={"max_iterations": limit},
                      tools=[{"name": "lookup", "parameters": {"type": "object", "properties": {}}}])
    control = MemoryControl(payload)
    model = ScriptedModel([
        *[[ToolCallBlock(id=f"lookup-{i}", name="lookup", input="{}")] for i in range(limit - 1)],
        [TextBlock(text="Verified final result: 42")],
    ])
    result = await execute(payload, control, model)
    assert result.answer == control.committed["answer"] == "Verified final result: 42"
    assert len(model.requests) == limit
    for index, messages in enumerate(model.requests):
        assert f"maximum={limit}" in str(messages)
        assert f"remaining={limit-index} " in str(messages)
    assert "FINAL DECISION" in str(model.requests[-1])


@pytest.mark.asyncio
async def test_invalid_final_answer_at_budget_does_not_commit_or_retry_beyond_budget():
    payload = request(runtime_config={"max_iterations": 15},
                      tools=[{"name": "lookup", "parameters": {"type": "object", "properties": {}}}])
    control = MemoryControl(payload)
    async def reject(result): return {"violations": ["Answer must not be empty."]}
    control.validate = reject
    model = ScriptedModel([
        *[[ToolCallBlock(id=f"lookup-{i}", name="lookup", input="{}")] for i in range(14)],
        [TextBlock(text="invalid candidate")],
    ])
    with pytest.raises(BudgetExhausted):
        await execute(payload, control, model)
    assert len(model.requests) == 15
    assert control.committed is None


@pytest.mark.asyncio
@pytest.mark.parametrize("boundary", ["model_decision", "before_commit"])
async def test_recovery_of_completed_answer_uses_no_additional_model_call(boundary):
    payload = request()
    control = MemoryControl(payload)
    original = control.checkpoint

    async def crash(state, *, boundary: str):
        await original(state, boundary=boundary)
        if boundary == crash_at:
            raise RuntimeError("simulated process death")

    crash_at = boundary
    control.checkpoint = crash
    with pytest.raises(RuntimeError, match="simulated process death"):
        await execute(payload, control, ScriptedModel([[TextBlock(text="verified answer")]]))
    checkpoint = next(snapshot for name, snapshot in reversed(control.snapshots) if name == boundary)
    resumed = payload.model_copy(update={"owner_epoch": 2, "checkpoint": checkpoint})
    recovery = MemoryControl(resumed)
    model = ScriptedModel([])
    result = await execute(resumed, recovery, model)
    assert result.answer == "verified answer"
    assert not model.requests


@pytest.mark.asyncio
async def test_recovery_replays_pending_tool_identity_without_replanning():
    payload = request(tools=[{"name": "lookup", "parameters": {"type": "object", "properties": {}}}])
    control = MemoryControl(payload)
    original = control.checkpoint

    async def crash(state, *, boundary):
        await original(state, boundary=boundary)
        if boundary == "model_decision":
            raise RuntimeError("simulated process death")

    control.checkpoint = crash
    with pytest.raises(RuntimeError, match="simulated process death"):
        await execute(payload, control, ScriptedModel([[ToolCallBlock(id="lookup-durable", name="lookup", input="{}")]]))
    checkpoint = control.snapshots[-1][1]
    resumed = payload.model_copy(update={"owner_epoch": 2, "checkpoint": checkpoint})
    recovery = MemoryControl(resumed)
    recovery.snapshots = copy.deepcopy(control.snapshots)
    model = ScriptedModel([[TextBlock(text="42")]])
    result = await execute(resumed, recovery, model)
    assert result.answer == "42"
    assert recovery.calls == [("lookup", {}, "lookup-durable")]
    assert len(model.requests) == 1


@pytest.mark.asyncio
async def test_tool_decision_persisted_before_execution_then_answer_commits():
    payload = request(tools=[{"name": "lookup", "parameters": {"type": "object", "properties": {}},
                              "is_read_only": True, "is_concurrency_safe": True}])
    control = MemoryControl(payload)
    model = ScriptedModel([[TextBlock(text="I will look this up."), ToolCallBlock(id="lookup-1", name="lookup", input="{}")],
                           [TextBlock(text="42")]])
    result = await execute(payload, control, model)
    assert result.answer == "42"
    assert control.calls == [("lookup", {}, "lookup-1")]
    assert "The verified value is 42" in str(model.requests[1])


@pytest.mark.asyncio
async def test_delivery_correction_stays_in_same_sdk_context():
    payload = request()
    control = MemoryControl(payload)
    control.reject_once = True
    model = ScriptedModel([[TextBlock(text="unknown reference")], [TextBlock(text="correct reference")]])
    result = await execute(payload, control, model)
    assert result.answer == "correct reference"
    assert len(model.requests) == 2
    assert "Unknown source ID" in str(model.requests[1])


@pytest.mark.asyncio
async def test_prefetch_is_visible_in_first_model_call():
    payload = request(runtime_config={"prefetch_knowledge": True})
    control = MemoryControl(payload)
    model = ScriptedModel([[TextBlock(text="42")]])
    await execute(payload, control, model)
    assert len(model.requests) == 1
    assert "The verified value is 42" in str(model.requests[0])


@pytest.mark.asyncio
@pytest.mark.parametrize("prefetch", [True, False])
async def test_source_contract_reaches_model_for_both_retrieval_routes(prefetch):
    payload = request(runtime_config={"prefetch_knowledge": prefetch},
                      tools=[{"name": "lookup", "parameters": {"type": "object", "properties": {}}}])
    control = MemoryControl(payload)
    evidence = {"success": True, "output": 'Value 42, source <src id="S1" />',
                "citation_output_contract": "Cite the evidence beside the supported conclusion."}

    async def post(*args, **kwargs):
        return evidence

    async def tool(*args, **kwargs):
        return evidence

    control.post, control.tool = post, tool
    decisions = [] if prefetch else [[ToolCallBlock(id="source-1", name="lookup", input="{}")]]
    model = ScriptedModel(decisions + [[TextBlock(text='42 <src id="S1" />')]])
    result = await execute(payload, control, model)
    assert result.answer == '42 <src id="S1" />'
    assert len(model.requests) == (1 if prefetch else 2)
    assert evidence["citation_output_contract"] in str(model.requests[-1])


@pytest.mark.asyncio
async def test_cancellation_never_commits_or_starts_another_model_request():
    payload = request()
    control = MemoryControl(payload)
    model = ScriptedModel([asyncio.Event(), [TextBlock(text="unexpected continuation")]])
    task = asyncio.create_task(execute(payload, control, model))
    while not model.requests:
        await asyncio.sleep(.005)
    task.cancel()
    with pytest.raises(asyncio.CancelledError):
        await task
    assert control.committed is None
    assert len(model.requests) == 1


@pytest.mark.asyncio
async def test_sdk_workspace_tool_has_one_complete_durable_receipt():
    from agentscope.tool import ToolBase, ToolChunk
    from agentscope.message import ToolResultState
    class Local(ToolBase):
        def __init__(self):
            super().__init__()
            self.name = "local_write"
            self.description = "Write fixture"
            self.is_read_only = False
            self.is_concurrency_safe = False
            self.input_schema = {"type":"object", "properties":{}}
        async def call(self):
            return ToolChunk(content=[TextBlock(text="verified")], state=ToolResultState.SUCCESS)
        async def check_permissions(self, tool_input, context):
            from agentscope.permission import PermissionBehavior, PermissionDecision
            return PermissionDecision(PermissionBehavior.ALLOW, "fixture")
    payload = request()
    control = MemoryControl(payload)
    model = ScriptedModel([[ToolCallBlock(id="local-1", name="local_write", input="{}")], [TextBlock(text="done")]])
    await execute(payload, control, model, extra_tools=[Local()])
    records = [e for e in control.emitted if e["type"].startswith("local_tool_")]
    assert [e["type"] for e in records] == ["local_tool_start", "local_tool_result"]
    assert records[1]["data"]["output"] == "verified"
    assert records[1]["data"]["success"] is True
    assert len(model.requests) == 2
