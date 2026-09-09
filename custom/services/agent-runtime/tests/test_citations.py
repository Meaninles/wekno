import asyncio
import copy
import json

import pytest
from agentscope.message import TextBlock, ToolCallBlock

from app.citations import anchors, TOOL
from app.control import ControlError
from app.context import system_prompt
from test_run import MemoryControl, ScriptedModel, execute, request


class EvidenceControl(MemoryControl):
    async def budget(self, role=''):
        return {**await super().budget(role), 'current_run_sources': [{'id': 'S1'}]}

    async def post(self, path, **kwargs):
        assert path == 'runs/budget' and kwargs == {'include_evidence': True}
        return {'citation_evidence': [{'id': 'S1', 'title': '制度', 'text': '完整的真实证据。'}]}


def mapping(value):
    return [ToolCallBlock(id='citations', name=TOOL, input=json.dumps(value))]


def test_unicode_repeated_markdown_and_safe_table_code_anchors():
    answer = '# 标题\n\n**正文😀。**\n\n**正文😀。**\n\n```txt\n引用不能插在这里\n\n```\n\n| 字段 | 值 |\n| --- | --- |\n| 中文 | 42 |\n\n    code\n'
    locations = anchors(answer)
    assert len(locations) == 3
    assert locations[0]['text'] == locations[1]['text'] == '**正文😀。**'
    assert locations[0]['end'] < locations[1]['end']
    for a in locations:
        raw = answer.encode()
        assert raw[a['end'] - len(a['text'].encode()):a['end']].decode() == a['text']
    assert answer.encode()[locations[-1]['end']:].decode().startswith(' |')


def test_list_items_are_independently_addressed_without_splitting_generation():
    body = '## 步骤\n1. **申请**：条件甲。\n2. **审核**：条件乙。\n3. **发布**：条件丙。'
    assert [a['text'] for a in anchors(body)] == ['1. **申请**：条件甲。', '2. **审核**：条件乙。', '3. **发布**：条件丙。']


def test_history_guidance_requires_restored_evidence_not_repeated_assistant_prose():
    payload = request(history=[{'role': 'assistant', 'content': '旧结论', 'source_id': 'assistant_message_prior',
        'context_metadata': {'source_id': 'assistant_message_prior'}}])
    prompt = system_prompt(payload)
    assert 'including a repeated question or follow-up' in prompt
    assert 'Pure text transformations and general conversation do not require retrieval' in prompt
    assert 'including a repeated question or follow-up' not in system_prompt(request())


@pytest.mark.asyncio
async def test_two_pass_body_immutable_and_only_current_evidence_tool():
    payload = request()
    control = EvidenceControl(payload)
    body = '**正文😀。**\n\n**正文😀。**'
    model = ScriptedModel([[TextBlock(text=body)], mapping({'citations': [
        {'anchor_id': 'A1', 'source_ids': ['S1']}, {'anchor_id': 'A2', 'source_ids': ['S1']} ]})])
    result = await execute(payload, control, model)
    assert result.answer == body and result.citation_status == 'complete'
    assert len(result.citations) == 2 and result.citations[0]['end'] != result.citations[1]['end']
    assert len(model.requests) == 2
    assert [t['function']['name'] for t in model.options[1]['tools']] == [TOOL]
    fields = model.options[1]['tools'][0]['function']['parameters']['properties']['citations']['items']['properties']
    assert fields['anchor_id']['enum'] == ['A1', 'A2']
    assert fields['source_ids']['items']['enum'] == ['S1']
    assert set(fields) == {'anchor_id', 'source_ids'}
    assert '完整的真实证据' in str(model.requests[1])
    assert result.status == 'completed'


@pytest.mark.asyncio
@pytest.mark.parametrize('value', [{}, {'citations': 'bad'}, {'citations': [{'anchor_id': 'A99', 'source_ids': ['S1']}]},
    {'citations': [{'anchor_id': 'A1', 'source_ids': ['S999']}]}, {'citations': [{'text': '误抄原文', 'source_ids': ['S1']}]}])
async def test_invalid_mapping_preserves_body_and_reports_failure(value):
    payload = request()
    model = ScriptedModel([[TextBlock(text='保留正文。')], mapping(value)])
    result = await execute(payload, EvidenceControl(payload), model)
    assert result.answer == '保留正文。' and result.citations == []
    assert result.citation_status == 'failed' and result.status == 'incomplete'
    assert result.failure_code == 'citation_generation_failed' and len(model.requests) == 2


@pytest.mark.asyncio
@pytest.mark.parametrize('boundary', ['before_citations', 'after_citations', 'before_commit'])
async def test_recovery_never_regenerates_body_or_completed_citations(boundary):
    payload = request()
    control = EvidenceControl(payload)
    original = control.checkpoint
    async def crash(state, *, boundary: str):
        await original(state, boundary=boundary)
        if boundary == crash.boundary:
            raise RuntimeError('worker stopped')
    crash.boundary = boundary
    control.checkpoint = crash
    with pytest.raises(RuntimeError, match='worker stopped'):
        await execute(payload, control, ScriptedModel([[TextBlock(text='保留正文。')], mapping({'citations': []})]))
    resumed = payload.model_copy(update={'checkpoint': copy.deepcopy(control.snapshots[-1][1]), 'owner_epoch': 2})
    model = ScriptedModel([mapping({'citations': []})] if boundary == 'before_citations' else [])
    result = await execute(resumed, EvidenceControl(resumed), model)
    assert result.answer == '保留正文。'
    assert len(model.requests) == (1 if boundary == 'before_citations' else 0)


@pytest.mark.asyncio
async def test_cancellation_is_not_converted_to_citation_failure():
    payload = request()
    control = EvidenceControl(payload)
    async def cancelled(*args, **kwargs):
        raise asyncio.CancelledError('ownership lost')
    control.post = cancelled
    with pytest.raises(asyncio.CancelledError):
        await execute(payload, control, ScriptedModel([[TextBlock(text='保留正文。')]]))
    assert control.committed is None


@pytest.mark.asyncio
@pytest.mark.parametrize('error', [TimeoutError('timeout'), ControlError('task_limit', 'budget exhausted')])
async def test_model_timeout_or_budget_denial_preserves_answer(error):
    class FailingModel(ScriptedModel):
        async def _call_api(self, *args, **kwargs):
            if self.requests:
                raise error
            return await super()._call_api(*args, **kwargs)
    payload = request()
    result = await execute(payload, EvidenceControl(payload), FailingModel([[TextBlock(text='完整正文。')]]))
    assert result.answer == '完整正文。' and result.citation_status == 'failed'
