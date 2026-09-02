from __future__ import annotations

import json
import unittest
from pathlib import Path

from curation.build_general_agent_post_salience_regression_v1 import (
    SUITE,
    build_cases,
)
from curation.build_rag_primary_codex_matrix_v4 import build_cases as build_v4_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]


class GeneralAgentPostSalienceRegressionTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.built = build_cases()
        cls.committed = load_jsonl(
            ROOT / "datasets" / "general-agent-post-salience-regression.v1.jsonl"
        )

    def test_dataset_is_exact_question_preserving_general_subset(self):
        source = {
            case.case_id: case
            for case in build_v4_cases()
            if case.agent_profile_id == "general-agent"
        }
        self.assertEqual(len(self.built), 6)
        self.assertEqual(self.built, self.committed)
        self.assertEqual(set(source), {case.case_id for case in self.built})
        for case in self.built:
            self.assertEqual(case.suite, SUITE)
            self.assertEqual(case.agent_profile_id, "general-agent")
            self.assertEqual(case.repetitions, 3)
            self.assertGreaterEqual(len(case.turns), 12)
            self.assertEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertEqual(
                [turn.query for turn in case.turns],
                [turn.query for turn in source[case.case_id].turns],
            )
            self.assertEqual(
                [turn.contract for turn in case.turns],
                [turn.contract for turn in source[case.case_id].turns],
            )

    def test_gate_is_manual_production_only_and_not_semantic(self):
        policy = json.loads(
            (ROOT / "policies" / "general-agent-post-salience-codex-gate.v1.json").read_text(encoding="utf-8")
        )
        profile = json.loads(
            (ROOT / "profiles" / "general-agent-post-salience-regression.v1.json").read_text(encoding="utf-8")
        )
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(policy["required_agent_profiles"], ["general-agent"])
        self.assertFalse(policy["require_judge"])
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertEqual(policy["min_pass_rate_per_case"], 0.666666)
        self.assertTrue(profile["execution_contract"]["hard_semantic_rules"].startswith("none:"))

    def test_manifest_freezes_component_dataset_and_dependencies(self):
        manifest = json.loads(
            (ROOT / "manifests" / "general-agent-post-salience-regression.v1.manifest.json").read_text(encoding="utf-8")
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
            "policy": ROOT / "policies" / "general-agent-post-salience-codex-gate.v1.json",
            "profiles": ROOT / "profiles" / "general-agent-post-salience-regression.v1.json",
            "corpus_unseen_project": ROOT / "fixtures" / "unseen-corpora" / "project-delivery-handbook.v1.md",
            "corpus_facilities": ROOT / "fixtures" / "regression-corpora" / "facilities-access-playbook.v1.md",
            "corpus_hr": ROOT / "fixtures" / "regression-corpora" / "remote-onboarding-handbook.v1.md",
            "corpus_support": ROOT / "fixtures" / "regression-corpora" / "customer-device-support-handbook.v1.md",
            "corpus_editorial": ROOT / "fixtures" / "regression-corpora" / "editorial-release-handbook.v1.md",
            "corpus_privacy": ROOT / "fixtures" / "regression-corpora" / "privacy-request-handbook.v1.md",
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertNotEqual(
            manifest["dependency_sha256"]["orchestration"],
            file_sha256(dependencies.pop("orchestration")),
        )
        frozen = dict(manifest["dependency_sha256"])
        frozen.pop("orchestration")
        self.assertEqual(
            frozen,
            {name: file_sha256(path) for name, path in sorted(dependencies.items())},
        )


if __name__ == "__main__":
    unittest.main()
