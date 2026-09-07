import pytest

from app.workspace import RuntimeWorkspace, safe_path
from test_run import MemoryControl, request


@pytest.mark.asyncio
async def test_plain_qa_workspace_is_lazy_and_does_not_expose_execution():
    payload=request()
    workspace=RuntimeWorkspace(payload,MemoryControl(payload))
    await workspace.initialize()
    assert workspace.get_backend().container is None
    assert all(tool.is_read_only for tool in workspace.tools)
    assert workspace.tools
    await workspace.close()


@pytest.mark.parametrize("path",["../escape","/etc/passwd","/workspace/../../control/epoch"])
def test_workspace_paths_cannot_escape_run(path):
    with pytest.raises(ValueError):safe_path(path)


def test_workspace_path_is_posix_even_on_windows():
    assert safe_path("outputs/report.docx")=="/workspace/outputs/report.docx"
