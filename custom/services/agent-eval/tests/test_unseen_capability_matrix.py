from __future__ import annotations

import json
import unittest
from collections import defaultdict
from pathlib import Path

from curation.build_unseen_capability_matrix_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import Capability, ReviewMode, Split
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]
PROFILES = {"quick-answer", "rag-reasoning", "general-agent"}


class UnseenCapabilityMatrixTests(unittest.TestCase):
    def setUp(self) -> None:
        self.path = ROOT / "datasets" / "unseen-capability-matrix.v1.jsonl"
        self.cases = load_jsonl(self.path)

    def test_committed_dataset_matches_compiler_and_is_visible_only(self) -> None:
        compiled = build_cases()
        self.assertEqual(dataset_sha256(self.cases), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(self.cases), [])
        self.assertEqual(len(self.cases), 7)
        self.assertEqual({case.split for case in self.cases}, {Split.DEV, Split.GATE})
        self.assertNotIn(Split.SEALED_HOLDOUT, {case.split for case in self.cases})
        self.assertTrue(
            all(case.provenance.metadata["sealed_holdout_used"] is False for case in self.cases)
        )

    def test_every_split_covers_three_agents_with_repeated_twelve_turn_runs(self) -> None:
        by_split: dict[Split, list] = defaultdict(list)
        for case in self.cases:
            by_split[case.split].append(case)
            self.assertEqual(len(case.turns), 12)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.review_mode, ReviewMode.CODEX_CONVERSATION)
            self.assertIn(Capability.LONG_CONTEXT_DIALOGUE, case.capabilities)
            self.assertIn(Capability.TOOL_USE, case.capabilities)
            self.assertEqual(case.provenance.reference_answers, {})
            self.assertFalse(case.provenance.metadata["sut_prompt_injection"])
        for split in (Split.DEV, Split.GATE):
            self.assertEqual(
                {case.agent_profile_id for case in by_split[split]}, PROFILES
            )

    def test_domains_and_knowledge_scopes_are_distributionally_distinct(self) -> None:
        domains = {case.provenance.metadata["domain"] for case in self.cases}
        self.assertTrue(
            {
                "human-resources",
                "it-operations",
                "product-manual",
                "policy-governance",
                "project-management",
                "portfolio-planning",
            }
            <= domains
        )
        knowledge_scopes = {
            item for case in self.cases for item in case.setup.knowledge_base_ids
        }
        self.assertEqual(len(knowledge_scopes), 4)
        self.assertTrue(any(case.setup.knowledge_base_ids for case in self.cases))
        self.assertTrue(any(not case.setup.knowledge_base_ids for case in self.cases))
        dev_kbs = {
            item
            for case in self.cases
            if case.split == Split.DEV
            for item in case.setup.knowledge_base_ids
        }
        gate_kbs = {
            item
            for case in self.cases
            if case.split == Split.GATE
            for item in case.setup.knowledge_base_ids
        }
        self.assertTrue(dev_kbs.isdisjoint(gate_kbs))
        by_profile: dict[str, set[str]] = defaultdict(set)
        for case in self.cases:
            by_profile[case.agent_profile_id].add(
                "knowledge_base" if case.setup.knowledge_base_ids else "no_knowledge_base"
            )
        for profile in PROFILES:
            self.assertEqual(
                by_profile[profile],
                {"knowledge_base", "no_knowledge_base"},
                f"{profile} must be exercised with and without a knowledge base",
            )

    def test_matrix_contains_capability_counterexamples_not_renamed_old_cases(self) -> None:
        prompts = "\n".join(
            turn.query for case in self.cases for turn in case.turns
        )
        for example in (
            "问原因，不是在登记一个新状态",
            "不要检索。",
            "修改排障方案",
            "不要修改文件",
            "句子里的‘不能’不是禁止你查资料",
            "Reply in English",
            "different word order",
            "旧值作废",
            "来源归属",
        ):
            self.assertIn(example, prompts)
        for legacy_surface in ("采购文档", "培训说明", "固定三项"):
            self.assertNotIn(legacy_surface, prompts)
        self.assertNotIn("WEKNORA_CURRENT_TURN_EXECUTION", prompts)
        self.assertNotIn("required_claims", prompts)
        self.assertNotIn("reference_answer", prompts)

    def test_action_boundaries_vary_by_action_and_object(self) -> None:
        boundary_text = "\n".join(
            turn.query
            for case in self.cases
            for turn in case.turns
        )
        for object_name in (
            "邀请",
            "重置",
            "Jira",
            "生产配置",
            "报销单",
            "CRM",
        ):
            self.assertIn(object_name, boundary_text)

    def test_lexical_contracts_do_not_decide_codex_review_cases(self) -> None:
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

    def test_fixture_corpora_are_independent_and_domain_specific(self) -> None:
        fixture_root = ROOT / "fixtures" / "unseen-corpora"
        fixtures = sorted(fixture_root.glob("*.md"))
        self.assertEqual(len(fixtures), 4)
        rendered = "\n".join(path.read_text(encoding="utf-8") for path in fixtures)
        for anchor in ("Orion", "Sev-2", "2,000", "Northstar"):
            self.assertIn(anchor, rendered)

    def test_current_manifests_freeze_codex_and_dual_track_dependencies(self) -> None:
        policy_and_manifest = (
            (
                "production-multiturn-release-gate.v3.json",
                "unseen-capability-matrix.v1-production-release-v4.manifest.json",
            ),
            (
                "eval-optimization-gate.v3.json",
                "unseen-capability-matrix.v1-eval-optimization-v4.manifest.json",
            ),
            (
                "repair-dependency-gate.v2.json",
                "unseen-capability-matrix.v1-repair-dependency-v3.manifest.json",
            ),
        )
        dependency_paths = {
            "profiles": ROOT / "profiles" / "unseen-capability-matrix.v1.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
        }
        for policy_name, manifest_name in policy_and_manifest:
            with self.subTest(policy=policy_name):
                policy_path = ROOT / "policies" / policy_name
                policy = json.loads(policy_path.read_text(encoding="utf-8"))
                self.assertTrue(policy["require_codex_review"])
                self.assertTrue(policy["require_dual_track_codex_review"])
                manifest = json.loads(
                    (ROOT / "manifests" / manifest_name).read_text(encoding="utf-8")
                )
                self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.cases))
                expected_paths = {**dependency_paths, "policy": policy_path}
                self.assertEqual(
                    manifest["dependency_sha256"],
                    {
                        name: file_sha256(path)
                        for name, path in sorted(expected_paths.items())
                    },
                )

    def test_v2_manifests_remain_frozen_for_historical_reproduction(self) -> None:
        historical = (
            "unseen-capability-matrix.v1-production-release-v2.manifest.json",
            "unseen-capability-matrix.v1-eval-optimization-v2.manifest.json",
            "unseen-capability-matrix.v1-repair-dependency-v1.manifest.json",
        )
        for manifest_name in historical:
            with self.subTest(manifest=manifest_name):
                manifest = json.loads(
                    (ROOT / "manifests" / manifest_name).read_text(encoding="utf-8")
                )
                self.assertEqual(
                    manifest["dependency_sha256"]["client"],
                    "e424b83f466c225668b6208d12b6f5a3daf7b87752830ef17cdeb287335cc1dd",
                )

    def test_previous_v3_manifests_are_retained_for_reproduction(self) -> None:
        historical = (
            "unseen-capability-matrix.v1-production-release-v3.manifest.json",
            "unseen-capability-matrix.v1-eval-optimization-v3.manifest.json",
            "unseen-capability-matrix.v1-repair-dependency-v2.manifest.json",
        )
        for manifest_name in historical:
            with self.subTest(manifest=manifest_name):
                manifest = json.loads(
                    (ROOT / "manifests" / manifest_name).read_text(encoding="utf-8")
                )
                self.assertEqual(
                    manifest["dependency_sha256"]["client"],
                    "503648b198832fc7d0010fdddb371643f425a63a9e675336ebbe6341aa3ff923",
                )


if __name__ == "__main__":
    unittest.main()
