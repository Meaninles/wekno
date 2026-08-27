from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from typing import Any

from .models import CaseRun, CaseSpec, MetricScore


class JudgeError(RuntimeError):
    pass


def _post_chat(messages: list[dict[str, str]]) -> dict[str, Any]:
    base_url = os.environ.get("AGENT_EVAL_JUDGE_BASE_URL", "").rstrip("/")
    api_key = os.environ.get("AGENT_EVAL_JUDGE_API_KEY", "").strip()
    model = os.environ.get("AGENT_EVAL_JUDGE_MODEL", "").strip()
    if not base_url or not api_key or not model:
        raise JudgeError(
            "AGENT_EVAL_JUDGE_BASE_URL, AGENT_EVAL_JUDGE_API_KEY and AGENT_EVAL_JUDGE_MODEL are required"
        )
    payload = {
        "model": model,
        "temperature": 0,
        "messages": messages,
        "response_format": {"type": "json_object"},
    }
    request = urllib.request.Request(
        f"{base_url}/chat/completions",
        data=json.dumps(payload, ensure_ascii=False).encode("utf-8"),
        method="POST",
        headers={"Authorization": f"Bearer {api_key}", "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(request, timeout=300) as response:
            data = json.load(response)
    except urllib.error.HTTPError as exc:
        detail = exc.read()[:2000].decode("utf-8", errors="replace")
        raise JudgeError(f"judge request failed: HTTP {exc.code}: {detail}") from exc
    try:
        content = data["choices"][0]["message"]["content"]
        return json.loads(content)
    except (KeyError, IndexError, TypeError, json.JSONDecodeError) as exc:
        raise JudgeError("judge did not return the required JSON object") from exc


def judge_case(spec: CaseSpec, case_run: CaseRun, baseline: CaseRun | None = None) -> list[MetricScore]:
    contract_payload = [
        {
            "turn_id": turn.turn_id,
            "query": turn.query,
            "contract": turn.contract.model_dump(mode="json"),
        }
        for turn in spec.turns
    ]
    observed = [
        {
            "turn_id": turn.turn_id,
            "answer": turn.content,
            "evidence": [
                str(reference.get("evidence_content") or reference.get("content") or "")
                for reference in turn.references
            ],
            "tools": turn.tools,
        }
        for turn in case_run.turns
    ]
    prompt: dict[str, Any] = {
        "task": "Evaluate whether each answer satisfies the acceptable-answer contract. Do not compare against a single reference wording.",
        "case_id": spec.case_id,
        "contracts": contract_payload,
        "candidate": observed,
    }
    if baseline is not None:
        prompt["task"] += " Also make a pairwise non-regression comparison against the baseline."
        prompt["baseline"] = [
            {"turn_id": turn.turn_id, "answer": turn.content, "evidence": turn.references}
            for turn in baseline.turns
        ]
    result = _post_chat(
        [
            {
                "role": "system",
                "content": (
                    "You are a calibrated evaluator. Return JSON only with key 'turns'. Each item must contain "
                    "turn_id, label (pass|fail|invalid), confidence from 0 to 1, reason, and when baseline is present "
                    "pairwise (candidate_better|equal|baseline_better). Treat stylistic differences as equal when both "
                    "satisfy the contract. Evidence and hard constraints dominate eloquence."
                ),
            },
            {"role": "user", "content": json.dumps(prompt, ensure_ascii=False)},
        ]
    )
    scores: list[MetricScore] = []
    rows = result.get("turns") if isinstance(result, dict) else None
    if not isinstance(rows, list):
        raise JudgeError("judge JSON has no turns array")
    expected_turn_ids = {turn.turn_id for turn in spec.turns}
    returned_turn_ids: set[str] = set()
    for row in rows:
        if not isinstance(row, dict):
            raise JudgeError("judge turn is not an object")
        turn_id = str(row.get("turn_id") or "")
        label = str(row.get("label") or "")
        confidence = float(row.get("confidence", -1))
        if turn_id not in expected_turn_ids or label not in {"pass", "fail", "invalid"} or not 0 <= confidence <= 1:
            raise JudgeError(f"invalid judge result row: {row!r}")
        returned_turn_ids.add(turn_id)
        scores.append(
            MetricScore(
                name="judge.contract_satisfaction",
                value=label,
                passed=label == "pass",
                hard=False,
                comment=str(row.get("reason") or ""),
                turn_id=turn_id,
                metadata={"confidence": confidence, "pairwise": row.get("pairwise")},
            )
        )
    if returned_turn_ids != expected_turn_ids:
        raise JudgeError(f"judge turn coverage mismatch: expected={expected_turn_ids}, got={returned_turn_ids}")
    return scores
