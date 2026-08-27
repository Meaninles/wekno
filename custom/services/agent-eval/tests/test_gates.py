from __future__ import annotations

import unittest

from weknora_eval.gates import evaluate_gate
from weknora_eval.dataset import dataset_sha256
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    ExperimentRun,
    GatePolicy,
    MetricScore,
    ObservedTurn,
    SUTFingerprint,
    Split,
    TextRule,
    TurnContract,
    TurnSpec,
    Verdict,
)


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


def run_for(dataset: list[CaseSpec], run_id: str, case_runs: list[CaseRun]) -> ExperimentRun:
    return ExperimentRun(
        run_id=run_id,
        suite="suite",
        dataset_sha256=dataset_sha256(dataset),
        splits=[Split.GATE],
        sut=SUTFingerprint(
            mode="eval",
            capabilities=[Capability.RAG_RETRIEVAL.value],
            raw={"recorder_enabled": True},
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


if __name__ == "__main__":
    unittest.main()
