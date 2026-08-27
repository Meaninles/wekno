from __future__ import annotations

import json
from collections import Counter
from pathlib import Path
from typing import Any, Literal

from pydantic import Field, model_validator

from .judge import _post_chat
from .models import StrictModel


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
    contract: dict[str, Any]
    candidate: CalibrationCandidate
    expected_label: Literal["pass", "fail", "invalid"]
    rationale: str
    boundary_kind: Literal["positive", "negative", "boundary"]


class JudgeCalibrationSuite(StrictModel):
    schema_version: Literal[1] = 1
    suite_id: str
    minimum_accuracy: float = Field(default=0.85, ge=0, le=1)
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
                    "content": (
                        "You are a calibrated evaluator. Return JSON only with keys label, confidence and reason. "
                        "label must be pass, fail or invalid. Hard evidence and state constraints dominate style. "
                        "Use invalid only when execution is missing, incomplete, or impossible to judge."
                    ),
                },
                {
                    "role": "user",
                    "content": json.dumps(
                        {
                            "contract": item.contract,
                            "candidate": item.candidate.model_dump(mode="json"),
                        },
                        ensure_ascii=False,
                    ),
                },
            ]
        )
        label = str(result.get("label") or "")
        confidence = float(result.get("confidence", -1))
        if label not in {"pass", "fail", "invalid"} or not 0 <= confidence <= 1:
            raise CalibrationError(f"{item.calibration_id}: invalid judge payload {result!r}")
        rows.append(
            {
                "calibration_id": item.calibration_id,
                "expected_label": item.expected_label,
                "actual_label": label,
                "matched": label == item.expected_label,
                "confidence": confidence,
                "reason": str(result.get("reason") or ""),
            }
        )
    accuracy = sum(row["matched"] for row in rows) / len(rows)
    confusion = Counter(
        f"{row['expected_label']}->{row['actual_label']}" for row in rows
    )
    return {
        "schema_version": 1,
        "suite_id": suite.suite_id,
        "accuracy": accuracy,
        "minimum_accuracy": suite.minimum_accuracy,
        "passed": accuracy >= suite.minimum_accuracy,
        "confusion": dict(sorted(confusion.items())),
        "items": rows,
    }
