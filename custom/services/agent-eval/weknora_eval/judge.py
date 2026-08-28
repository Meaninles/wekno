from __future__ import annotations

import json
import os
import urllib.error
import urllib.request
from typing import Any

from .models import (
    CaseRun,
    CaseSpec,
    MetricScore,
    is_measured_sut_execution_error,
)


class JudgeError(RuntimeError):
    pass


JUDGE_SYSTEM_PROMPT = (
    "You are a calibrated evaluator. Return JSON only with key 'turns'. Each item must contain "
    "turn_id, label (pass|fail|invalid), confidence from 0 to 1, reason, and when baseline is present "
    "pairwise (candidate_better|equal|baseline_better). Treat stylistic differences as equal when both "
    "satisfy the contract. The contract is the serialized TurnContract schema. For every TextRule, all_of "
    "means every listed concept is required and any_of means at least one listed semantic alternative is "
    "required; unless_any_of makes the rule inapplicable only when the candidate answer itself states an "
    "exception. Semantic paraphrases are acceptable unless the contract requires exact wording. Every item "
    "in required_claims, conversation_state.active_facts, retired_facts, unknown_facts, action_boundaries, "
    "decision.required_unknowns, and decision.required_defer_claims is independently mandatory. An item "
    "omitted from the candidate answer is a fail even when it appeared in the user query or earlier context. "
    "Retired facts must be identified as retired, unknown facts as unknown, and action boundaries as prohibited "
    "actions. When require_scoped_sections is true, facts must also appear in the correct section. Rules in "
    "forbidden_claims, forbidden_inferences, forbidden_unknown_facts, and forbidden_recommendations fail when "
    "the answer asserts the prohibited meaning. Explicitly audit every conversation-state rule; never pass "
    "merely because the answer avoids a contradiction. Use invalid only when execution is missing, incomplete, "
    "or impossible to judge. "
    "Evidence and hard constraints dominate eloquence."
)


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
            "completed": turn.is_completed,
            "error": turn.error,
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
    measured_turn_ids = {
        turn.turn_id
        for turn in case_run.turns
        if is_measured_sut_execution_error(turn.error)
    }
    semantic_turn_ids = {turn.turn_id for turn in spec.turns} - measured_turn_ids
    result = (
        _post_chat(
            [
                {
                    "role": "system",
                    "content": JUDGE_SYSTEM_PROMPT,
                },
                {"role": "user", "content": json.dumps(prompt, ensure_ascii=False)},
            ]
        )
        if semantic_turn_ids
        else {"turns": []}
    )
    scores: list[MetricScore] = []
    rows = result.get("turns") if isinstance(result, dict) else None
    if not isinstance(rows, list):
        raise JudgeError("judge JSON has no turns array")
    returned_turn_ids: set[str] = set()
    for row in rows:
        if not isinstance(row, dict):
            raise JudgeError("judge turn is not an object")
        turn_id = str(row.get("turn_id") or "")
        label = str(row.get("label") or "")
        confidence = float(row.get("confidence", -1))
        if turn_id in measured_turn_ids:
            continue
        if turn_id not in semantic_turn_ids or label not in {"pass", "fail", "invalid"} or not 0 <= confidence <= 1:
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
    if returned_turn_ids != semantic_turn_ids:
        raise JudgeError(f"judge turn coverage mismatch: expected={semantic_turn_ids}, got={returned_turn_ids}")

    baseline_by_turn = {
        turn.turn_id: turn for turn in baseline.turns
    } if baseline is not None else {}
    for turn_id in sorted(measured_turn_ids):
        paired = baseline_by_turn.get(turn_id)
        pairwise = None
        if paired is not None:
            pairwise = (
                "equal"
                if is_measured_sut_execution_error(paired.error)
                else "baseline_better"
            )
        scores.append(
            MetricScore(
                name="judge.contract_satisfaction",
                value="fail",
                passed=False,
                hard=False,
                comment="SUT response deadline is a deterministic execution failure",
                turn_id=turn_id,
                metadata={"confidence": 1.0, "pairwise": pairwise, "synthetic": True},
            )
        )
    return scores
