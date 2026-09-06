"""Installed SDK process + loopback provider + real MCP/workspace execution.

This tests transport identity only, not model answer quality. Run in the eval
sidecar with the updated app isolated on PYTHONPATH; no live service is changed.
"""
import asyncio
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import sys
import tempfile
import threading

from app.runner import ArtifactStore
from app.runtime_tools import RuntimeTools
from app.schemas import ChatPayload
from app.sdk_runtime import run_sdk


async def main():
    out = Path(sys.argv[1]); out.mkdir(parents=True, exist_ok=True)
    os.environ["WEKNORA_AGENT_PROTOCOL_CAPTURE_DIR"] = str(out / "protocol")
    requests, events = [], []

    class Provider(BaseHTTPRequestHandler):
        protocol_version = "HTTP/1.1"
        def log_message(self, *args): pass
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", 0))))
            if "count_tokens" in self.path:
                self.send_response(200); self.send_header("Content-Length", "20"); self.end_headers()
                self.wfile.write(b'{"input_tokens":100}'); return
            requests.append(body)
            results = {c.get("tool_use_id"): c for m in body.get("messages", []) for c in m.get("content", []) if isinstance(c, dict) and c.get("type") == "tool_result"}
            if "fixture-execute" not in results:
                call = ("fixture-execute", "execute_code", {"language":"python", "code":"from pathlib import Path\nPath('proof.txt').write_text('execution persisted')\nprint('workspace operation completed')"})
            elif "fixture-read" not in results:
                call = ("fixture-read", "read_file", {"file_path":"proof.txt"})
            else:
                call = None
            self.send_response(200); self.send_header("Content-Type", "text/event-stream"); self.send_header("Connection", "close"); self.end_headers()
            def send(kind, **payload):
                self.wfile.write(("event: " + kind + "\ndata: " + json.dumps({"type":kind, **payload}) + "\n\n").encode()); self.wfile.flush()
            send("message_start", message={"id":"msg-fixture-"+str(len(requests)),"type":"message","role":"assistant","content":[],"model":"protocol-fixture","stop_reason":None,"stop_sequence":None,"usage":{"input_tokens":100,"output_tokens":0}})
            if call:
                send("content_block_start", index=0, content_block={"type":"tool_use","id":call[0],"name":"mcp__weknora__"+call[1],"input":{}})
                send("content_block_delta", index=0, delta={"type":"input_json_delta","partial_json":json.dumps(call[2])})
            else:
                send("content_block_start", index=0, content_block={"type":"text","text":""})
                send("content_block_delta", index=0, delta={"type":"text_delta","text":"The protocol fixture has completed."})
            send("content_block_stop", index=0)
            send("message_delta", delta={"stop_reason":"tool_use" if call else "end_turn","stop_sequence":None},usage={"output_tokens":60})
            send("message_stop")
            self.close_connection = True

    server = ThreadingHTTPServer(("127.0.0.1", 0), Provider)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    failure = None
    try:
        with tempfile.TemporaryDirectory(prefix="repair-sdk-") as folder:
            root = Path(folder)
            payload = ChatPayload(run_id="sdk-identity-fixture",session_id="local-fixture",assistant_message_id="answer",query="Run the requested workspace operation and read its output file.",
                llm={"model_name":"protocol-fixture","runtime_adapter":"claude-sdk","provider":"anthropic","base_url":f"http://127.0.0.1:{server.server_port}","api_key":"loopback-fixture"},
                runtime_config={"thinking":False,"max_iterations":5},tool_callback_url="http://127.0.0.1:1/unused",enable_artifacts=True)
            runtime = RuntimeTools(payload, ArtifactStore(root, payload))
            observation = {}
            async with asyncio.timeout(50):
                async for event in run_sdk(payload,"Use available tools to complete the request.",payload.query,runtime,observation,root/"claude-config"):
                    events.append(event.model_dump())
            assert (runtime.workspace.root/"proof.txt").read_text() == "execution persisted"
            assert set(runtime.sdk_executions) == {"fixture-execute", "fixture-read"}
            for identity in runtime.sdk_executions:
                assert any(e["type"] == "runtime_tool" and e.get("id") == identity and e.get("done") for e in events)
            assert len(requests) == 3, "unexpected hidden model request or tool retry"
            assert not any("__weknora_transport_binding" in json.dumps(r.get("tools", [])) for r in requests)
    except Exception as exc:
        failure = repr(exc)
    finally:
        server.shutdown(); server.server_close()
        (out/"sdk-identity.json").write_text(json.dumps({"error":failure,"requests":requests,"events":events},ensure_ascii=False,indent=2))
    print(json.dumps({"error":failure,"provider_requests":len(requests),"events":len(events)},ensure_ascii=False),flush=True)
    if failure: raise SystemExit(1)


if __name__ == "__main__": asyncio.run(main())
