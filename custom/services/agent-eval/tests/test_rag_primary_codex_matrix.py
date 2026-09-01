import json
import sys
import unittest
from collections import Counter
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_rag_primary_codex_matrix_v1 import build_cases  # noqa: E402
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode, ReviewMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class RagPrimaryCodexMatrixTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.cases = build_cases()
        cls.dataset_path = ROOT / "datasets" / "rag-primary-codex-matrix.v1.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(
            dataset_sha256(self.committed),
            dataset_sha256(self.cases),
        )

    def test_is_a_large_rag_primary_three_agent_matrix(self):
        self.assertEqual(len(self.cases), 7)
        self.assertEqual(sum(case.repetitions for case in self.cases), 21)
        self.assertEqual(
            sum(len(case.turns) * case.repetitions for case in self.cases),
            258,
        )
        counts = Counter(case.agent_profile_id for case in self.cases)
        self.assertEqual(
            set(counts),
            {"quick-answer", "rag-reasoning", "general-agent"},
        )
        self.assertTrue(all(count >= 2 for count in counts.values()))

    def test_covers_required_business_domains_with_independent_kbs(self):
        domains = {
            case.provenance.metadata["domain"]
            for case in self.cases
        }
        self.assertTrue(
            {
                "human-resources",
                "it-operations",
                "product-manual",
                "policy-governance",
                "project-management",
            }.issubset(domains)
        )
        kb_ids = []
        for case in self.cases:
            self.assertEqual(
                case.setup.knowledge_selection_mode,
                KnowledgeSelectionMode.EXPLICIT,
            )
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertEqual(case.setup.knowledge_ids, [])
            kb_ids.extend(case.setup.knowledge_base_ids)
        self.assertEqual(len(kb_ids), len(set(kb_ids)))
        self.assertEqual(len(kb_ids), 7)

    def test_every_case_is_long_repeated_and_codex_reviewed(self):
        for case in self.cases:
            self.assertGreaterEqual(len(case.turns), 12, case.case_id)
            self.assertEqual(case.repetitions, 3, case.case_id)
            self.assertEqual(
                case.review_mode,
                ReviewMode.CODEX_CONVERSATION,
                case.case_id,
            )
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertEqual(case.provenance.reference_evidence, [])
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])
            self.assertFalse(case.provenance.metadata["reference_answers_used"])
            self.assertFalse(case.provenance.metadata["judge_feedback_used"])

    def test_no_semantic_hard_rules_or_fixed_answer_shapes(self):
        for case in self.cases:
            for current_turn in case.turns:
                contract = current_turn.contract
                state = contract.conversation_state
                self.assertEqual(contract.required_claims, [])
                self.assertEqual(contract.forbidden_claims, [])
                self.assertEqual(contract.evidence_anchors, [])
                self.assertEqual(contract.evidence_claims, [])
                self.assertEqual(contract.min_evidence_anchors, 0)
                self.assertFalse(contract.citation_required)
                self.assertEqual(contract.min_citations, 0)
                self.assertEqual(contract.min_retrieved_sources, {})
                self.assertIsNone(contract.max_tool_calls)
                self.assertIsNone(contract.max_response_chars)
                self.assertIsNone(contract.max_total_latency_ms)
                self.assertIsNone(contract.decision)
                self.assertIsNone(contract.judge_rubric)
                self.assertEqual(state.active_facts, [])
                self.assertEqual(state.retired_facts, [])
                self.assertEqual(state.unknown_facts, [])
                self.assertEqual(state.forbidden_unknown_facts, [])
                self.assertEqual(state.forbidden_inferences, [])
                self.assertEqual(state.action_boundaries, [])
                self.assertFalse(state.require_scoped_sections)

    def test_new_post_change_cases_cover_proof_direction_and_local_constraints(self):
        new_cases = [
            case
            for case in self.cases
            if case.provenance.metadata[
                "created_after_proof_direction_prompt_change"
            ]
        ]
        self.assertEqual(len(new_cases), 2)
        dialogue = "\n".join(
            current_turn.query
            for case in new_cases
            for current_turn in case.turns
        )
        for signal in (
            "不能证明完成",
            "不要登记成‘运输尚未完成’",
            "上一轮不调用工具只限上一轮",
            "不代表账号尚未启用",
            "句子含‘不得安装’不等于禁止你检索",
            "不要创建文件、改 HRIS",
            "Reply in English",
        ):
            self.assertIn(signal, dialogue)
        self.assertNotIn("采购", dialogue)
        self.assertNotIn("培训", dialogue)

    def test_release_gate_is_codex_only_and_production_only(self):
        policy_path = ROOT / "policies" / "rag-primary-codex-release-gate.v1.json"
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["gate_kind"], "production_release")
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(policy["required_splits"], ["gate"])
        self.assertEqual(policy["min_repetitions_per_case"], 3)
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertFalse(policy["forbid_hard_failures"])
        self.assertEqual(policy["critical_metric_prefixes"], [])
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertTrue(policy["require_production_surface_equivalence"])
        self.assertFalse(policy["require_baseline"])
        for hard_quality_key in (
            "max_p95_latency_ms_by_agent",
            "max_latency_ms_by_agent",
            "max_repair_trigger_rate",
            "max_repair_only_pass_rate",
            "max_average_repair_attempts",
            "max_added_model_calls_per_turn",
            "max_added_tool_calls_per_turn",
            "max_average_added_latency_ms",
        ):
            self.assertNotIn(hard_quality_key, policy)

    def test_profile_has_only_explicit_production_agents(self):
        profile_path = ROOT / "profiles" / "rag-primary-codex-matrix.v1.json"
        profile = json.loads(profile_path.read_text(encoding="utf-8"))
        profiles = {item["profile_id"]: item for item in profile["profiles"]}
        self.assertEqual(
            set(profiles),
            {"quick-answer", "rag-reasoning", "general-agent"},
        )
        for item in profiles.values():
            self.assertEqual(
                set(item["agent_ids_by_knowledge_selection"]),
                {"explicit"},
            )
            self.assertTrue(item["agent_id"].startswith("builtin-"))

    def test_eval_loop_prepares_corpora_by_markers_and_aggregates_bindings(self):
        loop = (ROOT / "eval-loop.ps1").read_text(encoding="utf-8")
        for marker in (
            "AGENT_EVAL_KB_UNSEEN_",
            "AGENT_EVAL_KB_SEMANTIC_ROUTING_FACILITIES_ID",
            "AGENT_EVAL_KB_POST_CHANGE_LAB_ID",
            "AGENT_EVAL_KB_RAG_PRIMARY_HR_ID",
        ):
            self.assertIn(marker, loop)
        self.assertIn("datasetKbEnvironmentKeys", loop)
        self.assertIn("combinedBindingHash", loop)
        self.assertIn("binding_identity_sha256", loop)
        self.assertIn("source_sha256", loop)
        self.assertIn("AGENT_EVAL_KB_BINDINGS_SHA256", loop)
        self.assertNotIn(
            '(Split-Path -Leaf $Dataset) -eq "post-change-evidence-canary.v1.jsonl"',
            loop,
        )

    def test_manifest_freezes_dataset_and_evaluator_dependencies(self):
        manifest_path = (
            ROOT
            / "manifests"
            / "rag-primary-codex-matrix.v1.manifest.json"
        )
        if not manifest_path.exists():
            self.skipTest("manifest is generated after evaluator files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "rag-primary-codex-matrix.v1.json",
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
            "policy": ROOT / "policies" / "rag-primary-codex-release-gate.v1.json",
            "orchestration": ROOT / "eval-loop.ps1",
            "compose": ROOT / "docker-compose.yml",
            "corpus_unseen_product": ROOT / "fixtures" / "unseen-corpora" / "product-orion-manual.v1.md",
            "corpus_unseen_project": ROOT / "fixtures" / "unseen-corpora" / "project-delivery-handbook.v1.md",
            "corpus_unseen_it": ROOT / "fixtures" / "unseen-corpora" / "it-operations-runbook.v1.md",
            "corpus_unseen_policy": ROOT / "fixtures" / "unseen-corpora" / "governance-expense-policy.v1.md",
            "corpus_facilities": ROOT / "fixtures" / "regression-corpora" / "facilities-access-playbook.v1.md",
            "corpus_lab": ROOT / "fixtures" / "regression-corpora" / "lab-sample-handoff.v1.md",
            "corpus_hr": ROOT / "fixtures" / "regression-corpora" / "remote-onboarding-handbook.v1.md",
        }
        self.assertEqual(
            manifest["dataset_sha256"],
            dataset_sha256(self.committed),
        )
        expected = {
            name: file_sha256(path)
            for name, path in sorted(dependency_paths.items())
        }
        # v1 is the immutable evidence package for full-flow #2.  Its frozen
        # orchestration and readiness hashes intentionally stay bound to that
        # historical run; current framework changes are frozen by v2 instead
        # of rewriting the old manifest and making the earlier result
        # unreproducible.
        for dependency in ("orchestration", "readiness"):
            expected[dependency] = manifest["dependency_sha256"][dependency]
        self.assertEqual(manifest["dependency_sha256"], expected)


if __name__ == "__main__":
    unittest.main()
