from __future__ import annotations

import argparse
import json
import os
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

from weknora_eval.cli import cmd_judge
from weknora_eval.dataset import write_jsonl
from weknora_eval.judge import JudgeError
from weknora_eval.models import MetricScore, Verdict
from weknora_eval.report import load_run, write_json

from test_gates import case, run_for, spec


class JudgeCliTests(unittest.TestCase):
    def test_calibrated_judge_pairs_baseline_by_attempt_and_records_identity(self) -> None:
        dataset = [spec(repetitions=2)]
        candidate = run_for(
            dataset,
            "candidate",
            [case(Verdict.PASS, attempt_index=1), case(Verdict.PASS, attempt_index=2)],
        )
        baseline = run_for(
            dataset,
            "baseline",
            [case(Verdict.PASS, attempt_index=1), case(Verdict.PASS, attempt_index=2)],
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            dataset_path = root / "dataset.jsonl"
            candidate_path = root / "candidate.json"
            baseline_path = root / "baseline.json"
            calibration_path = root / "calibration.json"
            output_path = root / "judged.json"
            write_jsonl(dataset_path, dataset)
            write_json(candidate_path, candidate)
            write_json(baseline_path, baseline)
            calibration_path.write_text(
                json.dumps(
                    {
                        "passed": True,
                        "judge_model": "judge-x",
                        "suite_sha256": "suite-sha",
                        "accuracy": 1.0,
                        "minimum_accuracy": 0.85,
                    }
                ),
                encoding="utf-8",
            )
            seen_attempts: list[int] = []

            def fake_judge(_spec, _candidate, paired_baseline):
                self.assertIsNotNone(paired_baseline)
                seen_attempts.append(paired_baseline.attempt_index)
                return [
                    MetricScore(
                        name="judge.contract_satisfaction",
                        value="pass",
                        passed=True,
                        hard=False,
                        turn_id="turn",
                        metadata={"confidence": 0.99},
                    )
                ]

            args = argparse.Namespace(
                dataset=str(dataset_path),
                run=str(candidate_path),
                baseline=str(baseline_path),
                calibration_result=str(calibration_path),
                output=str(output_path),
            )
            with patch.dict(os.environ, {"AGENT_EVAL_JUDGE_MODEL": "judge-x"}), patch(
                "weknora_eval.cli.judge_case", side_effect=fake_judge
            ):
                self.assertEqual(cmd_judge(args), 0)

            judged = load_run(output_path)
            self.assertEqual(seen_attempts, [1, 2])
            identity = judged.metadata["execution_contract"]
            self.assertEqual(identity["judge_model"], "judge-x")
            self.assertEqual(identity["judge_calibration_suite_sha256"], "suite-sha")
            self.assertTrue(identity["judge_prompt_sha256"])

    def test_case_level_judge_failure_is_invalid_and_does_not_abort_artifact(self) -> None:
        dataset = [spec(repetitions=2)]
        candidate = run_for(
            dataset,
            "candidate",
            [case(Verdict.PASS, attempt_index=1), case(Verdict.PASS, attempt_index=2)],
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            dataset_path = root / "dataset.jsonl"
            candidate_path = root / "candidate.json"
            output_path = root / "judged.json"
            write_jsonl(dataset_path, dataset)
            write_json(candidate_path, candidate)

            passed = [
                MetricScore(
                    name="judge.contract_satisfaction",
                    value="pass",
                    passed=True,
                    hard=False,
                    turn_id="turn",
                    metadata={"confidence": 0.99},
                )
            ]
            args = argparse.Namespace(
                dataset=str(dataset_path),
                run=str(candidate_path),
                baseline=None,
                calibration_result=None,
                output=str(output_path),
            )
            with patch.dict(os.environ, {"AGENT_EVAL_JUDGE_MODEL": "judge-x"}), patch(
                "weknora_eval.cli.judge_case",
                side_effect=[JudgeError("read timed out"), passed],
            ):
                self.assertEqual(cmd_judge(args), 2)

            judged = load_run(output_path)
            self.assertEqual(
                [item.verdict for item in judged.cases],
                [Verdict.INVALID, Verdict.PASS],
            )
            self.assertIn("judge_error:read timed out", judged.cases[0].error or "")
            self.assertEqual(judged.metadata["judge"]["error_count"], 1)
            self.assertEqual(
                judged.metadata["judge"]["errors"][0]["attempt_index"], 1
            )

    def test_peer_disconnect_is_invalid_and_does_not_abort_artifact(self) -> None:
        dataset = [spec(repetitions=2)]
        candidate = run_for(
            dataset,
            "candidate",
            [case(Verdict.PASS, attempt_index=1), case(Verdict.PASS, attempt_index=2)],
        )
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            dataset_path = root / "dataset.jsonl"
            candidate_path = root / "candidate.json"
            output_path = root / "judged.json"
            write_jsonl(dataset_path, dataset)
            write_json(candidate_path, candidate)

            passed = [
                MetricScore(
                    name="judge.contract_satisfaction",
                    value="pass",
                    passed=True,
                    hard=False,
                    turn_id="turn",
                    metadata={"confidence": 0.99},
                )
            ]
            args = argparse.Namespace(
                dataset=str(dataset_path),
                run=str(candidate_path),
                baseline=None,
                calibration_result=None,
                output=str(output_path),
            )
            with patch.dict(os.environ, {"AGENT_EVAL_JUDGE_MODEL": "judge-x"}), patch(
                "weknora_eval.cli.judge_case",
                side_effect=[ConnectionResetError("peer reset"), passed],
            ):
                self.assertEqual(cmd_judge(args), 2)

            judged = load_run(output_path)
            self.assertEqual(
                [item.verdict for item in judged.cases],
                [Verdict.INVALID, Verdict.PASS],
            )
            self.assertIn("judge_error:peer reset", judged.cases[0].error or "")
            self.assertEqual(judged.metadata["judge"]["error_count"], 1)


if __name__ == "__main__":
    unittest.main()
