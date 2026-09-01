from __future__ import annotations

import json
import unittest
from pathlib import Path

from curation.build_fresh_generalization_regression_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode, Split
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]
PROFILES = {"quick-answer", "rag-reasoning", "general-agent"}


class FreshGeneralizationRegressionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.path = ROOT / "datasets" / "fresh-generalization-regression.v1.jsonl"
        self.cases = load_jsonl(self.path)

    def test_committed_suite_matches_post_change_compiler(self) -> None:
        compiled = build_cases()
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(len(self.cases), 4)
        self.assertEqual({case.split for case in self.cases}, {Split.DEV})
        self.assertEqual({case.agent_profile_id for case in self.cases}, PROFILES)
        self.assertTrue(
            all(case.provenance.metadata["sealed_holdout_used"] is False for case in self.cases)
        )
        self.assertTrue(
            all(
                case.provenance.metadata["created_after_terminal_projection_change"] is True
                for case in self.cases
            )
        )

    def test_every_case_is_long_repeated_and_codex_reviewed(self) -> None:
        for case in self.cases:
            self.assertEqual(len(case.turns), 13)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])

    def test_knowledge_selection_is_unambiguous_and_covers_all_no_kb_profiles(self) -> None:
        self.assertNotIn(
            KnowledgeSelectionMode.AGENT_DEFAULT,
            {case.setup.knowledge_selection_mode for case in self.cases},
        )
        no_kb = [
            case
            for case in self.cases
            if case.setup.knowledge_selection_mode == KnowledgeSelectionMode.NONE
        ]
        selected = [
            case
            for case in self.cases
            if case.setup.knowledge_selection_mode == KnowledgeSelectionMode.EXPLICIT
        ]
        self.assertEqual({case.agent_profile_id for case in no_kb}, PROFILES)
        self.assertEqual(len(selected), 1)
        self.assertEqual(selected[0].agent_profile_id, "quick-answer")
        for case in no_kb:
            self.assertEqual(case.setup.knowledge_base_ids, [])
            self.assertIn("_NO_KB_ID}", case.agent.agent_id)
        self.assertEqual(
            selected[0].setup.knowledge_base_ids,
            ["${AGENT_EVAL_KB_FRESH_GENERALIZATION_MEDIA_ID}"],
        )

    def test_codex_is_the_only_semantic_quality_decider(self) -> None:
        for case in self.cases:
            for turn in case.turns:
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

    def test_new_cases_are_not_legacy_scenario_substitutions(self) -> None:
        prompts = "\n".join(turn.query for case in self.cases for turn in case.turns)
        for expected in (
            "Ripple-6",
            "社区菜园",
            "访谈研究",
            "展览彩排",
            "逐字摘录用户原话",
            "写内容是允许的",
            "不代表灯已经开过",
        ):
            self.assertIn(expected, prompts)
        for legacy_surface in (
            "采购文档",
            "培训说明",
            "Meridian",
            "紫色双闪",
            "Orion",
            "Northstar",
            "P-17",
            "R-8",
        ):
            self.assertNotIn(legacy_surface, prompts)

    def test_new_corpus_is_disjoint_from_existing_regression_corpora(self) -> None:
        fixture = ROOT / "fixtures" / "regression-corpora" / "audio-release-handbook.v1.md"
        content = fixture.read_text(encoding="utf-8")
        self.assertIn("Northbank", content)
        self.assertIn("原始人声轨", content)
        other = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "fixtures" / "regression-corpora").glob("*.md"))
            if path != fixture
        )
        self.assertNotIn("Northbank", other)
        self.assertNotIn("原始人声轨", other)

    def test_gate_scores_only_production_candidate_with_codex_threshold(self) -> None:
        policy_path = ROOT / "policies" / "fresh-generalization-regression-gate.v1.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["gate_kind"], "production_release")
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertFalse(policy["require_baseline"])
        self.assertAlmostEqual(policy["min_pass_rate_per_case"], 0.666666)

    def test_manifest_freezes_new_regression_dependencies(self) -> None:
        manifest_path = ROOT / "manifests" / "fresh-generalization-regression.v1-production-gate.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after the final evaluator dependency edit")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "fresh-generalization-regression.v1.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": ROOT / "policies" / "fresh-generalization-regression-gate.v1.json",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependency_paths.items())},
        )


if __name__ == "__main__":
    unittest.main()
