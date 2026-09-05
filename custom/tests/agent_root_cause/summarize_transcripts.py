"""Read audit-run SDK transcripts; export only diagnostic events, not secrets."""
import collections,json,pathlib
ROOT=pathlib.Path(__file__).resolve().parents[3]
OUT=ROOT/".local-data/agent-audit-20260905/root-cause-document"
summary=[]
for label in ("A-history-only","B-carry-original"):
    entries=[];events=[];usages=[];initial=None;seen_message_ids=set()
    for path in (OUT/label).rglob("*.jsonl"):
        for line in path.read_text(encoding="utf-8").splitlines():
            try:row=json.loads(line)
            except json.JSONDecodeError:continue
            message=row.get("message") or {}
            if row.get("type")=="assistant" and message.get("id") not in seen_message_ids:
                seen_message_ids.add(message.get("id"))
                usages.append({"timestamp":row.get("timestamp"),"usage":message.get("usage"),"model":message.get("model")})
            content=message.get("content")
            if initial is None and row.get("type")=="user":initial=content
            if isinstance(content,list):
                for block in content:
                    if block.get("type")=="tool_use":events.append({"timestamp":row.get("timestamp"),"type":"tool_use","name":block.get("name"),"input":block.get("input")})
                    if block.get("type")=="tool_result":events.append({"timestamp":row.get("timestamp"),"type":"tool_result","is_error":block.get("is_error"),"content":block.get("content")})
    record={"label":label,"events":events,"model_calls":usages,"initial_prompt_chars":len(str(initial)),"has_original_input_block":"<original_input_files" in str(initial)}
    (OUT/(label+"-transcript-summary.json")).write_text(json.dumps(record,ensure_ascii=False,indent=2),encoding="utf-8")
    summary.append({"label":label,"tool_names":[x["name"] for x in events if x["type"]=="tool_use"],"tools":[{"name":x["name"],"input_preview":str(x["input"])[:1200]} for x in events if x["type"]=="tool_use"],"model_calls":usages,"initial_prompt_chars":record["initial_prompt_chars"],"has_original_input_block":record["has_original_input_block"]})
print(json.dumps(summary,ensure_ascii=False,indent=2))
