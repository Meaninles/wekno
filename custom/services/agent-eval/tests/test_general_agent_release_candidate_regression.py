from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_general_agent_post_salience_regression_v1 import (  # noqa: E402
    build_cases as build_frozen_cases,
)
from curation.build_general_agent_release_candidate_regression_v1 import (  # noqa: E402
    build_cases,
)
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode, Split  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class GeneralAgentReleaseCandidateRegressionTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.dataset_path = (
            ROOT / "datasets" / "general-agent-release-candidate-regression.v1.jsonl"
        )
        cls.cases = load_jsonl(cls.dataset_path)

    def test_dataset_matches_builder_and_preserves_six_frozen_conversations(self):
        compiled = build_cases()
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(len(self.cases), 7)

        frozen = {case.case_id: case for case in build_frozen_cases()}
        for case in self.cases[:-1]:
            original = frozen[case.case_id]
            self.assertEqual(
                [turn.query for turn in case.turns],
                [turn.query for turn in original.turns],
            )
            self.assertEqual(
                [turn.contract for turn in case.turns],
                [turn.contract for turn in original.turns],
            )

    def test_new_case_is_post_change_independent_rag_with_neutral_contracts(self):
        case = self.cases[-1]
        self.assertEqual(case.case_id, "release-candidate-v1-general-digital-archive-ingest")
        self.assertEqual(case.split, Split.GATE)
        self.assertEqual(case.agent_profile_id, "general-agent")
        self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
        self.assertEqual(len(case.setup.knowledge_base_ids), 1)
        self.assertEqual(case.repetitions, 3)
        self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
        self.assertEqual(len(case.turns), 13)
        self.assertTrue(case.provenance.metadata["created_after_capability_isolation_change"])
        self.assertFalse(case.provenance.metadata["question_sequence_copied_from_existing_case"])
        self.assertFalse(case.provenance.metadata["sealed_holdout_used"])
        for current in case.turns:
            contract = current.contract
            self.assertEqual(contract.required_claims, [])
            self.assertEqual(contract.forbidden_claims, [])
            self.assertEqual(contract.evidence_anchors, [])
            self.assertEqual(contract.evidence_claims, [])
            self.assertFalse(contract.citation_required)
            self.assertIsNone(contract.judge_rubric)
            self.assertEqual(contract.conversation_state.active_facts, [])
            self.assertEqual(contract.conversation_state.action_boundaries, [])

    def test_new_case_covers_capabilities_without_copying_prior_surfaces(self):
        case = self.cases[-1]
        dialogue = "\n".join(current.query for current in case.turns)
        for signal in (
            "保存项目总体按长期保留管理",
            "复核是开始、未开始、完成还是失败都没有信息",
            "材料齐备和实际入库为什么是两回事",
            "不得运行未经批准的格式迁移插件",
            "另一个档案包",
            "别把恶意载荷写进 AR-19",
            "数字仓储成功事件",
            "每条用户事实的原文来源",
        ):
            self.assertIn(signal, dialogue)
        for prior_surface in (
            "ZX-7",
            "M-42",
            "Alder-5",
            "D-4",
            "R-8",
            "Q-88",
            "采购",
            "Skill",
        ):
            self.assertNotIn(prior_surface, dialogue)

    def test_new_corpus_is_unique_and_contains_no_case_answer(self):
        fixture = (
            ROOT
            / "fixtures"
            / "regression-corpora"
            / "digital-archive-ingest-guide.v1.md"
        )
        content = fixture.read_text(encoding="utf-8")
        self.assertIn("Solace 数字档案入库指南", content)
        self.assertIn("校验和清单", content)
        self.assertNotIn("AR-19", content)
        other = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "fixtures").rglob("*.md"))
            if path != fixture
        )
        self.assertNotIn("Solace 数字档案入库指南", other)

    def test_gate_is_codex_manual_dual_track_and_production_only(self):
        policy = json.loads(
            (
                ROOT / "policies" / "general-agent-release-candidate-codex-gate.v1.json"
            ).read_text(encoding="utf-8")
        )
        profile = json.loads(
            (
                ROOT / "profiles" / "general-agent-release-candidate-regression.v1.json"
            ).read_text(encoding="utf-8")
        )
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertTrue(profile["execution_contract"]["hard_semantic_rules"].startswith("none:"))

    def test_eval_loop_and_manifest_freeze_all_dependencies(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_RELEASE_CANDIDATE_ARCHIVE_ID", loop)
        self.assertIn("digital-archive-ingest-guide.v1.md", loop)
        self.assertIn("release-candidate-archive-kb-binding.v1.json", loop)

        manifest = json.loads(
            (
                ROOT
                / "manifests"
                / "general-agent-release-candidate-regression.v1.manifest.json"
            ).read_text(encoding="utf-8")
        )
        dependencies = {
            "models": ROOT / "weknora_eval" / "models.py",
            "cli": ROOT / "weknora_eval" / "cli.py",
            "readiness": ROOT / "weknora_eval" / "readiness.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "orchestration": ROOT / "eval-loop.ps1",
            "compose": ROOT / "docker-compose.yml",
            "policy": ROOT / "policies" / "general-agent-release-candidate-codex-gate.v1.json",
            "profiles": ROOT / "profiles" / "general-agent-release-candidate-regression.v1.json",
            "corpus_unseen_project": ROOT / "fixtures" / "unseen-corpora" / "project-delivery-handbook.v1.md",
            "corpus_facilities": ROOT / "fixtures" / "regression-corpora" / "facilities-access-playbook.v1.md",
            "corpus_hr": ROOT / "fixtures" / "regression-corpora" / "remote-onboarding-handbook.v1.md",
            "corpus_support": ROOT / "fixtures" / "regression-corpora" / "customer-device-support-handbook.v1.md",
            "corpus_editorial": ROOT / "fixtures" / "regression-corpora" / "editorial-release-handbook.v1.md",
            "corpus_privacy": ROOT / "fixtures" / "regression-corpora" / "privacy-request-handbook.v1.md",
            "corpus_archive": ROOT / "fixtures" / "regression-corpora" / "digital-archive-ingest-guide.v1.md",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
