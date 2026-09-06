"""Actual installed SDK against a loopback protocol fixture, not a real LLM.

Run inside eval general-agent: python remaining_sdk_probe.py OUTPUT_DIRECTORY
The fixture contrasts text with typed tool_use, optional framing, and the
SDK's documented --name option. No production runtime setting is changed.
"""
from __future__ import annotations

import asyncio
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import sys
import tempfile
import threading
import time

sys.path.insert(0, "/app")
from app import runner as runtime
from app.final_delivery import terminal_binding_marker, TERMINAL_ANSWER_OPEN
from app.schemas import ChatPayload, LLMConfig, RuntimeConfigSpec


def provider(state):
    class Provider(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"

        def log_message(self, *args): pass

        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            if "count_tokens" in self.path:
                data = b'{"input_tokens":100}'
                self.send_response(200); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data); return
            is_title = "Generate a concise, sentence-case title" in json.dumps(body.get("system", []))
            state["requests"].append({"t_s": time.perf_counter() - state["start"], "is_title": is_title, "body": body})
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.send_header("Connection", "close"); self.end_headers()

            def send(kind, payload):
                self.wfile.write(("event: " + kind + "\ndata: " + json.dumps({"type": kind, **payload}, ensure_ascii=False) + "\n\n").encode()); self.wfile.flush()

            send("message_start", {"message": {"id": "msg_probe_" + str(len(state["requests"])), "type": "message", "role": "assistant", "content": [], "model": "protocol-fixture", "stop_reason": None, "stop_sequence": None, "usage": {"input_tokens": 100, "output_tokens": 0}}})
            tool_done = any(c.get("type") == "tool_result" for m in body.get("messages", []) for c in m.get("content", []) if isinstance(c, dict))
            typed_tool = not is_title and state["mode"] == "typed_tool" and not tool_done
            if typed_tool:
                send("content_block_start", {"index": 0, "content_block": {"type": "tool_use", "id": "fixture_call", "name": "Bash", "input": {}}})
                send("content_block_delta", {"index": 0, "delta": {"type": "input_json_delta", "partial_json": json.dumps({"command": "printf 'audit-tool-result\\n'", "description": "Print a fixed diagnostic value"})}})
            else:
                if is_title:
                    text = '{"title":"Protocol fixture"}'
                elif state["mode"] == "raw_tool_text":
                    text = 'I will run the requested operation. </parameter>\n</invoke>\n</｜DSML｜tool_calls>'
                elif state["mode"] == "typed_tool":
                    text = "工具返回 audit-tool-result，已完成。"
                else:
                    text = "这份传输样本只检查文字能否逐段到达。第一段介绍测试目的，第二段说明消息边界，后续内容用于观察缓存。传输服务每隔固定时间返回一段文本，并在全部文字发完后结束。此处没有外部模型参与，也没有对业务答案进行评分。记录首段抵达与最终完成的时间，可以区分生成等待和应用缓冲。各段使用不同句子，避免把正常结束与重复内容检测混在同一个实验中。最后一段说明采集过程将在服务关闭后保存原始消息和工具事件，以供人工检查。"
                    if state["mode"] == "bound":
                        text = terminal_binding_marker(state["run_id"]) + TERMINAL_ANSWER_OPEN + text
                send("content_block_start", {"index": 0, "content_block": {"type": "text", "text": ""}})
                parts = [text[i:i + 30] for i in range(0, len(text), 30)]
                for part in parts:
                    if not is_title: time.sleep(0.15)
                    send("content_block_delta", {"index": 0, "delta": {"type": "text_delta", "text": part}})
                    if not is_title: state.setdefault("first_provider_text_s", time.perf_counter() - state["start"])
            send("content_block_stop", {"index": 0})
            send("message_delta", {"delta": {"stop_reason": "tool_use" if typed_tool else "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 80}})
            send("message_stop", {})
            self.close_connection = True
    return Provider


async def one(root, out, mode, named, number):
    import claude_agent_sdk as sdk
    state = {"mode": mode, "run_id": f"fixture-{number}", "start": time.perf_counter(), "requests": []}
    server = ThreadingHTTPServer(("127.0.0.1", 0), provider(state))
    thread = threading.Thread(target=server.serve_forever, daemon=True); thread.start()
    original = sdk.query

    async def observed_query(*args, **kwargs):
        if named: kwargs["options"].extra_args = {**kwargs["options"].extra_args, "name": "Diagnostic session"}
        async for message in original(*args, **kwargs):
            state.setdefault("sdk_events", []).append({"t_s": time.perf_counter() - state["start"], "type": message.__class__.__name__})
            yield message

    sdk.query = observed_query
    payload = ChatPayload(run_id=state["run_id"], session_id="isolated-protocol-fixture", assistant_message_id="answer", query="执行一个简单操作，然后说明结果。", llm=LLMConfig(model_name="protocol-fixture", base_url=f"http://127.0.0.1:{server.server_port}", api_key="loopback-fixture"), tool_callback_url="http://127.0.0.1:1/unused", runtime_config=RuntimeConfigSpec(agent_type="general-agent", thinking=False, max_iterations=3))
    run = runtime.GeneralAgentRunner(root, payload); events = []; failure = None
    try:
        async for event in run.run(): events.append({"t_s": time.perf_counter() - state["start"], "event": event.model_dump()})
    except Exception as error: failure = str(error)
    finally:
        sdk.query = original; run.cleanup(); server.shutdown(); server.server_close()
    final = next((x["event"]["data"] for x in events if x["event"]["type"] == "result"), {})
    summary = {"mode": mode, "named": named, "number": number, "main_requests": sum(not q["is_title"] for q in state["requests"]), "title_requests": sum(q["is_title"] for q in state["requests"]), "first_provider_text_s": state.get("first_provider_text_s"), "first_answer_s": next((x["t_s"] for x in events if x["event"]["type"] == "answer_delta" and x["event"].get("content")), None), "result_s": next((x["t_s"] for x in events if x["event"]["type"] == "result"), None), "tool_events": [x["event"]["data"] for x in events if x["event"]["type"] == "progress" and (x["event"].get("data") or {}).get("tool_name")], "answer": final.get("answer"), "terminal": final.get("prompt_observation", {}).get("terminal_delivery"), "error": failure}
    (out / f"sdk-{number}.json").write_text(json.dumps({"summary": summary, "state": state, "events": events}, ensure_ascii=False, indent=2))
    print(json.dumps({k: v for k, v in summary.items() if k != "tool_events"}, ensure_ascii=False), flush=True)


async def main():
    out = Path(sys.argv[1]); out.mkdir(parents=True, exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="remaining-sdk-") as temp:
        for i, (mode, named) in enumerate([("plain", False), ("plain", True), ("bound", True), ("raw_tool_text", True), ("typed_tool", True)], 1):
            await one(Path(temp), out, mode, named, i)


if __name__ == "__main__": asyncio.run(main())
