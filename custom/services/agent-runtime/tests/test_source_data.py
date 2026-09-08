import copy
import json
from types import SimpleNamespace

import pytest

from app.context import system_prompt
from app.contracts import RuntimeToolSpec
from app.tools import BusinessTool, invocation_id
from test_run import request


@pytest.mark.asyncio
async def test_large_authorized_results_are_lossless_workspace_inputs_with_unchanged_evidence():
    result = {'success': True, 'output': '原始内容\n' * 1800,
              'data': {'rows': [{'名称': '保留原始事实', '值': 42}]},
              'source_references': [{'id': 'S1', 'chunk_id': 'chunk-1', 'cite_exactly': '<src id="S1" />'}], 'citation_output_contract': 'Use S1'}
    original = copy.deepcopy(result)
    files = {}
    async def tool(*args): return result
    async def write(path, content): files[path] = content
    bridge = BusinessTool(RuntimeToolSpec(name='source_query'), SimpleNamespace(tool=tool),
                          SimpleNamespace(write_file=write))
    token = invocation_id.set('../../untrusted-call-id')
    try:
        chunk = await bridge.call()
    finally:
        invocation_id.reset(token)
    path = chunk.metadata['source_data_file']
    assert path.startswith('/workspace/source-data/') and '..' not in path
    saved = json.loads(files[path])
    assert saved == {'tool': 'source_query', **original}
    assert result == original
    assert original['output'] not in chunk.content[0].text
    assert original['output'][:1600] in chunk.content[0].text
    assert len(chunk.content[0].text) < 2500
    assert 'partial preview' in chunk.content[0].text
    assert original['citation_output_contract'] in chunk.content[0].text
    assert path in chunk.content[0].text
    assert chunk.content[0].text.startswith(original['citation_output_contract'])
    assert '\"cite_exactly\": \"<src id=\\\"S1\\\" />\"' in chunk.content[0].text
    assert '\"data_json_pointer\": \"/data\"' in chunk.content[0].text
    assert chunk.metadata['source_references'] == original['source_references']


@pytest.mark.asyncio
@pytest.mark.parametrize('enabled,success,large', [(False,True,True),(True,False,True),(True,True,False)])
async def test_source_files_require_workspace_success_and_substantial_data(enabled, success, large):
    async def tool(*args): return {'success':success,'output':'x' * (10000 if large else 10)}
    async def forbidden(*args): pytest.fail('Unexpected workspace write')
    bridge = BusinessTool(RuntimeToolSpec(name='query'),SimpleNamespace(tool=tool),
                          SimpleNamespace(write_file=forbidden) if enabled else None)
    token=invocation_id.set('call')
    try: chunk=await bridge.call()
    finally: invocation_id.reset(token)
    assert chunk.metadata['source_data_file'] is None


def test_source_data_guidance_follows_file_capability_and_excludes_knowledge_qa():
    for agent_type in ['general-agent','document-processing-agent','table-analysis','data-analysis','custom-agent']:
        assert '[FILE_SOURCE_DATA]' in system_prompt(request(enable_artifacts=True,runtime_config={'agent_type':agent_type}))
    assert '[FILE_SOURCE_DATA]' not in system_prompt(request(enable_artifacts=False))
    assert '[FILE_SOURCE_DATA]' not in system_prompt(request(enable_artifacts=True,runtime_config={'agent_type':'knowledge-qa'}))
