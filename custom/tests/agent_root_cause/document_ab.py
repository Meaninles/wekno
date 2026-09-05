"""Generic intervention: supply the previously generated artifact through the
existing original-file transfer path. The user prompt, source bytes, model,
agent configuration and pre-existing conversation text stay fixed in A/B.
No prompt is altered or augmented with a suggested solution.
"""
import base64,json,pathlib,runpy,subprocess,sys,time,uuid,zipfile,difflib
ROOT=pathlib.Path(__file__).resolve().parents[3]
OUT=ROOT/".local-data/agent-audit-20260905"
sys.argv=["live_probe.py","inspect"];P=runpy.run_path(str(OUT/"live_probe.py"));C=P["C"]
CAPTURE=OUT/"root-cause-document";CAPTURE.mkdir(parents=True,exist_ok=True)
ORIGINAL=OUT/"document-turn-1.docx"
SOURCE_SESSION="893983df-24ab-4ef5-90c0-5814100a497e"
QUERY="把刚才文件中的评审日期改为2026年10月16日，其余内容保持不变，给我修改后的Word文件。"

def sql(command):
    result=subprocess.run(["docker","exec","-i","WeKnora-agent-eval-postgres-dev","psql","-X","-q","-U","postgres","-d","WeKnora","-At","-v","ON_ERROR_STOP=1"],input=command,text=True,encoding="utf-8",capture_output=True)
    if result.returncode:raise RuntimeError(result.stderr[:500])
    return result.stdout

def package_compare(original,new):
    with zipfile.ZipFile(original) as a,zipfile.ZipFile(new) as b:
        ad={n:a.read(n) for n in a.namelist()};bd={n:b.read(n) for n in b.namelist()}
    changes=[n for n in sorted(set(ad)|set(bd)) if ad.get(n)!=bd.get(n)]
    expected=ad["word/document.xml"].replace("2026年10月15日".encode(),"2026年10月16日".encode())
    preserved=bd.get("word/document.xml")==expected
    return {"parts_changed":changes,"document_xml_only_requested_date_changed":preserved}

def run(label,carry):
    session=C.create_session();uuid.UUID(session)
    sql(f"INSERT INTO messages (id,session_id,request_id,role,content,is_completed,created_at,updated_at,agent_steps) SELECT gen_random_uuid()::text,'{session}',request_id,role,content,is_completed,created_at,updated_at,agent_steps FROM messages WHERE session_id='{SOURCE_SESSION}' ORDER BY created_at,id LIMIT 2;")
    payload={"query":QUERY,"knowledge_base_ids":[],"knowledge_ids":[],"agent_id":"builtin-document-processing","agent_enabled":True,"web_search_enabled":False,"summary_model_id":P["MODEL"],"disable_title":True,"channel":"web"}
    if carry:payload["attachment_uploads"]=[{"file_name":"雪鹭评审通知.docx","file_size":ORIGINAL.stat().st_size,"data":base64.b64encode(ORIGINAL.read_bytes()).decode()}]
    watches=[];timeline=[];start=time.perf_counter()
    def event(e):
        timeline.append({"t_s":time.perf_counter()-start,"event":e})
        progress=(e.get("data") or {}).get("progress_id","")
        prefix="final-delivery-active-"
        if progress.startswith(prefix) and not watches:
            run_id=progress[len(prefix):];uuid.UUID(run_id)
            log=(CAPTURE/f"{label}-watch.log").open("w",encoding="utf-8")
            process=subprocess.Popen(["docker","exec","weknora-agent-eval-document-processing-agent","python","/tmp/weknora-root-cause-watch.py",run_id],stdout=log,stderr=subprocess.STDOUT)
            watches.append((process,log,run_id))
    print(json.dumps({"start":label,"session":session,"original_file_supplied":carry},ensure_ascii=False),flush=True)
    events,ttfb,total=C.stream(f"/agent-chat/{session}",payload,on_event=event)
    messages=C.request("GET",f"/messages/{session}/load?limit=100").get("data") or []
    latest=max((m for m in messages if m["role"]=="assistant"),key=lambda m:m.get("created_at",""))
    artifacts=[]
    for s in latest.get("agent_steps") or []:
        for c in s.get("tool_calls") or []:
            artifacts.extend(((c.get("result") or {}).get("data") or {}).get("artifacts") or [])
    from urllib import request as urlrequest
    artifact_results=[]
    for artifact in artifacts:
        path=CAPTURE/f"{label}-{artifact['artifact_id']}.docx"
        req=urlrequest.Request(C.base_url+f"/custom/general-agent/artifacts/{artifact['artifact_id']}/download",headers=C._headers())
        with urlrequest.urlopen(req,timeout=30) as response:path.write_bytes(response.read())
        artifact_results.append({"artifact":artifact,"local_file":str(path),"comparison":package_compare(ORIGINAL,path)})
    for process,log,run_id in watches:
        try:process.wait(timeout=8)
        except subprocess.TimeoutExpired:pass
        log.close()
        subprocess.run(["docker","cp",f"weknora-agent-eval-document-processing-agent:/tmp/weknora-root-cause-capture/{run_id}",str(CAPTURE/label)],check=True,capture_output=True)
    summary={"label":label,"session_id":session,"original_file_supplied":carry,"total_s":total/1000,"answer":P["streamed_production_candidate"](events),"tool_count":latest.get("agent_tool_count"),"artifacts":artifact_results}
    (CAPTURE/f"{label}.json").write_text(json.dumps({"summary":summary,"timeline":timeline,"messages":messages},ensure_ascii=False,indent=2),encoding="utf-8")
    print(json.dumps(summary,ensure_ascii=False),flush=True)
    return summary

if __name__=="__main__":
    results=[run("A-history-only",False),run("B-carry-original",True)]
    (CAPTURE/"summary.json").write_text(json.dumps(results,ensure_ascii=False,indent=2),encoding="utf-8")
