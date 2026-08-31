from __future__ import annotations

import hashlib
from typing import Any

from .dataset import canonical_json
from .models import (
    AnswerTrack,
    CaseRun,
    CaseSpec,
    CodexConversationReview,
    CodexDimensionReview,
    ReviewRating,
    Verdict,
)
from .scoring import project_case_to_answer_track


DEFAULT_CODEX_REVIEW_DIMENSIONS = (
    "task_fulfillment",
    "factual_grounding",
    "conversation_state",
    "source_attribution",
    "action_boundary",
    "communication_quality",
)

CODEX_MINIMUM_STANDARD = """
Review the complete conversation, not isolated keywords. PASS means the answer
is useful and materially correct for the user's actual request. Small wording,
formatting, or completeness issues may be marked minor_issue and still PASS.
Any invented material fact, stale-state resurrection that changes the answer,
misattributed source, unsupported evidence claim, action-boundary violation, or
failure to address the core request is a major issue and cannot PASS. Use
not_applicable only when a dimension genuinely does not arise in the dialogue.
The review rubric and findings are Eval-side only and must never be sent to the
system under test.
""".strip()


def review_basis_payload(
    spec: CaseSpec,
    case_run: CaseRun,
    answer_track: AnswerTrack,
) -> dict[str, Any]:
    projected = project_case_to_answer_track(case_run, answer_track)
    return {
        "case": {
            "case_id": spec.case_id,
            "family_id": spec.family_id,
            "split": spec.split.value,
            "agent_profile_id": spec.agent_profile_id,
            "turns": [
                {"turn_id": turn.turn_id, "query": turn.query}
                for turn in spec.turns
            ],
        },
        "observation": {
            "case_id": projected.case_id,
            "attempt_index": projected.attempt_index,
            "answer_track": answer_track.value,
            "turns": [
                {
                    "turn_id": turn.turn_id,
                    "content": turn.content,
                    "references": turn.references,
                    "tools": turn.tools,
                    "is_completed": turn.is_completed,
                    "error": turn.error,
                }
                for turn in projected.turns
            ],
        },
    }


def review_basis_sha256(
    spec: CaseSpec,
    case_run: CaseRun,
    answer_track: AnswerTrack,
) -> str:
    payload = review_basis_payload(spec, case_run, answer_track)
    return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()


def build_codex_review_packet(
    spec: CaseSpec,
    case_run: CaseRun,
    answer_track: AnswerTrack,
) -> dict[str, Any]:
    """Export one immutable whole-conversation review assignment.

    The packet intentionally excludes turn contracts, required claims,
    reference answers and prior judge feedback.  It contains only the user's
    original turns and the selected observed answer track, so a reviewer cannot
    accidentally feed Eval expectations back into the SUT or review a repaired
    answer while believing it is the production response.
    """

    return {
        "case_id": case_run.case_id,
        "attempt_index": case_run.attempt_index,
        "answer_track": answer_track.value,
        "review_basis_sha256": review_basis_sha256(
            spec, case_run, answer_track
        ),
        "rubric_version": "codex-conversation-minimum-v1",
        "minimum_standard": CODEX_MINIMUM_STANDARD,
        "conversation": review_basis_payload(spec, case_run, answer_track),
        "verdict": "",
        "summary": "",
        "dimensions": {
            name: {
                "rating": "",
                "comment": "",
                "evidence_turn_ids": [],
            }
            for name in DEFAULT_CODEX_REVIEW_DIMENSIONS
        },
        "strengths": [],
        "findings": [],
        "critical_findings": [],
    }


def review_for_track(
    case_run: CaseRun,
    answer_track: AnswerTrack,
) -> CodexConversationReview | None:
    if answer_track == AnswerTrack.PRODUCTION_CANDIDATE:
        return case_run.production_codex_review
    if answer_track == AnswerTrack.EVAL_ASSISTED_ANSWER:
        return case_run.eval_assisted_codex_review
    return None


def build_codex_review(
    spec: CaseSpec,
    case_run: CaseRun,
    payload: dict[str, Any],
) -> CodexConversationReview:
    answer_track = AnswerTrack(str(payload.get("answer_track") or ""))
    expected_basis = review_basis_sha256(spec, case_run, answer_track)
    supplied_basis = str(payload.get("review_basis_sha256") or "").strip()
    if supplied_basis != expected_basis:
        raise ValueError(
            "review_basis_sha256 does not match the exported complete conversation"
        )
    dimensions_raw = payload.get("dimensions")
    if not isinstance(dimensions_raw, dict):
        raise ValueError("review dimensions must be an object")
    dimensions = {
        str(name): CodexDimensionReview.model_validate(value)
        for name, value in dimensions_raw.items()
    }
    return CodexConversationReview(
        answer_track=answer_track,
        case_id=case_run.case_id,
        attempt_index=case_run.attempt_index,
        review_basis_sha256=supplied_basis,
        verdict=Verdict(str(payload.get("verdict") or "")),
        summary=str(payload.get("summary") or "").strip(),
        dimensions=dimensions,
        strengths=[str(item) for item in payload.get("strengths") or []],
        findings=[str(item) for item in payload.get("findings") or []],
        critical_findings=[
            str(item) for item in payload.get("critical_findings") or []
        ],
    )


def attach_codex_review(
    case_run: CaseRun,
    review: CodexConversationReview,
) -> CaseRun:
    if review.answer_track == AnswerTrack.PRODUCTION_CANDIDATE:
        return case_run.model_copy(update={"production_codex_review": review})
    if review.answer_track == AnswerTrack.EVAL_ASSISTED_ANSWER:
        return case_run.model_copy(update={"eval_assisted_codex_review": review})
    raise ValueError("Codex reviews require an explicit dual-track answer surface")


def validate_codex_review(
    spec: CaseSpec,
    case_run: CaseRun,
    answer_track: AnswerTrack,
    required_dimensions: list[str],
) -> list[str]:
    review = review_for_track(case_run, answer_track)
    if review is None:
        return [f"missing Codex review for {answer_track.value}"]
    errors: list[str] = []
    if review.case_id != case_run.case_id:
        errors.append("review case_id mismatch")
    if review.attempt_index != case_run.attempt_index:
        errors.append("review attempt_index mismatch")
    if review.answer_track != answer_track:
        errors.append("review answer_track mismatch")
    expected_basis = review_basis_sha256(spec, case_run, answer_track)
    if review.review_basis_sha256 != expected_basis:
        errors.append("review basis hash does not match the current conversation")
    missing_dimensions = sorted(set(required_dimensions) - set(review.dimensions))
    if missing_dimensions:
        errors.append("missing review dimensions: " + ", ".join(missing_dimensions))
    known_turns = {turn.turn_id for turn in spec.turns}
    unknown_turns = sorted(
        {
            turn_id
            for dimension in review.dimensions.values()
            for turn_id in dimension.evidence_turn_ids
            if turn_id not in known_turns
        }
    )
    if unknown_turns:
        errors.append("review references unknown turns: " + ", ".join(unknown_turns))
    if review.verdict == Verdict.PASS:
        material_dimensions = sorted(
            name
            for name, dimension in review.dimensions.items()
            if dimension.rating == ReviewRating.MAJOR_ISSUE
        )
        if material_dimensions or review.critical_findings:
            errors.append("PASS review contains a material or critical finding")
    return errors
