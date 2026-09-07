import json

import httpx
import pytest

from app.models import GovernedTransport, model_for
from app.models import ProviderIncomplete, StreamOutcome
from app.run import execute
from test_run import MemoryControl, request


@pytest.mark.asyncio
async def test_native_openai_client_streams_through_injected_http_transport():
    calls = []
    def provider(req):
        calls.append(json.loads(req.content))
        chunks = [
            {"id":"completion-1", "object":"chat.completion.chunk", "created":1,"model":"fixture",
             "choices":[{"index":0,"delta":{"role":"assistant","content":"Verified"},"finish_reason":None}]},
            {"id":"completion-1", "object":"chat.completion.chunk", "created":1,"model":"fixture",
             "choices":[{"index":0,"delta":{},"finish_reason":"stop"}],
             "usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}},
        ]
        return httpx.Response(200,headers={"content-type":"text/event-stream"},
                              content="".join("data: "+json.dumps(c)+"\n\n" for c in chunks)+"data: [DONE]\n\n")
    p = request()
    control = MemoryControl(p)
    async with model_for(p, control, transport=httpx.MockTransport(provider)) as model:
        result = await execute(p, control, model)
    assert result.answer == "Verified"
    assert len(calls) == 1
    assert calls[0]["messages"][-1]["role"] == "user"


@pytest.mark.asyncio
@pytest.mark.parametrize("protocol", ["anthropic", "openai-responses"])
async def test_other_native_provider_streams_complete_in_same_harness(protocol):
    calls = []
    if protocol == "anthropic":
        packets = [
            {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"fixture","content":[],"stop_reason":None,"stop_sequence":None,"usage":{"input_tokens":10,"output_tokens":0}}},
            {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}},
            {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Verified"}},
            {"type":"content_block_stop","index":0},
            {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":None},"usage":{"output_tokens":2}},
            {"type":"message_stop"},
        ]
    else:
        message={"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Verified","annotations":[]}]}
        response={"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"fixture","output":[message],"usage":{"input_tokens":10,"output_tokens":2,"total_tokens":12}}
        packets=[
            {"type":"response.created","sequence_number":0,"response":{**response,"status":"in_progress","output":[]}},
            {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{**message,"status":"in_progress","content":[]}},
            {"type":"response.content_part.added","sequence_number":2,"item_id":"msg_1","output_index":0,"content_index":0,"part":{"type":"output_text","text":"","annotations":[]}},
            {"type":"response.output_text.delta","sequence_number":3,"item_id":"msg_1","output_index":0,"content_index":0,"delta":"Verified"},
            {"type":"response.output_text.done","sequence_number":4,"item_id":"msg_1","output_index":0,"content_index":0,"text":"Verified"},
            {"type":"response.output_item.done","sequence_number":5,"output_index":0,"item":message},
            {"type":"response.completed","sequence_number":6,"response":response},
        ]

    def provider(req):
        calls.append(req)
        return httpx.Response(200,headers={"content-type":"text/event-stream"},
            content="".join("event: "+p["type"]+"\ndata: "+json.dumps(p)+"\n\n" for p in packets))

    payload=request()
    payload.llm.protocol=protocol
    control=MemoryControl(payload)
    async with model_for(payload,control,transport=httpx.MockTransport(provider)) as model:
        result=await execute(payload,control,model)
    assert result.answer == "Verified"
    assert len(calls) == 1


@pytest.mark.asyncio
async def test_gateway_sampling_and_secondary_vision_thinking_are_independent():
    payload=request(runtime_config={"thinking":True,"temperature":0.1})
    payload.llm.thinking_control="chat_template_kwargs"
    payload.llm.generation_policy="gateway"
    async with model_for(payload,MemoryControl(payload),transport=httpx.MockTransport(lambda _:httpx.Response(200)),model_role="vision") as model:
        assert model.parameters.temperature is None
        assert not model.parameters.thinking_enable
        assert "chat_template_kwargs" not in model.extra_body
    async with model_for(payload,MemoryControl(payload),transport=httpx.MockTransport(lambda _:httpx.Response(200))) as model:
        assert model.parameters.temperature is None
        assert model.extra_body["chat_template_kwargs"]=={"enable_thinking":True}


@pytest.mark.asyncio
async def test_admission_remains_held_until_provider_stream_is_closed():
    timeline = []
    class Lease:
        def __init__(self, control): pass
        async def acquire(self): timeline.append("acquired")
        async def close(self, status=0, error="", usage=None): timeline.append(("released",status))
    class Body(httpx.AsyncByteStream):
        async def __aiter__(self):
            yield b"one"
            yield b"two"
        async def aclose(self): timeline.append("body_closed")
    transport=GovernedTransport(None,httpx.MockTransport(lambda req: httpx.Response(200,stream=Body())),Lease)
    async with httpx.AsyncClient(transport=transport) as client:
        async with client.stream("GET","http://fixture") as response:
            assert timeline == ["acquired"]
            async for part in response.aiter_bytes():
                assert timeline == ["acquired"]
    assert timeline == ["acquired","body_closed",("released",200)]


@pytest.mark.parametrize("packet",[
    {"choices":[{"finish_reason":"length"}]},
    {"type":"message_delta","delta":{"stop_reason":"max_tokens"}},
    {"type":"response.incomplete"},
])
def test_incomplete_provider_responses_cannot_become_success(packet):
    outcome=StreamOutcome(httpx.Response(200,headers={"content-type":"text/event-stream"}))
    with pytest.raises(ProviderIncomplete):
        outcome.feed(b"data: "+json.dumps(packet).encode()+b"\n\n")


def test_stream_outcome_handles_split_frames_and_missing_termination():
    outcome=StreamOutcome(httpx.Response(200,headers={"content-type":"text/event-stream"}))
    outcome.feed(b'data: {"choices":[{"finish_reason":')
    with pytest.raises(ProviderIncomplete):outcome.finish()
    outcome.feed(b'"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}\n\n')
    outcome.finish()
    assert outcome.usage=={"input_tokens":3,"output_tokens":2}
