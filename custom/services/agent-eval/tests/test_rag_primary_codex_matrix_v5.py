import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v5 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixV5Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v5.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.cases))

    def test_retains_v4_and_adds_a_three_profile_unseen_comparison(self):
        self.assertEqual(len(self.cases), 13)
        self.assertEqual(sum(case.repetitions for case in self.cases), 39)
        self.assertEqual(sum(len(case.turns) * case.repetitions for case in self.cases), 492)
        self.assertEqual(
            Counter(case.agent_profile_id for case in self.cases),
            Counter({"general-agent": 7, "quick-answer": 3, "rag-reasoning": 3}),
        )
        additions = [
            case
            for case in self.cases
            if case.provenance.metadata.get("created_after_model_owned_tool_selection")
        ]
        self.assertEqual(len(additions), 3)
        self.assertEqual(
            {case.agent_profile_id for case in additions},
            {"quick-answer", "rag-reasoning", "general-agent"},
        )
        self.assertEqual(len({case.setup.knowledge_base_ids[0] for case in additions}), 1)
        self.assertEqual(len({tuple(item.query for item in case.turns) for case in additions}), 1)

    def test_every_case_is_explicit_repeated_long_context_and_semantically_neutral(self):
        for case in self.cases:
            self.assertEqual(case.suite, "weknora-rag-primary-codex-matrix-v5")
            self.assertEqual(case.corpus_version, "rag-primary-corpora-v5")
            self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
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

    def test_new_scenario_composes_capabilities_without_an_answer_key(self):
        additions = [
            case
            for case in self.cases
            if case.provenance.metadata.get("created_after_model_owned_tool_selection")
        ]
        for case in additions:
            self.assertEqual(len(case.turns), 13)
            self.assertFalse(case.provenance.metadata["question_sequence_copied_from_existing_case"])
            self.assertFalse(case.provenance.metadata["sealed_holdout_used"])
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])
            self.assertFalse(case.provenance.metadata["reference_answers_used"])
            self.assertFalse(case.provenance.metadata["judge_feedback_used"])
            self.assertIn("cross-profile-comparison", case.tags)
            self.assertIn("long-context", case.tags)

    def test_release_policy_is_manual_production_candidate_judgment(self):
        policy = json.loads(
            (ROOT / "policies" / "rag-primary-codex-release-gate.v5.json").read_text(
                encoding="utf-8"
            )
        )
        profile = json.loads(
            (ROOT / "profiles" / "rag-primary-codex-matrix.v5.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertFalse(policy["forbid_hard_failures"])
        self.assertEqual(policy["critical_metric_prefixes"], [])
        self.assertEqual(policy["max_metric_rate_regression"], {})
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertIn("corpus_water", policy["required_frozen_dependencies"])
        contract = profile["execution_contract"]
        self.assertIn("whole-conversation Codex verdict", contract["decision_policy"])
        self.assertIn("no individual answer is required to be perfect", contract["decision_policy"])
        self.assertTrue(contract["hard_semantic_rules"].startswith("none:"))
        self.assertIn("stable permission-scoped tool catalog", contract["tool_selection"])

    def test_eval_loop_prepares_new_kb_from_dataset_marker(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_RAG_PRIMARY_WATER_ID", loop)
        self.assertIn("water-quality-incident-guide.v1.md", loop)
        self.assertIn("rag-primary-water-kb-binding.v1.json", loop)

    def test_manifest_freezes_v5_corpora_and_evaluator(self):
        manifest_path = ROOT / "manifests" / "rag-primary-codex-matrix.v5.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after v5 files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependencies = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v5.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "cli": ROOT / "weknora_eval" / "cli.py",
            "readiness": ROOT / "weknora_eval" / "readiness.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v5.json",
            "orchestration": ROOT / "eval-loop.ps1",
            "compose": ROOT / "docker-compose.yml",
            "corpus_unseen_product": ROOT / "fixtures" / "unseen-corpora" / "product-orion-manual.v1.md",
            "corpus_unseen_project": ROOT / "fixtures" / "unseen-corpora" / "project-delivery-handbook.v1.md",
            "corpus_unseen_it": ROOT / "fixtures" / "unseen-corpora" / "it-operations-runbook.v1.md",
            "corpus_unseen_policy": ROOT / "fixtures" / "unseen-corpora" / "governance-expense-policy.v1.md",
            "corpus_facilities": ROOT / "fixtures" / "regression-corpora" / "facilities-access-playbook.v1.md",
            "corpus_lab": ROOT / "fixtures" / "regression-corpora" / "lab-sample-handoff.v1.md",
            "corpus_hr": ROOT / "fixtures" / "regression-corpora" / "remote-onboarding-handbook.v1.md",
            "corpus_support": ROOT / "fixtures" / "regression-corpora" / "customer-device-support-handbook.v1.md",
            "corpus_editorial": ROOT / "fixtures" / "regression-corpora" / "editorial-release-handbook.v1.md",
            "corpus_privacy": ROOT / "fixtures" / "regression-corpora" / "privacy-request-handbook.v1.md",
            "corpus_water": ROOT / "fixtures" / "regression-corpora" / "water-quality-incident-guide.v1.md",
        }
        self.assertEqual(
            manifest["dependency_files"],
            {
                "corpus_editorial": "fixtures/regression-corpora/editorial-release-handbook.v1.md",
                "corpus_privacy": "fixtures/regression-corpora/privacy-request-handbook.v1.md",
                "corpus_support": "fixtures/regression-corpora/customer-device-support-handbook.v1.md",
                "corpus_water": "fixtures/regression-corpora/water-quality-incident-guide.v1.md",
            },
        )
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
