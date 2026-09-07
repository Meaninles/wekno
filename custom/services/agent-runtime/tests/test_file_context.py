import pytest
from agentscope.state import AgentState, ToolContext
from agentscope.tool import Read, Edit

from app.file_context import WorkspaceToolContext


class RemoteFiles:
    def __init__(self):
        self.files = {"/workspace/script.py": b"value = 1\r\n"}

    async def read_file(self, path):
        return self.files[path]

    async def write_file(self, path, data):
        self.files[path] = data

    async def file_exists(self, path):
        return path in self.files

    async def is_dir(self, path):
        return False

    def isabs(self, path):
        return path.startswith("/")

    def basename(self, path):
        return path.rsplit("/", 1)[-1]


@pytest.mark.asyncio
async def test_remote_read_edit_cache_survives_checkpoint_and_rejects_external_changes():
    backend = RemoteFiles()
    state = AgentState()
    state.tool_context = WorkspaceToolContext.bind(ToolContext(), backend)
    reader, editor = Read(backend=backend), Edit(backend=backend)
    await reader.call(file_path="/workspace/script.py", _agent_state=state)
    restored = AgentState.model_validate(state.model_dump())
    restored.tool_context = WorkspaceToolContext.bind(restored.tool_context, backend)
    result = await editor.call(file_path="/workspace/script.py", old_string="1", new_string="2", _agent_state=restored)
    assert "Successfully" in str(result.content)
    assert backend.files["/workspace/script.py"] == b"value = 2\n"
    # A shell write invalidates the old read; stale content must not overwrite it.
    backend.files["/workspace/script.py"] = b"value = 3\n"
    result = await editor.call(file_path="/workspace/script.py", old_string="1", new_string="4", _agent_state=restored)
    assert "must first read" in str(result.content)
    assert backend.files["/workspace/script.py"] == b"value = 3\n"


@pytest.mark.asyncio
async def test_binary_read_returns_recoverable_tool_error_without_poisoning_checkpoint():
    backend = RemoteFiles()
    backend.files["/workspace/book.xlsx"] = b"PK\x03\x04\x00\x00binary"
    state = AgentState()
    state.tool_context = WorkspaceToolContext.bind(ToolContext(), backend)
    result = await Read(backend=backend).call(file_path="/workspace/book.xlsx", _agent_state=state)
    assert str(result.state) == "error"
    assert "binary file" in str(result.content)
    assert not state.tool_context.read_file_cache
