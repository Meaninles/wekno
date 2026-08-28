from __future__ import annotations

import unittest

from weknora_eval.models import MetricScore, Verdict
from weknora_eval.report import render_markdown

from test_gates import case, run_for, spec


class ReportTests(unittest.TestCase):
    def test_report_exposes_attempt_agent_failed_turn_and_latency(self) -> None:
        dataset = [spec(profile_id="general-agent")]
        failed = case(
            Verdict.FAIL,
            latency=371923,
            attempt_index=1,
            profile_id="general-agent",
        )
        failed = failed.model_copy(
            update={
                "scores": [
                    MetricScore(
                        name="forbidden_claim.no-internal-planning",
                        value=False,
                        passed=False,
                        turn_id="turn",
                    )
                ]
            }
        )
        report = render_markdown(run_for(dataset, "candidate", [failed]))
        self.assertIn("## Per-agent summary", report)
        self.assertIn("`general-agent`", report)
        self.assertIn("371923ms", report)
        self.assertIn("## Failed turns", report)
        self.assertIn("## Failure taxonomy", report)
        self.assertIn("response hygiene", report)
        self.assertIn("forbidden_claim.no-internal-planning", report)
        self.assertIn("| 1 |", report)


if __name__ == "__main__":
    unittest.main()
