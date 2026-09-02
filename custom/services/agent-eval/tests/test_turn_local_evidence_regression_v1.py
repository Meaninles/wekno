import json
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from curation.build_turn_local_evidence_regression_v1 import build_cases


class TurnLocalEvidenceRegressionTests(unittest.TestCase):
    def test_new_case_is_neutral_repeated_and_rag_primary(self):
        cases = build_cases()
        self.assertEqual(len(cases), 1)
        case = cases[0]
        self.assertEqual(case.agent_profile_id, "rag-reasoning")
        self.assertEqual(case.repetitions, 3)
        self.assertEqual(len(case.turns), 12)
        self.assertEqual(case.provenance.reference_answers, {})
        self.assertEqual(case.provenance.reference_evidence, [])
        self.assertFalse(case.provenance.metadata["question_sequence_copied_from_existing_case"])
        self.assertTrue(all(not turn.contract.required_claims for turn in case.turns))
        self.assertTrue(all(not turn.contract.forbidden_claims for turn in case.turns))

    def test_dataset_has_no_semantic_answer_key(self):
        dataset = ROOT / "datasets" / "turn-local-evidence-regression.v1.jsonl"
        rows = [json.loads(line) for line in dataset.read_text(encoding="utf-8").splitlines() if line.strip()]
        self.assertEqual(len(rows), 1)
        raw = json.dumps(rows[0], ensure_ascii=False)
        for forbidden in ("required_claims\": [\"", "reference_answers\": {\""):
            self.assertNotIn(forbidden, raw)
        self.assertFalse(rows[0]["provenance"]["metadata"]["judge_feedback_used"])


if __name__ == "__main__":
    unittest.main()
