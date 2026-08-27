from __future__ import annotations

import os
import unittest
from collections import Counter, defaultdict
from pathlib import Path
from unittest.mock import patch

from curation.build_multiturn_ready_v1 import build_cases
from weknora_eval.calibration import load_calibration
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.langfuse_store import publish_dataset
from weknora_eval.models import Capability, SUTFingerprint, Split
from weknora_eval.readiness import evaluate_readiness


ROOT = Path(__file__).resolve().parents[1]


def eval_sut() -> SUTFingerprint:
    return SUTFingerprint(
        mode="eval",
        release="test",
        commit="test",
        environment="agent-eval",
        capabilities=[
            Capability.RAG_RETRIEVAL.value,
            Capability.DOCUMENT_PROCESSING.value,
            Capability.LONG_CONTEXT_DIALOGUE.value,
            Capability.TOOL_USE.value,
            Capability.CITATION.value,
        ],
        raw={"recorder_enabled": True, "capture_policy": "full"},
    )


class ReadinessTests(unittest.TestCase):
    def env(self) -> dict[str, str]:
        return {
            "AGENT_EVAL_SUMMARY_MODEL_ID": "prod-deepseek-v4-flash-int8-chat",
            "AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID": "knowledge-id",
            "AGENT_EVAL_CORPUS_VERSION": "sha256:corpus",
        }

    def test_committed_ready_dataset_matches_compiler(self) -> None:
        committed = load_jsonl(ROOT / "datasets" / "multiturn-ready.v1.jsonl")
        compiled = build_cases()
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])

    def test_gate_matrix_is_new_repeated_and_overflows_each_history_window(self) -> None:
        cases = [case for case in build_cases() if case.split == Split.GATE]
        self.assertEqual(len(cases), 6)
        self.assertTrue(all(case.repetitions == 3 for case in cases))
        self.assertEqual(
            Counter(case.agent_profile_id for case in cases),
            Counter({"quick-answer": 2, "rag-reasoning": 2, "general-agent": 2}),
        )
        families: dict[str, set[str | None]] = defaultdict(set)
        for case in cases:
            families[case.family_id].add(case.agent_profile_id)
        self.assertEqual(len(families), 2)
        self.assertTrue(
            all(value == {"quick-answer", "rag-reasoning", "general-agent"} for value in families.values())
        )
        long_cases = {
            case.agent_profile_id: case
            for case in cases
            if Capability.LONG_CONTEXT_DIALOGUE in case.capabilities
        }
        self.assertEqual(len(long_cases["quick-answer"].turns), 12)
        self.assertEqual(len(long_cases["rag-reasoning"].turns), 12)
        self.assertEqual(len(long_cases["general-agent"].turns), 12)

    def test_preflight_is_read_only_and_ready_for_gate(self) -> None:
        with patch.dict(os.environ, self.env(), clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v1.jsonl",
                manifest_path=ROOT / "manifests" / "multiturn-ready.v1.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "READY")
        self.assertFalse(report["formal_eval_executed"])
        self.assertEqual(report["case_count"], 6)
        self.assertEqual(report["planned_session_count"], 18)
        self.assertTrue(all(check["passed"] for check in report["checks"]))

    def test_preflight_fails_closed_on_model_drift(self) -> None:
        env = self.env()
        env["AGENT_EVAL_SUMMARY_MODEL_ID"] = "wrong-model"
        with patch.dict(os.environ, env, clear=False):
            report = evaluate_readiness(
                dataset_path=ROOT / "datasets" / "multiturn-ready.v1.jsonl",
                manifest_path=ROOT / "manifests" / "multiturn-ready.v1.manifest.json",
                profile_path=ROOT / "profiles" / "multiturn-agents.v1.json",
                policy_path=ROOT / "policies" / "multiturn-release-gate.v1.json",
                calibration_path=ROOT / "calibration" / "judge-multiturn.v1.json",
                split=Split.GATE,
                sut=eval_sut(),
            )
        self.assertEqual(report["status"], "NOT_READY")
        failed = {check["name"] for check in report["checks"] if not check["passed"]}
        self.assertIn("model_binding", failed)

    def test_judge_calibration_has_all_verdict_and_boundary_classes(self) -> None:
        suite = load_calibration(ROOT / "calibration" / "judge-multiturn.v1.json")
        self.assertEqual(len(suite.items), 8)
        self.assertEqual({item.expected_label for item in suite.items}, {"pass", "fail", "invalid"})
        self.assertEqual({item.boundary_kind for item in suite.items}, {"positive", "negative", "boundary"})

    def test_langfuse_publishes_each_repetition_as_an_independent_item(self) -> None:
        class FakeLangfuse:
            def __init__(self) -> None:
                self.items: list[dict] = []

            def create_dataset(self, **_: object) -> None:
                return None

            def create_dataset_item(self, **kwargs: object) -> None:
                self.items.append(kwargs)

            def flush(self) -> None:
                return None

        case = next(case for case in build_cases() if case.split == Split.GATE)
        fake = FakeLangfuse()
        with patch("weknora_eval.langfuse_store._client", return_value=fake):
            publish_dataset([case], "test/repeated")
        self.assertEqual(len(fake.items), 3)
        self.assertEqual(
            [item["input"]["attempt_index"] for item in fake.items],
            [1, 2, 3],
        )


if __name__ == "__main__":
    unittest.main()
