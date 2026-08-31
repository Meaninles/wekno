from __future__ import annotations

import json
import unittest

from weknora_eval.codex_review import (
    DEFAULT_CODEX_REVIEW_DIMENSIONS,
    attach_codex_review,
    build_codex_review,
    build_codex_review_packet,
    review_basis_sha256,
)
from weknora_eval.dataset import dataset_sha256
from weknora_eval.gates import evaluate_gate
from weknora_eval.models import (
    AnswerSnapshot,
    AnswerTrack,
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    ExperimentRun,
    GateKind,
    GatePolicy,
    ObservedTurn,
    RepairTrace,
    ReviewMode,
    SUTFingerprint,
    Split,
    TextRule,
    TurnContract,
    TurnSpec,
    Verdict,
)
from weknora_eval.scoring import score_case_tracks


def review_spec() -> CaseSpec:
    return CaseSpec(
        case_id="codex-case",
        family_id="codex-family",
        suite="codex-suite",
        split=Split.GATE,
        capabilities=[Capability.RAG_RETRIEVAL],
        agent=AgentSelector(agent_id="agent"),
        turns=[
            TurnSpec(
                turn_id="turn-001",
                query="Give a useful paraphrase; exact wording is not required.",
                contract=TurnContract(
                    required_claims=[
                        TextRule(rule_id="brittle-diagnostic", all_of=["EXACT TOKEN"])
                    ]
                ),
            )
        ],
        review_mode=ReviewMode.CODEX_CONVERSATION,
    )


def observed_case(content: str = "A materially correct paraphrase.") -> CaseRun:
    return score_case_tracks(
        review_spec(),
        CaseRun(
            case_id="codex-case",
            family_id="codex-family",
            split=Split.GATE,
            verdict=Verdict.INVALID,
            turns=[
                ObservedTurn(
                    turn_id="turn-001",
                    session_id="session",
                    content=content,
                    is_completed=True,
                    production_candidate=AnswerSnapshot(content=content),
                    eval_assisted_answer=AnswerSnapshot(content=content),
                )
            ],
        ),
    )


def review_payload(
    verdict: Verdict,
    case_run: CaseRun,
    *,
    answer_track: AnswerTrack = AnswerTrack.PRODUCTION_CANDIDATE,
    minor_dimension: str | None = None,
) -> dict:
    dimensions = {
        name: {
            "rating": (
                "minor_issue"
                if name == minor_dimension
                else "major_issue"
                if verdict == Verdict.FAIL and name == "task_fulfillment"
                else "acceptable"
            ),
            "comment": (
                "Small presentation issue; the answer remains useful."
                if name == minor_dimension
                else "The answer misses a material part of the user's request."
                if verdict == Verdict.FAIL and name == "task_fulfillment"
                else "Meets the minimum material-quality standard."
            ),
            "evidence_turn_ids": ["turn-001"],
        }
        for name in DEFAULT_CODEX_REVIEW_DIMENSIONS
    }
    return {
        "answer_track": answer_track.value,
        "review_basis_sha256": review_basis_sha256(
            review_spec(), case_run, answer_track
        ),
        "verdict": verdict.value,
        "summary": "Whole-conversation Codex assessment.",
        "dimensions": dimensions,
        "strengths": ["Addresses the user's actual intent."],
        "findings": ["Wording could be tighter."] if minor_dimension else [],
        "critical_findings": [],
    }


def run(case: CaseRun) -> ExperimentRun:
    spec = review_spec()
    return ExperimentRun(
        run_id="run",
        suite=spec.suite,
        dataset_sha256=dataset_sha256([spec]),
        splits=[Split.GATE],
        sut=SUTFingerprint(
            mode="eval",
            capabilities=[Capability.RAG_RETRIEVAL.value],
            raw={"recorder_enabled": True, "capture_policy": "full"},
        ),
        cases=[case],
    )


def policy(track: AnswerTrack = AnswerTrack.PRODUCTION_CANDIDATE) -> GatePolicy:
    kind = (
        GateKind.PRODUCTION_RELEASE
        if track == AnswerTrack.PRODUCTION_CANDIDATE
        else GateKind.EVAL_OPTIMIZATION
    )
    return GatePolicy(
        schema_version=2,
        policy_id="codex-policy",
        gate_kind=kind,
        answer_track=track,
        required_capabilities=[Capability.RAG_RETRIEVAL],
        require_baseline=False,
        forbid_hard_failures=False,
        forbid_pass_to_fail_regressions=False,
        critical_metric_prefixes=[],
        require_codex_review=True,
        required_codex_review_dimensions=list(DEFAULT_CODEX_REVIEW_DIMENSIONS),
    )


