import pytest
from agentscope.state import AgentState, ToolContext
from agentscope.tool import Read

from app.file_context import WorkspaceEdit as Edit, WorkspaceToolContext


class RemoteFiles:
    def __init__(self):
        self.files = {"/workspace/script.py": b"value = 1\r\n"}

    async def read_file(self, path):
        return self.files[path]

    async def write_file(self, path, data):
        self.files[path] = data

    async def exec_shell(self, *args, **kwargs):
        return None

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


@pytest.mark.asyncio
async def test_newly_written_content_is_editable_but_external_changes_still_fence_it():
    from app.file_context import WorkspaceWrite
    backend=RemoteFiles()
    state=AgentState()
    state.tool_context=WorkspaceToolContext.bind(ToolContext(),backend)
    writer=WorkspaceWrite(backend=backend)
    assert writer.name=='Write'
    await writer.call('/workspace/new.py','value = 1\n',_agent_state=state)
    assert await state.tool_context.get_cache('/workspace/new.py') is not None
    result=await Edit(backend=backend).call('/workspace/new.py','1','2',_agent_state=state)
    assert 'Successfully' in str(result.content)
    restored = AgentState.model_validate(state.model_dump())
    restored.tool_context = WorkspaceToolContext.bind(restored.tool_context, backend)
    result = await Edit(backend=backend).call('/workspace/new.py', '2', '4', _agent_state=restored)
    assert 'Successfully' in str(result.content)
    assert backend.files['/workspace/new.py'] == b'value = 4\n'
    backend.files['/workspace/new.py']=b'value = 3\n'
    assert await restored.tool_context.get_cache('/workspace/new.py') is None


@pytest.mark.asyncio
async def test_repeated_edits_refresh_cache_and_failed_edits_preserve_it():
    backend = RemoteFiles()
    backend.files['/workspace/script.py'] = b'value = 1 + 1\n'
    state = AgentState()
    state.tool_context = WorkspaceToolContext.bind(ToolContext(), backend)
    await Read(backend=backend).call('/workspace/script.py', _agent_state=state)
    editor = Edit(backend=backend)
    assert editor.name == 'Edit'
    result = await editor.call('/workspace/script.py', '1', '2', _agent_state=state)
    assert str(result.state) == 'error'  # Ambiguous replacement must not write.
    assert await state.tool_context.get_cache('/workspace/script.py') is not None
    result = await editor.call('/workspace/script.py', '1', '2', replace_all=True, _agent_state=state)
    assert 'Successfully' in str(result.content)
    result = await editor.call('/workspace/script.py', '2 + 2', '4', _agent_state=state)
    assert 'Successfully' in str(result.content)
    assert backend.files['/workspace/script.py'] == b'value = 4\n'
    backend.files['/workspace/script.py'] = b'external = 5\n'
    result = await editor.call('/workspace/script.py', '4', '6', _agent_state=state)
    assert str(result.state) == 'error'
    assert backend.files['/workspace/script.py'] == b'external = 5\n'
