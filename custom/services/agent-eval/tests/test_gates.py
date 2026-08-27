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


def spec() -> CaseSpec:
    return CaseSpec(
        case_id="gate-case",
        family_id="family",
        suite="suite",
        split=Split.GATE,
        capabilities=[Capability.RAG_RETRIEVAL],
        agent=AgentSelector(agent_id="agent"),
        turns=[
            TurnSpec(
                turn_id="turn",
                query="q",
                contract=TurnContract(required_claims=[TextRule(rule_id="a", any_of=["a"])]),
            )
        ],
    )


def case(verdict: Verdict, latency: int = 100) -> CaseRun:
    return CaseRun(
        case_id="gate-case",
        family_id="family",
        split=Split.GATE,
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


def run(run_id: str, case_run: CaseRun) -> ExperimentRun:
    dataset = [spec()]
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
        cases=[case_run],
    )


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


if __name__ == "__main__":
    unittest.main()
