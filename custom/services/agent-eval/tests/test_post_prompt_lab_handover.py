import json
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_post_prompt_lab_handover_v1 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class PostPromptLabHandoverTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.case = build_cases()[0]
        cls.dataset_path = ROOT / "datasets" / "post-prompt-lab-handover.v1.jsonl"
        cls.committed_cases = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed_cases), [])
        self.assertEqual(
            dataset_sha256(self.committed_cases),
            dataset_sha256([self.case]),
        )

    def test_is_new_long_context_codex_review_case(self):
        self.assertEqual(len(self.case.turns), 14)
        self.assertEqual(self.case.repetitions, 2)
        self.assertEqual(self.case.review_mode, ReviewMode.CODEX_CONVERSATION)
        self.assertEqual(self.case.provenance.metadata["configured_history_turns"], 10)
        self.assertTrue(self.case.provenance.metadata["created_after_prompt_change"])
        self.assertEqual(self.case.provenance.reference_answers, {})

    def test_uses_real_no_kb_runtime_boundary(self):
        self.assertEqual(
            self.case.setup.knowledge_selection_mode,
            KnowledgeSelectionMode.NONE,
        )
        self.assertEqual(self.case.setup.knowledge_base_ids, [])
        self.assertIn("_NO_KB_ID}", self.case.agent.agent_id)

    def test_composes_distinct_adversarial_capabilities(self):
        dialogue = "\n".join(turn.query for turn in self.case.turns)
        for signal in (
            "同一个未决状态",
            "不是 Atlas 已签字",
            "聊天里的交接说明",
            "Answer in English",
            "checksum",
            "若没有可靠 source_id",
        ):
            self.assertIn(signal, dialogue)
        self.assertNotIn("采购", dialogue)
        self.assertNotIn("培训", dialogue)

    def test_semantic_quality_is_left_to_whole_conversation_codex_review(self):
        for current_turn in self.case.turns:
            contract = current_turn.contract
            self.assertEqual(contract.required_claims, [])
            self.assertEqual(contract.forbidden_claims, [])
            self.assertEqual(contract.evidence_anchors, [])
            self.assertIsNone(contract.judge_rubric)

    def test_gate_scores_only_production_candidate(self):
        policy_path = ROOT / "policies" / "post-prompt-lab-handover-gate.v1.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(policy["required_agent_profiles"], ["rag-reasoning"])
        self.assertEqual(policy["min_repetitions_per_case"], 2)
        self.assertEqual(policy["min_pass_rate_per_case"], 1.0)
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertFalse(policy["require_baseline"])

    def test_profile_resolves_none_to_the_isolated_rag_clone(self):
        profile_path = ROOT / "profiles" / "post-prompt-lab-handover.v1.json"
        profile = json.loads(profile_path.read_text(encoding="utf-8"))
        rag = profile["profiles"][0]
        self.assertEqual(rag["profile_id"], "rag-reasoning")
        self.assertEqual(
            rag["agent_ids_by_knowledge_selection"]["none"],
            "${AGENT_EVAL_AGENT_RAG_NO_KB_ID}",
        )

    def test_manifest_freezes_dataset_and_evaluator_dependencies(self):
        manifest_path = ROOT / "manifests" / "post-prompt-lab-handover.v1.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after the evaluator files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "post-prompt-lab-handover.v1.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": ROOT / "policies" / "post-prompt-lab-handover-gate.v1.json",
        }
        self.assertEqual(
            manifest["dataset_sha256"],
            dataset_sha256(self.committed_cases),
        )
        self.assertEqual(
            manifest["dependency_sha256"],
            {
                name: file_sha256(path)
                for name, path in sorted(dependency_paths.items())
            },
        )


if __name__ == "__main__":
    unittest.main()
