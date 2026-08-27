from __future__ import annotations

import unittest
from collections import Counter, defaultdict
from pathlib import Path

from curation.build_multiturn_dev_v1 import build_cases
from weknora_eval.dataset import dataset_sha256, load_jsonl, validate_dataset
from weknora_eval.models import Split


class MultiturnCurationTests(unittest.TestCase):
    def test_committed_jsonl_matches_the_reviewed_compiler(self) -> None:
        dataset_path = (
            Path(__file__).resolve().parents[1]
            / "datasets"
            / "multiturn-dev.v1.jsonl"
        )

        committed = load_jsonl(dataset_path)
        reviewed = build_cases()

        self.assertEqual(dataset_sha256(committed), dataset_sha256(reviewed))

    def test_curated_dev_matrix_is_branch_safe_and_complete(self) -> None:
        cases = build_cases()

        self.assertEqual(len(cases), 12)
        self.assertEqual(sum(len(case.turns) for case in cases), 57)
        self.assertEqual(validate_dataset(cases), [])
        self.assertEqual({case.split for case in cases}, {Split.DEV})
        self.assertTrue(all(case.enabled for case in cases))
        self.assertTrue(all(not case.provenance.needs_codex_review for case in cases))
        self.assertTrue(
            all(case.provenance.metadata.get("branch_safe") is True for case in cases)
        )
        self.assertTrue(
            all(
                case.provenance.metadata.get("requires_branch_curation") is False
                for case in cases
            )
        )
        self.assertTrue(all(not case.provenance.reference_answers for case in cases))
        self.assertTrue(all(case.provenance.source_hash for case in cases))
        self.assertTrue(
            all(
                case.setup.knowledge_ids
                == ["${AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID}"]
                for case in cases
            )
        )

        profile_counts = Counter(case.agent_profile_id for case in cases)
        self.assertEqual(
            profile_counts,
            Counter(
                {
                    "quick-answer": 4,
                    "rag-reasoning": 4,
                    "general-agent": 4,
                }
            ),
        )

        family_profiles: dict[str, set[str | None]] = defaultdict(set)
        for case in cases:
            family_profiles[case.family_id].add(case.agent_profile_id)
        self.assertEqual(len(family_profiles), 4)
        self.assertTrue(
            all(
                profiles == {"quick-answer", "rag-reasoning", "general-agent"}
                for profiles in family_profiles.values()
            )
        )

    def test_rag_families_bind_each_required_method_claim_to_its_evidence(self) -> None:
        cases = build_cases()
        alignment_cases = [
            case
            for case in cases
            if case.family_id == "current-turn-alignment-topic-detour"
        ]
        decision_cases = [
            case
            for case in cases
            if case.family_id == "decision-under-unknowns-procurement-path"
        ]

        for case in alignment_cases:
            self.assertEqual(len(case.turns[0].contract.evidence_claims), 3)
            self.assertEqual(len(case.turns[2].contract.evidence_claims), 3)
            self.assertEqual(len(case.turns[3].contract.evidence_claims), 2)
        for case in decision_cases:
            self.assertEqual(len(case.turns[0].contract.evidence_anchors), 4)
            self.assertEqual(len(case.turns[0].contract.evidence_claims), 5)
            self.assertEqual(case.turns[0].contract.min_citations, 4)

    def test_window_cases_have_twelve_branch_independent_user_turns(self) -> None:
        cases = [
            case
            for case in build_cases()
            if case.family_id == "active-retired-state-window-plus-two"
        ]
        self.assertEqual(len(cases), 3)
        for case in cases:
            self.assertEqual(len(case.turns), 12)
            self.assertNotIn("你刚才", "\n".join(turn.query for turn in case.turns))
            final = case.turns[-1].contract.conversation_state
            self.assertGreaterEqual(len(final.active_facts), 7)
            self.assertGreaterEqual(len(final.retired_facts), 3)
            self.assertGreaterEqual(len(final.unknown_facts), 4)
            self.assertGreaterEqual(len(final.action_boundaries), 1)
            self.assertTrue(final.require_scoped_sections)


if __name__ == "__main__":
    unittest.main()
