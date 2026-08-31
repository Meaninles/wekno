from __future__ import annotations

import json
import unittest
from unittest.mock import patch

from weknora_eval.assistance import (
    BoundedEvalAssistant,
    EvalAssistantConfig,
    _asks_for_external_evidence,
)
from weknora_eval.models import AnswerSnapshot, CaseSetup


class SearchClient:
    def __init__(self) -> None:
        self.calls: list[tuple[str, list[str], list[str]]] = []

    def search_knowledge(
        self,
        query: str,
        *,
        knowledge_base_ids: list[str],
        knowledge_ids: list[str],
    ) -> list[dict[str, object]]:
        self.calls.append((query, knowledge_base_ids, knowledge_ids))
        return [
            {
                "id": "fragment-1",
                "evidence_content": "审批窗口为两个工作日。",
                "metadata": {"chunk_id": "fragment-1"},
            }
        ]


class StubAssistant(BoundedEvalAssistant):
    def __init__(self, client: SearchClient, outputs: list[object]) -> None:
        super().__init__(
            client,  # type: ignore[arg-type]
            EvalAssistantConfig("http://editor", "secret", "editor", max_attempts=2),
        )
        self.outputs = list(outputs)
        self.rewrite_calls = 0

    def _rewrite(self, **_kwargs: object) -> str:
        self.rewrite_calls += 1
        output = self.outputs.pop(0)
        if isinstance(output, Exception):
            raise output
        return str(output)


class AssistanceTests(unittest.TestCase):
    def test_negated_retrieval_is_not_a_positive_search_request(self) -> None:
        self.assertFalse(_asks_for_external_evidence("不要检索知识库，只分析为什么不能执行。"))
        self.assertFalse(_asks_for_external_evidence("Do not search; explain why it cannot run."))
        self.assertTrue(_asks_for_external_evidence("不要检索旧库；请搜索新库并给出处。"))

    def test_no_generic_trigger_keeps_production_candidate_unchanged(self) -> None:
        client = SearchClient()
        assistant = StubAssistant(client, ["should not be used"])
        production = AnswerSnapshot(content="这是对原因的分析。")

        assisted, trace = assistant.assist(
            query="不要检索知识库，只分析为什么不能执行。",
            prior_user_statements=[],
            setup=CaseSetup(knowledge_base_ids=["kb"]),
            production=production,
            max_response_chars=None,
            force=False,
        )

        self.assertEqual(assisted, production)
        self.assertFalse(trace.triggered)
        self.assertEqual(client.calls, [])
        self.assertEqual(assistant.rewrite_calls, 0)

    def test_assistance_is_bounded_and_records_extra_work(self) -> None:
        client = SearchClient()
        assistant = StubAssistant(client, ['审批窗口为两个工作日。<src id="S1" />'])

        assisted, trace = assistant.assist(
            query="请依据文档说明审批窗口。",
            prior_user_statements=["范围只包括线上申请。"],
            setup=CaseSetup(knowledge_base_ids=["kb"]),
            production=AnswerSnapshot(content="暂时无法确认。"),
            max_response_chars=100,
            force=False,
        )

        self.assertEqual(len(client.calls), 1)
        self.assertEqual(assistant.rewrite_calls, 1)
        self.assertIn('<src id="S1" />', assisted.content)
        self.assertTrue(trace.triggered)
        self.assertTrue(trace.succeeded)
        self.assertEqual(trace.attempts, 1)
        self.assertEqual(trace.added_model_calls, 1)
        self.assertEqual(trace.added_tool_calls, 1)
        self.assertEqual(
            trace.repair_types, ["supplemental_retrieval", "terminal_rewrite"]
        )

    def test_two_failed_rewrites_fail_open_to_production(self) -> None:
        client = SearchClient()
        assistant = StubAssistant(
            client,
            ['无效引用 <src id="S9" />', '仍无效 <src id="S8" />'],
        )
        production = AnswerSnapshot(content="原始生产答案")

        assisted, trace = assistant.assist(
            query="请给引用来源。",
            prior_user_statements=[],
            setup=CaseSetup(),
            production=production,
            max_response_chars=None,
            force=False,
        )

        self.assertEqual(assisted, production)
        self.assertTrue(trace.triggered)
        self.assertFalse(trace.succeeded)
        self.assertEqual(trace.attempts, 2)
        self.assertEqual(trace.added_model_calls, 2)
        self.assertEqual(trace.added_tool_calls, 0)
        self.assertIn("unknown citation IDs", trace.failure_reason)

    def test_editor_prompt_contains_only_user_evidence_and_production_inputs(self) -> None:
        client = SearchClient()
        assistant = BoundedEvalAssistant(
            client,  # type: ignore[arg-type]
            EvalAssistantConfig("http://editor", "secret", "editor", max_attempts=1),
        )
        captured: dict[str, object] = {}

        class Response:
            def __enter__(self):
                return self

            def __exit__(self, *_args: object) -> None:
                return None

            def read(self) -> bytes:
                return json.dumps(
                    {"choices": [{"message": {"content": "仅保留用户事实。"}}]}
                ).encode()

        def fake_open(request, **_kwargs: object):
            captured["body"] = json.loads(request.data.decode("utf-8"))
            return Response()

        with patch("weknora_eval.assistance.urllib.request.urlopen", side_effect=fake_open):
            result = assistant._rewrite(
                query=(
                    "只总结用户事实。\n"
                    "[WEKNORA_CURRENT_TURN_EXECUTION_V1]\n"
                    '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["隐藏检索词"]'
                ),
                prior_user_statements=["负责人仍待确认。"],
                draft="负责人未知。",
                references=[],
                max_response_chars=None,
            )

        self.assertEqual(result, "仅保留用户事实。")
        body = captured["body"]
        rendered = json.dumps(body, ensure_ascii=False)
        self.assertIn("只总结用户事实", rendered)
        self.assertIn("负责人仍待确认", rendered)
        self.assertNotIn("WEKNORA_CURRENT_TURN_EXECUTION", rendered)
        self.assertNotIn("隐藏检索词", rendered)
        self.assertNotIn("required_claims", rendered)
        self.assertNotIn("reference_answer", rendered)


if __name__ == "__main__":
    unittest.main()
