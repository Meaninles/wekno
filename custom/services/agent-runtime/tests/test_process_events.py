import pytest
from agentscope.event import ModelCallStartEvent, ModelCallEndEvent, TextBlockDeltaEvent, ThinkingBlockDeltaEvent, ToolCallStartEvent
from app.events import Events
from test_run import MemoryControl, request
from app.context import system_prompt, ProcessLanguage, PROCESS_LANGUAGE

@pytest.mark.asyncio
async def test_language_contract_reaches_actual_sdk_model_request_after_skill_instructions(monkeypatch):
    from agentscope.tool import Toolkit
    from agentscope.message import TextBlock
    from test_run import ScriptedModel, execute

    async def skill_instructions(self, *args, **kwargs):
        return 'Loaded English skill instructions for generating a presentation.'

    monkeypatch.setattr(Toolkit, 'get_skill_instructions', skill_instructions)
    payload = request()
    control = MemoryControl(payload)
    model = ScriptedModel([[TextBlock(text='完成')]])
    await execute(payload, control, model)
    assert len(model.requests) == 1
    system_message = next(message for message in model.requests[0] if message.role == 'system')
    system = ''.join(block.text for block in system_message.content if isinstance(block, TextBlock))
    assert system.index('[PROCESS_LANGUAGE]') > system.index('Loaded English skill instructions')
    assert system.count('[PROCESS_LANGUAGE]') == 1

@pytest.mark.parametrize('agent_type', ['knowledge-qa', 'general-agent', 'document-processing-agent', 'table-analysis', 'custom-agent'])
@pytest.mark.asyncio
async def test_process_language_is_shared_without_changing_answer_language(agent_type):
    payload = request()
    payload.runtime_config.agent_type = agent_type
    payload.system_prompt = 'Keep the configured agent behavior.'
    base = system_prompt(payload) + '\nEnglish skill and workspace instructions.'
    prompt = await ProcessLanguage().on_system_prompt(None, base)
    assert prompt.count('[PROCESS_LANGUAGE]') == 1
    assert '自然语言统一使用简体中文' in prompt
    assert '最终答复仍遵循用户要求的语言' in prompt
    assert '工具名称、代码、路径、网址及原文引用保留原样' in prompt
    assert prompt.startswith(payload.system_prompt)
    assert prompt.endswith(PROCESS_LANGUAGE)
    assert '会直接显示在聊天页面' in prompt

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
