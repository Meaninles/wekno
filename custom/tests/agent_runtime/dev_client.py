"""Focused probes against the user's existing development environment."""
import base64
import hashlib
import json
import re
from pathlib import Path
import subprocess
import time
import uuid

import httpx
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from dev_probe import sql

ROOT=Path(".local-data/agent-runtime-validation")
KB="e40aa7c1-0fcd-407f-aa01-1a5088afed28"
MODEL="prod-deepseek-v4-flash-int8-chat"


class DevClient:
    def __init__(self):
        key=sql("SELECT row_to_json(t) FROM (SELECT api_key FROM tenants WHERE id=10000)t;")[0]["api_key"]
        if key.startswith("enc:v1:"):
            info=json.loads(subprocess.check_output(["docker","inspect","weknora-agent-eval-runtime-api-1"],text=True))
            env=dict(value.split("=",1) for value in info[0]["Config"]["Env"])
            data=base64.urlsafe_b64decode(key[7:]+"="*(-len(key[7:])%4))
            key=AESGCM(env["SYSTEM_AES_KEY"].encode()).decrypt(data[:12],data[12:],None).decode()
        self.client=httpx.Client(base_url="http://localhost:8080/api/v1",headers={"X-API-Key":key},timeout=7200)

    def session(self,title):
        r=self.client.post("/sessions",json={"title":"统一内核场景检查 · "+title});r.raise_for_status()
        return r.json()["data"]["id"]

    def upload(self,session,path,agent="builtin-general-agent",wait=True):
        path=Path(path);started=time.monotonic()
        with path.open("rb") as f:
            r=self.client.post(f"/custom/chat-uploads/sessions/{session}",data={"agent_id":agent,"upload_id":str(uuid.uuid4())},files={"file":(path.name,f)})
        if not r.is_success:
            raise RuntimeError(f"Upload {path.name}: HTTP {r.status_code}: {r.text[:500]}")
        item=r.json()["data"]
        print(json.dumps({"uploaded":path.name,"bytes":path.stat().st_size,"session":session,"knowledge_id":item["id"],"seconds":round(time.monotonic()-started,2)},ensure_ascii=False),flush=True)
        if wait:
            item=self.wait_upload(session,item["id"])
        return item

    def wait_upload(self,session,identity):
        started=time.monotonic()
        while time.monotonic()-started<1800:
            r=self.client.get(f"/custom/chat-uploads/sessions/{session}/{identity}");r.raise_for_status();item=r.json()["data"]
            if item.get("ready"):return item
            if item.get("parse_status") in ("failed","cancelled"):
                raise RuntimeError(json.dumps(item,ensure_ascii=False))
            time.sleep(3)
        raise TimeoutError("Upload was not ready within 30 minutes")

    def qa(self,name,query,*,agent="builtin-knowledge-qa",session=None,kb=None,uploads=None,model=MODEL,expected=(),extra=None):
        session=session or self.session(name)
        payload={"query":query,"agent_id":agent,"summary_model_id":model,"knowledge_base_ids":kb or [],"enable_memory":False,"web_search_enabled":False,"upload_ids":uploads or []}
        payload.update(extra or {})
        started=time.monotonic();first=None;events=[]
        with self.client.stream("POST",f"/knowledge-chat/{session}",json=payload) as response:
            if not response.is_success:
                response.read();raise RuntimeError(f"QA HTTP {response.status_code}: {response.text[:500]}")
            for line in response.iter_lines():
                if not line.startswith("data:"):continue
                raw=line[5:].strip()
                if raw=="[DONE]":break
                item=json.loads(raw);events.append(item)
                if first is None and item.get("response_type") in ("answer","final_answer") and item.get("content"):
                    first=time.monotonic()-started
        rows=sql("SELECT row_to_json(t) FROM (SELECT id,status,model_requests,input_tokens,output_tokens,unknown_usage_requests,reserved_tokens,error,result FROM custom_agent_runs WHERE session_id='"+session+"' ORDER BY created_at DESC LIMIT 1)t;")
        row=rows[0] if rows else {};answer=(row.get("result") or {}).get("answer","")
        completed=[e for e in events if e.get("response_type")=="complete"]
        refs=(row.get("result") or {}).get("references") or []
        normalized=re.sub(r'(?<=\d),(?=\d{3}(?:\D|$))','',answer)
        checks={"completed":row.get("status")=="completed","one_completion":len(completed)==1,"no_stream_error":not any(e.get("response_type")=="error" and e.get("done") for e in events),"canonical_answer":bool(completed and completed[-1].get("data",{}).get("final_answer")==answer),"expected_facts":all(str(x) in normalized for x in expected),"released_budget":row.get("reserved_tokens")==0}
        report={"name":name,"session":session,"query":query,"agent":agent,"seconds":round(time.monotonic()-started,3),"first_text_seconds":round(first,3) if first else None,"run":row,"checks":checks,"events":events}
        ROOT.mkdir(parents=True,exist_ok=True);(ROOT/(name+".json")).write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding="utf-8")
        print(json.dumps({"name":name,"session":session,"seconds":report["seconds"],"model_requests":row.get("model_requests"),"references":len(refs),"checks":checks,"answer":answer[:180]},ensure_ascii=False),flush=True)
        return report

    def artifacts(self,report):
        results=[]
        for item in report["run"]["result"].get("artifacts") or []:
            path=item["download_url"].removeprefix("/api/v1")
            r=self.client.get(path);r.raise_for_status()
            assert len(r.content)==item["file_size"] and hashlib.sha256(r.content).hexdigest()==item["sha256"]
            target=ROOT/"downloads"/item["filename"];target.parent.mkdir(parents=True,exist_ok=True);target.write_bytes(r.content)
            with httpx.Client(base_url="http://localhost:8080",timeout=30) as anonymous:
                denied=anonymous.get(item["download_url"])
                assert denied.status_code in (401,403,404),denied.status_code
            results.append({"file":str(target),"hash_verified":True,"anonymous_denied":True})
        return results

