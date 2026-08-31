from __future__ import annotations

import unittest

from weknora_eval.models import AnswerSnapshot, MetricScore, RepairTrace, Verdict
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
        self.assertIn("## Deterministic diagnostic failures", report)
        self.assertIn("## Failure taxonomy", report)
        self.assertIn("response hygiene", report)
        self.assertIn("forbidden_claim.no-internal-planning", report)
        self.assertIn("| 1 |", report)

    def test_report_exposes_sut_source_and_running_images(self) -> None:
        dataset = [spec(profile_id="general-agent")]
        run = run_for(dataset, "candidate", [case(Verdict.PASS)])
        run = run.model_copy(
            update={
                "sut": run.sut.model_copy(
                    update={
                        "commit": "source-commit",
                        "raw": {
                            **run.sut.raw,
                            "worktree_dirty": "false",
                            "runtime_image_id": "sha256:runtime",
                            "general_agent_image_id": "sha256:general",
                        },
                    }
                )
            }
        )
        report = render_markdown(run)
        self.assertIn("## SUT provenance", report)
        self.assertIn("source-commit", report)
        self.assertIn("sha256:runtime", report)

    def test_report_exposes_both_answer_tracks_scores_and_repair_trace(self) -> None:
        dataset = [spec(profile_id="general-agent")]
        observed = case(Verdict.FAIL, profile_id="general-agent")
        turn = observed.turns[0].model_copy(
            update={
                "production_candidate": AnswerSnapshot(
                    content="production text", references=[{"id": "p"}]
                ),
                "eval_assisted_answer": AnswerSnapshot(
                    content="assisted text", references=[{"id": "a"}]
                ),
                "repair": RepairTrace(
                    triggered=True,
                    succeeded=True,
                    repair_types=["bounded_rewrite"],
                    attempts=1,
                    added_model_calls=1,
                    added_latency_ms=12,
                ),
                "production_surface_equivalent": True,
            }
        )
        observed = observed.model_copy(
            update={
                "turns": [turn],
                "production_scores": [
                    MetricScore(
                        name="diagnostic.production",
                        value=False,
                        passed=False,
                        turn_id="turn",
                    )
                ],
                "eval_assisted_scores": [
                    MetricScore(
                        name="diagnostic.assisted",
                        value=True,
                        passed=True,
                        turn_id="turn",
                    )
                ],
            }
        )

        report = render_markdown(run_for(dataset, "candidate", [observed]))

        self.assertIn("## Dual-track answer ledger", report)
        self.assertIn("production text", report)
        self.assertIn("assisted text", report)
        self.assertIn("diagnostic.production", report)
        self.assertIn("diagnostic.assisted", report)
        self.assertIn("bounded_rewrite", report)
        self.assertIn("SSE = history", report)


if __name__ == "__main__":
    unittest.main()
