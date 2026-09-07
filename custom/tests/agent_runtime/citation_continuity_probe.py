"""Replay the reported conversation and long follow-ups against development.

Run twice with separate --pass names. Only creates test conversations; never
changes the reported conversation, source documents, or agent configuration.
"""
import argparse
import json
import re
from dev_client import DevClient, ROOT
from dev_probe import sql

ORIGINAL="623f383b-13cc-4d70-9aed-6cc5ba400681"

def main():
    parser=argparse.ArgumentParser()
    parser.add_argument("--pass",dest="name",required=True)
    args=parser.parse_args()
    questions=sql(f"SELECT row_to_json(t) FROM (SELECT content FROM messages WHERE session_id='{ORIGINAL}' AND role='user' ORDER BY created_at LIMIT 5)t;")
    assert len(questions)==5
    turns=[(row["content"],True) for row in questions]
    turns += [
      ("根据刚才的高质量发展绩效考核制度，考核结果有异议时怎么申请复核？申请和答复各是多少个工作日？",True),
      ("这里的五天是自然日还是工作日？请简洁回答。",True),
      ("回到主数据管理，项目管理系统上线前后，项目主数据分别由谁负责？",True),
      ("主数据管理单位与主数据生产单位在数据质量方面有什么区别？",True),
      ("回到高质量发展绩效考核，年度考核指标补充调整应在什么时间前提交？",True),
      ("把上一条回答压缩成一句话，不增加事实。",False),
      ("你好，谢谢。",False),
      ("继续高质量发展绩效考核话题，企业发展部和组织与人力资源部在年终考核中的分工是什么？",True),
      ("回到最初的主数据话题，数科事业部是归口部门还是所有主数据的生产单位？",True),
      ("回到之前的复核问题，请再次给出申请复核和收到申请后答复的期限及制度依据。",True),
      ("依据我们谈到的高质量发展绩效考核制度，能否认定企业发展部负责干部任免？不要把推测写成制度规定。",False),
    ]
    client=DevClient(); session=client.session("引用连续性回归 "+args.name)
    results=[]
    for index,(query,needs_refs) in enumerate(turns,1):
        report=client.qa(f"citation-{args.name}-{index:02}",query,session=session,model="prod-qwen36-27b-chat")
        run=report["run"]; answer=(run.get("result") or {}).get("answer",""); refs=(run.get("result") or {}).get("references") or []
        detail=sql("SELECT row_to_json(t) FROM (SELECT jsonb_array_length(\"references\") AS acquired, checkpoint->'agent'->'middle_context' AS state FROM custom_agent_runs WHERE id='"+run["id"]+"')t;")[0]
        calls=sql("SELECT row_to_json(t) FROM (SELECT name,arguments,jsonb_array_length(response->'source_references') AS restored_count FROM custom_agent_tool_calls WHERE run_id='"+run["id"]+"' ORDER BY created_at)t;")
        handles=set(re.findall(r'<src id="(S\d+)" />',answer))
        known={r.get("metadata",{}).get("citation_id") for r in refs}
        checks={**report["checks"],"required_citations":bool(refs) if needs_refs else True,"all_handles_resolve":handles <= known,
                "no_corrective_generation":not (detail.get("state") or {}).get("delivery_violations"),"within_budget":run["model_requests"]<=15,
                "no_internal_metadata":not any(marker in answer for marker in ("conversation_record", "assistant_message_", "evidence_catalog", "INTERNAL_CONVERSATION_NAVIGATION")),
                "no_encoding_damage":"\ufffd" not in answer}
        results.append({"turn":index,"query":query,"answer":answer,"seconds":report["seconds"],"models":run["model_requests"],"citations":len(refs),"tools":calls,"checks":checks})
        print(json.dumps({"pass":args.name,"turn":index,"checks":checks},ensure_ascii=False),flush=True)
    summary={"pass":args.name,"session":session,"original":ORIGINAL,"results":results,"passed":all(all(r["checks"].values()) for r in results)}
    target=ROOT/f"citation-{args.name}-summary.json"
    target.write_text(json.dumps(summary,ensure_ascii=False,indent=2),encoding="utf-8")
    print(json.dumps({"pass":args.name,"session":session,"passed":summary["passed"],"report":str(target)},ensure_ascii=False),flush=True)
    if not summary["passed"]: raise SystemExit(1)

if __name__ == "__main__": main()
