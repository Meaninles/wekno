from __future__ import annotations

import json
import unittest
from collections import defaultdict
from pathlib import Path

from curation.build_production_multiturn_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import Split
from weknora_eval.readiness import file_sha256


ROOT = Path(__file__).resolve().parents[1]
EXPECTED_PROFILES = {"quick-answer", "rag-reasoning", "general-agent"}


class ProductionMultiturnPreparationTests(unittest.TestCase):
    def test_visible_dataset_matches_compiler_and_split_contract(self) -> None:
        path = ROOT / "datasets" / "production-multiturn-ready.v1.jsonl"
        committed = load_jsonl(path)
        compiled = build_cases()
        self.assertEqual(dataset_sha256(committed), dataset_sha256(compiled))
        self.assertEqual(validate_dataset(committed), [])
        self.assertEqual(len(committed), 6)

        by_split: dict[Split, list] = defaultdict(list)
        for case in committed:
            by_split[case.split].append(case)
            self.assertEqual(case.repetitions, 3)
            self.assertEqual(case.setup.knowledge_ids, [])
            self.assertLessEqual(len(case.setup.knowledge_base_ids), 1)
            self.assertTrue(case.provenance.metadata["branch_safe"])
            self.assertFalse(case.provenance.metadata["requires_branch_curation"])
            self.assertFalse(
                case.provenance.metadata["observed_assistant_answers_used_as_gold"]
            )
            self.assertEqual(case.provenance.reference_answers, {})

        self.assertEqual(set(by_split), {Split.DEV, Split.GATE})
        for split in (Split.DEV, Split.GATE):
            self.assertEqual(
                {case.agent_profile_id for case in by_split[split]},
                EXPECTED_PROFILES,
            )
            depths = {case.agent_profile_id: len(case.turns) for case in by_split[split]}
            self.assertGreaterEqual(depths["quick-answer"], 7)
            self.assertGreaterEqual(depths["rag-reasoning"], 12)
            self.assertGreaterEqual(depths["general-agent"], 12)

        dev_kbs = {
            item
            for case in by_split[Split.DEV]
            for item in case.setup.knowledge_base_ids
        }
        gate_kbs = {
            item
            for case in by_split[Split.GATE]
            for item in case.setup.knowledge_base_ids
        }
        self.assertTrue(dev_kbs.isdisjoint(gate_kbs))
        self.assertNotIn("${AGENT_EVAL_KB_SYSTEM_POLICY_ID}", dev_kbs | gate_kbs)

    def test_manifest_hashes_and_policy_identity_are_frozen(self) -> None:
        dataset = load_jsonl(ROOT / "datasets" / "production-multiturn-ready.v1.jsonl")
        digest = dataset_sha256(dataset)
        pairs = {
            "experiment": "production-multiturn-experiment-gate.v1.json",
            "optimization": "production-multiturn-optimization-gate.v1.json",
            "release": "production-multiturn-release-gate.v1.json",
        }
        for stage, policy_name in pairs.items():
            manifest = json.loads(
                (
                     ROOT
                     / "manifests"
                     / f"production-multiturn-ready.v1-{stage}-evaluator-v9.manifest.json"
                ).read_text(encoding="utf-8")
            )
            self.assertEqual(manifest["dataset_sha256"], digest)
            self.assertEqual(
                manifest["dependency_sha256"]["policy"],
                file_sha256(ROOT / "policies" / policy_name),
            )
            self.assertEqual(
                manifest["dependency_sha256"]["profiles"],
                file_sha256(ROOT / "profiles" / "production-derived-multiturn.v1.json"),
            )
            self.assertEqual(
                manifest["dependency_sha256"]["scorer"],
                file_sha256(ROOT / "weknora_eval" / "scoring.py"),
            )
            self.assertEqual(
                manifest["dependency_sha256"]["judge"],
                file_sha256(ROOT / "weknora_eval" / "judge.py"),
            )

            policy = json.loads(
                (ROOT / "policies" / policy_name).read_text(encoding="utf-8")
            )
            self.assertIn("tool_use", policy["required_capabilities"])
            self.assertIn(
                "production_corpus_version",
                policy["required_execution_identity_fields"],
            )
            self.assertIn(
                "kb_bindings_sha256",
                policy["required_execution_identity_fields"],
            )

    def test_sealed_manifest_exposes_identity_not_prompts(self) -> None:
        manifest_path = (
            ROOT
            / "manifests"
            / "production-multiturn-holdout.v1-evaluator-v9.manifest.json"
        )
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        self.assertEqual(manifest["case_count"], 3)
        self.assertEqual(manifest["family_count"], 3)
        self.assertEqual(manifest["split_counts"], {"sealed_holdout": 3})
        self.assertNotIn("turns", manifest)
        self.assertEqual(
            manifest["dependency_sha256"]["scorer"],
            file_sha256(ROOT / "weknora_eval" / "scoring.py"),
        )

        sealed_path = ROOT / "sealed" / "production-multiturn-holdout.v1.jsonl"
        if sealed_path.exists():
            cases = load_jsonl(sealed_path)
            self.assertEqual(validate_dataset(cases), [])
            self.assertEqual(dataset_sha256(cases), manifest["dataset_sha256"])
            self.assertEqual(
                {case.agent_profile_id for case in cases}, EXPECTED_PROFILES
            )
            for case in cases:
                self.assertEqual(case.setup.knowledge_ids, [])
                self.assertLessEqual(len(case.setup.knowledge_base_ids), 1)

    def test_adaptive_scenario_contains_one_seed_per_profile(self) -> None:
        scenario = json.loads(
            (
                ROOT
                / "discovery"
                / "scenarios"
                / "production-derived-multiturn-problem-finding.v1.json"
            ).read_text(encoding="utf-8")
        )
        self.assertEqual(
            {session["profile_id"] for session in scenario["sessions"]},
            EXPECTED_PROFILES,
        )
        for session in scenario["sessions"]:
            self.assertEqual(len(session["turns"]), 1)
            self.assertEqual(session["knowledge_ids"], [])
            self.assertLessEqual(len(session["knowledge_base_ids"]), 1)


if __name__ == "__main__":
    unittest.main()
