from contextlib import asynccontextmanager

import pytest
from agentscope.message import TextBlock
from agentscope.state import AgentState
from types import SimpleNamespace

from app.tools import media_block
from app.vision import Vision
from agentscope.tool import ToolResponse
from test_run import MemoryControl, ScriptedModel, request


@pytest.mark.asyncio
async def test_vision_observations_reuse_content_identity_across_restored_blocks(monkeypatch):
    model=ScriptedModel([[TextBlock(text="The total is 52.")]], structured=False)
    @asynccontextmanager
    async def provider(*args, **kwargs):
        assert kwargs["model_role"] == "vision"
        yield model
    monkeypatch.setattr("app.vision.model_for",provider)
    control=MemoryControl(request(vision_llm={"model_name":"vision"}))
    middleware=Vision(control)
    agent=SimpleNamespace(state=AgentState(session_id="run-1"))
    def blocks():
        return [TextBlock(text="Current image"),media_block("data:image/png;base64,aGVsbG8=")]
    assert await middleware.describe(agent,blocks()) == "The total is 52."
    assert await middleware.describe(agent,blocks()) == "The total is 52."
    assert len(model.requests)==1


@pytest.mark.asyncio
async def test_tool_inspection_does_not_replay_the_original_generation_task(monkeypatch):
    model=ScriptedModel([[TextBlock(text='One label overlaps the chart.')]], structured=False)
    @asynccontextmanager
    async def provider(payload,*args,**kwargs):
        assert payload.runtime_config.max_completion_tokens == 1024
        yield model
    monkeypatch.setattr('app.vision.model_for',provider)
    payload=request(vision_llm={'model_name':'vision'})
    payload.query='Generate a comprehensive file about TOPIC_SENTINEL'
    control=MemoryControl(payload)
    agent=SimpleNamespace(state=AgentState(session_id='run-1'))
    answer=await Vision(control).describe(agent,[media_block('data:image/png;base64,aGVsbG8=')],inspection=True)
    assert 'overlaps' in answer
    assert 'TOPIC_SENTINEL' not in str(model.requests)
    assert 'observation only' in str(model.requests)


@pytest.mark.asyncio
async def test_auxiliary_inspection_preserves_final_budget(monkeypatch):
    control=MemoryControl(request(vision_llm={'model_name':'vision'}))
    async def budget(role=''):
        return {'remaining_tokens':10000,'final_reserve':8192,'remaining_requests':1,'remaining_seconds':100}
    control.budget=budget
    def forbidden(*args,**kwargs): pytest.fail('Must not buy another vision call')
    monkeypatch.setattr('app.vision.model_for',forbidden)
    answer=await Vision(control).describe(SimpleNamespace(state=AgentState()),[media_block('data:image/png;base64,aGVsbG8=')],inspection=True)
    assert 'not completed' in answer


@pytest.mark.asyncio
async def test_primary_vision_model_never_triggers_auxiliary_vision():
    payload=request(image_urls=["data:image/png;base64,aGVsbG8="])
    payload.llm.supports_vision=True
    middleware=Vision(MemoryControl(payload))
    agent=SimpleNamespace(state=AgentState(session_id="run-1"))
    async def next_handler():
        yield "native"
    assert [v async for v in middleware.on_reasoning(agent,{},next_handler)] == ["native"]
    assert not agent.state.context


@pytest.mark.asyncio
async def test_tool_image_is_replaced_with_cached_observation(monkeypatch):
    model=ScriptedModel([[TextBlock(text="Budget 186400")]], structured=False)
    @asynccontextmanager
    async def provider(*args, **kwargs):
        yield model
    monkeypatch.setattr("app.vision.model_for",provider)
    control=MemoryControl(request(vision_llm={"model_name":"vision"}))
    middleware=Vision(control)
    agent=SimpleNamespace(state=AgentState(session_id="run-1"))
    async def next_handler():
        yield ToolResponse(content=[TextBlock(text="Read succeeded"),media_block("data:image/png;base64,aGVsbG8=")])
    for _ in range(2):
        result=[v async for v in middleware.on_acting(agent,{},next_handler)]
        assert all(isinstance(b,TextBlock) for b in result[0].content)
        assert "186400" in result[0].content[-1].text
    assert len(model.requests)==1


@pytest.mark.asyncio
async def test_failed_optional_vision_is_a_tool_error_and_ownership_loss_still_propagates(monkeypatch):
    import asyncio
    control=MemoryControl(request(vision_llm={"model_name":"vision"}))
    middleware=Vision(control)
    agent=SimpleNamespace(state=AgentState())
    async def failed(*args,**kwargs): raise ValueError("provider had no completed observation")
    monkeypatch.setattr(middleware,"describe",failed)
    async def next_handler(): yield ToolResponse(content=[media_block("data:image/png;base64,aGVsbG8=")])
    result=[v async for v in middleware.on_acting(agent,{},next_handler)]
    assert str(result[0].state)=="error" and "no visual observation" in result[0].content[-1].text
    assert not agent.state.middle_context.get("vision_observations")
    async def cancelled(*args,**kwargs): raise asyncio.CancelledError()
    monkeypatch.setattr(middleware,"describe",cancelled)
    with pytest.raises(asyncio.CancelledError):
        [v async for v in middleware.on_acting(agent,{},next_handler)]


@pytest.mark.asyncio
async def test_image_failure_does_not_relabel_successful_business_mutation(monkeypatch):
    from agentscope.message import ToolCallBlock
    control=MemoryControl(request(tools=[{"name":"external_create","is_read_only":False}]))
    middleware=Vision(control)
    async def failed(*args,**kwargs): raise ValueError("vision unavailable")
    monkeypatch.setattr(middleware,"describe",failed)
    async def next_handler(**kwargs):
        yield ToolResponse(state="success",content=[TextBlock(text="Created object 42"),media_block("data:image/png;base64,aGVsbG8=")])
    result=[v async for v in middleware.on_acting(SimpleNamespace(state=AgentState()),
        {"tool_call":ToolCallBlock(id="create",name="external_create",input="{}")},next_handler)]
    assert str(result[0].state)=="success"
    assert result[0].content[0].text=="Created object 42"
    assert "no visual observation" in result[0].content[-1].text
