"""Diagnostic retrieval/reranker ablations; no agent or KB configuration writes.

Usage: python remaining_retrieval_probe.py PRIVATE_AUDIT_DIRECTORY
Captured natural questions and original tool queries are replayed unchanged.
Raw results are private evidence for manual inspection, never pass/fail labels.
"""
from __future__ import annotations

import hashlib
import base64
import json
from pathlib import Path
import subprocess
import sys
import time
from urllib.request import Request, urlopen

from remaining_chunk_probe import client


def model_config(model_id: str = "prod-bge-reranker-v2-m3") -> dict:
    if not model_id or not all(ch.isalnum() or ch in "_-" for ch in model_id):
        raise ValueError("Invalid diagnostic model ID")
    result = subprocess.run(["docker", "exec", "-i", "WeKnora-agent-eval-postgres-dev", "psql", "-X", "-q", "-U", "postgres", "-d", "WeKnora", "-At"], input=f"SELECT jsonb_build_object('name',name,'parameters',parameters) FROM models WHERE id='{model_id}';", text=True, encoding="utf-8", capture_output=True, check=True)
    model = json.loads(result.stdout)  # Credentials remain in memory only.
    value = model["parameters"].get("api_key", "")
    if value.startswith("enc:v1:"):
        from cryptography.hazmat.primitives.ciphers.aead import AESGCM
        env_result = subprocess.run(["docker", "inspect", "--format", "{{json .Config.Env}}", "weknora-agent-eval-runtime-api-1"], capture_output=True, text=True, encoding="utf-8", check=True)
        env = dict(x.split("=", 1) for x in json.loads(env_result.stdout))
        encoded = value[len("enc:v1:"):]
        data = base64.urlsafe_b64decode(encoded + "=" * (-len(encoded) % 4))
        model["parameters"]["api_key"] = AESGCM(env["SYSTEM_AES_KEY"].encode()).decrypt(data[:12], data[12:], None).decode()
    return model


def enrich(result: dict) -> str:
    body = result.get("content") or ""
    extra = []

    def add(label: str, value: str) -> None:
        value = (value or "").strip()
        if value and value not in body and not any(value in x for x in extra):
            extra.append(label + value)

    def obj(value, default):
        if isinstance(value, str):
            try: return json.loads(value)
            except ValueError: return default
        return value or default

    add("Matched text: ", result.get("matched_content"))
    for image in obj(result.get("image_info"), []):
        add("Image Caption: ", image.get("caption"))
        add("Image Text: ", image.get("ocr_text"))
    meta = obj(result.get("chunk_metadata"), {})
    for q in meta.get("questions", []) or []:
        add("Related question: ", q.get("question", "") if isinstance(q, dict) else q)
    add("FAQ question: ", meta.get("standard_question"))
    for q in meta.get("similar_questions", []) or []: add("Equivalent question: ", q)
    for a in meta.get("answers", []) or []: add("FAQ answer: ", a)
    return body + ("\n\n" if body and extra else "") + "\n".join(extra)


def rerank(model: dict, query: str, documents: list[str], limit: int) -> dict:
    p = model["parameters"]
    headers = {"Content-Type": "application/json", "Authorization": "Bearer " + p.get("api_key", ""), **(p.get("custom_headers") or {})}
    payload = {"model": model["name"], "query": query, "documents": documents, "truncate_prompt_tokens": limit, "additional_data": None}
    t = time.perf_counter()
    with urlopen(Request(p["base_url"].rstrip("/") + "/rerank", json.dumps(payload, ensure_ascii=False).encode(), headers), timeout=60) as response:
        result = json.load(response)
    result["elapsed_s"] = time.perf_counter() - t
    return result


def main() -> None:
    audit = Path(sys.argv[1]); out = audit / "remaining"; out.mkdir(exist_ok=True)
    c = client(); model = model_config()
    cases = [
        ("ops-delegation", "20e7472a-892b-4dff-9d68-bbf07eb568de"),
        ("aimt-wifi", "3c36fe82-34a6-4a39-bdc2-fdc8616cd9a1"),
        ("guide-dividend-fields", "e1c34eb8-3a91-45a9-866c-7455457efc98"),
        ("company-ticket", "e40aa7c1-0fcd-407f-aa01-1a5088afed28"),
    ]
    previous = out / "retrieval-ablation.json"
    observations = json.loads(previous.read_text(encoding="utf-8")) if previous.exists() else []
    for case, kb in cases:
        row = json.loads((audit / f"probe-fix-v2-{case}.json").read_text(encoding="utf-8"))[0]
        call = next(c for s in row["final_db_observation"]["agent_steps"] for c in s.get("tool_calls", []) if c["name"] == "knowledge_search")
        queries = call["args"]["queries"]
        for budget in [10, 50]:
            if any(x["case"] == case and x["candidate_budget"] == budget for x in observations):
                continue
            candidates = {}; raw_results = []
            for query in queries:
                data = c.request("POST", f"/knowledge-bases/{kb}/hybrid-search", {"query_text": query, "match_count": budget, "vector_threshold": 0.5, "keyword_threshold": 0.3})["data"] or []
                raw_results.append({"query": query, "results": data})
                for result in data:
                    key = result["id"]
                    if key not in candidates or result["score"] > candidates[key]["score"]:
                        candidates[key] = result
            items = sorted(candidates.values(), key=lambda x: (-x["score"], x["id"]))
            passages = [enrich(x) for x in items]
            ranked = rerank(model, " ".join(queries), passages, 511)
            entry = {"case": case, "natural_question": row["query"], "queries": queries, "candidate_budget": budget, "raw_retrievals": raw_results, "candidates": items, "passages": passages, "rerank": ranked}
            observations.append(entry)
            top = sorted(ranked["results"], key=lambda x: -x.get("relevance_score", x.get("score", 0)))[:5]
            print(json.dumps({"case": case, "candidate_budget": budget, "candidate_count": len(items), "top": [{"id": items[x["index"]]["id"], "title": items[x["index"]].get("knowledge_title"), "score": x.get("relevance_score", x.get("score")), "head": items[x["index"]]["content"][:90]} for x in top]}, ensure_ascii=False), flush=True)
            (out / "retrieval-ablation.json").write_text(json.dumps(observations, ensure_ascii=False, indent=2), encoding="utf-8")

    query = "项目 Cedar 的传感器校准周期是什么？"
    relevant = "项目 Cedar 的传感器每隔 17 天进行一次校准，校准结果登记在设备台账中。"
    padding = "食堂菜单包括米饭和蔬菜，运动场开放时间见公告，日常考勤按部门安排。" * 100
    docs = [relevant + padding, padding + relevant, padding]
    result = []
    for limit in [511, 4096]:
        ranked = rerank(model, query, docs, limit)
        result.append({"truncate_prompt_tokens": limit, "documents": docs, "query": query, "response": ranked})
        print(json.dumps({"experiment": "rerank_position", "truncate_prompt_tokens": limit, "scores": [{"index": x["index"], "score": x.get("relevance_score", x.get("score"))} for x in ranked["results"]], "usage": ranked.get("usage")}, ensure_ascii=False), flush=True)
    (out / "rerank-position.json").write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    main()
