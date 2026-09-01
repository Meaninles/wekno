import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v2 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixV2Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v2.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.cases))

    def test_keeps_v1_and_adds_one_independent_post_change_case(self):
        self.assertEqual(len(self.cases), 8)
        self.assertEqual(sum(case.repetitions for case in self.cases), 24)
        self.assertEqual(sum(len(case.turns) * case.repetitions for case in self.cases), 297)
        self.assertEqual(
            Counter(case.agent_profile_id for case in self.cases),
            Counter({"general-agent": 4, "quick-answer": 2, "rag-reasoning": 2}),
        )
        new_cases = [
            case
            for case in self.cases
            if case.provenance.metadata.get(
                "created_after_semantic_binding_prompt_change"
            )
        ]
        self.assertEqual([case.case_id for case in new_cases], [
            "rag-primary-v2-general-customer-device-support"
        ])

    def test_all_cases_use_unique_explicit_knowledge_bases_and_codex_review(self):
        kb_ids = []
        for case in self.cases:
            self.assertEqual(case.suite, "weknora-rag-primary-codex-matrix-v2")
            self.assertEqual(case.corpus_version, "rag-primary-corpora-v2")
            self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
            kb_ids.extend(case.setup.knowledge_base_ids)
        self.assertEqual(len(kb_ids), len(set(kb_ids)))
        self.assertEqual(len(kb_ids), 8)

    def test_new_case_composes_capabilities_without_semantic_contracts(self):
        case = next(case for case in self.cases if case.case_id.endswith("customer-device-support"))
        dialogue = "\n".join(item.query for item in case.turns)
        for signal in (
            "这里只确定角色",
            "客户身份仍未知",
            "这是独立假设",
            "不要把包装规则记成 ZX-7 已经寄回",
            "不是让你擦除、执行命令或生成文件",
            "不要创建 RMA、改服务系统、写文件、发邮件或操作设备",
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

    def test_release_policy_is_production_codex_only(self):
        policy = json.loads(
            (ROOT / "policies" / "rag-primary-codex-release-gate.v2.json").read_text(encoding="utf-8")
        )
        self.assertEqual(policy["gate_kind"], "production_release")
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertFalse(policy["forbid_hard_failures"])
        self.assertEqual(policy["critical_metric_prefixes"], [])
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertIn("corpus_support", policy["required_frozen_dependencies"])

    def test_manifest_freezes_the_v2_corpus_and_evaluator(self):
        manifest_path = ROOT / "manifests" / "rag-primary-codex-matrix.v2.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after v2 files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependencies = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v2.json",
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
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v2.json",
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
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
