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
from app.context import KNOWLEDGE_QA_SCOPE, messages, system_prompt


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
            return {"violations": ["Requested file has not been published"]}
        return {"result": result}

    async def commit(self, result):
        assert result["answer"].strip()
        assert self.committed is None
        self.committed = copy.deepcopy(result)
        return {"result": result}

    async def post(self, path, **kwargs):
        assert path == "runs/prefetch"
        return {"output": "The verified value is 42."}


@pytest.mark.asyncio
async def test_knowledge_qa_can_redirect_without_prefetch_even_with_history_and_selected_kb():
    payload = request(history=[{"role":"user", "content":"Earlier policy question"}],
        runtime_config={"agent_type":"knowledge-qa", "prefetch_knowledge":True},
        tools=[{"name":"read_conversation", "parameters":{"type":"object","properties":{}}}])
    control = MemoryControl(payload)
    async def forbidden(*args, **kwargs):
        pytest.fail("Capability response must not trigger automatic evidence reads")
    control.post = forbidden
    model = ScriptedModel([[TextBlock(text="请切换到文档处理智能体生成文件。")]])
    result = await execute(payload, control, model)
    assert result.answer == "请切换到文档处理智能体生成文件。"
    assert len(model.requests) == 1 and not control.calls


def test_capability_scope_is_last_and_only_applies_to_knowledge_qa():
    payload = request(runtime_config={"agent_type":"knowledge-qa"},
        history=[{"role":"assistant","content":"earlier answer","context_metadata":{"read_required":True}}])
    prompt = system_prompt(payload)
    assert prompt.endswith(KNOWLEDGE_QA_SCOPE)
    assert prompt.count("[KNOWLEDGE_QA_SCOPE]") == 1
    payload.runtime_config.agent_type = "general-agent"
    assert "[KNOWLEDGE_QA_SCOPE]" not in system_prompt(payload)


def test_history_keeps_image_only_turns_and_image_provenance():
    payload = request(history=[
        {"role":"user","content":"","images":[{"url":"https://example.test/first.png"}]},
        {"role":"user","content":"Second diagram","images":[{"url":"https://example.test/second.png"}]},
    ])
    payload.llm.supports_vision = True
    history = messages(payload)[0]
    assert history.role == "user"
    assert '"image_positions": [1]' in history.content[0].text
    assert '"image_positions": [2]' in history.content[0].text
    assert len(history.content) == 3
    assert str(history.content[1].source.url) == "https://example.test/first.png"
    assert str(history.content[2].source.url) == "https://example.test/second.png"


@pytest.mark.asyncio
async def test_uncited_history_sources_are_available_before_one_generation():
    payload = request(history=[{"role":"assistant", "content":"Earlier summary without citations",
        "source_id":"assistant_message_prior", "context_metadata":{"source_id":"assistant_message_prior",
        "evidence_catalog":{"read_tool":"read_conversation","section":"evidence","sources":[{"title":"Original policy"}]}}}],
        runtime_config={"prefetch_knowledge":True},
        tools=[{"name":"read_conversation","parameters":{"type":"object","properties":{}}}])
    control = MemoryControl(payload)
    posts = []
    async def restored(path, **kwargs):
        posts.append(path)
        assert path == "runs/reuse-evidence", "Do not search again when historical evidence is loaded"
        return {"output":'Original policy: verified value 42. <src id="S1" />',
                "source_references":[{"id":"S1"}],"citation_output_contract":"Cite supplied source handles."}
    control.post = restored
    model = ScriptedModel([[TextBlock(text='42. <src id="S1" />')]])
    result = await execute(payload, control, model)
    assert result.answer == '42. <src id="S1" />'
    assert len(model.requests) == 1 and posts == ["runs/reuse-evidence"]
    assert "evidence_catalog" in str(model.requests[0])
    assert "Original policy: verified value 42" in str(model.requests[0])
    for message in model.requests[0]:
        if message.role != "system":
            assert "evidence_catalog" not in str(message)
        if message.role == "assistant":
            assert "Earlier summary without citations" not in str(message)


@pytest.mark.asyncio
async def test_archived_history_handles_are_control_state_not_prose():
    payload = request(history=[
        {"role":"user", "content":"", "source_id":"user_message_long",
         "context_metadata":{"read_required":True}},
        {"role":"assistant", "content":"", "source_id":"assistant_message_long",
         "context_metadata":{"source_id":"assistant_message_long","read_required":True}},
        {"role":"user", "content":"Correction: use working days, not calendar days."},
    ])
    model = ScriptedModel([[TextBlock(text="Five working days.")]])
    await execute(payload, MemoryControl(payload), model)
    for message in model.requests[0]:
        if message.role != "system":
            assert "read_required" not in str(message)
            assert "assistant_message_long" not in str(message)
    assert any(m.role == "user" and "Correction: use working days" in str(m) for m in model.requests[0])


@pytest.mark.asyncio
async def test_failed_turn_is_control_state_not_an_answer_to_rewrite():
    payload = request(history=[
        {"role":"user","content":"What is the deadline?","source_id":"user_message_before"},
        {"role":"assistant","content":"","source_id":"assistant_message_failed",
         "context_metadata":{"source_id":"assistant_message_failed","outcome":"failed"}},
    ])
    model = ScriptedModel([[TextBlock(text="The previous attempt did not produce an answer.")]])
    await execute(payload, MemoryControl(payload), model)
    assert any(m.role == "system" and "outcome" in str(m) for m in model.requests[0])
    assert any(m.role == "user" and '"outcome": "failed"' in str(m.content) for m in model.requests[0])
    assert not any(m.role == "assistant" and m.id == "assistant_message_failed" for m in model.requests[0])
    assert len(model.requests) == 1


@pytest.mark.asyncio
async def test_empty_historical_cache_still_allows_selected_source_prefetch():
    payload = request(history=[{"role":"user","content":"previous topic"}],
        runtime_config={"prefetch_knowledge":True},
        tools=[{"name":"read_conversation","parameters":{"type":"object","properties":{}}}])
    control = MemoryControl(payload)
    posts = []
    async def restored(path, **kwargs):
        posts.append(path)
        if path == "runs/reuse-evidence": return {"output":"", "source_references":[]}
        return {"output":"New source fact"}
    control.post = restored
    model = ScriptedModel([[TextBlock(text="New source fact")]])
    await execute(payload, control, model)
    assert posts == ["runs/reuse-evidence","runs/prefetch"]
    assert len(model.requests) == 1


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
async def test_artifact_delivery_feedback_stays_in_same_sdk_context():
    payload = request()
    control = MemoryControl(payload)
    control.reject_once = True
    model = ScriptedModel([[TextBlock(text="File not yet published")], [TextBlock(text="Published document")]])
    result = await execute(payload, control, model)
    assert result.answer == "Published document"
    assert len(model.requests) == 2
    assert "Requested file has not been published" in str(model.requests[1])


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
