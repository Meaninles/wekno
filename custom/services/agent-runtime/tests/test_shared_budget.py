import asyncio
from types import SimpleNamespace

import httpx
import pytest
from agentscope.message import TextBlock

from app.budget import IterationBudget
from app.control import ControlError
from app.failures import error_code
from app.models import Admission, finalizing_call, input_budget
from app.run import execute
from test_run import MemoryControl, ScriptedModel, request


@pytest.mark.asyncio
@pytest.mark.parametrize('agent_type',['general-agent','document-processing-agent','table-analysis','custom-skill-agent','knowledge-qa'])
async def test_shared_token_limit_finalizes_in_the_same_harness(agent_type):
    p=request(runtime_config={'agent_type':agent_type,'max_iterations':15 if agent_type=='knowledge-qa' else 100})
    c=MemoryControl(p)
    async def budget(role=''):
        return dict(max_tokens=3000000,remaining_tokens=9000,max_requests=100,remaining_requests=1,final_reserve=8192,remaining_seconds=100)
    c.budget=budget
    model=ScriptedModel([[TextBlock(text='Available result.')]])
    result=await execute(p,c,model)
    assert result.answer=='Available result.' and len(model.requests)==1
    prompt=str(model.requests[0])
    assert 'FINAL DECISION' in prompt and 'remaining=9000' in prompt
    assert not finalizing_call.get()


@pytest.mark.asyncio
async def test_rejected_admission_uses_reserved_call_without_generating_a_retry():
    c=MemoryControl(request())
    budget=IterationBudget(50,c)
    agent=SimpleNamespace(state=SimpleNamespace(cur_iter=17,middle_context={}))
    calls=[]
    generations=[]
    async def next_handler(**kwargs):
        calls.append(finalizing_call.get())
        if len(calls)==1:
            try: raise ControlError('finalization_required','reserved')
            except ControlError as inner: raise RuntimeError('wrapped SDK error') from inner
        assert kwargs['tool_choice'].mode=='GenerateStructuredOutput'
        assert kwargs['tool_choice'].tools==['GenerateStructuredOutput']
        generations.append('final')
        return 'final'
    assert await budget.on_model_call(agent,{'messages':[]},next_handler)=='final'
    assert calls==[False,True] and generations==['final']
    assert not finalizing_call.get()


@pytest.mark.asyncio
@pytest.mark.parametrize('code',['task_limit','finalization_required','ownership_lost'])
async def test_admission_returns_typed_errors_and_closes_rejected_socket(monkeypatch,code):
    import json
    closed=[]
    class Socket:
        async def send(self,data): pass
        async def recv(self): return json.dumps({'code':code,'error':'rejected'})
        async def close(self): closed.append(True)
    async def connect(*args,**kwargs): return Socket()
    monkeypatch.setattr('app.models.connect',connect)
    c=SimpleNamespace(base_url='http://control',payload=request())
    with pytest.raises(asyncio.CancelledError if code=='ownership_lost' else ControlError) as caught:
        await Admission(c).acquire()
    assert closed==[True]
    if code!='ownership_lost': assert error_code(caught.value)=='task_limit'


def test_token_estimate_handles_chinese_without_charging_utf8_bytes_as_tokens():
    assert input_budget('中'*1000)==1004
    assert input_budget('abcd'*1000)==1004
    assert input_budget('data:image/png;base64,AAAA')==16384


def test_wrapped_local_budget_error_is_not_a_network_error():
    import openai
    try:
        try: raise ControlError('task_limit','budget exhausted')
        except ControlError as cause:
            raise openai.APIConnectionError(request=httpx.Request('POST','http://fixture')) from cause
    except Exception as exc:
        assert error_code(exc)=='task_limit'


@pytest.mark.asyncio
async def test_provider_cannot_execute_tools_after_final_budget_is_reserved():
    from app.budget import BudgetExhausted
    from agentscope.message import ToolCallBlock
    p=request(tools=[{'name':'lookup','parameters':{'type':'object','properties':{}}}])
    c=MemoryControl(p)
    async def budget(role=''):
        return dict(max_tokens=3000000,remaining_tokens=9000,max_requests=100,remaining_requests=1,final_reserve=8192,remaining_seconds=100)
    c.budget=budget
    model=ScriptedModel([[ToolCallBlock(id='forbidden',name='lookup',input='{}')]])
    with pytest.raises(BudgetExhausted):
        await execute(p,c,model)
    assert not c.calls
