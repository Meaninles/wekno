from __future__ import annotations

import json
import os
from collections import Counter
from pathlib import Path
from typing import Any, Literal

from pydantic import Field, model_validator

from .judge import JUDGE_SYSTEM_PROMPT, _post_chat
from .models import StrictModel, TurnContract


class CalibrationError(ValueError):
    pass


class CalibrationCandidate(StrictModel):
    answer: str
    evidence: list[str] = Field(default_factory=list)
    tools: list[str] = Field(default_factory=list)
    completed: bool = True


class JudgeCalibrationItem(StrictModel):
    schema_version: Literal[1] = 1
    calibration_id: str
    capability: str
    query: str
    contract: TurnContract
    candidate: CalibrationCandidate
    expected_label: Literal["pass", "fail", "invalid"]
    rationale: str
    boundary_kind: Literal["positive", "negative", "boundary"]
    critical: bool = False


class JudgeCalibrationSuite(StrictModel):
    schema_version: Literal[1] = 1
    suite_id: str
    minimum_accuracy: float = Field(default=0.85, ge=0, le=1)
    minimum_confidence: float = Field(default=0.85, ge=0, le=1)
    items: list[JudgeCalibrationItem]

    @model_validator(mode="after")
    def validate_coverage(self) -> "JudgeCalibrationSuite":
        if len(self.items) < 6:
            raise ValueError("judge calibration suite requires at least six fixtures")
        ids = [item.calibration_id for item in self.items]
        if len(ids) != len(set(ids)):
            raise ValueError("judge calibration_id must be unique")
        labels = {item.expected_label for item in self.items}
        if labels != {"pass", "fail", "invalid"}:
            raise ValueError("judge calibration must cover pass, fail and invalid")
        kinds = {item.boundary_kind for item in self.items}
        if kinds != {"positive", "negative", "boundary"}:
            raise ValueError("judge calibration must cover positive, negative and boundary examples")
        critical = [item for item in self.items if item.critical]
        if {item.expected_label for item in critical} != {"pass", "fail"}:
            raise ValueError(
                "critical judge calibration must include both passing and failing fixtures"
            )
        for item in critical:
            state = item.contract.conversation_state
            if not any(
                (
                    state.active_facts,
                    state.retired_facts,
                    state.unknown_facts,
                    state.forbidden_unknown_facts,
                    state.forbidden_inferences,
                    state.action_boundaries,
                )
            ):
                raise ValueError(
                    f"critical fixture {item.calibration_id} must exercise the formal conversation_state protocol"
                )
        return self


def load_calibration(path: str | Path) -> JudgeCalibrationSuite:
    with Path(path).open("r", encoding="utf-8") as handle:
        return JudgeCalibrationSuite.model_validate(json.load(handle))


def run_judge_calibration(suite: JudgeCalibrationSuite) -> dict[str, Any]:
    rows: list[dict[str, Any]] = []
    for item in suite.items:
        result = _post_chat(
            [
                {
                    "role": "system",
                    "content": JUDGE_SYSTEM_PROMPT,
                },
                {
                    "role": "user",
                    "content": json.dumps(
                        {
                            "task": "Evaluate whether each answer satisfies the acceptable-answer contract. Do not compare against a single reference wording.",
                            "case_id": item.calibration_id,
                            "contracts": [
                                {
                                    "turn_id": item.calibration_id,
                                    "query": item.query,
                                    "contract": item.contract.model_dump(mode="json"),
                                }
                            ],
                            "candidate": [
                                {
                                    "turn_id": item.calibration_id,
                                    **item.candidate.model_dump(mode="json"),
                                }
                            ],
                        },
                        ensure_ascii=False,
                    ),
                },
            ]
        )
        result_turns = result.get("turns") if isinstance(result, dict) else None
        if not isinstance(result_turns, list) or len(result_turns) != 1:
            raise CalibrationError(
                f"{item.calibration_id}: judge calibration must return exactly one turn"
            )
        row = result_turns[0]
        if not isinstance(row, dict) or row.get("turn_id") != item.calibration_id:
            raise CalibrationError(
                f"{item.calibration_id}: judge calibration turn identity mismatch"
            )
        label = str(row.get("label") or "")
        confidence = float(row.get("confidence", -1))
        if label not in {"pass", "fail", "invalid"} or not 0 <= confidence <= 1:
            raise CalibrationError(f"{item.calibration_id}: invalid judge payload {row!r}")
        rows.append(
            {
                "calibration_id": item.calibration_id,
                "expected_label": item.expected_label,
                "actual_label": label,
                "matched": label == item.expected_label,
                "confidence": confidence,
                "reason": str(row.get("reason") or ""),
            }
        )
    accuracy = sum(row["matched"] for row in rows) / len(rows)
    minimum_observed_confidence = min(row["confidence"] for row in rows)
    critical_mismatches = sorted(
        item.calibration_id
        for item, row in zip(suite.items, rows, strict=True)
        if item.critical and not row["matched"]
    )
    confusion = Counter(
        f"{row['expected_label']}->{row['actual_label']}" for row in rows
    )
    return {
        "schema_version": 1,
        "suite_id": suite.suite_id,
        "judge_model": os.environ.get("AGENT_EVAL_JUDGE_MODEL", "").strip(),
        "accuracy": accuracy,
        "minimum_accuracy": suite.minimum_accuracy,
        "minimum_confidence": suite.minimum_confidence,
        "minimum_observed_confidence": minimum_observed_confidence,
        "passed": accuracy >= suite.minimum_accuracy
        and minimum_observed_confidence >= suite.minimum_confidence
        and not critical_mismatches,
        "critical_mismatches": critical_mismatches,
        "confusion": dict(sorted(confusion.items())),
        "items": rows,
    }
