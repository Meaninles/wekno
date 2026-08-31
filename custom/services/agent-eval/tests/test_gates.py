from __future__ import annotations

import unittest

from weknora_eval.gates import _adjudicate_case, evaluate_gate
from weknora_eval.dataset import dataset_sha256
from weknora_eval.models import (
    AnswerSnapshot,
    AnswerTrack,
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    ConversationStateContract,
    ExperimentRun,
    GatePolicy,
    GateKind,
    MetricScore,
    ObservedTurn,
    RepairTrace,
    SUTFingerprint,
    Split,
    TextRule,
    TurnContract,
    TurnSpec,
    Verdict,
)
from weknora_eval.scoring import score_case_tracks


def spec(*, repetitions: int = 1, profile_id: str | None = None) -> CaseSpec:
    return CaseSpec(
        case_id="gate-case",
        family_id="family",
        suite="suite",
        split=Split.GATE,
        capabilities=[Capability.RAG_RETRIEVAL],
        agent_profile_id=profile_id,
        agent=AgentSelector(agent_id="agent"),
        repetitions=repetitions,
        turns=[
            TurnSpec(
                turn_id="turn",
                query="q",
                contract=TurnContract(required_claims=[TextRule(rule_id="a", any_of=["a"])]),
            )
        ],
    )


def case(
    verdict: Verdict,
    latency: int = 100,
    *,
    attempt_index: int = 1,
    profile_id: str | None = None,
) -> CaseRun:
    return CaseRun(
        case_id="gate-case",
        family_id="family",
        split=Split.GATE,
        agent_profile_id=profile_id,
        attempt_index=attempt_index,
        verdict=verdict,
        turns=[
            ObservedTurn(
                turn_id="turn",
                session_id="s",
                content="a" if verdict == Verdict.PASS else "wrong",
                is_completed=True,
                total_latency_ms=latency,
            )
        ],
        scores=[
            MetricScore(name="required_claim.a", value=verdict == Verdict.PASS, passed=verdict == Verdict.PASS)
        ],
    )


def with_judge(case_run: CaseRun, label: str, confidence: float = 0.95) -> CaseRun:
    return case_run.model_copy(
        update={
            "scores": [
                *case_run.scores,
                MetricScore(
                    name="judge.contract_satisfaction",
                    value=label,
                    passed=label == "pass",
                    hard=False,
                    turn_id="turn",
                    metadata={"confidence": confidence},
                ),
            ]
        }
    )


def run_for(dataset: list[CaseSpec], run_id: str, case_runs: list[CaseRun]) -> ExperimentRun:
    return ExperimentRun(
        run_id=run_id,
        suite="suite",
        dataset_sha256=dataset_sha256(dataset),
        splits=[Split.GATE],
        sut=SUTFingerprint(
            mode="eval",
            capabilities=[Capability.RAG_RETRIEVAL.value],
            raw={"recorder_enabled": True, "capture_policy": "full"},
        ),
        cases=case_runs,
    )


def run(run_id: str, case_run: CaseRun) -> ExperimentRun:
    return run_for([spec()], run_id, [case_run])


