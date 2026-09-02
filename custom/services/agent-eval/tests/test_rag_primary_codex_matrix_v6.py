import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v6 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixV6Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v6.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.cases))

    def test_retains_v5_and_adds_one_shot_three_profile_regression(self):
        self.assertEqual(len(self.cases), 16)
        self.assertEqual(sum(case.repetitions for case in self.cases), 42)
        self.assertEqual(sum(len(case.turns) * case.repetitions for case in self.cases), 531)
        additions = [
            case for case in self.cases
            if case.provenance.metadata.get("created_after_generic_prompt_revision")
        ]
        self.assertEqual(len(additions), 3)
        self.assertEqual(Counter(case.agent_profile_id for case in additions), Counter({
            "quick-answer": 1, "rag-reasoning": 1, "general-agent": 1,
        }))
        self.assertTrue(all(case.repetitions == 1 for case in additions))
        self.assertEqual(len({tuple(item.query for item in case.turns) for case in additions}), 1)
        self.assertEqual(len({case.setup.knowledge_base_ids[0] for case in additions}), 1)

    def test_all_cases_use_explicit_rag_and_neutral_manual_review(self):
        for case in self.cases:
            self.assertEqual(case.suite, "weknora-rag-primary-codex-matrix-v6")
            self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
            for current in case.turns:
                self.assertEqual(current.contract.required_claims, [])
                self.assertEqual(current.contract.forbidden_claims, [])
                self.assertEqual(current.contract.evidence_anchors, [])
                self.assertEqual(current.contract.evidence_claims, [])
                self.assertFalse(current.contract.citation_required)
                self.assertIsNone(current.contract.judge_rubric)

    def test_one_shot_cases_cannot_feed_subsequent_tuning(self):
        additions = [
            case for case in self.cases
            if case.provenance.metadata.get("created_after_generic_prompt_revision")
        ]
        for case in additions:
            self.assertEqual(len(case.turns), 13)
            self.assertFalse(case.provenance.metadata["used_for_subsequent_tuning"])
            self.assertFalse(case.provenance.metadata["question_sequence_copied_from_existing_case"])
            self.assertFalse(case.provenance.metadata["sealed_holdout_used"])
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])
            self.assertFalse(case.provenance.metadata["reference_answers_used"])
            self.assertFalse(case.provenance.metadata["judge_feedback_used"])

    def test_policy_and_profile_keep_codex_manual_release_contract(self):
        policy = json.loads((ROOT / "policies" / "rag-primary-codex-release-gate.v6.json").read_text(encoding="utf-8"))
        profile = json.loads((ROOT / "profiles" / "rag-primary-codex-matrix.v6.json").read_text(encoding="utf-8"))
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertFalse(policy["forbid_hard_failures"])
        self.assertEqual(policy["critical_metric_prefixes"], [])
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertIn("corpus_observatory", policy["required_frozen_dependencies"])
        self.assertTrue(profile["execution_contract"]["hard_semantic_rules"].startswith("none:"))
        self.assertIn("one-shot", profile["execution_contract"]["decision_policy"])

    def test_eval_loop_prepares_observatory_kb_from_dataset_marker(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_RAG_PRIMARY_OBSERVATORY_ID", loop)
        self.assertIn("observatory-night-operations-guide.v1.md", loop)
        self.assertIn("rag-primary-observatory-kb-binding.v1.json", loop)

    def test_manifest_freezes_v6_dependencies(self):
        manifest = json.loads((ROOT / "manifests" / "rag-primary-codex-matrix.v6.manifest.json").read_text(encoding="utf-8"))
        self.assertEqual(
            manifest["dependency_files"],
            {
                "corpus_editorial": "fixtures/regression-corpora/editorial-release-handbook.v1.md",
                "corpus_observatory": "fixtures/regression-corpora/observatory-night-operations-guide.v1.md",
                "corpus_privacy": "fixtures/regression-corpora/privacy-request-handbook.v1.md",
                "corpus_support": "fixtures/regression-corpora/customer-device-support-handbook.v1.md",
                "corpus_water": "fixtures/regression-corpora/water-quality-incident-guide.v1.md",
            },
        )
        dependencies = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v6.json",
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
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v6.json",
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
            "corpus_observatory": ROOT / "fixtures" / "regression-corpora" / "observatory-night-operations-guide.v1.md",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(manifest["dependency_sha256"], {name: file_sha256(path) for name, path in sorted(dependencies.items())})


if __name__ == "__main__":
    unittest.main()
