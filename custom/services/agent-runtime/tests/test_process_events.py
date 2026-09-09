import pytest
from agentscope.event import ModelCallStartEvent, ModelCallEndEvent, TextBlockDeltaEvent, ThinkingBlockDeltaEvent, ToolCallStartEvent
from app.events import Events
from test_run import MemoryControl, request

@pytest.mark.asyncio
async def test_process_block_identity_and_preamble_do_not_add_model_calls():
    control = MemoryControl(request())
    events = Events(control)
    await events.sdk(ModelCallStartEvent(reply_id='reply',model_name='fixture'))
    for text in ['核对', '资料']:
        await events.sdk(ThinkingBlockDeltaEvent(reply_id='reply',block_id='b',delta=text))
    await events.sdk(TextBlockDeltaEvent(reply_id='reply',block_id='text',delta='接下来查找制度'))
    await events.sdk(ToolCallStartEvent(reply_id='reply',tool_call_id='call',tool_call_name='knowledge_search'))
    await events.sdk(ModelCallEndEvent(reply_id='reply',input_tokens=1,output_tokens=2))
    await events.flush()
    thoughts=[e for e in control.emitted if e['type']=='thought_delta']
    assert {e['id'] for e in thoughts}=={'b'}
    assert ''.join(e['content'] for e in thoughts)=='核对资料'
    assert thoughts[-1]['done']
    assert [e['content'] for e in control.emitted if e['type']=='commentary']==['接下来查找制度']
    assert not control.calls

@pytest.mark.asyncio
async def test_final_candidate_text_is_never_a_preamble():
    control=MemoryControl(request())
    events=Events(control)
    await events.sdk(ModelCallStartEvent(reply_id='reply',model_name='fixture'))
    await events.sdk(TextBlockDeltaEvent(reply_id='reply',block_id='text',delta='Private answer candidate'))
    await events.sdk(ToolCallStartEvent(reply_id='reply',tool_call_id='final',tool_call_name='GenerateStructuredOutput'))
    await events.sdk(ModelCallEndEvent(reply_id='reply',input_tokens=1,output_tokens=2))
    await events.flush()
    assert not any(e['type'] in ('commentary','answer_delta') for e in control.emitted)
