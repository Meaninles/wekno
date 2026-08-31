from __future__ import annotations

import unittest
from types import SimpleNamespace
from unittest.mock import patch

from weknora_eval.client import (
    WeKnoraClient,
    WeKnoraResponseDeadlineExceeded,
    streamed_production_candidate,
)


class FakeSocket:
    def __init__(self) -> None:
        self.timeouts: list[float] = []

    def settimeout(self, value: float) -> None:
        self.timeouts.append(value)


class FakeStreamResponse:
    def __init__(self, clock: list[float]) -> None:
        self.clock = clock
        self.lines = iter(
            [
                b'data: {"response_type":"message","data":"partial"}\n',
                b"\n",
                b": keepalive\n",
            ]
        )
        self.socket = FakeSocket()
        self.fp = SimpleNamespace(raw=SimpleNamespace(_sock=self.socket))

    def __enter__(self) -> "FakeStreamResponse":
        return self

    def __exit__(self, *_args: object) -> None:
        return None

    def close(self) -> None:
        return None

    def readline(self) -> bytes:
        self.clock[0] += 0.4
        return next(self.lines, b"")


class ClientTests(unittest.TestCase):
    def test_streamed_candidate_matches_active_answer_segments(self) -> None:
        events = [
            {"id": "draft", "response_type": "answer", "content": "I will check."},
            {
                "response_type": "tool_call",
                "data": {"tool_name": "knowledge_search", "preserve_answer": False},
            },
            {"id": "final", "response_type": "answer", "content": "Grounded "},
            {"id": "final", "response_type": "answer", "content": "answer."},
            {
                "response_type": "tool_call",
                "data": {"tool_name": "create_artifact", "preserve_answer": True},
            },
            {
                "response_type": "complete",
                "data": {"final_answer": "a different hidden rewrite"},
            },
        ]

        self.assertEqual(
            streamed_production_candidate(events),
            "Grounded answer.",
        )

    def test_completion_is_fallback_only_when_no_answer_event_exists(self) -> None:
        self.assertEqual(
            streamed_production_candidate(
                [
                    {
                        "response_type": "complete",
                        "data": {"final_answer": "fallback"},
                    }
                ]
            ),
            "fallback",
        )
        self.assertIsNone(streamed_production_candidate([]))

    def test_stream_timeout_is_a_total_wall_deadline_not_idle_timeout(self) -> None:
        clock = [0.0]
        response = FakeStreamResponse(clock)
        client = WeKnoraClient("http://weknora", "key", timeout=1.0)

        with (
            patch("weknora_eval.client.time.perf_counter", side_effect=lambda: clock[0]),
            patch("weknora_eval.client.urllib.request.urlopen", return_value=response),
        ):
            with self.assertRaises(WeKnoraResponseDeadlineExceeded) as raised:
                client.stream("/agent-chat/session", {"query": "q"})

        self.assertEqual(len(raised.exception.events), 1)
        self.assertEqual(raised.exception.ttfb_ms, 800)
        self.assertGreaterEqual(raised.exception.total_latency_ms, 1200)
        self.assertEqual(len(response.socket.timeouts), 3)
        self.assertAlmostEqual(response.socket.timeouts[-1], 0.2)

    def test_non_positive_deadline_is_rejected(self) -> None:
        with self.assertRaises(ValueError):
            WeKnoraClient("http://weknora", "key", timeout=0)


if __name__ == "__main__":
    unittest.main()
