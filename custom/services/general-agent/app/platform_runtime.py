"""Typed tool loop using the platform's configured model adapter."""
import json
import time
import uuid

import httpx

from .schemas import RunEvent
from .protocol_capture import capture
from .working_context import ResponseRecovery, RECOVERY_MESSAGE, recovery_reason


async def run_platform(payload, system_prompt, prompt, tools, observation):
    messages=[{"role":"system","content":system_prompt}]
    for historical in payload.history:
        body=historical.content
        if historical.source_id:
            body+="\n"+json.dumps({"conversation_record":historical.source_id},ensure_ascii=False)
        if historical.attachments:
            body+="\n"+json.dumps({"attachments":[a.model_dump(exclude={"content"}) for a in historical.attachments]},ensure_ascii=False)
        if historical.mentioned_items:
            body+="\n"+json.dumps({"mentions":historical.mentioned_items},ensure_ascii=False)
        if historical.images:
            body+="\n"+json.dumps({"image_records":[a.model_dump() for a in historical.images]},ensure_ascii=False)
        messages.append({"role":historical.role,"content":body})
    messages.append({"role":"user","content":prompt})
    if payload.image_urls and payload.llm.supports_vision:
        messages[-1]["images"]=payload.image_urls
    endpoint=payload.tool_callback_url.rsplit("/tools/call",1)[0]+"/model/call"
    headers={"Authorization":"Bearer "+payload.tool_callback_api_key}
    timeout=payload.runtime_config.llm_call_timeout or 300
    start=time.monotonic()
    calls=[]
    recovery=ResponseRecovery()
    answer_id="general-answer-"+payload.run_id
    # These are runtime facts, not advertised-but-ignored SDK parameters.
    observation["runtime_capabilities"]={"adapter":"platform", "history_owner":"platform",
        "temperature":True,"thinking":True,"max_completion_tokens":True,"automatic_title":False}
    async with httpx.AsyncClient(timeout=httpx.Timeout(timeout,connect=15)) as client:
        for iteration in range(payload.runtime_config.max_iterations or 30):
            call_id=str(uuid.uuid4())
            began=time.monotonic()
            record={"call_id":call_id,"iteration":iteration,"model":payload.llm.model_name,
                "provider":payload.llm.provider,"input_characters":len(json.dumps(messages,ensure_ascii=False)),
                "tools_characters":len(json.dumps(tools.definitions(),ensure_ascii=False))}
            content=[];reasoning=[];tool_calls=[];finish="";done=False
            body={"run_id":payload.run_id,"call_id":call_id,"messages":messages,"tools":tools.definitions()}
            capture(payload.run_id,"platform_model_request",body)
            async with client.stream("POST",endpoint,headers=headers,json=body) as response:
                if response.status_code!=200:
                    error=(await response.aread()).decode("utf-8","replace")[:4096]
                    raise RuntimeError(f"model callback HTTP {response.status_code}: {error}")
                async for line in response.aiter_lines():
                    if not line:
                        continue
                    chunk=json.loads(line)
                    capture(payload.run_id,"platform_model_event",chunk)
                    record.setdefault("first_event_ms",round((time.monotonic()-began)*1000))
                    kind=chunk.get("response_type")
                    if kind=="error":
                        raise RuntimeError(chunk.get("content") or "model stream error")
                    if kind=="thinking":
                        reasoning.append(chunk.get("content", ""))
                    elif kind=="answer":
                        text=chunk.get("content", "")
                        content.append(text)
                        if text:
                            record.setdefault("first_text_ms",round((time.monotonic()-began)*1000))
                    if chunk.get("tool_calls"):
                        tool_calls=chunk["tool_calls"]
                    if chunk.get("usage"):
                        record["usage"]=chunk["usage"]
                    if chunk.get("finish_reason"):
                        finish=chunk["finish_reason"]
                    done=done or (kind!="thinking" and bool(chunk.get("done")))
            record.update(duration_ms=round((time.monotonic()-began)*1000),finish_reason=finish,
                tool_call_ids=[call["id"] for call in tool_calls],answer_characters=sum(map(len,content)))
            calls.append(record)
            observation["model_calls"]=calls
            if not done:
                raise RuntimeError("model stream ended without a terminal event")
            text="".join(content)
            reason=recovery_reason(finish,text,bool(tool_calls))
            if reason:
                record["invalid_terminal"]=reason
                if recovery.take(finish,text,bool(tool_calls)):
                    observation.setdefault("response_recoveries",[]).append({"call_id":call_id,"reason":reason})
                    # Do not commit unfinished assistant text, thinking or tool
                    # arguments. Keep all completed protocol pairs unchanged.
                    messages.append({"role":"user","content":RECOVERY_MESSAGE})
                    yield RunEvent(type="progress",id="recovery-"+call_id,content="上次模型输出未完成，正在从已完成步骤继续",done=True,
                        data={"progress_kind":"assistant_status","progress_id":"recovery-"+call_id,"phase":"success","reason":reason})
                    continue
                raise RuntimeError(f"model repeatedly ended without a complete response: {reason} ({finish})")
            if tool_calls:
                if finish not in {"tool_calls","tool_use","stop"}:
                    raise RuntimeError(f"incomplete tool response: {finish}")
                messages.append({"role":"assistant","content":text,"reasoning_content":"".join(reasoning),"tool_calls":tool_calls})
                image_messages=[]
                for call in tool_calls:
                    name=call["function"]["name"]
                    before=time.monotonic()
                    try:
                        args=json.loads(call["function"]["arguments"])
                    except (TypeError,json.JSONDecodeError) as exc:
                        result={"success":False,"error":"arguments must be a complete JSON object: "+str(exc)}
                        raw={"arguments_raw":call["function"]["arguments"]}
                        for phase in ("start","error"):
                            yield RunEvent(type="runtime_tool",id=call["id"],content=name+" 参数校验" ,done=phase=="error",
                                data={"tool_call_id":call["id"],"tool_name":name,"phase":phase,"arguments":raw,
                                    "output":result if phase=="error" else None,"origin":"runtime_validation",
                                    "duration_ms":round((time.monotonic()-before)*1000)})
                    else:
                        origin=tools.entries.get(name,(None,None,None,"workspace"))[3]
                        before=time.monotonic()
                        if origin=="workspace":
                            yield RunEvent(type="runtime_tool",id=call["id"],content="正在执行 "+name,
                                data={"tool_call_id":call["id"],"tool_name":name,"phase":"start","arguments":args,"origin":"workspace"})
                        result=await tools.execute(name,args,call["id"])
                        if origin=="workspace":
                            yield RunEvent(type="runtime_tool",id=call["id"],content=name+" 执行结束",done=True,
                                data={"tool_call_id":call["id"],"tool_name":name,"phase":"error" if result.get("success") is False else "success",
                                "arguments":args,"output":result,"duration_ms":round((time.monotonic()-before)*1000),"origin":"workspace"})
                    tool_message={"role":"tool","tool_call_id":call["id"],"name":name,"content":tools.model_result(result,name)}
                    messages.append(tool_message)
                    if result.get("images") and payload.llm.supports_vision:
                        image_messages.append({"role":"user","content":"Image output from tool call "+call["id"],"images":result["images"]})
                # Keep every tool result adjacent to its assistant tool-call
                # group before introducing a new multimodal user message.
                messages.extend(image_messages)
                continue
            if finish not in {"stop","end_turn"} or not text.strip():
                raise RuntimeError(f"model did not deliver a complete answer: finish_reason={finish!r}")
            # A tool-enabled message is classified only after its typed finish.
            # Operational preambles are not counted as first user answer text.
            observation["first_delivered_answer_ms"]=round((time.monotonic()-start)*1000)
            yield RunEvent(type="answer_delta",id=answer_id,content=text,done=True)
            yield RunEvent(type="terminal_answer",content=text)
            return
    raise RuntimeError("agent reached the configured tool-turn limit without completing the request")
