"""Frozen final-stage history ablation, preserving current evidence verbatim.

This is not an end-to-end benchmark. Only previous-turn assistant/tool messages
are removed in the two controls. Current evidence and model parameters are
identical. The 'final' capture is from the native finalizer with no callable
tools. The exploratory 'training' capture is from a tool-capable iteration:
its missing tool schemas limit fidelity, so its short/empty outputs must not
be used to claim a speed or accuracy improvement. Responses require manual
review, including the need for further retrieval.
"""
from __future__ import annotations

import hashlib
import json
from pathlib import Path
import sys
import time
from urllib.request import Request, urlopen

from remaining_retrieval_probe import model_config


def main() -> None:
    out = Path(sys.argv[1])
    model = model_config("prod-deepseek-v4-flash-int8-chat")
    params = model["parameters"]
    results = []
    for case in ["final", "training"]:
        frozen = json.loads((out / f"frozen-native-{case}.json").read_text(encoding="utf-8"))
        messages = frozen["messages"]
        boundary = max(i for i, m in enumerate(messages) if m["role"] == "user" and str(m.get("content") or "").startswith('<runtime_context scope="this_turn">'))
        evidence_hash = hashlib.sha256(json.dumps(messages[boundary:], ensure_ascii=False, sort_keys=True).encode()).hexdigest()
        for mode in ["current", "without_old_tool_cycles", "user_history_only"]:
            selected = []
            for i, message in enumerate(messages):
                remove = i < boundary and (
                    (mode == "without_old_tool_cycles" and (message["role"] == "tool" or message.get("tool_calls")))
                    or (mode == "user_history_only" and message["role"] in ["assistant", "tool"])
                )
                if not remove: selected.append(message)
            body = {"model": model["name"], "messages": selected, **frozen["model_parameters"], "stream": False, "chat_template_kwargs": {"enable_thinking": False}}
            headers = {"Content-Type": "application/json", "Authorization": "Bearer " + params.get("api_key", ""), **(params.get("custom_headers") or {})}
            started = time.perf_counter()
            with urlopen(Request(params["base_url"].rstrip("/") + "/chat/completions", json.dumps(body, ensure_ascii=False).encode(), headers), timeout=120) as response:
                answer = json.load(response)
            entry = {"case": case, "mode": mode, "capture_stage": "native_finalizer" if case == "final" else "tool_capable_iteration_without_original_tool_schemas", "source_observation": frozen["observation_id"], "current_evidence_sha256": evidence_hash, "elapsed_s": time.perf_counter() - started, "message_count": len(selected), "input_chars": sum(len(m.get("content") or "") for m in selected), "request": body, "response": answer}
            results.append(entry)
            (out / "context-ablation.json").write_text(json.dumps(results, ensure_ascii=False, indent=2), encoding="utf-8")
            print(json.dumps({k: v for k, v in entry.items() if k not in ["request", "response"]}, ensure_ascii=False), flush=True)


if __name__ == "__main__":
    main()
