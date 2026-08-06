import sys
import unittest
from dataclasses import dataclass
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from app.final_delivery import (  # noqa: E402
    CLAUDE_SDK_TERMINAL_CONTRACT,
    ClaudeSDKTerminalCollector,
    requires_passive_terminal_delivery,
    uses_claude_sdk_terminal_projection,
)
from app.runner import FINAL_ANSWER_SOURCE_CITATION_RULE, runtime_summary, tool_catalog  # noqa: E402
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
            self.assertFalse(requires_passive_terminal_delivery(agent_type), agent_type)
            self.assertTrue(uses_claude_sdk_terminal_projection(agent_type), agent_type)

        for agent_type in ("rag-qa", "wiki-qa", "custom", ""):
            self.assertFalse(requires_passive_terminal_delivery(agent_type), agent_type)
            self.assertFalse(uses_claude_sdk_terminal_projection(agent_type), agent_type)

        self.assertEqual(CLAUDE_SDK_TERMINAL_CONTRACT, "claude-sdk-terminal-v1")

    def test_passive_delivery_does_not_add_model_visible_final_answer_tool(self):
        general = self.payload("general-agent")
        self.assertNotIn("final_answer", tool_catalog(general))
        self.assertNotIn("final_answer", runtime_summary(general))

        structured = self.payload("data-analysis")
        self.assertIn("final_answer", tool_catalog(structured))
        self.assertIn("final_answer", runtime_summary(structured))

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
