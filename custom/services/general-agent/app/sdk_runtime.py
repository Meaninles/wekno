"""Explicit Anthropic SDK adapter; uses the same workspace tools as platform."""
import asyncio
import time
from dataclasses import replace

from .final_delivery import ClaudeSDKTerminalCollector
from .schemas import RunEvent
from .protocol_capture import capture
from .working_context import ResponseRecovery, RECOVERY_MESSAGE


async def sdk_response_stream(prompt, options, observation):
    """Resume the SDK's own transcript once; never synthesize or replay tools."""
    from claude_agent_sdk import query, SystemMessage
    recovery = ResponseRecovery()
    remaining = options.max_turns
    while True:
        retry = False
        stream = query(prompt=prompt, options=options)
        try:
            async for message in stream:
                if message.__class__.__name__ == "ResultMessage":
                    remaining -= max(1, getattr(message, "num_turns", 1))
                    finish = getattr(message, "stop_reason", "") or ""
                    text = getattr(message, "result", "") or ""
                    session_id = getattr(message, "session_id", "")
                    # API errors, cancellation and max-turn failures have their
                    # own terminal semantics; an old stop reason cannot retry them.
                    eligible = (not getattr(message, "api_error_status", None)
                        and getattr(message, "subtype", "") == "success"
                        and not getattr(message, "is_error", False))
                    reason = recovery.take(finish, text) if eligible and session_id and remaining > 0 else ""
                    if reason:
                        capture(options.extra_args.get("name", ""), "sdk_recovery_terminal", message)
                        observation.setdefault("response_recoveries", []).append({"reason": reason, "session_id": session_id})
                        options = replace(options, resume=session_id, max_turns=remaining)
                        prompt = RECOVERY_MESSAGE
                        retry = True
                        yield SystemMessage(subtype="weknora_response_recovery", data={"reason": reason})
                        break
                yield message
        finally:
            await stream.aclose()
        if not retry:
            return


async def run_sdk(payload, system_prompt, prompt, tools, observation, config_dir):
    from claude_agent_sdk import ClaudeAgentOptions, HookMatcher
    from .runner import claude_auth_env, effective_max_turns, sdk_thinking_config, tool_use_fragments, tool_result_fragments
    env,model,settings=claude_auth_env(payload,config_dir)
    env.update(CLAUDE_CODE_DISABLE_AUTO_MEMORY="1",CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1")
    if payload.runtime_config.max_completion_tokens:
        env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"]=str(payload.runtime_config.max_completion_tokens)
    options=ClaudeAgentOptions(cwd=str(tools.workspace.root),env=env,settings=settings,
        system_prompt=system_prompt,setting_sources=[],tools=[],mcp_servers={"weknora":tools.sdk_server()},
        strict_mcp_config=True,allowed_tools=["mcp__weknora__"+name for name in tools.entries],
        permission_mode="dontAsk",include_partial_messages=True,max_turns=effective_max_turns(payload),
        model=model,thinking=sdk_thinking_config(payload),
        effort=(payload.llm.reasoning_effort or None) if payload.runtime_config.thinking is not False else None,
        hooks={"PreToolUse":[HookMatcher(matcher="mcp__weknora__.*",hooks=[tools.sdk_before_tool])]},
        extra_args={"name":payload.run_id})
    observation["runtime_capabilities"]={"adapter":"claude-sdk","temperature":False,
        "configured_temperature":payload.runtime_config.temperature,"cross_turn_history_owner":"platform",
        "current_turn_context_owner":"claude-sdk","tool_identity":"typed hook to MCP binding",
        "sdk_persistence":"run-local transcript for bounded recovery","session_name":"platform run ID"}
    collector=ClaudeSDKTerminalCollector()
    started=time.monotonic()
    pending={}
    wire=[]
    stream=sdk_response_stream(prompt,options,observation)
    try:
        async for message in stream:
            capture(payload.run_id,"sdk_typed_event",message)
            kind=message.__class__.__name__
            if kind=="SystemMessage" and getattr(message,"subtype","")=="weknora_response_recovery":
                progress_id="recovery-"+payload.run_id
                yield RunEvent(type="progress",id=progress_id,content="上次模型输出未完成，正在从已完成步骤继续",done=True,
                    data={"progress_kind":"assistant_status","progress_id":progress_id,"phase":"success",**message.data})
                continue
            wire.append({"event_type":kind,"elapsed_ms":round((time.monotonic()-started)*1000),
                "message_id":getattr(message,"message_id",None),"uuid":getattr(message,"uuid",None)})
            collector.observe(message)
            for call in tool_use_fragments(message):
                name=call.name.removeprefix("mcp__weknora__")
                pending[call.tool_use_id]=(name,call.input,time.monotonic())
                if tools.entries.get(name,(None,None,None,"workspace"))[3]=="workspace":
                    yield RunEvent(type="runtime_tool",id=call.tool_use_id,content="正在执行 "+name,
                        data={"tool_call_id":call.tool_use_id,"tool_name":name,"arguments":call.input,"phase":"start","origin":"sdk"})
            for result in tool_result_fragments(message):
                record=pending.pop(result.tool_use_id,None)
                if record and tools.entries.get(record[0],(None,None,None,"workspace"))[3]=="workspace":
                    name,args,began=record
                    execution = tools.sdk_executions.get(result.tool_use_id)
                    output = (execution.result() if execution is not None and execution.done()
                              and not execution.cancelled() else result.content)
                    yield RunEvent(type="runtime_tool",id=result.tool_use_id,content=name+" 执行结束",done=True,
                        data={"tool_call_id":result.tool_use_id,"tool_name":name,"arguments":args,"output":output,
                            "phase":"error" if result.is_error else "success","duration_ms":round((time.monotonic()-began)*1000),"origin":"sdk"})
            if kind=="ResultMessage":
                answer=collector.answer()
                if not answer:
                    if getattr(message,"is_error",False):
                        status=getattr(message,"api_error_status",None)
                        detail=str(getattr(message,"result","") or collector.answer_integrity_reason)[:1500]
                        raise RuntimeError(f"SDK request failed (HTTP {status or 'unknown'}): {detail}")
                    raise RuntimeError("SDK did not complete: "+collector.answer_integrity_reason)
                observation["sdk_usage"]=getattr(message,"usage",None)
                observation["sdk_protocol_events"]=wire
                yield RunEvent(type="answer_delta",id="general-answer-"+payload.run_id,content=answer,done=True)
                yield RunEvent(type="terminal_answer",content=answer)
                return
        raise RuntimeError("SDK stream ended before its result event")
    finally:
        await stream.aclose()
