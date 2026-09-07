"""Exercise failed-history rewriting and general-agent continuity in development."""
import argparse
import json
import uuid

from dev_client import DevClient, ROOT
from dev_probe import sql


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--pass", dest="name", required=True)
    args = parser.parse_args()
    client = DevClient()
    session = client.session("失败后追问 " + args.name)
    # Copy the exact failed turn from the screenshot, preserving error_code.
    rows = sql("SELECT json_build_object('id',id) FROM messages WHERE session_id='554f3210-2a1b-4531-a070-b31217d86792' AND request_id='60f7b393-7606-41fc-ab06-aabc644fe32b' ORDER BY created_at;")
    assert len(rows) == 2
    request_id = str(uuid.uuid4())
    for row in rows:
        changes = json.dumps({"id":str(uuid.uuid4()), "session_id":session, "request_id":request_id})
        sql(f"WITH inserted AS (INSERT INTO messages SELECT (jsonb_populate_record(NULL::messages,to_jsonb(m)||'{changes}'::jsonb)).* FROM messages m WHERE id='{row['id']}' RETURNING id) SELECT json_build_object('count',count(*)) FROM inserted;")
    reports = [client.qa("boundary-failure-"+args.name, "把上一条回答压缩成一句话，不增加事实。", session=session, model="prod-qwen36-27b-chat")]
    general = client.session("通用智能体引用 " + args.name)
    for index, query in enumerate((
        "根据公司制度中的《高质量发展绩效考核管理规定》，对考核结果有异议时，申请复核和答复的期限各是多少？简洁说明依据。",
        "是工作日还是自然日？只需一句话。",
    ), 1):
        reports.append(client.qa(f"boundary-general-{args.name}-{index}", query, session=general,
            agent="builtin-general-agent", kb=["7fab0759-3ca5-42e7-b406-547ad6c82a83"], model="prod-qwen36-27b-chat"))
    for index, report in enumerate(reports):
        answer = (report["run"].get("result") or {}).get("answer", "")
        report["checks"]["no_internal_metadata"] = not any(x in answer for x in ("conversation_record", "assistant_message_", "evidence_catalog"))
        report["checks"]["no_failure_banner_as_answer"] = "这次未能完成，请稍后重试。" not in answer
        if index == 0:
            report["checks"]["failed_answer_is_not_researched_anew"] = (
                not (report["run"].get("result") or {}).get("references")
                and any(word in answer for word in ("上", "之前"))
                and any(word in answer for word in ("未", "没有", "失败", "无法", "无可")))
        if index:
            report["checks"]["cited"] = bool((report["run"].get("result") or {}).get("references"))
            report["checks"]["working_days"] = "工作日" in answer
    result = {"pass":args.name, "reports":reports, "passed":all(all(r["checks"].values()) for r in reports)}
    (ROOT/f"citation-boundary-{args.name}-summary.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    print(json.dumps({"pass":args.name, "passed":result["passed"]},ensure_ascii=False), flush=True)
    if not result["passed"]:
        raise SystemExit(1)


if __name__ == "__main__":
    main()
