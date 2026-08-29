from __future__ import annotations

import unittest
from unittest.mock import patch

from weknora_eval.client import WeKnoraResponseDeadlineExceeded
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSetup,
    CaseSpec,
    SUTFingerprint,
    SUT_RESPONSE_DEADLINE_EXCEEDED,
    SUT_TURN_SKIPPED_AFTER_DEADLINE,
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


class DeadlineClient(FakeClient):
    def __init__(self) -> None:
        super().__init__("eval")

    def create_session(self) -> str:
        return "deadline-session"

    def stream(self, _path: str, _payload: dict):
        raise WeKnoraResponseDeadlineExceeded(
            "/agent-chat/deadline-session",
            1.0,
            [{"response_type": "tool", "tool_name": "list_knowledge_chunks"}],
            100,
            1000,
        )


class RecoveredStreamClient(FakeClient):
    def __init__(self, *, completed: bool) -> None:
        super().__init__("eval")
        self.completed = completed

    def create_session(self) -> str:
        return "recovered-session"

    def stream(self, _path: str, _payload: dict):
        return ([{"response_type": "error", "done": True}], 10, 100)

    def load_completed_assistant(self, _session_id: str, **_kwargs: object):
        return {
            "id": "message",
            "role": "assistant",
            "content": "recovered answer" if self.completed else "",
            "is_completed": self.completed,
        }


class PersistedRuntimeErrorClient(FakeClient):
    def __init__(self) -> None:
        super().__init__("eval")

    def create_session(self) -> str:
        return "runtime-error-session"

    def stream(self, _path: str, _payload: dict):
        return ([], 10, 100)

    def load_completed_assistant(self, _session_id: str, **_kwargs: object):
        return {
            "id": "message",
            "role": "assistant",
            "content": (
                "ResultMessage(subtype='success', is_error=True, "
                "result='API Error: context window exceeded')"
            ),
            "is_completed": True,
        }


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

    def test_sut_deadline_is_a_bounded_fail_and_preserves_turn_coverage(self) -> None:
        case = CaseSpec(
            case_id="deadline-case",
            family_id="deadline-family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent_profile_id="general-agent",
            agent=AgentSelector(agent_id="builtin-general-agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[
                TurnSpec(turn_id="turn-1", query="q1", contract=TurnContract()),
                TurnSpec(turn_id="turn-2", query="q2", contract=TurnContract()),
            ],
        )

        result = EvalRunner(DeadlineClient()).run_case(case)

        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertEqual(len(result.turns), 2)
        self.assertEqual(result.turns[0].error, SUT_RESPONSE_DEADLINE_EXCEEDED)
        self.assertEqual(result.turns[1].error, SUT_TURN_SKIPPED_AFTER_DEADLINE)
        self.assertEqual(result.turns[0].tools, ["list_knowledge_chunks"])
        self.assertTrue(
            all(
                score.metadata.get("failure_origin") == "sut"
                for score in result.scores
                if score.name == "execution_valid"
            )
        )

    def test_completed_persisted_answer_wins_over_recovered_stream_error(self) -> None:
        case = CaseSpec(
            case_id="recovered",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )

        result = EvalRunner(RecoveredStreamClient(completed=True)).run_case(case)

        self.assertEqual(result.verdict, Verdict.PASS)
        self.assertIsNone(result.turns[0].error)

    def test_incomplete_persisted_answer_is_sut_fail_with_full_turn_coverage(self) -> None:
        case = CaseSpec(
            case_id="incomplete",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[
                TurnSpec(turn_id="turn-1", query="q1", contract=TurnContract()),
                TurnSpec(turn_id="turn-2", query="q2", contract=TurnContract()),
            ],
        )

        result = EvalRunner(RecoveredStreamClient(completed=False)).run_case(case)

        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertEqual(len(result.turns), 2)
        self.assertTrue(result.turns[0].error.startswith("sut_stream_error:"))
        self.assertEqual(result.turns[1].error, "sut_turn_skipped_after_failure")

    def test_persisted_runtime_error_payload_cannot_masquerade_as_answer(self) -> None:
        case = CaseSpec(
            case_id="runtime-error",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )

        result = EvalRunner(PersistedRuntimeErrorClient()).run_case(case)

        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertTrue(result.turns[0].error.startswith("sut_stream_error:"))


if __name__ == "__main__":
    unittest.main()
