"""Real SDK + governed HTTP streams; no live model or business mutations."""
import asyncio
import json

import httpx
import pytest

from app.control import ControlError, ControlUnavailable
from app.models import GovernedTransport, ProviderIncomplete, model_for
from app.state import model_interrupted
from test_run import MemoryControl, execute, request


def packet(delta=None, finish=None):
    return {"id": "fixture", "object": "chat.completion.chunk", "model": "fixture",
            "choices": [{"index": 0, "delta": delta or {}, "finish_reason": finish}]}


def tool(call_id, value):
    return packet({"tool_calls": [{"index": 0, "id": call_id, "type": "function",
                    "function": {"name": "record", "arguments": json.dumps({"value": value})}}]})


class Body(httpx.AsyncByteStream):
    def __init__(self, packets, error=None):
        self.packets, self.error = packets, error

    async def __aiter__(self):
        for item in self.packets:
            yield b"data: " + json.dumps(item).encode() + b"\n\n"
        if self.error:
            raise self.error


def fixture(bodies, **updates):
    payload = request(tools=[{"name": "record", "parameters": {
        "type": "object", "properties": {"value": {"type": "string"}}, "required": ["value"]}}], **updates)
    control = MemoryControl(payload)
    requests, leases = [], []
    bodies = iter(bodies)

    class Lease:
        def __init__(self, _): pass
        async def acquire(self): leases.append("acquired")
        async def close(self, *args): leases.append("released")

    def provider(req):
        requests.append(json.loads(req.content))
        return httpx.Response(200, headers={"content-type": "text/event-stream"}, stream=next(bodies))

    transport = GovernedTransport(control, httpx.MockTransport(provider), Lease)
    return payload, control, requests, leases, transport


def broken(error=None):
    return Body([packet({"content": "discard this partial answer"}),
                 packet({"tool_calls": [{"index": 0, "id": "partial-call", "type": "function",
                         "function": {"name": "record", "arguments": '{"value":"unfinished'}}]})],
                error)


def final():
    return Body([packet({"tool_calls":[{"index":0,"id":"answer","type":"function","function":{
        "name":"GenerateStructuredOutput","arguments":json.dumps({"answer":"Verified"})}}]}), packet(finish="tool_calls")])


@pytest.mark.asyncio
async def test_partial_final_answer_is_replaced_by_new_revision_after_disconnect():
    partial=Body([packet({'tool_calls':[{'index':0,'id':'interrupted-answer','type':'function','function':{
        'name':'GenerateStructuredOutput','arguments':'{"answer":"partial candidate'}}]})], httpx.ReadError('disconnected'))
    payload,control,requests,_,transport=fixture([partial,final()])
    async with model_for(payload,control,transport=transport) as model:
        result=await execute(payload,control,model)
    deltas=[e for e in control.emitted if e['type']=='answer_delta']
    assert deltas[0]['content']=='partial candidate' and deltas[-1]['content']=='Verified'
    assert deltas[-1]['revision']>deltas[0]['revision']
    assert result.answer=='Verified' and len(requests)==2 and not control.calls


@pytest.mark.asyncio
@pytest.mark.parametrize("kind", ["knowledge-qa", "general-agent", "document-processing-agent",
                                  "data-analysis", "table-analysis", "custom"])
