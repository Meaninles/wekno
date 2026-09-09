import json
import pytest

from app.workspace import RuntimeWorkspace
from app.artifact_delivery import file_capable
from app.context import system_prompt
from test_run import MemoryControl, request


@pytest.mark.asyncio
async def test_file_qa_can_execute_without_publishing_and_preserves_turn_identity():
    payload = request(enable_artifacts=False, runtime_config={"agent_type": "knowledge-qa"}, original_input_files=[
        {"id": "old", "file_name": "report.pptx", "source": "weknora_chat_upload_original", "role": "historical_uploaded_file"},
        {"id": "new", "file_name": "report.xlsx", "source": "weknora_chat_upload_original", "role": "user_uploaded_original_file"},
    ])
    workspace = RuntimeWorkspace(payload, MemoryControl(payload))
    await workspace.initialize()
    assert "Bash" in {tool.name for tool in workspace.tools}
    assert not file_capable(payload)
    manifest = json.loads(payload.system_prompt.split("[ORIGINAL_INPUT_FILES]\n")[1].split("\n[/ORIGINAL_INPUT_FILES]")[0])
    assert [(f["input_file_id"], f["current_turn"]) for f in manifest["input_files"]] == [("old", False), ("new", True)]
    assert "python-pptx" in system_prompt(payload)
    assert "不因文件格式或使用代码而要求切换" in system_prompt(payload)
    assert workspace.get_backend().container is None
    await workspace.close()
