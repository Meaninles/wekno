from __future__ import annotations

import json
import unittest
from pathlib import Path

from curation.build_semantic_routing_regression_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import Capability, ReviewMode, Split
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]
PROFILES = {"quick-answer", "rag-reasoning", "general-agent"}


class SemanticRoutingRegressionTests(unittest.TestCase):
    def setUp(self) -> None:
        self.path = ROOT / "datasets" / "semantic-routing-regression.v1.jsonl"
        self.cases = load_jsonl(self.path)

    def test_committed_suite_matches_compiler_and_is_independent(self) -> None:
        compiled = build_cases()
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(len(self.cases), 3)
        self.assertEqual({case.split for case in self.cases}, {Split.DEV})
        self.assertEqual({case.agent_profile_id for case in self.cases}, PROFILES)
        self.assertTrue(
            all(case.provenance.metadata["sealed_holdout_used"] is False for case in self.cases)
        )
        self.assertTrue(
            all(
                case.provenance.metadata["independent_from_existing_dev_gate_wording"] is True
                for case in self.cases
            )
        )

    def test_every_agent_runs_three_repeated_long_conversations(self) -> None:
        for case in self.cases:
            self.assertEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertIn(Capability.LONG_CONTEXT_DIALOGUE, case.capabilities)
            self.assertIn(Capability.TOOL_USE, case.capabilities)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])

    def test_counterfactuals_cover_both_sides_of_semantic_routing(self) -> None:
        prompts = "\n".join(turn.query for case in self.cases for turn in case.turns)
        for phrase in (
            "‘不能’不是禁止检索",
            "仍要检索并引用",
            "不要查资料",
            "不要创建文件",
            "不要写文件",
            "不是一个已经发生的业务状态",
            "只依据用户消息，不要检索",
            "只输出聊天文本",
        ):
            self.assertIn(phrase, prompts)
        for legacy_surface in ("采购文档", "培训说明", "Orion", "Northstar", "P-17", "R-8"):
            self.assertNotIn(legacy_surface, prompts)

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

    def test_dedicated_fixture_does_not_reuse_main_matrix_corpora(self) -> None:
        fixture = ROOT / "fixtures" / "regression-corpora" / "facilities-access-playbook.v1.md"
        content = fixture.read_text(encoding="utf-8")
        self.assertIn("Meridian", content)
        self.assertIn("紫色双闪", content)
        main_corpus = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "fixtures" / "unseen-corpora").glob("*.md"))
        )
        self.assertNotIn("Meridian", main_corpus)
        self.assertNotIn("紫色双闪", main_corpus)

    def test_regression_policy_scores_only_production_candidate(self) -> None:
        policy_path = ROOT / "policies" / "semantic-routing-regression-gate.v1.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["gate_kind"], "production_release")
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertFalse(policy["require_baseline"])
        self.assertAlmostEqual(policy["min_pass_rate_per_case"], 0.666666)

    def test_manifest_freezes_regression_and_codex_dependencies(self) -> None:
        manifest_path = ROOT / "manifests" / "semantic-routing-regression.v1-production-gate.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after the final evaluator dependency edit")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "semantic-routing-regression.v1.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": ROOT / "policies" / "semantic-routing-regression-gate.v1.json",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependency_paths.items())},
        )


if __name__ == "__main__":
    unittest.main()
