from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from weknora_eval.cli import build_parser
from weknora_eval.discovery import (
    DiscoveryCollector,
    _failure_class,
    assert_discovery_reviewed,
    load_adaptive_turn_plan,
    load_discovery_scenario,
    review_discovery_turn,
)


class FailingDiscoveryClient:
    def __init__(self) -> None:
        self.loads = 0
        self.deleted: list[str] = []

    def capabilities(self) -> dict:
        return {"mode": "eval", "capture_policy": "full", "recorder_enabled": True}

    def create_session(self) -> str:
        return "session-1"

    def stream(self, path: str, payload: dict):
        raise RuntimeError("transport failed")

    def request(self, method: str, path: str, body=None):
        if method == "GET":
            self.loads += 1
            messages = [{"id": "existing"}]
            if self.loads > 1:
                messages.extend([{"id": "new-user"}, {"id": "new-assistant"}])
            return {"data": messages}
        if method == "DELETE":
            self.deleted.append(path.rsplit("/", 1)[-1])
            return {"data": {"ok": True}}
        raise AssertionError((method, path))


class DiscoveryScenarioTests(unittest.TestCase):
    def _load(self, payload: dict) -> dict:
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "scenario.json"
            target.write_text(json.dumps(payload), encoding="utf-8")
            return load_discovery_scenario(target)

    def test_requires_two_turns_beyond_history_window(self) -> None:
        payload = {
            "schema_version": 1,
            "scenario_id": "s",
            "model_id": "model",
            "sessions": [
                {
                    "profile_id": "quick",
                    "agent_id": "builtin-quick-answer",
                    "endpoint": "knowledge-chat",
                    "history_turns": 5,
                    "minimum_turns": 6,
                    "turns": [{"turn_id": "turn-001", "user_message": "hello"}],
                }
            ],
        }
        with self.assertRaisesRegex(ValueError, r"history_turns \+ 2"):
            self._load(payload)

    def test_accepts_valid_discovery_scenario(self) -> None:
        payload = {
            "schema_version": 1,
            "scenario_id": "s",
            "model_id": "model",
            "sessions": [
                {
                    "profile_id": "quick",
                    "agent_id": "builtin-quick-answer",
                    "endpoint": "knowledge-chat",
                    "history_turns": 5,
                    "minimum_turns": 7,
                    "turns": [{"turn_id": "turn-001", "user_message": "hello"}],
                }
            ],
        }
        self.assertEqual(self._load(payload)["scenario_id"], "s")

    def test_rejects_a_fixed_multi_turn_discovery_script(self) -> None:
        payload = {
            "schema_version": 1,
            "scenario_id": "s",
            "model_id": "model",
            "sessions": [
                {
                    "profile_id": "quick",
                    "agent_id": "builtin-quick-answer",
                    "endpoint": "knowledge-chat",
                    "history_turns": 1,
                    "minimum_turns": 3,
                    "turns": [
                        {"turn_id": "turn-001", "user_message": "seed"},
                        {"turn_id": "turn-002", "user_message": "fixed continuation"},
                    ],
                }
            ],
        }
        with self.assertRaisesRegex(ValueError, "exactly one seed turn"):
            self._load(payload)

    def test_transport_failure_is_quarantined_and_state_is_rolled_back(self) -> None:
        client = FailingDiscoveryClient()
        scenario = {
            "schema_version": 1,
            "scenario_id": "s",
            "model_id": "model",
            "sessions": [
                {
                    "profile_id": "quick",
                    "agent_id": "builtin-quick-answer",
                    "endpoint": "knowledge-chat",
                    "history_turns": 1,
                    "minimum_turns": 3,
                    "turns": [
                        {
                            "turn_id": "turn-001",
                            "user_message": "hello",
                            "state_updates": [
                                {"key": "budget", "operation": "set", "value": "1"}
                            ],
                        }
                    ],
                }
            ],
        }

        result = DiscoveryCollector(client).collect(scenario)
        session = result["sessions"][0]

        self.assertEqual(session["turns"], [])
        self.assertEqual(len(session["invalid_attempts"]), 1)
        self.assertEqual(session["state_ledger"]["active"], {})
        self.assertEqual(set(client.deleted), {"new-user", "new-assistant"})
        self.assertTrue(session["invalid_attempts"][0]["rollback"]["recoverable"])
        self.assertEqual(session["invalid_attempts"][0]["failure_class"], "unknown")
        self.assertFalse(result["formal_eval_executed"])

    def test_failure_classes_keep_infrastructure_separate_from_quality(self) -> None:
        self.assertEqual(
            _failure_class("Failed to authenticate. API Error: 403"),
            ("upstream_authentication", True),
        )
        self.assertEqual(
            _failure_class("knowledge not in search target scope"),
            ("tool_scope", False),
        )
        self.assertEqual(_failure_class("request timed out"), ("timeout", True))

    def test_discovery_cli_defaults_to_one_review_checkpoint(self) -> None:
        args = build_parser().parse_args(
            ["discover", "--scenario", "scenario.json", "--output", "artifact.json"]
        )
        self.assertIsNone(args.next_turn_file)

    def test_live_run_accepts_repeated_focused_case_ids(self) -> None:
        args = build_parser().parse_args(
            [
                "run",
                "--dataset",
                "cases.jsonl",
                "--output",
                "run.json",
                "--case-id",
                "case-a",
                "--case-id",
                "case-b",
            ]
        )
        self.assertEqual(args.case_id, ["case-a", "case-b"])

    def test_completed_turn_requires_explicit_codex_review_before_resume(self) -> None:
        payload = {
            "sessions": [
                {
                    "profile_id": "general-agent",
                    "turns": [{"turn_id": "turn-001", "assistant_message": "wrong"}],
                }
            ]
        }
        with self.assertRaisesRegex(ValueError, "review completed discovery turns"):
            assert_discovery_reviewed(payload, {"general-agent"})

        review_discovery_turn(
            payload,
            profile_id="general-agent",
            turn_id="turn-001",
            disposition="rejected_answer",
            findings=["answered the prior turn"],
            next_action="correct the stale response before adding more facts",
        )
        assert_discovery_reviewed(payload, {"general-agent"})
        review = payload["sessions"][0]["turns"][0]["human_review"]
        self.assertEqual(review["disposition"], "rejected_answer")
        self.assertFalse(review["eligible_as_gold"])

    def test_adaptive_turn_plan_must_link_to_latest_review_and_pass_semantic_checks(self) -> None:
        prior = {
            "sessions": [
                {
                    "profile_id": "general-agent",
                    "turns": [
                        {
                            "turn_id": "turn-001",
                            "assistant_message": "wrong",
                            "human_review": {
                                "reviewed_at": "2026-08-27T00:00:00Z",
                                "disposition": "rejected_answer",
                            },
                        }
                    ],
                }
            ]
        }
        plan = {
            "schema_version": 1,
            "profile_id": "general-agent",
            "parent_turn_id": "turn-001",
            "review_basis": {
                "previous_disposition": "rejected_answer",
                "strategy": "correct_and_continue",
                "reason": "correct the observed stale answer before adding facts",
            },
            "semantic_checks": {
                "checked_by": "codex",
                "previous_answer_manually_reviewed": True,
                "current_prompt_matches_review_next_action": True,
                "does_not_assume_unverified_assistant_claims": True,
                "authoritative_user_state_reconciled": True,
            },
            "turn": {
                "turn_id": "turn-002",
                "purpose": "resynchronize",
                "user_message": "Please answer the current request and use these authoritative facts.",
            },
        }
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "next.json"
            target.write_text(json.dumps(plan), encoding="utf-8")
            loaded = load_adaptive_turn_plan(target, prior, profile_id="general-agent")

        self.assertEqual(loaded["turn"]["turn_id"], "turn-002")
        self.assertEqual(loaded["adaptive_parent"]["strategy"], "correct_and_continue")

    def test_rejected_answer_cannot_blindly_continue(self) -> None:
        prior = {
            "sessions": [
                {
                    "profile_id": "general-agent",
                    "turns": [
                        {
                            "turn_id": "turn-001",
                            "human_review": {
                                "reviewed_at": "now",
                                "disposition": "rejected_answer",
                            },
                        }
                    ],
                }
            ]
        }
        plan = {
            "schema_version": 1,
            "profile_id": "general-agent",
            "parent_turn_id": "turn-001",
            "review_basis": {
                "previous_disposition": "rejected_answer",
                "strategy": "continue",
                "reason": "bad",
            },
            "semantic_checks": {
                "checked_by": "codex",
                "previous_answer_manually_reviewed": True,
                "current_prompt_matches_review_next_action": True,
                "does_not_assume_unverified_assistant_claims": True,
                "authoritative_user_state_reconciled": True,
            },
            "turn": {
                "turn_id": "turn-002",
                "purpose": "bad continuation",
                "user_message": "next",
            },
        }
        with tempfile.TemporaryDirectory() as directory:
            target = Path(directory) / "next.json"
            target.write_text(json.dumps(plan), encoding="utf-8")
            with self.assertRaisesRegex(ValueError, "must be corrected"):
                load_adaptive_turn_plan(target, prior, profile_id="general-agent")


if __name__ == "__main__":
    unittest.main()