async def test_interrupted_decision_recovers_without_replaying_tools_or_partial_arguments(kind):
    payload, control, requests, leases, transport = fixture([
        Body([tool("previous-call", "previous"), packet(finish="tool_calls")]),
        broken(httpx.RemoteProtocolError("peer closed mid-stream")),
        Body([tool("replacement-call", "complete"), packet(finish="tool_calls")]), final(),
    ], runtime_config={"agent_type": kind})
    async with model_for(payload, control, transport=transport) as model:
        result = await execute(payload, control, model)
    assert result.answer == "Verified"
    assert [(args, call_id) for _, args, call_id in control.calls] == [
        ({"value": "previous"}, "previous-call"), ({"value": "complete"}, "replacement-call")]
    assert len(requests) == 4
    assert requests[1]["messages"] == requests[2]["messages"]
    assert leases == ["acquired", "released"] * 4
    assert all("partial-call" not in str(snapshot) for _, snapshot in control.snapshots)
    assert any(snapshot["agent"]["middle_context"].get("model_stream_retries") == 1
               for _, snapshot in control.snapshots)
    assert "model_stream_retries" not in control.snapshots[-1][1]["agent"]["middle_context"]
    deltas = [e for e in control.emitted if e["type"] == "answer_delta"]
    assert ''.join(e['content'] for e in deltas)=='Verified'
    assert deltas[-1]["revision"] == 4
    assert [e["seq"] for e in control.emitted] == sorted({e["seq"] for e in control.emitted})


@pytest.mark.asyncio
async def test_missing_stream_termination_recovers_but_length_limit_does_not():
    payload, control, calls, _, transport = fixture([broken(), final()])
    async with model_for(payload, control, transport=transport) as model:
        assert (await execute(payload, control, model)).answer == "Verified"
    assert len(calls) == 2 and not control.calls

    payload, control, calls, _, transport = fixture([Body([packet(finish="length")])])
    async with model_for(payload, control, transport=transport) as model:
        with pytest.raises(ProviderIncomplete):
            await execute(payload, control, model)
    assert len(calls) == 1 and not control.calls and control.committed is None


@pytest.mark.asyncio
async def test_repeated_disconnects_stop_after_two_retries():
    payload, control, calls, leases, transport = fixture([
        broken(httpx.ReadError("disconnected")) for _ in range(3)])
    async with model_for(payload, control, transport=transport) as model:
        with pytest.raises(httpx.ReadError):
            await execute(payload, control, model)
    assert len(calls) == 3 and leases == ["acquired", "released"] * 3
    assert not control.calls and control.committed is None
    assert control.snapshots[-1][1]["agent"]["middle_context"]["model_stream_retries"] == 2


@pytest.mark.asyncio
async def test_recovery_refreshes_budget_and_cannot_bypass_denial():
    payload, control, calls, _, transport = fixture([broken(httpx.ReadError("disconnected"))])
    original_budget = control.budget
    async def budget(role=""):
        if calls:
            raise ControlError("task_limit", "No budget remains")
        return await original_budget(role)
    control.budget = budget
    async with model_for(payload, control, transport=transport) as model:
        with pytest.raises(ControlError):
            await execute(payload, control, model)
    assert len(calls) == 1 and not control.calls


@pytest.mark.asyncio
async def test_cancel_during_retry_backoff_never_starts_another_request():
    payload, control, calls, _, transport = fixture([broken(httpx.ReadError("disconnected"))])
    ready = asyncio.Event()
    original_checkpoint = control.checkpoint
    async def checkpoint(state, *, boundary):
        await original_checkpoint(state, boundary=boundary)
        if state["agent"]["middle_context"].get("model_stream_retries"):
            ready.set()
    control.checkpoint = checkpoint
    async with model_for(payload, control, transport=transport) as model:
        task = asyncio.create_task(execute(payload, control, model))
        await asyncio.wait_for(ready.wait(), 5)
        task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await task
    assert len(calls) == 1 and not control.calls and control.committed is None


def test_control_failure_and_permanent_model_errors_are_not_retried():
    assert not model_interrupted(ControlUnavailable("control_unavailable", "offline"))
    assert not model_interrupted(ProviderIncomplete("length"))
    assert not model_interrupted(ValueError("invalid model parameters"))
    assert not model_interrupted(httpx.LocalProtocolError("invalid request"))
    assert model_interrupted(httpx.RemoteProtocolError("peer closed"))
    assert model_interrupted(httpx.ReadTimeout("idle"))
