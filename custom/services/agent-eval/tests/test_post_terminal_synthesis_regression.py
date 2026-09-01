from __future__ import annotations

import json
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_post_terminal_synthesis_regression_v1 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode, Split  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class PostTerminalSynthesisRegressionTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.dataset_path = (
            ROOT / "datasets" / "post-terminal-synthesis-regression.v1.jsonl"
        )
        cls.cases = load_jsonl(cls.dataset_path)

    def test_committed_case_matches_builder_and_is_independent_rag(self):
        compiled = build_cases()
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(len(self.cases), 1)
        case = self.cases[0]
        self.assertEqual(case.split, Split.GATE)
        self.assertEqual(case.agent_profile_id, "general-agent")
        self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
        self.assertEqual(len(case.setup.knowledge_base_ids), 1)
        self.assertEqual(case.repetitions, 3)
        self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
        self.assertEqual(len(case.turns), 13)
        self.assertTrue(
            case.provenance.metadata["created_after_terminal_task_salience_change"]
        )
        self.assertFalse(case.provenance.metadata["question_sequence_copied_from_existing_case"])
        self.assertFalse(case.provenance.metadata["sealed_holdout_used"])

    def test_quality_is_only_whole_conversation_codex_judgment(self):
        for current in self.cases[0].turns:
            contract = current.contract
            self.assertEqual(contract.required_claims, [])
            self.assertEqual(contract.forbidden_claims, [])
            self.assertEqual(contract.evidence_anchors, [])
            self.assertEqual(contract.evidence_claims, [])
            self.assertFalse(contract.citation_required)
            self.assertIsNone(contract.judge_rubric)
            self.assertEqual(contract.conversation_state.active_facts, [])
            self.assertEqual(contract.conversation_state.retired_facts, [])
            self.assertEqual(contract.conversation_state.unknown_facts, [])
            self.assertEqual(contract.conversation_state.action_boundaries, [])

    def test_case_exercises_current_task_after_expired_narrow_format(self):
        case = self.cases[0]
        dialogue = "\n".join(current.query for current in case.turns)
        self.assertIn("domain:manufacturing-quality", case.tags)
        self.assertIn("Reply in English with exactly five bullets", case.turns[-2].query)
        self.assertIn("回到中文，给 Q-88 做完整交接", case.turns[-1].query)
        self.assertIn("不要只回答某一条制度", case.turns[-1].query)
        for capability_signal in (
            "质量项目总体覆盖欧盟和加拿大",
            "角色更新没有说明取样开始",
            "包装校样被接受为什么仍不能直接说批次已经放行",
            "Line-B 作废",
            "缺一项推成已放行、未放行、拒绝或报废",
            "另一个批次",
            "不要把这个假设记进 Q-88",
            "批准只说明方案获批",
            "直接给我这句文字",
            "不能自动写成‘肯定还没放’",
        ):
            self.assertIn(capability_signal, dialogue)
        for prior_case_surface in (
            "PR-17",
            "M-42",
            "TR-9",
            "Atlas",
            "采购文档",
            "Skill",
        ):
            self.assertNotIn(prior_case_surface, dialogue)

    def test_corpus_is_new_and_not_a_dataset_answer_key(self):
        fixture = (
            ROOT
            / "fixtures"
            / "regression-corpora"
            / "quality-batch-release-manual.v1.md"
        )
        content = fixture.read_text(encoding="utf-8")
        self.assertIn("Orion 批次放行手册", content)
        self.assertIn("包装校样被接受只说明标签和版面检查获得认可", content)
        self.assertNotIn("Q-88", content)
        other = "\n".join(
            path.read_text(encoding="utf-8")
            for path in sorted((ROOT / "fixtures").rglob("*.md"))
            if path != fixture
        )
        self.assertNotIn("Orion 批次放行手册", other)

    def test_gate_is_manual_dual_track_and_production_only(self):
        policy = json.loads(
            (
                ROOT
                / "policies"
                / "post-terminal-synthesis-codex-gate.v1.json"
            ).read_text(encoding="utf-8")
        )
        profile = json.loads(
            (
                ROOT
                / "profiles"
                / "post-terminal-synthesis-regression.v1.json"
            ).read_text(encoding="utf-8")
        )
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(policy["required_agent_profiles"], ["general-agent"])
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertTrue(profile["execution_contract"]["hard_semantic_rules"].startswith("none:"))

    def test_eval_loop_and_manifest_freeze_the_new_corpus(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_POST_TERMINAL_QUALITY_ID", loop)
        self.assertIn("quality-batch-release-manual.v1.md", loop)
        self.assertIn("post-terminal-quality-kb-binding.v1.json", loop)

        manifest = json.loads(
            (
                ROOT
                / "manifests"
                / "post-terminal-synthesis-regression.v1.manifest.json"
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
            "orchestration": ROOT / "eval-loop.ps1",
            "compose": ROOT / "docker-compose.yml",
            "policy": ROOT / "policies" / "post-terminal-synthesis-codex-gate.v1.json",
            "profiles": ROOT / "profiles" / "post-terminal-synthesis-regression.v1.json",
            "corpus_quality": (
                ROOT
                / "fixtures"
                / "regression-corpora"
                / "quality-batch-release-manual.v1.md"
            ),
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