class GateTests(unittest.TestCase):
    def setUp(self) -> None:
        self.policy = GatePolicy(
            policy_id="policy",
            required_capabilities=[Capability.RAG_RETRIEVAL],
        )

    def test_judge_invalid_does_not_reclassify_measured_sut_deadline(self) -> None:
        deadline_case = CaseRun(
            case_id="gate-case",
            family_id="family",
            split=Split.GATE,
            verdict=Verdict.FAIL,
            turns=[
                ObservedTurn(
                    turn_id="turn",
                    session_id="session",
                    error="sut_response_deadline_exceeded",
                )
            ],
            scores=[
                MetricScore(
                    name="execution_valid",
                    value=False,
                    passed=False,
                    turn_id="turn",
                    metadata={"failure_origin": "sut"},
                ),
                MetricScore(
                    name="judge.contract_satisfaction",
                    value="invalid",
                    passed=False,
                    hard=False,
                    turn_id="turn",
                    metadata={"confidence": 0.1},
                ),
            ],
        )
        policy = self.policy.model_copy(update={"require_judge": True})

        adjudicated, detail = _adjudicate_case(deadline_case, policy)

        self.assertEqual(adjudicated.verdict, Verdict.FAIL)
        self.assertEqual(detail["invalid"], [])
        self.assertEqual(detail["judge_failures"], ["turn"])

    def test_pairwise_pass(self) -> None:
        result = evaluate_gate([spec()], run("candidate", case(Verdict.PASS)), self.policy, run("baseline", case(Verdict.PASS)))
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_pass_to_fail_is_fail(self) -> None:
        result = evaluate_gate([spec()], run("candidate", case(Verdict.FAIL)), self.policy, run("baseline", case(Verdict.PASS)))
        self.assertEqual(result.verdict, Verdict.FAIL)

    def test_missing_case_is_invalid_not_quality_fail(self) -> None:
        candidate = run("candidate", case(Verdict.PASS)).model_copy(update={"cases": []})
        result = evaluate_gate([spec()], candidate, self.policy, run("baseline", case(Verdict.PASS)))
        self.assertEqual(result.verdict, Verdict.INVALID)

    def test_dataset_file_must_match_run_artifact(self) -> None:
        candidate = run("candidate", case(Verdict.PASS)).model_copy(
            update={"dataset_sha256": "stale"}
        )
        result = evaluate_gate(
            [spec()], candidate, self.policy, run("baseline", case(Verdict.PASS))
        )
        self.assertEqual(result.verdict, Verdict.INVALID)

    def test_tampered_stored_verdict_is_invalid(self) -> None:
        tampered = case(Verdict.PASS).model_copy(update={"verdict": Verdict.FAIL})
        result = evaluate_gate(
            [spec()], run("candidate", tampered), self.policy, run("baseline", case(Verdict.PASS))
        )
        self.assertEqual(result.verdict, Verdict.INVALID)

    def test_repeated_sessions_are_paired_by_attempt(self) -> None:
        dataset = [spec(repetitions=2, profile_id="rag-reasoning")]
        candidate = run_for(
            dataset,
            "candidate",
            [
                case(Verdict.PASS, attempt_index=1, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=2, profile_id="rag-reasoning"),
            ],
        )
        baseline = run_for(
            dataset,
            "baseline",
            [
                case(Verdict.PASS, attempt_index=1, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=2, profile_id="rag-reasoning"),
            ],
        )
        policy = self.policy.model_copy(
            update={
                "min_repetitions_per_case": 2,
                "required_agent_profiles": ["rag-reasoning"],
            }
        )

        result = evaluate_gate(dataset, candidate, policy, baseline)

        self.assertEqual(result.verdict, Verdict.PASS)

    def test_missing_repeated_session_is_invalid(self) -> None:
        dataset = [spec(repetitions=2, profile_id="rag-reasoning")]
        candidate = run_for(
            dataset,
            "candidate",
            [case(Verdict.PASS, attempt_index=1, profile_id="rag-reasoning")],
        )
        baseline = run_for(
            dataset,
            "baseline",
            [
                case(Verdict.PASS, attempt_index=1, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=2, profile_id="rag-reasoning"),
            ],
        )

        result = evaluate_gate(dataset, candidate, self.policy, baseline)

        self.assertEqual(result.verdict, Verdict.INVALID)

    def test_required_agent_profile_must_exist_in_dataset(self) -> None:
        policy = self.policy.model_copy(
            update={"required_agent_profiles": ["general-agent"]}
        )

        result = evaluate_gate(
            [spec(profile_id="rag-reasoning")],
            run_for(
                [spec(profile_id="rag-reasoning")],
                "candidate",
                [case(Verdict.PASS, profile_id="rag-reasoning")],
            ),
            policy,
            run_for(
                [spec(profile_id="rag-reasoning")],
                "baseline",
                [case(Verdict.PASS, profile_id="rag-reasoning")],
            ),
        )

        self.assertEqual(result.verdict, Verdict.INVALID)

    def test_required_execution_identity_must_match_baseline(self) -> None:
        dataset = [spec()]
        candidate = run_for(dataset, "candidate", [case(Verdict.PASS)])
        baseline = run_for(dataset, "baseline", [case(Verdict.PASS)])
        candidate = candidate.model_copy(
            update={"metadata": {"execution_contract": {"summary_model_id": "model-new"}}}
        )
        baseline = baseline.model_copy(
            update={"metadata": {"execution_contract": {"summary_model_id": "model-old"}}}
        )
        policy = self.policy.model_copy(
            update={"required_execution_identity_fields": ["summary_model_id"]}
        )

        result = evaluate_gate(dataset, candidate, policy, baseline)

        self.assertEqual(result.verdict, Verdict.INVALID)
        paired = next(check for check in result.checks if check.name == "paired_execution_identity")
        self.assertEqual(paired.verdict, Verdict.INVALID)

    def test_sut_commits_may_differ_when_provenance_is_complete(self) -> None:
        dataset = [spec()]
        candidate = run_for(dataset, "candidate", [case(Verdict.PASS)])
        baseline = run_for(dataset, "baseline", [case(Verdict.PASS)])
        candidate = candidate.model_copy(
            update={
                "sut": candidate.sut.model_copy(
                    update={
                        "commit": "candidate-commit",
                        "raw": {
                            **candidate.sut.raw,
                            "worktree_dirty": "false",
                            "runtime_image_id": "sha256:runtime-new",
                            "general_agent_image_id": "sha256:general-new",
                        },
                    }
                )
            }
        )
        baseline = baseline.model_copy(
            update={
                "sut": baseline.sut.model_copy(
                    update={
                        "commit": "baseline-commit",
                        "raw": {
                            **baseline.sut.raw,
                            "worktree_dirty": "false",
                            "runtime_image_id": "sha256:runtime-old",
                            "general_agent_image_id": "sha256:general-old",
                        },
                    }
                )
            }
        )
        policy = self.policy.model_copy(
            update={
                "required_sut_identity_fields": [
                    "commit",
                    "raw.worktree_dirty",
                    "raw.runtime_image_id",
                    "raw.general_agent_image_id",
                ],
                "require_clean_sut": True,
            }
        )

        result = evaluate_gate(dataset, candidate, policy, baseline)

        self.assertEqual(result.verdict, Verdict.PASS)
        provenance = next(check for check in result.checks if check.name == "sut_provenance")
        self.assertEqual(provenance.verdict, Verdict.PASS)

    def test_required_judge_missing_or_low_confidence_is_invalid(self) -> None:
        policy = self.policy.model_copy(update={"require_judge": True})
        missing = evaluate_gate(
            [spec()],
            run("candidate", case(Verdict.PASS)),
            policy,
            run("baseline", case(Verdict.PASS)),
        )
        self.assertEqual(missing.verdict, Verdict.INVALID)

        low = evaluate_gate(
            [spec()],
            run("candidate", with_judge(case(Verdict.PASS), "pass", 0.5)),
            policy,
            run("baseline", with_judge(case(Verdict.PASS), "pass", 0.5)),
        )
        self.assertEqual(low.verdict, Verdict.INVALID)

    def test_judge_can_override_only_reviewable_semantic_failure(self) -> None:
        candidate = run(
            "candidate", with_judge(case(Verdict.FAIL), "pass")
        )
        baseline = run(
            "baseline", with_judge(case(Verdict.PASS), "pass")
        )
        reviewable = self.policy.model_copy(
            update={
                "require_judge": True,
                "judge_reviewable_metric_prefixes": ["required_claim"],
                "forbid_hard_failures": False,
            }
        )
        allowed = evaluate_gate([spec()], candidate, reviewable, baseline)
        self.assertEqual(allowed.verdict, Verdict.PASS)

        critical = reviewable.model_copy(
            update={"critical_metric_prefixes": ["required_claim"]}
        )
        blocked = evaluate_gate([spec()], candidate, critical, baseline)
        self.assertEqual(blocked.verdict, Verdict.FAIL)

    def test_judge_cannot_override_a_required_unknown_state_omission(self) -> None:
        deterministic_failure = case(Verdict.FAIL).model_copy(
            update={
                "scores": [
                    MetricScore(
                        name="state.unknown.user-identity",
                        value=False,
                        passed=False,
                        turn_id="turn",
                    )
                ]
            }
        )
        policy = self.policy.model_copy(
            update={
                "require_judge": True,
                "forbid_hard_failures": False,
                "critical_metric_prefixes": ["state.unknown"],
                "judge_reviewable_metric_prefixes": ["required_claim"],
            }
        )

        adjudicated, detail = _adjudicate_case(
            with_judge(deterministic_failure, "pass"),
            policy,
        )

        self.assertEqual(adjudicated.verdict, Verdict.FAIL)
        self.assertEqual(
            detail["non_reviewable_failures"],
            ["state.unknown.user-identity"],
        )

    def test_judge_can_downgrade_a_deterministic_pass(self) -> None:
        policy = self.policy.model_copy(
            update={"require_judge": True, "forbid_hard_failures": False}
        )
        candidate = run(
            "candidate", with_judge(case(Verdict.PASS), "fail")
        )
        baseline = run(
            "baseline", with_judge(case(Verdict.PASS), "pass")
        )
        result = evaluate_gate([spec()], candidate, policy, baseline)
        self.assertEqual(result.verdict, Verdict.FAIL)

    def test_repetition_non_regression_compares_case_rates_not_random_attempts(self) -> None:
        dataset = [spec(repetitions=3, profile_id="rag-reasoning")]
        candidate = run_for(
            dataset,
            "candidate",
            [
                case(Verdict.PASS, attempt_index=1, profile_id="rag-reasoning"),
                case(Verdict.FAIL, attempt_index=2, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=3, profile_id="rag-reasoning"),
            ],
        )
        baseline = run_for(
            dataset,
            "baseline",
            [
                case(Verdict.FAIL, attempt_index=1, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=2, profile_id="rag-reasoning"),
                case(Verdict.PASS, attempt_index=3, profile_id="rag-reasoning"),
            ],
        )
        policy = self.policy.model_copy(
            update={
                "min_repetitions_per_case": 3,
                "pair_repetitions_by_attempt": False,
                "min_pass_rate_per_case": 0.66,
                "forbid_hard_failures": False,
                "required_agent_profiles": ["rag-reasoning"],
            }
        )
        result = evaluate_gate(dataset, candidate, policy, baseline)
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_experiment_gate_requires_a_real_case_rate_improvement(self) -> None:
        policy = self.policy.model_copy(
            update={
                "forbid_hard_failures": False,
                "pair_repetitions_by_attempt": False,
                "min_pass_rate_per_case": 0.0,
                "min_improved_case_count": 1,
            }
        )

        unchanged = evaluate_gate(
            [spec()],
            run("candidate", case(Verdict.FAIL)),
            policy,
            run("baseline", case(Verdict.FAIL)),
        )
        self.assertEqual(unchanged.verdict, Verdict.FAIL)
        unchanged_check = next(
            check for check in unchanged.checks
            if check.name == "minimum_case_improvement"
        )
        self.assertEqual(unchanged_check.verdict, Verdict.FAIL)

        improved = evaluate_gate(
            [spec()],
            run("candidate", case(Verdict.PASS)),
            policy,
            run("baseline", case(Verdict.FAIL)),
        )
        self.assertEqual(improved.verdict, Verdict.PASS)
        improved_check = next(
            check for check in improved.checks
            if check.name == "minimum_case_improvement"
        )
        self.assertEqual(improved_check.details["count"], 1)

    def test_metric_non_regression_matches_a_metric_family_prefix(self) -> None:
        state_spec = spec().model_copy(
            update={
                "turns": [
                    TurnSpec(
                        turn_id="turn",
                        query="Which state is still unknown?",
                        contract=TurnContract(
                            conversation_state=ConversationStateContract(
                                unknown_facts=[
                                    TextRule(
                                        rule_id="owner",
                                        any_of=["owner unknown"],
                                    )
                                ]
                            )
                        ),
                    )
                ]
            }
        )

        def state_case(content: str, verdict: Verdict) -> CaseRun:
            return CaseRun(
                case_id="gate-case",
                family_id="family",
                split=Split.GATE,
                verdict=verdict,
                turns=[
                    ObservedTurn(
                        turn_id="turn",
                        session_id="s",
                        content=content,
                        is_completed=True,
                        total_latency_ms=100,
                    )
                ],
            )

        policy = self.policy.model_copy(
            update={
                "forbid_hard_failures": False,
                "forbid_pass_to_fail_regressions": False,
                "min_pass_rate_per_case": 0.0,
                "max_metric_rate_regression": {"state.unknown": 0.0},
            }
        )
        baseline = run_for(
            [state_spec], "baseline", [state_case("owner unknown", Verdict.PASS)]
        )

        unchanged = evaluate_gate([state_spec], baseline, policy, baseline)
        metric_check = next(
            check for check in unchanged.checks if check.name == "metric_non_regression"
        )
        self.assertEqual(metric_check.verdict, Verdict.PASS)

        regressed = evaluate_gate(
            [state_spec],
            run_for(
                [state_spec], "candidate", [state_case("not provided", Verdict.FAIL)]
            ),
            policy,
            baseline,
        )
        metric_check = next(
            check for check in regressed.checks if check.name == "metric_non_regression"
        )
        self.assertEqual(metric_check.verdict, Verdict.FAIL)
        self.assertIn("state.unknown", metric_check.details["regressions"])

    def test_absolute_latency_budget_is_per_agent(self) -> None:
        dataset = [spec(profile_id="quick-answer")]
        candidate = run_for(
            dataset,
            "candidate",
            [case(Verdict.PASS, latency=200, profile_id="quick-answer")],
        )
        baseline = run_for(
            dataset,
            "baseline",
            [case(Verdict.PASS, latency=100, profile_id="quick-answer")],
        )
        policy = self.policy.model_copy(
            update={
                "required_agent_profiles": ["quick-answer"],
                "max_latency_ms_by_agent": {"quick-answer": 150},
                "max_p95_latency_regression_ratio": 2,
            }
        )
        result = evaluate_gate(dataset, candidate, policy, baseline)
        absolute = next(
            check for check in result.checks if check.name == "absolute_latency_by_agent"
        )
        self.assertEqual(absolute.verdict, Verdict.FAIL)

    def test_eval_assisted_pass_cannot_rescue_production_release(self) -> None:
        dataset = [spec()]
        dual = score_case_tracks(
            dataset[0],
            CaseRun(
                case_id="gate-case",
                family_id="family",
                split=Split.GATE,
                verdict=Verdict.INVALID,
                turns=[
                    ObservedTurn(
                        turn_id="turn",
                        session_id="session",
                        content="wrong",
                        is_completed=True,
                        production_candidate=AnswerSnapshot(content="wrong"),
                        eval_assisted_answer=AnswerSnapshot(content="a"),
                        repair=RepairTrace(
                            triggered=True,
                            succeeded=True,
                            repair_types=["terminal_rewrite"],
                            attempts=1,
                            added_model_calls=1,
                            added_latency_ms=25,
                        ),
                    )
                ],
            ),
        )
        self.assertEqual(dual.production_verdict, Verdict.FAIL)
        self.assertEqual(dual.eval_assisted_verdict, Verdict.PASS)
        self.assertTrue(dual.repair_only_pass)
        artifact = run_for(dataset, "candidate", [dual])

        production_policy = GatePolicy(
            schema_version=2,
            policy_id="production-v2",
            gate_kind=GateKind.PRODUCTION_RELEASE,
            answer_track=AnswerTrack.PRODUCTION_CANDIDATE,
            required_capabilities=[Capability.RAG_RETRIEVAL],
            require_baseline=False,
            forbid_hard_failures=False,
            forbid_pass_to_fail_regressions=False,
            min_pass_rate_per_case=1,
            max_repair_only_pass_rate=1,
        )
        production_result = evaluate_gate(
            dataset, artifact, production_policy, None
        )

        self.assertEqual(production_result.verdict, Verdict.FAIL)
        repetition = next(
            check
            for check in production_result.checks
            if check.name == "repetition_pass_rate"
        )
        self.assertEqual(repetition.verdict, Verdict.FAIL)
        rescue = next(
            check
            for check in production_result.checks
            if check.name == "eval_assistance_cannot_rescue_release"
        )
        self.assertEqual(rescue.verdict, Verdict.PASS)
        self.assertEqual(rescue.details["cases"], ["gate-case#attempt-1"])
        self.assertFalse(rescue.details["counted_as_production_pass"])

        eval_policy = GatePolicy(
            schema_version=2,
            policy_id="eval-v2",
            gate_kind=GateKind.EVAL_OPTIMIZATION,
            answer_track=AnswerTrack.EVAL_ASSISTED_ANSWER,
            required_capabilities=[Capability.RAG_RETRIEVAL],
            require_baseline=False,
            forbid_pass_to_fail_regressions=False,
            max_repair_trigger_rate=1,
            max_repair_only_pass_rate=1,
            max_average_repair_attempts=1,
            max_added_model_calls_per_turn=1,
        )
        eval_result = evaluate_gate(dataset, artifact, eval_policy, None)

        self.assertEqual(eval_result.verdict, Verdict.PASS)
        dependency = next(
            check for check in eval_result.checks if check.name == "repair_dependency"
        )
        self.assertEqual(dependency.details["candidate"]["repair_trigger_rate"], 1)
        self.assertEqual(dependency.details["candidate"]["repair_success_rate"], 1)
        self.assertEqual(dependency.details["candidate"]["repair_only_pass_rate"], 1)
        self.assertEqual(dependency.details["candidate"]["added_model_calls"], 1)
        self.assertEqual(dependency.details["candidate"]["added_latency_ms"], 25)

    def test_production_gate_fails_closed_without_production_snapshot(self) -> None:
        dataset = [spec()]
        legacy = case(Verdict.PASS)
        policy = GatePolicy(
            schema_version=2,
            policy_id="production-v2",
            gate_kind=GateKind.PRODUCTION_RELEASE,
            answer_track=AnswerTrack.PRODUCTION_CANDIDATE,
            required_capabilities=[Capability.RAG_RETRIEVAL],
            require_baseline=False,
            forbid_pass_to_fail_regressions=False,
        )

        result = evaluate_gate(
            dataset, run_for(dataset, "candidate", [legacy]), policy, None
        )

        self.assertEqual(result.verdict, Verdict.INVALID)
        integrity = next(
            check
            for check in result.checks
            if check.name == "candidate_artifact_integrity"
        )
        self.assertIn(
            "missing production_candidate snapshot for turn",
            integrity.details["errors"]["gate-case#attempt-1"],
        )

    def test_required_production_surface_equivalence_is_mechanical_integrity(self) -> None:
        divergent = case(Verdict.PASS).model_copy(
            update={
                "turns": [
                    case(Verdict.PASS).turns[0].model_copy(
                        update={
                            "streamed_content": "SSE answer",
                            "production_surface_equivalent": False,
                        }
                    )
                ]
            }
        )
        policy = self.policy.model_copy(
            update={"require_production_surface_equivalence": True}
        )

        result = evaluate_gate(
            [spec()],
            run("candidate", divergent),
            policy,
            run("baseline", case(Verdict.PASS).model_copy(
                update={
                    "turns": [
                        case(Verdict.PASS).turns[0].model_copy(
                            update={
                                "streamed_content": "a",
                                "production_surface_equivalent": True,
                            }
                        )
                    ]
                }
            )),
        )

        self.assertEqual(result.verdict, Verdict.INVALID)
        integrity = next(
            check
            for check in result.checks
            if check.name == "candidate_artifact_integrity"
        )
        self.assertIn(
            "SSE/persisted production candidate equivalence is unproven or false",
            integrity.details["errors"]["gate-case#attempt-1"][0],
        )

    def test_required_formal_split_cannot_be_omitted(self) -> None:
        production_policy = self.policy.model_copy(
            update={
                "schema_version": 2,
                "policy_id": "production-v2",
                "gate_kind": GateKind.PRODUCTION_RELEASE,
                "answer_track": AnswerTrack.PRODUCTION_CANDIDATE,
                "required_splits": [Split.GATE, Split.SEALED_HOLDOUT],
                "require_baseline": False,
            }
        )
        production_case = case(Verdict.PASS).model_copy(
            update={
                "production_verdict": Verdict.PASS,
                "production_scores": case(Verdict.PASS).scores,
                "turns": [
                    case(Verdict.PASS).turns[0].model_copy(
                        update={
                            "production_candidate": AnswerSnapshot(content="a")
                        }
                    )
                ],
            }
        )

        result = evaluate_gate(
            [spec()],
            run_for([spec()], "candidate", [production_case]),
            production_policy,
            None,
        )

        self.assertEqual(result.verdict, Verdict.INVALID)
        coverage = next(
            check
            for check in result.checks
            if check.name == "required_split_coverage"
        )
        self.assertEqual(coverage.verdict, Verdict.INVALID)
        self.assertEqual(coverage.details["dataset_missing"], ["sealed_holdout"])
        self.assertEqual(coverage.details["candidate_missing"], ["sealed_holdout"])


if __name__ == "__main__":
    unittest.main()
