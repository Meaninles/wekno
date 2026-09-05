"""Run inside the eval sidecar. Uses the actual SDK and runner with a local,
deterministic Anthropic-protocol timing server. No external model or credential
is used. This measures transport/buffering, not model intelligence or accuracy.
"""
from __future__ import annotations
import asyncio
import hashlib
import json
import os
from pathlib import Path
import re
import sys
import tempfile
import threading
import time
from urllib import request as urlrequest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

sys.path.insert(0, "/app")
from app import runner as runtime
from app.schemas import ChatPayload, LLMConfig, RuntimeConfigSpec
from app.final_delivery import terminal_binding_marker, TERMINAL_ANSWER_OPEN

OUT = Path(sys.argv[1] if len(sys.argv)>1 else "/tmp/root-cause-output")
OUT.mkdir(parents=True, exist_ok=True)
STATE = {}

class Provider(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"
    def log_message(self,*args): pass
    def do_POST(self):
        payload=json.loads(self.rfile.read(int(self.headers.get("Content-Length",0))))
        if "count_tokens" in self.path:
            data=json.dumps({"input_tokens":100}).encode()
            self.send_response(200);self.send_header("Content-Type","application/json");self.send_header("Content-Length",str(len(data)));self.end_headers();self.wfile.write(data);return
        STATE.setdefault("wire_requests",[]).append({"path":self.path,"t_s":time.perf_counter()-STATE["start"],"body":payload})
        self.send_response(200);self.send_header("Content-Type","text/event-stream");self.send_header("Cache-Control","no-cache");self.send_header("Connection","close");self.end_headers()
        def event(kind,data):
            self.wfile.write(("event: "+kind+"\ndata: "+json.dumps({"type":kind,**data},ensure_ascii=False)+"\n\n").encode());self.wfile.flush()
        event("message_start",{"message":{"id":"msg_local_audit","type":"message","role":"assistant","content":[],"model":"audit-model","stop_reason":None,"stop_sequence":None,"usage":{"input_tokens":100,"output_tokens":0}}})
        event("content_block_start",{"index":0,"content_block":{"type":"text","text":""}})
        text=terminal_binding_marker(STATE["run_id"])+TERMINAL_ANSWER_OPEN+"这是用于测量流式传输的固定输出。"
        fragments=[text[:len(text)-16],text[-16:-12],text[-12:-8],text[-8:-4],text[-4:]]
        for i,fragment in enumerate(fragments):
            time.sleep(0.35)
            STATE.setdefault("provider_chunks",[]).append({"i":i,"t_s":time.perf_counter()-STATE["start"]})
            event("content_block_delta",{"index":0,"delta":{"type":"text_delta","text":fragment}})
        event("content_block_stop",{"index":0})
        event("message_delta",{"delta":{"stop_reason":"end_turn","stop_sequence":None},"usage":{"output_tokens":30}})
        event("message_stop",{})
        STATE["provider_done_s"]=time.perf_counter()-STATE["start"]
        self.close_connection=True

async def timed_run(root:Path, port:int, temperature:float, number:int, persistence:bool=True):
    run_id=f"root-cause-timing-{number}"
    payload=ChatPayload(run_id=run_id, session_id="isolated-audit",assistant_message_id="audit",query="请用一句话说明你已收到这条消息。",llm=LLMConfig(model_name="audit-model",base_url=f"http://127.0.0.1:{port}",api_key="local-mock-no-credential"),tool_callback_url="http://127.0.0.1:1/unused",runtime_config=RuntimeConfigSpec(agent_type="general-agent",temperature=temperature,thinking=False,max_iterations=2))
    STATE.clear();STATE.update({"start":time.perf_counter(),"run_id":run_id})
    run=runtime.GeneralAgentRunner(root,payload)
    events=[]
    import claude_agent_sdk as sdk
    from claude_agent_sdk._internal.transport.subprocess_cli import SubprocessCLITransport
    original_query=sdk.query
    original_close=SubprocessCLITransport.close
    async def observed_close(transport):
        start=time.perf_counter()-STATE["start"]
        try:await original_close(transport)
        finally:STATE.setdefault("transport_close",[]).append({"start_s":start,"end_s":time.perf_counter()-STATE["start"]})
    async def observed_query(*args,**kwargs):
        if not persistence:
            # Generic removal of unused SDK transcript persistence; no prompt,
            # model response, tool schema or task content is modified.
            kwargs["options"].extra_args={**kwargs["options"].extra_args,"no-session-persistence":None}
        STATE["sdk_query_start_s"]=time.perf_counter()-STATE["start"]
        try:
            async for message in original_query(*args,**kwargs):
                STATE.setdefault("sdk_events",[]).append({"type":message.__class__.__name__,"t_s":time.perf_counter()-STATE["start"]})
                yield message
        finally:STATE["sdk_query_end_s"]=time.perf_counter()-STATE["start"]
    sdk.query=observed_query
    SubprocessCLITransport.close=observed_close
    try:
        async for event in run.run():
            events.append({"t_s":time.perf_counter()-STATE["start"],"event":event.model_dump()})
    finally:
        sdk.query=original_query
        SubprocessCLITransport.close=original_close
        run.cleanup()
    result={"number":number,"temperature_configured":temperature,"total_s":time.perf_counter()-STATE["start"],"provider_first_request_s":STATE.get("wire_requests",[{}])[0].get("t_s"),"provider_first_chunk_s":STATE.get("provider_chunks",[{}])[0].get("t_s"),"provider_done_s":STATE.get("provider_done_s"),"first_answer_s":next((x["t_s"] for x in events if x["event"]["type"]=="answer_delta" and x["event"].get("content")),None),"model_request_count":len(STATE.get("wire_requests",[])),"temperature_on_wire":[x["body"].get("temperature","ABSENT") for x in STATE.get("wire_requests",[])],"event_types":[x["event"]["type"] for x in events]}
    result["sdk_session_persistence"]=persistence
    result["transport_close"]=STATE.get("transport_close")
    result["sdk_result_s"]=next((x["t_s"] for x in STATE.get("sdk_events",[]) if x["type"]=="ResultMessage"),None)
    (OUT/f"runtime-wire-{number}.json").write_text(json.dumps({"summary":result,"state":STATE,"events":events},ensure_ascii=False,indent=2))
    print(json.dumps(result,ensure_ascii=False),flush=True)
    return result

def direct_transport(port:int):
    STATE.clear();STATE.update({"start":time.perf_counter(),"run_id":"direct-control"})
    req=urlrequest.Request(f"http://127.0.0.1:{port}/v1/messages",json.dumps({"model":"audit-model","messages":[{"role":"user","content":"请用一句话说明你已收到这条消息。"}],"max_tokens":256,"stream":True}).encode(),{"Content-Type":"application/json"})
    first=None
    with urlrequest.urlopen(req,timeout=10) as response:
        for line in response:
            if first is None and b"text_delta" in line:first=time.perf_counter()-STATE["start"]
    data={"experiment":"direct_same_timed_provider","total_s":time.perf_counter()-STATE["start"],"first_text_s":first,"provider_done_s":STATE.get("provider_done_s")}
    print(json.dumps(data),flush=True);(OUT/"direct-transport.json").write_text(json.dumps(data,indent=2))

def review_probe(root:Path):
    p=ChatPayload(run_id="root-cause-review",session_id="isolated-audit",assistant_message_id="audit",query="Keep existing content.",llm=LLMConfig(model_name="local-mock"),tool_callback_url="http://127.0.0.1:1/unused",enable_artifacts=True,runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"))
    run=runtime.GeneralAgentRunner(root,p)
    file=run.run_dir/"sample.txt";file.write_text("known unchanged issue")
    files=[{"filename":file.name,"file_path":str(file)}]
    before=hashlib.sha256(file.read_bytes()).hexdigest()
    failed=run.artifacts.review_artifacts(files,False,[{"issue":"content is wrong"}],"fails original request","not applicable")
    # Do not change a byte after the failed review.
    run.artifacts.ensure_reviewed(file.name,str(file))
    after=hashlib.sha256(file.read_bytes()).hexdigest()
    result={"experiment":"review_gate","review_passed":failed["passed"],"file_bytes_changed":before!=after,"ensure_reviewed_accepts_failed_unchanged_file":True}
    run.cleanup();result["workspace_removed_after_cleanup"]=not run.run_dir.exists()
    print(json.dumps(result),flush=True)
    (OUT/"review-gate.json").write_text(json.dumps(result,indent=2))

async def main():
    with tempfile.TemporaryDirectory(prefix="weknora-root-cause-") as tmp:
        root=Path(tmp);review_probe(root)
        server=ThreadingHTTPServer(("127.0.0.1",0),Provider)
        thread=threading.Thread(target=server.serve_forever,daemon=True);thread.start()
        try:
            results=[]
            for i,(temp,persistence) in enumerate([(0.0,True),(0.0,False),(1.0,True),(1.0,False)],1):results.append(await timed_run(root,server.server_port,temp,i,persistence))
            (OUT/"runtime-timing-summary.json").write_text(json.dumps(results,indent=2))
            direct_transport(server.server_port)
        finally:server.shutdown();server.server_close()

if __name__=="__main__":asyncio.run(main())
