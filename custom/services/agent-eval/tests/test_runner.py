from __future__ import annotations

import unittest
from unittest.mock import patch

from weknora_eval.client import WeKnoraResponseDeadlineExceeded
from weknora_eval.models import (
    AnswerSnapshot,
    AgentSelector,
    Capability,
    CaseRun,
    CaseSetup,
    CaseSpec,
    KnowledgeSelectionMode,
    RepairTrace,
    SUTFingerprint,
    SUT_RESPONSE_DEADLINE_EXCEEDED,
    SUT_TURN_SKIPPED_AFTER_DEADLINE,
    Split,
    TextRule,
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


class TerminalStreamErrorClient(RecoveredStreamClient):
    def __init__(self) -> None:
        super().__init__(completed=True)

    def stream(self, _path: str, _payload: dict):
        return (
            [
                {
                    "response_type": "error",
                    "done": True,
                    "content": "智能体未能生成满足当前请求约束的完整回答，请重试",
                    "data": {"stage": "custom_agent_execution"},
                }
            ],
            10,
            100,
        )


class PayloadRecordingClient(FakeClient):
    def __init__(self) -> None:
        super().__init__("eval")
        self.payloads: list[dict] = []

    def create_session(self) -> str:
        return "payload-session"

    def stream(self, _path: str, payload: dict):
        self.payloads.append(payload)
        return (
            [
                {
                    "id": "answer-1",
                    "response_type": "answer",
                    "content": "bounded answer",
                },
                {
                    "response_type": "complete",
                    "data": {"final_answer": "bounded answer"},
                },
            ],
            10,
            100,
        )

    def load_completed_assistant(self, _session_id: str, **_kwargs: object):
        return {
            "id": "message",
            "role": "assistant",
            "content": "bounded answer",
            "is_completed": True,
        }


class KnowledgeSelectionClient(PayloadRecordingClient):
    def __init__(self, *, runtime_mode: str, configured_kbs: list[str] | None = None) -> None:
        super().__init__()
        self.runtime_mode = runtime_mode
        self.configured_kbs = configured_kbs or []
        self.agent_lookups: list[str] = []

    def get_agent(self, agent_id: str):
        self.agent_lookups.append(agent_id)
        return {
            "id": agent_id,
            "config": {
                "kb_selection_mode": self.runtime_mode,
                "knowledge_bases": self.configured_kbs,
            },
        }


class PassingAssistant:
    def __init__(self) -> None:
        self.calls: list[dict[str, object]] = []

    def assist(self, **kwargs: object):
        self.calls.append(kwargs)
        return (
            AnswerSnapshot(content="general improvement"),
            RepairTrace(
                triggered=True,
                succeeded=True,
                repair_types=["terminal_rewrite"],
                attempts=1,
                added_model_calls=1,
                added_latency_ms=12,
            ),
        )


class DivergentSurfaceClient(PayloadRecordingClient):
    def stream(self, _path: str, payload: dict):
        self.payloads.append(payload)
        return (
            [
                {
                    "id": "answer-1",
                    "response_type": "answer",
                    "content": "SSE-only answer",
                },
                {
                    "response_type": "complete",
                    "data": {"final_answer": "bounded answer"},
                },
            ],
            10,
            100,
        )


class RunnerTests(unittest.TestCase):
    @staticmethod
    def _knowledge_selection_case(mode: KnowledgeSelectionMode) -> CaseSpec:
        kb_ids = ["kb-selected"] if mode == KnowledgeSelectionMode.EXPLICIT else []
        return CaseSpec(
            case_id=f"knowledge-selection-{mode.value}",
            family_id="knowledge-selection",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(
                knowledge_base_ids=kb_ids,
                knowledge_selection_mode=mode,
                summary_model_id="model",
            ),
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )

    def test_no_kb_contract_accepts_only_a_runtime_none_agent(self) -> None:
        client = KnowledgeSelectionClient(runtime_mode="none")

        result = EvalRunner(client).run_case(
            self._knowledge_selection_case(KnowledgeSelectionMode.NONE)
        )

        self.assertNotEqual(result.verdict, Verdict.INVALID)
        self.assertEqual(client.agent_lookups, ["agent"])
        self.assertEqual(client.payloads[0]["knowledge_base_ids"], [])

    def test_no_kb_contract_rejects_agent_default_all_before_session_creation(self) -> None:
        client = KnowledgeSelectionClient(runtime_mode="all")

        result = EvalRunner(client).run_case(
            self._knowledge_selection_case(KnowledgeSelectionMode.NONE)
        )

        self.assertEqual(result.verdict, Verdict.INVALID)
        self.assertIn("kb_selection_mode=none", result.error)
        self.assertEqual(client.payloads, [])

    def test_explicit_kb_contract_rejects_runtime_none_agent(self) -> None:
        client = KnowledgeSelectionClient(runtime_mode="none")

        result = EvalRunner(client).run_case(
            self._knowledge_selection_case(KnowledgeSelectionMode.EXPLICIT)
        )

        self.assertEqual(result.verdict, Verdict.INVALID)
        self.assertIn("cannot use an agent", result.error)
        self.assertEqual(client.payloads, [])

    def test_runner_uses_complete_event_as_the_public_sse_surface(self) -> None:
        case = CaseSpec(
            case_id="surface-divergence",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )

        result = EvalRunner(DivergentSurfaceClient()).run_case(case)

        observed = result.turns[0]
        self.assertEqual(observed.streamed_content, "bounded answer")
        self.assertEqual(observed.production_candidate.content, "bounded answer")
        self.assertTrue(observed.production_surface_equivalent)

    def test_runner_never_sends_eval_contract_to_sut(self) -> None:
        client = PayloadRecordingClient()
        case = CaseSpec(
            case_id="shape-contract",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[
                TurnSpec(
                    turn_id="turn",
                    query="q",
                    contract=TurnContract(max_response_chars=321),
                )
            ],
        )

        result = EvalRunner(client).run_case(case)

        self.assertNotIn("eval_response_contract", client.payloads[0])
        observed = result.turns[0]
        self.assertEqual(observed.content, "bounded answer")
        self.assertEqual(observed.production_candidate.content, "bounded answer")
        self.assertEqual(observed.eval_assisted_answer.content, "bounded answer")
        self.assertEqual(observed.streamed_content, "bounded answer")
        self.assertTrue(observed.production_surface_equivalent)

    def test_runner_persists_production_and_records_assisted_track_separately(self) -> None:
        client = PayloadRecordingClient()
        assistant = PassingAssistant()
        case = CaseSpec(
            case_id="dual-track",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[
                TurnSpec(
                    turn_id="turn",
                    query="Improve this generally.",
                    contract=TurnContract(
                        required_claims=[
                            TextRule(rule_id="improvement", any_of=["general improvement"])
                        ]
                    ),
                )
            ],
        )

        result = EvalRunner(client, assistant=assistant).run_case(case)

        self.assertEqual(len(assistant.calls), 1)
        self.assertTrue(assistant.calls[0]["force"])
        observed = result.turns[0]
        self.assertEqual(observed.content, "bounded answer")
        self.assertEqual(observed.production_candidate.content, "bounded answer")
        self.assertEqual(
            observed.eval_assisted_answer.content, "general improvement"
        )
        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertEqual(result.production_verdict, Verdict.FAIL)
        self.assertEqual(result.eval_assisted_verdict, Verdict.PASS)
        self.assertTrue(result.repair_only_pass)
        self.assertTrue(observed.repair.triggered)

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

    def test_terminal_staged_stream_error_cannot_masquerade_as_completed_answer(self) -> None:
        case = CaseSpec(
            case_id="terminal-runtime-error",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            setup=CaseSetup(summary_model_id="model"),
            turns=[TurnSpec(turn_id="turn", query="q", contract=TurnContract())],
        )

        result = EvalRunner(TerminalStreamErrorClient()).run_case(case)

        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertTrue(result.turns[0].error.startswith("sut_stream_error:"))


if __name__ == "__main__":
    unittest.main()