class CodexReviewTests(unittest.TestCase):
    def test_export_packet_contains_only_review_basis_and_is_hash_bound(self) -> None:
        spec = review_spec()
        case = observed_case()
        packet = build_codex_review_packet(
            spec, case, AnswerTrack.PRODUCTION_CANDIDATE
        )
        rendered = json.dumps(packet, ensure_ascii=False)

        self.assertIn("Give a useful paraphrase", rendered)
        self.assertIn("A materially correct paraphrase", rendered)
        self.assertNotIn("EXACT TOKEN", rendered)
        self.assertNotIn("required_claims", rendered)
        self.assertNotIn("reference_answers", rendered)
        self.assertEqual(
            packet["review_basis_sha256"],
            review_basis_sha256(
                spec, case, AnswerTrack.PRODUCTION_CANDIDATE
            ),
        )

        stale_payload = review_payload(Verdict.PASS, case)
        stale_payload["review_basis_sha256"] = "0" * 64
        with self.assertRaisesRegex(ValueError, "exported complete conversation"):
            build_codex_review(spec, case, stale_payload)

    def test_codex_pass_with_minor_issue_overrides_brittle_phrase_diagnostic(self) -> None:
        spec = review_spec()
        case = observed_case()
        self.assertEqual(case.production_verdict, Verdict.FAIL)
        review = build_codex_review(
            spec,
            case,
            review_payload(
                Verdict.PASS,
                case,
                minor_dimension="communication_quality",
            ),
        )
        reviewed = attach_codex_review(case, review)

        result = evaluate_gate([spec], run(reviewed), policy(), None)

        self.assertEqual(result.verdict, Verdict.PASS)
        codex_check = next(
            check for check in result.checks if check.name == "codex_conversation_review"
        )
        self.assertEqual(codex_check.verdict, Verdict.PASS)
        self.assertEqual(
            codex_check.details["cases"]["codex-case#attempt-1"]["dimensions"][
                "communication_quality"
            ],
            "minor_issue",
        )

    def test_missing_or_stale_review_fails_closed(self) -> None:
        spec = review_spec()
        missing = evaluate_gate([spec], run(observed_case()), policy(), None)
        self.assertEqual(missing.verdict, Verdict.INVALID)

        case = observed_case()
        reviewed = attach_codex_review(
            case,
            build_codex_review(spec, case, review_payload(Verdict.PASS, case)),
        )
        changed = reviewed.model_copy(
            update={
                "turns": [
                    reviewed.turns[0].model_copy(
                        update={
                            "content": "A changed answer.",
                            "production_candidate": AnswerSnapshot(
                                content="A changed answer."
                            ),
                        }
                    )
                ]
            }
        )
        stale = evaluate_gate([spec], run(changed), policy(), None)
        self.assertEqual(stale.verdict, Verdict.INVALID)
        check = next(
            item for item in stale.checks if item.name == "codex_conversation_review"
        )
        errors = check.details["cases"]["codex-case#attempt-1"]["errors"]
        self.assertIn(
            "review basis hash does not match the current conversation", errors
        )

    def test_codex_fail_is_quality_failure_not_framework_invalidity(self) -> None:
        spec = review_spec()
        case = observed_case()
        reviewed = attach_codex_review(
            case,
            build_codex_review(spec, case, review_payload(Verdict.FAIL, case)),
        )

        result = evaluate_gate([spec], run(reviewed), policy(), None)

        self.assertEqual(result.verdict, Verdict.FAIL)
        codex_check = next(
            check for check in result.checks if check.name == "codex_conversation_review"
        )
        self.assertEqual(codex_check.verdict, Verdict.PASS)
        repetition = next(
            check for check in result.checks if check.name == "repetition_pass_rate"
        )
        self.assertEqual(repetition.verdict, Verdict.FAIL)

    def test_eval_assisted_track_requires_its_own_review(self) -> None:
        spec = review_spec()
        case = observed_case()
        production_reviewed = attach_codex_review(
            case,
            build_codex_review(spec, case, review_payload(Verdict.PASS, case)),
        )

        missing = evaluate_gate(
            [spec],
            run(production_reviewed),
            policy(AnswerTrack.EVAL_ASSISTED_ANSWER),
            None,
        )
        self.assertEqual(missing.verdict, Verdict.INVALID)

        assisted_review = build_codex_review(
            spec,
            production_reviewed,
            review_payload(
                Verdict.PASS,
                production_reviewed,
                answer_track=AnswerTrack.EVAL_ASSISTED_ANSWER,
            ),
        )
        both = attach_codex_review(production_reviewed, assisted_review)
        passed = evaluate_gate(
            [spec], run(both), policy(AnswerTrack.EVAL_ASSISTED_ANSWER), None
        )
        self.assertEqual(passed.verdict, Verdict.PASS)

    def test_repair_dependency_requires_both_codex_answer_tracks(self) -> None:
        spec = review_spec()
        case = observed_case()
        production_reviewed = attach_codex_review(
            case,
            build_codex_review(spec, case, review_payload(Verdict.PASS, case)),
        )
        dual_policy = policy().model_copy(
            update={"require_dual_track_codex_review": True}
        )

        missing = evaluate_gate(
            [spec], run(production_reviewed), dual_policy, None
        )
        self.assertEqual(missing.verdict, Verdict.INVALID)
        coverage = next(
            check
            for check in missing.checks
            if check.name == "dual_track_codex_review"
        )
        self.assertEqual(coverage.verdict, Verdict.INVALID)

        fully_reviewed = attach_codex_review(
            production_reviewed,
            build_codex_review(
                spec,
                production_reviewed,
                review_payload(
                    Verdict.PASS,
                    production_reviewed,
                    answer_track=AnswerTrack.EVAL_ASSISTED_ANSWER,
                ),
            ),
        )
        passed = evaluate_gate([spec], run(fully_reviewed), dual_policy, None)
        self.assertEqual(passed.verdict, Verdict.PASS)
        coverage = next(
            check
            for check in passed.checks
            if check.name == "dual_track_codex_review"
        )
        self.assertEqual(coverage.verdict, Verdict.PASS)

    def test_assisted_codex_pass_cannot_rescue_production_codex_failure(self) -> None:
        spec = review_spec()
        dual = score_case_tracks(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[
                    ObservedTurn(
                        turn_id="turn-001",
                        session_id="session",
                        content="A wrong production answer.",
                        is_completed=True,
                        production_candidate=AnswerSnapshot(
                            content="A wrong production answer."
                        ),
                        eval_assisted_answer=AnswerSnapshot(
                            content="A useful answer containing EXACT TOKEN."
                        ),
                        repair=RepairTrace(
                            triggered=True,
                            succeeded=True,
                            repair_types=["terminal_rewrite"],
                            attempts=1,
                            added_model_calls=1,
                            added_latency_ms=20,
                        ),
                    )
                ],
            ),
        )
        with_production_review = attach_codex_review(
            dual,
            build_codex_review(
                spec,
                dual,
                review_payload(Verdict.FAIL, dual),
            ),
        )
        fully_reviewed = attach_codex_review(
            with_production_review,
            build_codex_review(
                spec,
                with_production_review,
                review_payload(
                    Verdict.PASS,
                    with_production_review,
                    answer_track=AnswerTrack.EVAL_ASSISTED_ANSWER,
                ),
            ),
        )

        production_result = evaluate_gate(
            [spec], run(fully_reviewed), policy(), None
        )
        assisted_result = evaluate_gate(
            [spec],
            run(fully_reviewed),
            policy(AnswerTrack.EVAL_ASSISTED_ANSWER),
            None,
        )

        self.assertEqual(production_result.verdict, Verdict.FAIL)
        self.assertEqual(assisted_result.verdict, Verdict.PASS)
        rescue = next(
            check
            for check in production_result.checks
            if check.name == "eval_assistance_cannot_rescue_release"
        )
        self.assertEqual(rescue.verdict, Verdict.PASS)
        self.assertEqual(rescue.details["cases"], ["codex-case#attempt-1"])
        self.assertFalse(rescue.details["counted_as_production_pass"])


if __name__ == "__main__":
    unittest.main()
