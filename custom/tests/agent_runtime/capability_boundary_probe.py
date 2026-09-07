"""Check capability guidance through the existing development API, without a router model."""
import argparse
import json
from dev_client import DevClient, ROOT
from dev_probe import sql


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--pass", dest="pass_name", required=True)
    args = parser.parse_args()
    client = DevClient()
    session = client.session("知识问答能力边界 " + args.pass_name)
    cases = [
        ("policy", "高质量发展绩效考核的年度考核指标补充调整，应当在什么时间前提交？", None),
        ("files", "帮我生成一个ppt和word介绍下主数据管理流程和机制，两边保持一致。", "文档处理"),
        ("table", "帮我对Excel表格逐行计算各部门费用合计，并生成统计图表。", "表格分析"),
        ("database", "请连接业务数据库，执行SQL统计各部门今年的订单总额。", "数据分析"),
        ("browser", "请打开网页 https://example.com 并截屏给我。", "通用智能体"),
        ("rewrite", "只改写这句话，不生成文件：请于周五前提交Word文档。改写为礼貌的一句话。", ""),
    ]
    reports = []
    for name, query, destination in cases:
        report = client.qa("qa-scope-" + args.pass_name + "-" + name, query,
            session=session, model="prod-qwen36-27b-chat", kb=["7fab0759-3ca5-42e7-b406-547ad6c82a83"])
        run = report["run"]
        answer = run["result"]["answer"]
        calls = sql("SELECT row_to_json(t) FROM (SELECT name FROM custom_agent_tool_calls WHERE run_id='" + run["id"] + "')t;")
        if destination:
            report["checks"].update(one_model_call=run["model_requests"] == 1,
                no_tools=not calls, correct_guidance=destination in answer, concise=len(answer) <= 160)
        elif destination == "":
            report["checks"].update(one_model_call=run["model_requests"] == 1, no_tools=not calls,
                preserves_text="周五" in answer and "Word" in answer, no_false_refusal="切换" not in answer)
        else:
            report["checks"].update(has_citations=bool(run["result"].get("references")),
                correct_deadline="三月底" in answer or "3月底" in answer or "3月31" in answer)
        reports.append({"case":name,"run_id":run["id"],"answer":answer,"seconds":report["seconds"],
                        "model_requests":run["model_requests"],"tool_calls":calls,"checks":report["checks"]})
    summary = {"session":session,"pass":args.pass_name,"cases":reports,
               "passed":all(all(r["checks"].values()) for r in reports)}
    (ROOT / ("qa-scope-" + args.pass_name + "-summary.json")).write_text(json.dumps(summary,ensure_ascii=False,indent=2),encoding="utf-8")
    print(json.dumps(summary,ensure_ascii=False),flush=True)
    if not summary["passed"]: raise SystemExit("Capability boundary regression failed")


if __name__ == "__main__": main()
