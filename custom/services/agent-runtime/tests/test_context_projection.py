import copy
import json
from types import SimpleNamespace

import pytest
from agentscope.message import Msg, TextBlock, ToolCallBlock, ToolResultBlock

from app.context_projection import ContextProjection, transient_messages


@pytest.mark.asyncio
async def test_archive_is_lossless_and_does_not_mutate_checkpoints_or_pending_calls():
    files = {}
    async def write(path, data): files[path] = data
    middleware = ContextProjection(SimpleNamespace(write_file=write))
    blocks = []
    for i in range(6):
        blocks.extend([ToolCallBlock(id=str(i), name="Write", input=json.dumps({"file_path":"/workspace/a.py", "content":"line\n"*5000})),
                       ToolResultBlock(id=str(i), name="Write", state="success", output=[TextBlock(text="ok"*10000)])])
    pending = ToolCallBlock(id="pending", name="Write", input='{"content":"untouched"}')
    messages = [Msg(name="system",role="system",content=[TextBlock(text="Keep all requirements.")]),
                Msg(name="assistant",role="assistant",content=[*blocks,pending])]
    before = copy.deepcopy(messages)
    async def receive(**kwargs): return kwargs["messages"]
    projected = await middleware.on_model_call(None,{"messages":messages},receive)
    assert messages == before and len(files) == 2
    assert projected[0] == before[0] and projected[1].content[4:] == before[1].content[4:]
    assert len(str(projected)) < len(str(before)) * .8
    for i, raw in enumerate(files.values()):
        archived = json.loads(raw)
        assert archived["call"] == blocks[i*2].model_dump(mode="json")
        assert archived["result"] == blocks[i*2+1].model_dump(mode="json")
    await middleware.on_model_call(None,{"messages":messages},receive)
    assert len(files) == 2


def test_transient_budget_preserves_stable_system_prefix():
    stable=Msg(name="system",role="system",content=[])
    user=Msg(name="user",role="user",content=[])
    budget=Msg(name="budget",role="system",content=[])
    assert transient_messages([stable,user],budget) == [stable,budget,user]
