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


@pytest.mark.asyncio
async def test_file_agents_never_expose_a_model_publish_tool():
    payload=request(enable_artifacts=True)
    workspace=RuntimeWorkspace(payload,MemoryControl(payload))
    await workspace.initialize()
    assert 'publish_artifact' not in {tool.name for tool in workspace.tools}
    assert {'Write','Edit','Bash'} <= {tool.name for tool in workspace.tools}
    await workspace.close()


@pytest.mark.parametrize("path",["../escape","/etc/passwd","/workspace/../../control/epoch"])
def test_workspace_paths_cannot_escape_run(path):
    with pytest.raises(ValueError):safe_path(path)


def test_workspace_path_is_posix_even_on_windows():
    assert safe_path("outputs/report.docx")=="/workspace/outputs/report.docx"


@pytest.mark.asyncio
async def test_inputs_load_only_for_requested_paths_and_never_replace_recovered_edits():
    from types import SimpleNamespace
    payload=request(enable_artifacts=True,original_input_files=[
        {"id":"a","file_name":"a.txt"},{"id":"b","file_name":"b.txt"}])
    workspace=RuntimeWorkspace(payload,MemoryControl(payload))
    files={"/workspace/inputs/b/b.txt":b"modified in earlier epoch"}
    downloads=[]
    async def read(path):
        if path not in files: raise OSError("absent")
        return files[path]
    async def write(path,data): files[path]=data
    async def download(spec): downloads.append(spec.id);return spec.id.encode()
    workspace._backend=SimpleNamespace(read_file=read,write_file=write)
    workspace.input_bytes=download
    await workspace.initialize()
    assert not downloads and len(files)==1
    await workspace.prepare_tool("Read",{"file_path":"/workspace/inputs/a/a.txt"})
    assert downloads==["a"] and files["/workspace/inputs/a/a.txt"]==b"a"
    await workspace.prepare_tool("Read",{"file_path":"/workspace/inputs/b/b.txt"})
    assert downloads==["a"] and files["/workspace/inputs/b/b.txt"]==b"modified in earlier epoch"
    await workspace.prepare_tool("Read",{"file_path":"/workspace/inputs/a/a.txt"})
    assert downloads==["a"]


@pytest.mark.asyncio
async def test_output_baseline_precedes_first_mutation_and_read_does_not_start_inventory(monkeypatch):
    from types import SimpleNamespace
    calls=[]
    payload=request(enable_artifacts=True)
    workspace=RuntimeWorkspace(payload,MemoryControl(payload))
    await workspace.initialize()
    async def capture(p,c,w):
        if p.output_baseline is None:
            calls.append("baseline")
            p.output_baseline={"/workspace/outputs/old.txt":"old-hash"}
    monkeypatch.setattr("app.artifact_delivery.capture_baseline",capture)
    await workspace.prepare_tool("Read",{"file_path":"/workspace/a"})
    assert not calls and payload.output_baseline is None
    await workspace.prepare_tool("Bash",{"command":"write output"})
    workspace.tool_finished("Bash")
    await workspace.prepare_tool("Write",{"file_path":"/workspace/outputs/b.txt"})
    assert calls==["baseline"] and workspace.output_revision==1
