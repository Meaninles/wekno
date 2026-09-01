import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v4 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixV4Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v4.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.cases))

    def test_retains_v3_and_adds_one_independent_post_change_case(self):
        self.assertEqual(len(self.cases), 10)
        self.assertEqual(sum(case.repetitions for case in self.cases), 30)
        self.assertEqual(sum(len(case.turns) * case.repetitions for case in self.cases), 375)
        self.assertEqual(
            Counter(case.agent_profile_id for case in self.cases),
            Counter({"general-agent": 6, "quick-answer": 2, "rag-reasoning": 2}),
        )
        new_cases = [
            case
            for case in self.cases
            if case.provenance.metadata.get(
                "created_after_entailment_and_operation_authority_change"
            )
        ]
        self.assertEqual(
            [case.case_id for case in new_cases],
            ["rag-primary-v4-general-privacy-request"],
        )

    def test_all_cases_use_unique_explicit_kbs_and_neutral_codex_review(self):
        kb_ids = []
        for case in self.cases:
            self.assertEqual(case.suite, "weknora-rag-primary-codex-matrix-v4")
            self.assertEqual(case.corpus_version, "rag-primary-corpora-v4")
            self.assertEqual(case.setup.knowledge_selection_mode, KnowledgeSelectionMode.EXPLICIT)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
            kb_ids.extend(case.setup.knowledge_base_ids)
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
        self.assertEqual(len(kb_ids), len(set(kb_ids)))
        self.assertEqual(len(kb_ids), 10)

    def test_new_case_composes_new_capability_sequence_and_counterexamples(self):
        case = next(case for case in self.cases if case.case_id.endswith("privacy-request"))
        dialogue = "\n".join(item.query for item in case.turns)
        for signal in (
            "正式响应计时开始需要哪两个可验证条件",
            "请求人身份这一字段没有提供",
            "别改写成请求人没有提供证明材料",
            "别把我当请求人",
            "指定角色本身不证明核验开始、未开始或完成",
            "这是在问规则，不是让你导出",
            "旧的普通网页表单表述废弃",
            "另一个案例暴露了生物识别模板",
            "不要写进 PR-17",
            "这个批准不说明方法已写入记录或执行",
            "即使此前没有成稿也直接给文字",
            "不要建工单、写文件、发邮件、导出或删除数据",
            "Reply in English",
            "真实引用",
        ):
            self.assertIn(signal, dialogue)

    def test_release_policy_is_complete_conversation_codex_judgment(self):
        policy = json.loads(
            (ROOT / "policies" / "rag-primary-codex-release-gate.v4.json").read_text(
                encoding="utf-8"
            )
        )
        profile = json.loads(
            (ROOT / "profiles" / "rag-primary-codex-matrix.v4.json").read_text(
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
        self.assertIn("corpus_privacy", policy["required_frozen_dependencies"])
        contract = profile["execution_contract"]
        self.assertIn("whole-conversation Codex verdict", contract["decision_policy"])
        self.assertIn("no individual answer is required to be perfect", contract["decision_policy"])
        self.assertTrue(contract["hard_semantic_rules"].startswith("none:"))

    def test_eval_loop_prepares_privacy_kb_from_dataset_marker(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        self.assertIn("AGENT_EVAL_KB_RAG_PRIMARY_PRIVACY_ID", loop)
        self.assertIn("privacy-request-handbook.v1.md", loop)
        self.assertIn("rag-primary-privacy-kb-binding.v1.json", loop)

    def test_manifest_freezes_v4_corpora_and_evaluator(self):
        manifest_path = ROOT / "manifests" / "rag-primary-codex-matrix.v4.manifest.json"
        if not manifest_path.exists():
            self.skipTest("manifest is generated after v4 files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(
            manifest["dependency_files"],
            {
                "corpus_editorial": "fixtures/regression-corpora/editorial-release-handbook.v1.md",
                "corpus_privacy": "fixtures/regression-corpora/privacy-request-handbook.v1.md",
                "corpus_support": "fixtures/regression-corpora/customer-device-support-handbook.v1.md",
            },
        )
        dependencies = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v4.json",
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
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v4.json",
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
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(
            manifest["dependency_sha256"],
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
