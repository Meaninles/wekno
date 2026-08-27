from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from weknora_eval.dataset import (
    build_quarantine_from_transcripts,
    split_by_family,
    validate_dataset,
)
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseSpec,
    Split,
    TextRule,
    TurnContract,
    TurnSpec,
)


def make_case(case_id: str, family_id: str) -> CaseSpec:
    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite="suite",
        split=Split.DEV,
        capabilities=[Capability.RAG_RETRIEVAL],
        agent=AgentSelector(agent_id="agent"),
        turns=[
            TurnSpec(
                turn_id="turn-1",
                query="question",
                contract=TurnContract(
                    required_claims=[TextRule(rule_id="claim", any_of=["answer"])]
                ),
            )
        ],
    )


class DatasetTests(unittest.TestCase):
    def test_family_split_is_deterministic_and_has_no_leakage(self) -> None:
        cases = [make_case(f"case-{i}-{j}", f"family-{i}") for i in range(8) for j in range(2)]
        first = split_by_family(cases, salt="stable")
        second = split_by_family(cases, salt="stable")
        self.assertEqual([case.split for case in first], [case.split for case in second])
        family_splits: dict[str, set[Split]] = {}
        for case in first:
            family_splits.setdefault(case.family_id, set()).add(case.split)
        self.assertTrue(all(len(splits) == 1 for splits in family_splits.values()))
        self.assertEqual(validate_dataset(first), [])

    def test_real_transcript_is_quarantined_not_treated_as_gold(self) -> None:
        payload = {
            "messages": [
                {"role": "user", "content": "联系 me@example.com 并回答"},
                {
                    "role": "assistant",
                    "content": "一个旧回答",
                    "knowledge_references": [{"id": "chunk-1", "evidence_content": "证据"}],
                },
            ]
        }
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "transcript.json"
            path.write_text(json.dumps(payload, ensure_ascii=False), encoding="utf-8")
            cases = build_quarantine_from_transcripts(
                [path], suite="suite", agent_id="agent"
            )
        self.assertEqual(len(cases), 1)
        self.assertEqual(cases[0].split, Split.QUARANTINE)
        self.assertFalse(cases[0].enabled)
        self.assertTrue(cases[0].provenance.needs_codex_review)
        self.assertIn("<EMAIL>", cases[0].turns[0].query)
        self.assertEqual(cases[0].turns[0].contract.required_claims, [])


if __name__ == "__main__":
    unittest.main()
