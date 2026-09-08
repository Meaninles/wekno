import copy
import json

import pytest
from agentscope.message import TextBlock, ToolCallBlock

from app.answer import ANSWER_TOOL, AnswerStream, DeliveryError
from test_run import MemoryControl, ScriptedModel, execute, request


def submit(text, identity="final"):
    return ToolCallBlock(id=identity, name=ANSWER_TOOL, input=json.dumps({"answer":text}, ensure_ascii=False))


@pytest.mark.parametrize("escaped", [False, True])
def test_json_stream_preserves_unicode_escapes_markdown_and_citations_at_every_split(escaped):
    answer = 'English 与中文 😀\n"引号" \\ 路径\t<src id="S1" />\n```xml\n</think>\n```'
    raw = json.dumps({"answer":answer}, ensure_ascii=escaped)
    for split in range(len(raw)+1):
        stream = AnswerStream(10000)
        assert stream.feed(raw[:split])+stream.feed(raw[split:]) == answer
    stream = AnswerStream(10000)
    assert ''.join(stream.feed(char) for char in raw) == answer


def test_stream_is_bounded_and_cannot_rewrite_an_answer_prefix():
    with pytest.raises(DeliveryError):
        AnswerStream(5).feed('{"answer":"long"}')
    stream = AnswerStream(1000)
    assert stream.feed('{"answer":"first",') == 'first'
    with pytest.raises(DeliveryError):
        stream.feed('"answer":"different"}')


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ['knowledge-qa','general-agent','document-processing-agent','table-analysis','data-analysis','custom'])
async def test_only_submitted_answer_reaches_stream_and_delivery_without_extra_generation(kind):
    payload=request(runtime_config={'agent_type':kind},tools=[{'name':'lookup','parameters':{'type':'object','properties':{}}}])
    control=MemoryControl(payload)
    answer='Answer / 正文 😀 <src id="S1" />'
    model=ScriptedModel([
        [TextBlock(text='I will inspect private internals.'),ToolCallBlock(id='read',name='lookup',input='{}')],
        [TextBlock(text='Internal validation succeeded.'),submit(answer)],
    ])
    result=await execute(payload,control,model)
    assert result.answer==answer and len(model.requests)==2
    assert ''.join(e['content'] for e in control.emitted if e['type']=='answer_delta')==answer
    assert not any(ANSWER_TOOL in json.dumps(e) for e in control.emitted)
    assert all(options['tool_choice'] is None for options in model.options)
    assert [call[0] for call in control.calls]==['lookup']


@pytest.mark.asyncio
@pytest.mark.parametrize("content", [
    [TextBlock(text='process text is not a submitted answer')],
    [submit('')], [submit(' \n ')],
    [ToolCallBlock(id='final',name=ANSWER_TOOL,input='{"answer":"unfinished')],
    [ToolCallBlock(id='final',name=ANSWER_TOOL,input='{"answer":42}')],
    [ToolCallBlock(id='final',name=ANSWER_TOOL,input='{"answer":"result","process":"internal"}')],
    [submit('done'),submit('again','final-2')],
    [ToolCallBlock(id='write',name='write',input='{}'),submit('done')],
    [submit('done'),ToolCallBlock(id='write',name='write',input='{}')],
])
async def test_invalid_submission_stops_without_correction_or_business_execution(content):
    payload=request(tools=[{'name':'write','parameters':{'type':'object','properties':{}}}])
    control=MemoryControl(payload)
    model=ScriptedModel([content],structured=False)
    with pytest.raises(DeliveryError):
        await execute(payload,control,model)
    assert len(model.requests)==1 and control.committed is None and not control.calls


@pytest.mark.asyncio
async def test_resume_keeps_iteration_budget_and_reply_identity():
    payload=request(runtime_config={'max_iterations':15},tools=[{'name':'lookup','parameters':{'type':'object','properties':{}}}])
    control=MemoryControl(payload)
    original=control.checkpoint
    async def crash(state, *, boundary):
        await original(state,boundary=boundary)
        if boundary=='before_reasoning' and state['agent']['reply_context']['cur_iter']==13:
            raise RuntimeError('worker stopped')
    control.checkpoint=crash
    with pytest.raises(RuntimeError,match='worker stopped'):
        await execute(payload,control,ScriptedModel([
            [ToolCallBlock(id=f'read-{i}',name='lookup',input='{}')] for i in range(13)
        ]))
    checkpoint=control.snapshots[-1][1]
    resumed=payload.model_copy(update={'checkpoint':checkpoint,'owner_epoch':2})
    recovery=MemoryControl(resumed)
    recovery.snapshots=copy.deepcopy(control.snapshots)
    model=ScriptedModel([[ToolCallBlock(id='last-read',name='lookup',input='{}')],[submit('done')]])
    assert (await execute(resumed,recovery,model)).answer=='done'
    assert len(model.requests)==2
    assert 'remaining=2 ' in str(model.requests[0]) and 'remaining=1 ' in str(model.requests[1])
    assert model.options[-1]['tool_choice'].mode==ANSWER_TOOL
    assert recovery.snapshots[-1][1]['agent']['reply_context']['reply_id']==checkpoint['agent']['reply_context']['reply_id']
    assert recovery.snapshots[-1][1]['agent']['reply_context']['cur_iter']==15
