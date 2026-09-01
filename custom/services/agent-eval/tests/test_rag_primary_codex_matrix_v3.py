import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v3 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixV3Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v3.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.cases))

    def test_keeps_v2_and_adds_one_independent_post_v6_case(self):
        self.assertEqual(len(self.cases), 9)
        self.assertEqual(sum(case.repetitions for case in self.cases), 27)
        self.assertEqual(sum(len(case.turns) * case.repetitions for case in self.cases), 336)
        self.assertEqual(
            Counter(case.agent_profile_id for case in self.cases),
            Counter({"general-agent": 5, "quick-answer": 2, "rag-reasoning": 2}),
        )
        new_cases = [
            case
            for case in self.cases
            if case.provenance.metadata.get("created_after_claim_ledger_prompt_change")
        ]
        self.assertEqual(
            [case.case_id for case in new_cases],
            ["rag-primary-v3-general-editorial-release"],
        )

    def test_all_cases_use_unique_explicit_kbs_and_codex_review(self):
        kb_ids = []
        for case in self.cases:
            self.assertEqual(case.suite, "weknora-rag-primary-codex-matrix-v3")
            self.assertEqual(case.corpus_version, "rag-primary-corpora-v3")
            self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
            kb_ids.extend(case.setup.knowledge_base_ids)
        self.assertEqual(len(kb_ids), len(set(kb_ids)))
        self.assertEqual(len(kb_ids), 9)

    def test_new_case_composes_claim_binding_and_retrieval_counterexamples(self):
        case = next(case for case in self.cases if case.case_id.endswith("editorial-release"))
        dialogue = "\n".join(item.query for item in case.turns)
        for signal in (
            "别把我当投稿作者",
            "只是在定角色",
            "没说复核开始了还是没开始",
            "不能因为校样看起来没问题就说已经发布",
            "这是问规则，不是让你发布",
            "另一篇文章意外写出了某人的家庭住址",
            "但别记到 M-42",
            "不能替代稿件目标读者",
            "只能说发布状态不清楚",
            "不要据缺材料直接判拒稿",
            "不改 CMS、不写文件、不发邮件，也别发布",
            "Reply in English",
        ):
            self.assertIn(signal, dialogue)
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

    def test_release_gate_uses_only_whole_conversation_codex_verdicts(self):
        policy = json.loads(
            (ROOT / "policies" / "rag-primary-codex-release-gate.v3.json").read_text(
                encoding="utf-8"
            )
        )
        profile = json.loads(
            (ROOT / "profiles" / "rag-primary-codex-matrix.v3.json").read_text(
                encoding="utf-8"
            )
        )
        self.assertEqual(policy["gate_kind"], "production_release")
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertFalse(policy["forbid_hard_failures"])
        self.assertFalse(policy["forbid_pass_to_fail_regressions"])
        self.assertEqual(policy["critical_metric_prefixes"], [])
        self.assertEqual(policy["max_metric_rate_regression"], {})
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertIn("corpus_editorial", policy["required_frozen_dependencies"])
        contract = profile["execution_contract"]
        self.assertIn("whole-conversation Codex verdict", contract["decision_policy"])
        self.assertIn("no individual answer is required to be perfect", contract["decision_policy"])
        self.assertTrue(contract["hard_semantic_rules"].startswith("none:"))

    def test_eval_loop_prepares_editorial_kb_from_dataset_marker(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_RAG_PRIMARY_EDITORIAL_ID", loop)
        self.assertIn("editorial-release-handbook.v1.md", loop)
        self.assertIn("rag-primary-editorial-kb-binding.v1.json", loop)

    def test_manifest_freezes_v3_corpora_and_evaluator(self):
        manifest_path = ROOT / "manifests" / "rag-primary-codex-matrix.v3.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after v3 files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(
            manifest["dependency_files"],
            {
                "corpus_editorial": "fixtures/regression-corpora/editorial-release-handbook.v1.md",
                "corpus_support": "fixtures/regression-corpora/customer-device-support-handbook.v1.md",
            },
        )
        dependencies = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v3.json",
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
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v3.json",
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
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
