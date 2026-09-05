import sys
import unittest
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from app.final_delivery import (  # noqa: E402
    CLAUDE_SDK_TERMINAL_CONTRACT,
    ClaudeSDKTerminalCollector,
    TERMINAL_ANSWER_CLOSE,
    TERMINAL_ANSWER_OPEN,
    project_terminal_answer,
    requires_passive_terminal_delivery,
    terminal_answer_integrity_reason,
    terminal_binding_marker,
    uses_claude_sdk_terminal_projection,
)
from app.runner import FINAL_ANSWER_SOURCE_CITATION_RULE  # noqa: E402
from app.schemas import ChatPayload, LLMConfig, RuntimeConfigSpec  # noqa: E402


@dataclass
class TextBlock:
    text: str


@dataclass
class ToolUseBlock:
    id: str
    name: str
    input: dict


@dataclass
class AssistantMessage:
    content: list
    message_id: str
    uuid: str


@dataclass
class ResultMessage:
    result: str | None
    subtype: str = "success"
    is_error: bool = False
    stop_reason: str = "end_turn"


class ClaudeSDKTerminalCollectorTest(unittest.TestCase):
    def payload(self, agent_type: str) -> ChatPayload:
        return ChatPayload(
            run_id="run-terminal-test",
            session_id="session-terminal-test",
            assistant_message_id="message-terminal-test",
            query="测试",
            runtime_config=RuntimeConfigSpec(agent_type=agent_type),
            llm=LLMConfig(model_name="test", api_key="test"),
            tool_callback_url="http://127.0.0.1/tool",
        )

    def test_only_normal_text_claude_agents_use_passive_collection(self):
        for agent_type in (
            "general-agent",
            "knowledge-base-manager",
            "document-processing-agent",
        ):
            self.assertTrue(requires_passive_terminal_delivery(agent_type), agent_type)
            self.assertTrue(uses_claude_sdk_terminal_projection(agent_type), agent_type)

        for agent_type in ("data-analysis", "table-analysis"):
            self.assertTrue(requires_passive_terminal_delivery(agent_type), agent_type)
            self.assertTrue(uses_claude_sdk_terminal_projection(agent_type), agent_type)

        for agent_type in ("rag-qa", "wiki-qa", "custom", ""):
            self.assertFalse(requires_passive_terminal_delivery(agent_type), agent_type)
            self.assertFalse(uses_claude_sdk_terminal_projection(agent_type), agent_type)

        self.assertEqual(CLAUDE_SDK_TERMINAL_CONTRACT, "claude-sdk-terminal-v3")

    def test_terminal_projection_hides_reasoning_around_envelope(self):
        raw = (
            "I should compare the sources first.\n"
            f"{TERMINAL_ANSWER_OPEN}\nDirect user answer.\n{TERMINAL_ANSWER_CLOSE}"
            "\nInternal post-check."
        )
        self.assertEqual(project_terminal_answer(raw), "Direct user answer.")
        self.assertEqual(project_terminal_answer(" plain provider fallback "), "plain provider fallback")

    def test_projection_strips_binding_and_whole_compatibility_wrapper(self):
        marker = terminal_binding_marker("run-42")
        self.assertEqual(
            project_terminal_answer(
                f"{marker}{TERMINAL_ANSWER_OPEN}Bound answer.{TERMINAL_ANSWER_CLOSE}"
            ),
            "Bound answer.",
        )
        self.assertEqual(
            project_terminal_answer(
                "<provider_final_response>Compatibility answer.</provider_final_response>"
            ),
            "Compatibility answer.",
        )
        self.assertEqual(
            project_terminal_answer(
                "<!-- weknora_final_response -->\nCompatibility answer without XML wrapper."
            ),
            "Compatibility answer without XML wrapper.",
        )

    def test_projection_preserves_user_defined_final_answer_xml(self):
        value = "<business_final_answer>Visible XML payload.</business_final_answer>"
        self.assertEqual(project_terminal_answer(value), value)

    def test_projection_removes_private_history_trailer(self):
        raw = (
            f"{TERMINAL_ANSWER_OPEN}Visible answer.\n"
            "</historical_assistant_output>private residue"
        )
        self.assertEqual(project_terminal_answer(raw), "Visible answer.")
        self.assertEqual(
            terminal_answer_integrity_reason("Visible answer.</user_source_ledger>"),
            "terminal_protocol_residue",
        )
        self.assertEqual(
            project_terminal_answer(f"{TERMINAL_ANSWER_OPEN}Visible.</user_request>"),
            "Visible.",
        )

    def test_collector_rejects_an_answer_bound_to_another_concurrent_run(self):
        collector = ClaudeSDKTerminalCollector(expected_binding="run-current")
        collector.observe(
            ResultMessage(
                f"{TERMINAL_ANSWER_OPEN}"
                f"{terminal_binding_marker('run-other')}Wrong conversation."
                f"{TERMINAL_ANSWER_CLOSE}"
            )
        )

        self.assertEqual(collector.answer(), "")
        self.assertEqual(collector.answer_integrity_reason, "terminal_binding_mismatch")

    def test_collector_accepts_only_the_matching_run_binding(self):
        collector = ClaudeSDKTerminalCollector(expected_binding="run-current")
        collector.observe(
            ResultMessage(
                f"{TERMINAL_ANSWER_OPEN}"
                f"{terminal_binding_marker('run-current')}Correct conversation."
                f"{TERMINAL_ANSWER_CLOSE}"
            )
        )

        self.assertEqual(collector.answer(), "Correct conversation.")
        self.assertEqual(collector.answer_integrity_reason, "")

    def test_successful_current_sdk_result_does_not_require_application_markers(self):
        collector = ClaudeSDKTerminalCollector(expected_binding="run-current")
        collector.observe(ResultMessage("Complete answer without a private marker."))

        self.assertEqual(collector.answer(), "Complete answer without a private marker.")
        self.assertEqual(collector.answer_integrity_reason, "")
        self.assertEqual(collector.answer_source, "native_result")

    def test_provider_end_turn_closes_bound_answer_without_optional_end_marker(self):
        collector = ClaudeSDKTerminalCollector(expected_binding="run-current")
        collector.observe(ResultMessage(
            f"{terminal_binding_marker('run-current')}{TERMINAL_ANSWER_OPEN}Complete answer."
        ))
        self.assertEqual(collector.answer(), "Complete answer.")
        self.assertEqual(collector.answer_integrity_reason, "")

    def test_unbound_partial_stream_is_not_accepted_without_success_result(self):
        collector = ClaudeSDKTerminalCollector(expected_binding="run-current")
        collector.observe(
            AssistantMessage(
                content=[TextBlock("Partial answer")],
                message_id="msg-partial",
                uuid="callback-partial",
            )
        )

        self.assertEqual(collector.answer(), "")
        self.assertEqual(collector.answer_integrity_reason, "terminal_binding_missing")

    def test_integrity_check_is_protocol_only_and_domain_independent(self):
        cases = {
            "": "empty_terminal_answer",
            "Useful prefix </weknora_final_placeholder>": "terminal_protocol_residue",
            "Supported claim <src id 'S1' />": "malformed_source_handle",
            "的。" * 80: "degenerate_repetition",
            "Now read the source " + "to get the full text " * 3: "degenerate_repetition",
            (
                "## Result\n\n---\n" +
                "** status" + (" " * 80) + "broken\n" +
                ("---\n**\n\n" * 14) +
                (" " * 80) + "fragment\n" +
                (" " * 80) + "tail"
            ): "degenerate_layout",
            'Supported.<src id="S7" />': "",
            "Retry once, retry twice, then report the evidence.": "",
            "| Field | Value |\n| --- | --- |\n| owner | pending |\n| date | pending |": "",
            (
                "## Long but valid report\n\n" +
                "\n".join(f"- Item {index}: supported explanation." for index in range(30))
            ): "",
        }
        for answer, expected in cases.items():
            with self.subTest(answer=answer[:40]):
                self.assertEqual(terminal_answer_integrity_reason(answer), expected)


    def test_structured_final_answer_uses_the_shared_source_handle(self):
        self.assertIn("source_references", FINAL_ANSWER_SOURCE_CITATION_RULE)
        self.assertIn("cite_exactly", FINAL_ANSWER_SOURCE_CITATION_RULE)
        self.assertIn('<src id="S1" />', FINAL_ANSWER_SOURCE_CITATION_RULE)

    def test_same_message_id_tool_use_invalidates_earlier_text_despite_new_uuid(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("我先查询制度。")],
                message_id="msg-operational",
                uuid="callback-text",
            )
        )
        self.assertEqual(collector.candidate_answer(), "我先查询制度。")

        collector.observe(
            AssistantMessage(
                content=[ToolUseBlock("tool-1", "lookup_policy", {"query": "制度"})],
                message_id="msg-operational",
                uuid="callback-tool",
            )
        )

        self.assertEqual(collector.epoch, 1)
        self.assertEqual(collector.candidate_answer(), "")
        self.assertIn("msg-operational", collector.operational_message_ids)

    def test_last_tool_followed_by_text_and_success_result_freezes_final_answer(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[ToolUseBlock("tool-1", "lookup_policy", {})],
                message_id="msg-tool",
                uuid="callback-tool",
            )
        )
        collector.observe(
            AssistantMessage(
                content=[TextBlock("这是工具之后的完整最终回答。")],
                message_id="msg-final",
                uuid="callback-final",
            )
        )
        collector.observe(ResultMessage("这是工具之后的完整最终回答。"))

        self.assertTrue(collector.frozen)
        self.assertTrue(collector.assistant_matches_result)
        self.assertEqual(collector.answer(), "这是工具之后的完整最终回答。")

    def test_gateway_reused_message_id_does_not_discard_post_tool_answer(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("我先查询制度。")],
                message_id="gateway-shared-message",
                uuid="callback-preamble",
            )
        )
        collector.observe(
            AssistantMessage(
                content=[ToolUseBlock("tool-1", "lookup_policy", {})],
                message_id="gateway-shared-message",
                uuid="callback-tool",
            )
        )
        collector.observe(
            AssistantMessage(
                content=[TextBlock("这是工具之后的完整最终回答。")],
                message_id="gateway-shared-message",
                uuid="callback-final",
            )
        )
        collector.observe(ResultMessage(None))

        self.assertTrue(collector.frozen)
        self.assertEqual(collector.candidate_answer(), "这是工具之后的完整最终回答。")
        self.assertEqual(collector.answer(), "这是工具之后的完整最终回答。")

    def test_success_result_is_authoritative_without_regeneration_on_mismatch(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("候选回答")],
                message_id="msg-final",
                uuid="callback-final",
            )
        )
        collector.observe(ResultMessage("SDK权威最终回答"))

        self.assertTrue(collector.frozen)
        self.assertFalse(collector.assistant_matches_result)
        self.assertEqual(collector.answer(), "SDK权威最终回答")
        self.assertEqual(collector.answer_source, "result")

    def test_corrupt_result_uses_valid_passive_assistant_candidate(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("这是完整、可读的最终回答。")],
                message_id="msg-final",
                uuid="callback-final",
            )
        )
        collector.observe(ResultMessage("的。" * 80))

        self.assertEqual(collector.answer(), "这是完整、可读的最终回答。")
        self.assertEqual(collector.answer_source, "assistant_candidate")
        self.assertEqual(collector.answer_integrity_reason, "")

    def test_success_result_projects_only_enveloped_answer(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("private notes")],
                message_id="msg-final",
                uuid="callback-final",
            )
        )
        collector.observe(
            ResultMessage(
                "先梳理来源。"
                f"{TERMINAL_ANSWER_OPEN}只展示这句。{TERMINAL_ANSWER_CLOSE}"
                "再检查一次。"
            )
        )

        self.assertEqual(collector.answer(), "只展示这句。")

    def test_success_result_without_result_field_uses_terminal_assistant_text(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[TextBlock("正常最终回答")],
                message_id="msg-final",
                uuid="callback-final",
            )
        )
        collector.observe(ResultMessage(None))

        self.assertTrue(collector.frozen)
        self.assertEqual(collector.answer(), "正常最终回答")

    def test_error_result_never_freezes_candidate(self):
        collector = ClaudeSDKTerminalCollector()
        collector.observe(
            AssistantMessage(
                content=[ToolUseBlock("tool-1", "lookup_policy", {})],
                message_id="msg-tool",
                uuid="callback-tool",
            )
        )
        collector.observe(
            ResultMessage(
                None,
                subtype="error_max_turns",
                is_error=True,
                stop_reason="tool_use",
            )
        )

        self.assertFalse(collector.frozen)
        self.assertEqual(collector.answer(), "")


if __name__ == "__main__":
    unittest.main()
