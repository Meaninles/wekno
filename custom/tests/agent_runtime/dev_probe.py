"""Exercise the existing development API; never create another environment.

Credentials remain in memory. Reports contain only the selected case, answers,
timings and public source/tool metadata, and are written under .local-data.
"""
import argparse
import base64
import json
import os
from pathlib import Path
import subprocess
import time
import urllib.request
from cryptography.hazmat.primitives.ciphers.aead import AESGCM


def sql(query):
    command = ["docker", "exec", "-i", os.environ.get("WEKNORA_PROBE_POSTGRES", "WeKnora-postgres-dev"), "sh", "-c",
               'psql -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -At']
    result = subprocess.run(command, input=query, capture_output=True, text=True, encoding="utf-8", check=True)
    return [json.loads(line) for line in result.stdout.splitlines() if line.strip()]


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--phase", default="before")
    parser.add_argument("--query")
    parser.add_argument("--agent", default="builtin-knowledge-qa")
    parser.add_argument("--model")
    parser.add_argument("--session")
    parser.add_argument("--expect")
    parser.add_argument("--upload", action="append", default=[])
    parser.add_argument("--kb", action="append", default=[])
    parser.add_argument("--base", default="http://localhost:8080")
    args = parser.parse_args()
    tenant = sql("SELECT row_to_json(t) FROM (SELECT id,api_key FROM tenants WHERE deleted_at IS NULL ORDER BY (SELECT count(*) FROM sessions s WHERE s.tenant_id=tenants.id) DESC LIMIT 1)t;")[0]
    key = tenant["api_key"]
    if key.startswith("enc:v1:"):
        info = json.loads(subprocess.check_output(["docker","inspect",os.environ.get("WEKNORA_PROBE_API", "weknora-runtime-api-1")],text=True))
        env = dict(value.split("=",1) for value in info[0]["Config"]["Env"])
        data = base64.urlsafe_b64decode(key[7:] + "=" * (-len(key[7:]) % 4))
        key = AESGCM(env["SYSTEM_AES_KEY"].encode()).decrypt(data[:12],data[12:],None).decode()
    headers = {"X-API-Key": key, "Content-Type": "application/json"}
    def request(path, body=None):
        req = urllib.request.Request(args.base+"/api/v1"+path, headers=headers,
            data=json.dumps(body,ensure_ascii=False).encode() if body is not None else None)
        return urllib.request.urlopen(req, timeout=600)
    if not args.query:
        print(json.dumps(sql("SELECT row_to_json(t) FROM (SELECT id,name,type,tenant_id FROM models WHERE deleted_at IS NULL)t;"),ensure_ascii=False))
        print(json.dumps(sql("SELECT row_to_json(t) FROM (SELECT id,name,tenant_id FROM knowledge_bases WHERE deleted_at IS NULL)t;"),ensure_ascii=False))
        return
    if args.session:
        session = {"id":args.session}
    else:
        with request("/sessions", {"title":"统一内核开发验证 · "+args.phase}) as response:
            session = json.load(response)["data"]
    payload = {"query":args.query, "agent_id":args.agent, "knowledge_base_ids":args.kb,
               "enable_memory":False, "web_search_enabled":False}
    if args.upload:
        payload["upload_ids"] = args.upload
    if args.model:
        payload["summary_model_id"] = args.model
    started, first_text, events = time.monotonic(), None, []
    with request("/knowledge-chat/"+session["id"],payload) as response:
        for line in response:
            if not line.startswith(b"data:"): continue
            raw=line[5:].strip()
            if raw==b"[DONE]":break
            item=json.loads(raw)
            events.append(item)
            if first_text is None and item.get("response_type",item.get("type")) in ("answer","final_answer"):
                first_text=time.monotonic()-started
    report={"phase":args.phase,"session_id":session["id"],"query":args.query,
            "first_text_seconds":first_text,"duration_seconds":time.monotonic()-started,"events":events}
    completed = [e for e in events if e.get("response_type") == "complete"]
    errors = [e for e in events if e.get("response_type") == "error"]
    rows = sql("SELECT row_to_json(t) FROM (SELECT id,status,model_requests,input_tokens,output_tokens,error,result->>'answer' AS answer FROM custom_agent_runs WHERE session_id='"+session["id"]+"' ORDER BY created_at DESC LIMIT 1)t;")
    report["run"] = rows[0] if rows else None
    report["checks"] = {"one_completion":len(completed)==1,"no_error":not errors,
        "durable_answer_matches":bool(rows and completed and rows[0]["answer"]==completed[-1].get("data",{}).get("final_answer")),
        "expected_answer":not args.expect or bool(rows and args.expect in (rows[0]["answer"] or ""))}
    folder=Path(".local-data/agent-runtime-validation")
    folder.mkdir(parents=True,exist_ok=True)
    path=folder/(args.phase+"-"+session["id"]+".json")
    path.write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding="utf-8")
    print(json.dumps({key:value for key,value in report.items() if key!="events"},ensure_ascii=False))
    print(str(path))
    if not all(report["checks"].values()):
        raise SystemExit("Direct development QA checks failed; see report")


if __name__ == "__main__":
    main()
