from __future__ import annotations

import io
import json
import os
import unittest
from unittest.mock import patch

from weknora_eval.judge import _post_chat, judge_case
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    ObservedTurn,
    Split,
    SUT_RESPONSE_DEADLINE_EXCEEDED,
    SUT_TURN_SKIPPED_AFTER_DEADLINE,
    TurnContract,
    TurnSpec,
    Verdict,
)


class JudgeTests(unittest.TestCase):
    def test_transient_timeout_is_retried_with_a_bounded_contract(self) -> None:
        response = io.BytesIO(
            json.dumps(
                {"choices": [{"message": {"content": '{"turns": []}'}}]}
            ).encode("utf-8")
        )
        env = {
            "AGENT_EVAL_JUDGE_BASE_URL": "https://judge.example/v1",
            "AGENT_EVAL_JUDGE_API_KEY": "test-only",
            "AGENT_EVAL_JUDGE_MODEL": "judge-x",
            "AGENT_EVAL_JUDGE_TIMEOUT_SECONDS": "7",
            "AGENT_EVAL_JUDGE_MAX_ATTEMPTS": "2",
        }
        with patch.dict(os.environ, env, clear=False), patch(
            "weknora_eval.judge.urllib.request.urlopen",
            side_effect=[TimeoutError("slow"), response],
        ) as urlopen, patch("weknora_eval.judge.time.sleep") as sleep:
            self.assertEqual(_post_chat([{"role": "user", "content": "x"}]), {"turns": []})

        self.assertEqual(urlopen.call_count, 2)
        self.assertEqual([call.kwargs["timeout"] for call in urlopen.call_args_list], [7, 7])
        sleep.assert_called_once_with(1)

    def test_measured_deadline_is_scored_without_calling_semantic_judge(self) -> None:
        spec = CaseSpec(
            case_id="deadline",
            family_id="family",
            suite="suite",
            split=Split.DEV,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            turns=[
                TurnSpec(turn_id="turn-1", query="q1", contract=TurnContract()),
                TurnSpec(turn_id="turn-2", query="q2", contract=TurnContract()),
            ],
        )
        run = CaseRun(
            case_id=spec.case_id,
            family_id=spec.family_id,
            split=spec.split,
            verdict=Verdict.FAIL,
            turns=[
                ObservedTurn(
                    turn_id="turn-1",
                    session_id="session",
                    error=SUT_RESPONSE_DEADLINE_EXCEEDED,
                ),
                ObservedTurn(
                    turn_id="turn-2",
                    session_id="session",
                    error=SUT_TURN_SKIPPED_AFTER_DEADLINE,
                ),
            ],
        )

        with patch("weknora_eval.judge._post_chat") as post_chat:
            scores = judge_case(spec, run)

        post_chat.assert_not_called()
        self.assertEqual({score.turn_id for score in scores}, {"turn-1", "turn-2"})
        self.assertTrue(all(score.value == "fail" for score in scores))
        self.assertTrue(all(score.metadata["confidence"] == 1.0 for score in scores))


if __name__ == "__main__":
    unittest.main()
