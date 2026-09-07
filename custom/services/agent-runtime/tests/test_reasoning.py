import json

import httpx
import pytest

from app.models import ProviderIncomplete, model_for
from app.reasoning import ThinkContent
from app.run import execute
from test_run import MemoryControl, request


@pytest.mark.parametrize("wire,thought,answer", [
    ('<think>draft</think>Final <src id="S1" />', 'draft', 'Final <src id="S1" />'),
    ('draft\n</think>\nFinal', 'draft\n', '\nFinal'),
    ('draft\r\n</think>\r\nFinal', 'draft\r\n', '\r\nFinal'),
    ('draft\n</think>', 'draft\n', ''),
    ('Ordinary answer', '', 'Ordinary answer'),
    ('Repeat. Repeat.', '', 'Repeat. Repeat.'),
    ('Literal `</think>` example', '', 'Literal `</think>` example'),
    ('```xml\n</think>\n```\nText', '', '```xml\n</think>\n```\nText'),
    ('~~~xml\n</think>\n~~~\nText', '', '~~~xml\n</think>\n~~~\nText'),
    ('> </think>\n    </think>\n\t</think>', '', '> </think>\n    </think>\n\t</think>'),
])
def test_reasoning_boundaries_independent_of_chunk_splits(wire, thought, answer):
    for cut in range(len(wire) + 1):
        decoder = ThinkContent()
        out = [decoder.feed(wire[:cut]), decoder.feed(wire[cut:]), decoder.finish()]
        assert ''.join(x[0] for x in out) == thought
        assert ''.join(x[1] for x in out) == answer
    decoder = ThinkContent()
    out = [decoder.feed(char) for char in wire] + [decoder.finish()]
    assert ''.join(x[0] for x in out) == thought
    assert ''.join(x[1] for x in out) == answer


def test_unfinished_explicit_reasoning_is_never_an_answer():
    decoder = ThinkContent()
    assert decoder.feed('<think>unfinished') == ('', '')
    with pytest.raises(ProviderIncomplete):
        decoder.finish()


@pytest.mark.asyncio
async def test_closing_decoded_stream_closes_provider_iterator(monkeypatch):
    from agentscope.model import ChatResponse, OpenAIChatModel
    from agentscope.message import TextBlock
    closed = []
    async def provider(self, *args, **kwargs):
        try:
            yield ChatResponse(content=[TextBlock(text='<think>draft</think>answer')], is_last=False)
        finally:
            closed.append(True)
    monkeypatch.setattr(OpenAIChatModel, '_parse_stream_response', provider)
    payload = request()
    payload.llm.reasoning_format = 'think-tags'
    async with model_for(payload, MemoryControl(payload), transport=httpx.MockTransport(lambda _:None)) as model:
        stream = model._parse_stream_response()
        await anext(stream)
        await stream.aclose()
    assert closed == [True]


def provider_stream(deltas):
    packets = [{'id':'fixture-1','object':'chat.completion.chunk','created':1,'model':'fixture',
                'choices':[{'index':0,'delta':delta,'finish_reason':None}]} for delta in deltas]
    packets.append({'id':'fixture-1','object':'chat.completion.chunk','created':1,'model':'fixture',
                    'choices':[{'index':0,'delta':{},'finish_reason':'stop'}],
                    'usage':{'prompt_tokens':10,'completion_tokens':20,'total_tokens':30}})
    return httpx.Response(200, headers={'content-type':'text/event-stream'},
        content=''.join('data: '+json.dumps(p)+'\n\n' for p in packets)+'data: [DONE]\n\n')


@pytest.mark.asyncio
@pytest.mark.parametrize('agent_type', ['knowledge-qa','general-agent','document-processing-agent','table-analysis'])
async def test_tagged_reasoning_is_split_before_stream_checkpoint_and_delivery(agent_type):
    answer = 'Final answer <src id="S1" />'
    wire = answer + '\n</think>\n' + answer
    calls = []
    def provider(req):
        calls.append(req)
        return provider_stream([{'content':char} for char in wire])
    payload = request(runtime_config={'agent_type':agent_type})
    payload.llm.reasoning_format = 'think-tags'
    control = MemoryControl(payload)
    async with model_for(payload, control, transport=httpx.MockTransport(provider)) as model:
        result = await execute(payload, control, model)
    assert result.answer.strip() == answer
    assert ''.join(e['content'] for e in control.emitted if e['type']=='answer_delta').strip() == answer
    assert ''.join(e['content'] for e in control.emitted if e['type']=='thought_delta').strip() == answer
    last = control.snapshots[-1][1]['agent']['context'][-1]['content']
    assert ''.join(b['text'] for b in last if b['type']=='text').strip() == answer
    assert any(b['type']=='thinking' for b in last)
    assert len(calls) == 1


@pytest.mark.asyncio
@pytest.mark.parametrize('wire_format', ['native','think-tags'])
async def test_native_reasoning_fields_are_preserved(wire_format):
    payload = request()
    payload.llm.reasoning_format = wire_format
    control = MemoryControl(payload)
    async with model_for(payload, control, transport=httpx.MockTransport(lambda _: provider_stream([
        {'reasoning_content':'Inspect evidence','content':'Final answer'},
    ]))) as model:
        result = await execute(payload, control, model)
    assert result.answer == 'Final answer'
    assert ''.join(e['content'] for e in control.emitted if e['type']=='thought_delta') == 'Inspect evidence'


@pytest.mark.asyncio
async def test_native_protocol_keeps_literal_tags_in_content():
    payload = request()
    control = MemoryControl(payload)
    text = '<think>literal markup</think>'
    async with model_for(payload, control, transport=httpx.MockTransport(lambda _: provider_stream([{'content':text}]))) as model:
        result = await execute(payload, control, model)
    assert result.answer == text


@pytest.mark.asyncio
async def test_tool_arguments_and_usage_survive_tagged_text_decoding():
    from agentscope.message import Msg, TextBlock, ToolCallBlock
    payload = request()
    payload.llm.reasoning_format = 'think-tags'
    arguments = '{"literal":"</think>","nested":{"value":42}}'
    response = provider_stream([
        {'content':'Inspecting evidence'},
        {'tool_calls':[{'index':0,'id':'call-1','type':'function','function':{'name':'lookup','arguments':arguments[:15]}}]},
        {'tool_calls':[{'index':0,'function':{'arguments':arguments[15:]}}]},
    ])
    async with model_for(payload, MemoryControl(payload), transport=httpx.MockTransport(lambda _:response)) as model:
        stream = await model(messages=[Msg(name='user', role='user', content=[TextBlock(text='Inspect')])])
        chunks = [chunk async for chunk in stream]
    final = chunks[-1]
    tool = next(b for b in final.content if isinstance(b, ToolCallBlock))
    assert tool.id == 'call-1' and tool.name == 'lookup'
    assert json.loads(tool.input) == json.loads(arguments)
    assert final.usage.input_tokens == 10 and final.usage.output_tokens == 20
    assert ''.join(b.text for b in final.content if isinstance(b, TextBlock)) == 'Inspecting evidence'
