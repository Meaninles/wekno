from __future__ import annotations

import unittest

from weknora_eval.runner import EvalModeRequired, EvalRunner


class FakeClient:
    def __init__(self, mode: str) -> None:
        self.mode = mode

    def capabilities(self):
        return {"mode": self.mode, "recorder_enabled": True, "release": "r", "commit": "c", "capabilities": []}


class RunnerTests(unittest.TestCase):
    def test_production_is_record_only(self) -> None:
        with self.assertRaises(EvalModeRequired):
            EvalRunner(FakeClient("production")).doctor()  # type: ignore[arg-type]

    def test_eval_mode_is_accepted(self) -> None:
        fingerprint = EvalRunner(FakeClient("eval")).doctor()  # type: ignore[arg-type]
        self.assertEqual(fingerprint.mode, "eval")


if __name__ == "__main__":
    unittest.main()
