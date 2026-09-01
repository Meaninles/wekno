import json
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_unseen_capability_matrix_v1 import (  # noqa: E402
    build_cases as build_v1_cases,
)
from curation.build_unseen_capability_matrix_v2 import (  # noqa: E402
    SUITE,
    build_cases,
)
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset  # noqa: E402
from weknora_eval.models import KnowledgeSelectionMode  # noqa: E402
from weknora_eval.readiness import file_sha256  # noqa: E402


class UnseenCapabilityMatrixV2Test(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.v1 = build_v1_cases()
        cls.v2 = build_cases()
        cls.dataset_path = ROOT / "datasets" / "unseen-capability-matrix.v2.jsonl"
        cls.committed = load_jsonl(cls.dataset_path)

    def test_committed_dataset_matches_v2_builder(self):
        self.assertEqual(validate_dataset(self.committed), [])
        self.assertEqual(dataset_sha256(self.committed), dataset_sha256(self.v2))

    def test_v1_is_preserved_and_v2_changes_only_orchestration(self):
        self.assertEqual(len(self.v2), len(self.v1))
        self.assertEqual([case.case_id for case in self.v2], [case.case_id for case in self.v1])
        for before, after in zip(self.v1, self.v2, strict=True):
            self.assertEqual(
                [turn.query for turn in after.turns],
                [turn.query for turn in before.turns],
            )
            self.assertEqual(after.suite, SUITE)

    def test_every_case_has_a_concrete_knowledge_boundary(self):
        for case in self.v2:
            has_targets = bool(case.setup.knowledge_base_ids or case.setup.knowledge_ids)
            expected = (
                KnowledgeSelectionMode.EXPLICIT
                if has_targets
                else KnowledgeSelectionMode.NONE
            )
            self.assertEqual(case.setup.knowledge_selection_mode, expected, case.case_id)

    def test_no_kb_case_uses_an_isolated_runtime_clone(self):
        no_kb = [
            case
            for case in self.v2
            if case.setup.knowledge_selection_mode == KnowledgeSelectionMode.NONE
        ]
        self.assertEqual(len(no_kb), 3)
        for case in no_kb:
            self.assertEqual(case.setup.knowledge_base_ids, [])
            self.assertEqual(case.setup.knowledge_ids, [])
            self.assertIn("_NO_KB_ID}", case.agent.agent_id)
            self.assertNotIn("builtin-", case.agent.agent_id)

    def test_v2_profile_maps_every_runtime_to_explicit_or_none(self):
        profile_path = ROOT / "profiles" / "unseen-capability-matrix.v2.json"
        profile_set = json.loads(profile_path.read_text(encoding="utf-8"))
        profiles = {
            profile["profile_id"]: profile
            for profile in profile_set["profiles"]
        }
        self.assertEqual(
            set(profiles),
            {"quick-answer", "rag-reasoning", "general-agent"},
        )
        for profile_id, profile in profiles.items():
            mappings = profile["agent_ids_by_knowledge_selection"]
            self.assertIn("explicit", mappings)
            self.assertIn("none", mappings)
            self.assertIn("_NO_KB_ID}", mappings["none"], profile_id)

    def test_visible_readiness_gate_uses_codex_and_production_answers(self):
        policy_path = (
            ROOT
            / "policies"
            / "unseen-capability-visible-readiness-gate.v1.json"
        )
        policy = json.loads(policy_path.read_text(encoding="utf-8"))
        self.assertEqual(policy["answer_track"], "production_candidate")
        self.assertEqual(set(policy["required_splits"]), {"dev", "gate"})
        self.assertEqual(
            set(policy["required_agent_profiles"]),
            {"quick-answer", "rag-reasoning", "general-agent"},
        )
        self.assertTrue(policy["require_codex_review"])
        self.assertTrue(policy["require_dual_track_codex_review"])
        self.assertFalse(policy["require_baseline"])

    def test_visible_readiness_manifest_freezes_v2_dependencies(self):
        manifest_path = (
            ROOT
            / "manifests"
            / "unseen-capability-matrix.v2-visible-readiness-v1.manifest.json"
        )
        if not manifest_path.exists():
            self.skipTest("manifest is generated after the evaluator files are final")
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        dependency_paths = {
            "profiles": ROOT / "profiles" / "unseen-capability-matrix.v2.json",
            "judge_calibration": ROOT / "calibration" / "judge-multiturn.v1.json",
            "models": ROOT / "weknora_eval" / "models.py",
            "runner": ROOT / "weknora_eval" / "runner.py",
            "assistance": ROOT / "weknora_eval" / "assistance.py",
            "codex_review": ROOT / "weknora_eval" / "codex_review.py",
            "client": ROOT / "weknora_eval" / "client.py",
            "scorer": ROOT / "weknora_eval" / "scoring.py",
            "gate": ROOT / "weknora_eval" / "gates.py",
            "policy": (
                ROOT
                / "policies"
                / "unseen-capability-visible-readiness-gate.v1.json"
            ),
        }
        self.assertEqual(manifest["dataset_sha256"], dataset_sha256(self.committed))
        self.assertEqual(
            manifest["dependency_sha256"],
            {
                name: file_sha256(path)
                for name, path in sorted(dependency_paths.items())
            },
        )


if __name__ == "__main__":
    unittest.main()
