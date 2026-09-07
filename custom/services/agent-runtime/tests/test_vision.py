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
    model=ScriptedModel([[TextBlock(text="The total is 52.")]])
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
    model=ScriptedModel([[TextBlock(text="Budget 186400")]])
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
