"""Copy the reported history into a test conversation, then reuse its uncited
original evidence. Source session and documents are never changed."""
import argparse
import json
import uuid
from dev_client import DevClient, ROOT
from dev_probe import sql

ORIGINAL="623f383b-13cc-4d70-9aed-6cc5ba400681"

def main():
    parser=argparse.ArgumentParser();parser.add_argument("--pass",dest="name",required=True);args=parser.parse_args()
    client=DevClient();session=client.session("未引用原文复用 "+args.name)
    rows=sql(f"SELECT row_to_json(t) FROM (SELECT id,request_id,role FROM messages WHERE session_id='{ORIGINAL}' ORDER BY created_at LIMIT 8)t;")
    request_ids={r["request_id"]:uuid.uuid4().hex[:12] for r in rows}
    for row in rows:
        identity=str(uuid.uuid4())
        changes=json.dumps({"id":identity,"session_id":session,"request_id":request_ids[row["request_id"]]})
        sql(f"WITH inserted AS (INSERT INTO messages SELECT (jsonb_populate_record(NULL::messages,to_jsonb(m)||'{changes}'::jsonb)).* FROM messages m WHERE id='{row['id']}' RETURNING id) SELECT json_build_object('count',count(*)) FROM inserted;")
        if row["role"]=="assistant":
            run=str(uuid.uuid4())
            sql(f"""WITH inserted AS (INSERT INTO custom_agent_runs(id,tenant_id,user_id,session_id,message_id,status,deadline,payload,scope,"references",created_at,updated_at)
            SELECT '{run}',r.tenant_id,s.user_id,'{session}','{identity}','completed',now(),'{{}}','{{}}',r."references",r.created_at,r.updated_at
            FROM custom_agent_runs r JOIN sessions s ON s.id='{session}' WHERE r.message_id='{row['id']}' RETURNING id)
            SELECT json_build_object('count',count(*)) FROM inserted;""")
    report=client.qa("citation-uncited-"+args.name,"他们在高质量发展上有哪些职责",session=session,model="prod-qwen36-27b-chat")
    run=report["run"];result=run.get("result") or {};answer=result.get("answer","")
    tools=sql("SELECT row_to_json(t) FROM (SELECT name,arguments,jsonb_array_length(response->'source_references') AS sources FROM custom_agent_tool_calls WHERE run_id='"+run["id"]+"' ORDER BY created_at)t;")
    checks={**report["checks"],"cited_original":any(r.get("knowledge_id")=="bc5f3d49-874e-4ca0-8634-3533ffd09ac4" for r in result.get("references") or []),
        "correct_title":"高质量发展绩效考核管理规定" in answer,"history_restored":any(t["name"]=="read_conversation" and (t["sources"] or 0)>0 for t in tools)}
    report["checks"]=checks;report["tools"]=tools
    (ROOT/f"citation-uncited-{args.name}-summary.json").write_text(json.dumps(report,ensure_ascii=False,indent=2),encoding="utf-8")
    print(json.dumps({"name":args.name,"session":session,"checks":checks,"tools":tools,"models":run["model_requests"]},ensure_ascii=False),flush=True)
    if not all(checks.values()):raise SystemExit(1)

if __name__=="__main__":main()
