"""Materialize Codex's manual conversation decisions into the review schema.

This module does not score an answer or infer a verdict.  It only expands an
explicit, human-authored decision matrix into the verbose dual-track artifact
accepted by ``codex-review-apply`` while binding each decision to the packet's
complete-conversation hash.
"""

from __future__ import annotations

import argparse
import json
from pathlib import Path
from typing import Any


DIMENSIONS = (
    "task_fulfillment",
    "factual_grounding",
    "conversation_state",
    "source_attribution",
    "action_boundary",
    "communication_quality",
)


def _read(path: Path) -> dict[str, Any]:
    return json.loads(path.read_text(encoding="utf-8"))


def _write(path: Path, payload: dict[str, Any]) -> None:
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
    )


def _key(item: dict[str, Any]) -> tuple[str, int]:
    return str(item["case_id"]), int(item["attempt_index"])


def _materialize(
    packet: dict[str, Any],
    decisions: dict[tuple[str, int], dict[str, Any]],
    assisted_overrides: dict[tuple[str, int], dict[str, Any]],
) -> dict[str, Any]:
    track = str(packet["answer_track"])
    reviews: list[dict[str, Any]] = []
    for packet_review in packet["reviews"]:
        key = _key(packet_review)
        if key not in decisions:
            raise ValueError(f"missing manual decision for {key[0]} attempt {key[1]}")
        decision = dict(decisions[key])
        if track == "eval_assisted_answer" and key in assisted_overrides:
            decision.update(assisted_overrides[key])

        verdict = str(decision["verdict"])
        evidence = [str(item) for item in decision.get("evidence_turn_ids", [])]
        major = set(decision.get("major_dimensions", []))
        minor = set(decision.get("minor_dimensions", []))
        unknown = (major | minor) - set(DIMENSIONS)
        if unknown:
            raise ValueError(f"unknown dimensions for {key}: {sorted(unknown)}")
        if verdict == "PASS" and major:
            raise ValueError(f"PASS decision cannot contain major dimensions: {key}")

        dimensions: dict[str, Any] = {}
        for name in DIMENSIONS:
            rating = (
                "major_issue"
                if name in major
                else "minor_issue"
                if name in minor
                else "acceptable"
            )
            dimensions[name] = {
                "rating": rating,
                "comment": str(
                    decision.get("dimension_comments", {}).get(
                        name,
                        decision["summary"],
                    )
                ),
                "evidence_turn_ids": evidence,
            }

        critical = [str(item) for item in decision.get("critical_findings", [])]
        if verdict == "FAIL" and not critical:
            critical = [str(decision["summary"])]
        reviews.append(
            {
                "case_id": key[0],
                "attempt_index": key[1],
                "answer_track": track,
                "review_basis_sha256": packet_review["review_basis_sha256"],
                "verdict": verdict,
                "summary": str(decision["summary"]),
                "dimensions": dimensions,
                "strengths": [str(item) for item in decision.get("strengths", [])],
                "findings": [str(item) for item in decision.get("findings", [])],
                "critical_findings": critical,
            }
        )

    packet_keys = {_key(item) for item in packet["reviews"]}
    extra = set(decisions) - packet_keys
    if extra:
        raise ValueError(f"manual decisions not present in packet: {sorted(extra)}")
    return {"schema_version": 1, "answer_track": track, "reviews": reviews}


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--decisions", type=Path, required=True)
    parser.add_argument("--production-packet", type=Path, required=True)
    parser.add_argument("--assisted-packet", type=Path, required=True)
    parser.add_argument("--production-output", type=Path, required=True)
    parser.add_argument("--assisted-output", type=Path, required=True)
    args = parser.parse_args()

    source = _read(args.decisions)
    decisions = {_key(item): item for item in source["reviews"]}
    assisted_overrides = {
        _key(item): item for item in source.get("assisted_overrides", [])
    }
    _write(
        args.production_output,
        _materialize(_read(args.production_packet), decisions, assisted_overrides),
    )
    _write(
        args.assisted_output,
        _materialize(_read(args.assisted_packet), decisions, assisted_overrides),
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
