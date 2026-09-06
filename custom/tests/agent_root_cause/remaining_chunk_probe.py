"""Read-only chunking experiments against the local preview endpoint.

Supply the private directory containing safety-parsed.json and safety-chunks.json.
These observations do not score answers, rebuild indexes or modify KB settings.
"""
from __future__ import annotations

import json
from pathlib import Path
import sys

REPO = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(REPO / "custom/services/agent-eval"))
from weknora_eval.client import WeKnoraClient


def client() -> WeKnoraClient:
    env = dict(line.split("=", 1) for line in (REPO / "custom/services/agent-eval/runner.env").read_text(encoding="utf-8-sig").splitlines() if "=" in line and not line.lstrip().startswith("#"))
    return WeKnoraClient("http://localhost:8080/api/v1", env["WEKNORA_E2E_TENANT_API_KEY"].strip().strip('"'), timeout=60)


def main() -> None:
    out = Path(sys.argv[1])
    c = client()
    separator = ["\n\n", "\n", "。", "！", "？", ";", "；"]

    def preview(text: str, size: int, overlap: int, tokens: int = 0, strategy: str = "") -> dict:
        result = c.request("POST", "/chunker/preview", {"text": text, "chunking_config": {"chunk_size": size, "chunk_overlap": overlap, "separators": separator, "strategy": strategy, "token_limit": tokens}})
        return result.get("data", result)

    parsed = json.loads((out / "safety-parsed.json").read_text(encoding="utf-8"))
    stored = json.loads((out / "safety-chunks.json").read_text(encoding="utf-8"))
    parents = preview(parsed["content"], 4096, 80)
    experiments = []
    for tokens in [0, 8192]:
        children = [child for parent in parents["chunks"] for child in preview(parent["content"], 384, 76, tokens)["chunks"]]
        same = [s["id"] for s in stored if s["chunk_type"] == "text" and any(ch["content"].strip() == s["content"].strip() for ch in children)]
        entry = {"case": "original_document", "token_limit": tokens, "parent_count": len(parents["chunks"]), "child_count": len(children), "exact_stored_text_matches": len(same), "children": children}
        experiments.append(entry)
        print(json.dumps({k: v for k, v in entry.items() if k != "children"}, ensure_ascii=False))

    # Vary the length of an ordinary cell; the row key and value remain at
    # opposite ends. Neither the subjects nor the values are business answers.
    for length in [200, 500, 900]:
        text = "| Object | Description | Value |\n| --- | --- | --- |\n| GROUP_A | " + "普通说明。" * (length // 5) + " | VALUE_Z |\n"
        for tokens in [0, 8192]:
            result = preview(text, 384, 76, tokens)
            carriers = [ch for ch in result["chunks"] if "VALUE_Z" in ch["content"]]
            entry = {"case": "generic_table_row", "cell_chars": length, "token_limit": tokens, "chunks": result["chunks"], "value_keeps_row_key": all("GROUP_A" in ch["content"] for ch in carriers), "selected_tier": result["selected_tier"]}
            experiments.append(entry)
            print(json.dumps({k: v for k, v in entry.items() if k != "chunks"}, ensure_ascii=False))
    (out / "chunk-experiments.json").write_text(json.dumps(experiments, ensure_ascii=False, indent=2), encoding="utf-8")


if __name__ == "__main__":
    main()
