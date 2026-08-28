from __future__ import annotations

import unittest
from unittest.mock import patch

from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    SUTFingerprint,
    Split,
    TurnContract,
    TurnSpec,
    Verdict,
)
from weknora_eval.runner import EvalModeRequired, EvalRunner


class FakeClient:
    def __init__(self, mode: str) -> None:
        self.mode = mode

    def capabilities(self):
        return {"mode": self.mode, "capture_policy": "full", "recorder_enabled": True, "release": "r", "commit": "c", "capabilities": []}


class RecordingRunner(EvalRunner):
    def __init__(self) -> None:
        super().__init__(FakeClient("eval"))  # type: ignore[arg-type]
        self.attempts: list[int] = []

    def doctor(self) -> SUTFingerprint:
        return SUTFingerprint(mode="eval", raw={"recorder_enabled": True, "capture_policy": "full"})

    def run_case(self, spec: CaseSpec, *, attempt_index: int = 1) -> CaseRun:
        self.attempts.append(attempt_index)
        return CaseRun(
            case_id=spec.case_id,
            family_id=spec.family_id,
            split=spec.split,
            agent_profile_id=spec.agent_profile_id,
            attempt_index=attempt_index,
            verdict=Verdict.PASS,
        )


class RunnerTests(unittest.TestCase):
    def test_production_is_record_only(self) -> None:
        with self.assertRaises(EvalModeRequired):
            EvalRunner(FakeClient("production")).doctor()  # type: ignore[arg-type]

    def test_eval_mode_is_accepted(self) -> None:
        with patch.dict("os.environ", {"AGENT_EVAL_SUT_COMMIT": ""}, clear=False):
            fingerprint = EvalRunner(FakeClient("eval")).doctor()  # type: ignore[arg-type]
        self.assertEqual(fingerprint.mode, "eval")

    def test_orchestrated_provenance_fills_blank_build_commit(self) -> None:
        client = FakeClient("eval")
        client.capabilities = lambda: {  # type: ignore[method-assign]
            "mode": "eval",
            "capture_policy": "full",
            "recorder_enabled": True,
            "commit": "",
            "capabilities": [],
        }
        with patch.dict(
            "os.environ",
            {
                "AGENT_EVAL_SUT_COMMIT": "source-commit",
                "AGENT_EVAL_SUT_WORKTREE_DIRTY": "false",
                "AGENT_EVAL_RUNTIME_IMAGE_ID": "sha256:runtime",
                "AGENT_EVAL_GENERAL_AGENT_IMAGE_ID": "sha256:general",
            },
            clear=False,
        ):
            fingerprint = EvalRunner(client).doctor()  # type: ignore[arg-type]
        self.assertEqual(fingerprint.commit, "source-commit")
        self.assertEqual(fingerprint.raw["runtime_image_id"], "sha256:runtime")

    def test_suite_executes_each_session_repetition(self) -> None:
        case = CaseSpec(
            case_id="case",
            family_id="family",
            suite="suite",
            split=Split.GATE,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent_profile_id="general-agent",
            agent=AgentSelector(agent_id="builtin-general-agent"),
            repetitions=3,
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )
        runner = RecordingRunner()

        result = runner.run_suite([case], selected_splits={Split.GATE})

        self.assertEqual(runner.attempts, [1, 2, 3])
        self.assertEqual([item.attempt_index for item in result.cases], [1, 2, 3])
        self.assertTrue(all(item.agent_profile_id == "general-agent" for item in result.cases))


if __name__ == "__main__":
    unittest.main()
