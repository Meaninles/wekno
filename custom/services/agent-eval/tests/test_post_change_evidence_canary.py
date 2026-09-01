from __future__ import annotations

import json
import unittest
from pathlib import Path

from curation.build_post_change_evidence_canary_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode, Split
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]


class PostChangeEvidenceCanaryTests(unittest.TestCase):
    def setUp(self) -> None:
        self.dataset_path = ROOT / "datasets" / "post-change-evidence-canary.v1.jsonl"
        self.cases = load_jsonl(self.dataset_path)

    def test_committed_case_matches_the_post_change_compiler(self) -> None:
        compiled = build_cases()
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(len(self.cases), 1)
        case = self.cases[0]
        self.assertEqual(case.split, Split.DEV)
        self.assertEqual(case.agent_profile_id, "quick-answer")
        self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
        self.assertEqual(case.repetitions, 3)
        self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
        self.assertEqual(len(case.turns), 10)
        self.assertTrue(case.provenance.metadata["created_after_evidence_query_change"])
        self.assertFalse(case.provenance.metadata["sealed_holdout_used"])

    def test_semantic_quality_is_not_encoded_as_phrase_matching(self) -> None:
        for turn in self.cases[0].turns:
            contract = turn.contract
            self.assertEqual(contract.required_claims, [])
            self.assertEqual(contract.forbidden_claims, [])
            self.assertEqual(contract.evidence_anchors, [])
            self.assertEqual(contract.conversation_state.active_facts, [])
            self.assertEqual(contract.conversation_state.retired_facts, [])
            self.assertEqual(contract.conversation_state.unknown_facts, [])
            self.assertEqual(contract.conversation_state.action_boundaries, [])
            self.assertIsNone(contract.judge_rubric)
            self.assertIsNone(contract.max_response_chars)

    def test_case_is_a_new_capability_sequence_and_corpus(self) -> None:
        prompts = "\n".join(turn.query for turn in self.cases[0].turns)
        for expected in ("Tern-42", "防拆封条", "冷库C", "异常记录数是0", "exactly one English"):
            self.assertIn(expected, prompts)
        for previous_surface in ("Ripple-6", "Willow", "Kite", "Ember", "采购文档", "Skill"):
            self.assertNotIn(previous_surface, prompts)
        fixture = ROOT / "fixtures" / "regression-corpora" / "lab-sample-handoff.v1.md"
        content = fixture.read_text(encoding="utf-8")
        self.assertIn("Helix", content)
        self.assertIn("LIMS", content)
        other = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "fixtures").rglob("*.md"))
            if path != fixture
        )
        self.assertNotIn("Tern-42", other)
        self.assertNotIn("Helix 实验室样本交接规程", other)

    def test_canary_gate_uses_only_the_production_candidate(self) -> None:
        policy_path = ROOT / "policies" / "post-change-evidence-canary-gate.v1.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(policy["required_agent_profiles"], ["quick-answer"])
        self.assertTrue(policy["require_codex_review"])
        self.assertFalse(policy["require_baseline"])

    def test_manifest_freezes_dataset_and_evaluator_dependencies(self) -> None:
        manifest_path = ROOT / "manifests" / "post-change-evidence-canary.v1.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after the dataset is compiled")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "post-change-evidence-canary.v1.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": ROOT / "policies" / "post-change-evidence-canary-gate.v1.json",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependency_paths.items())},
        )


if __name__ == "__main__":
    unittest.main()
