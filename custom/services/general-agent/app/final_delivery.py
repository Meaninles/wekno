from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass, field
from typing import Any


CLAUDE_SDK_TERMINAL_CONTRACT = "claude-sdk-terminal-v1"

CLAUDE_SDK_AGENT_TYPES = frozenset(
    {
        "general-agent",
        "knowledge-base-manager",
        "document-processing-agent",
        "data-analysis",
        "table-analysis",
    }
)

# Structured analysis already has its own model-visible final_answer tool and
# chart validation contract. The passive collector is intentionally limited to
# Claude SDK agents that previously ended with normal assistant text.
PASSIVE_TERMINAL_AGENT_TYPES = frozenset(
    {
        "general-agent",
        "knowledge-base-manager",
        "document-processing-agent",
    }
)


def uses_claude_sdk_terminal_projection(agent_type: str) -> bool:
    return (agent_type or "").strip() in CLAUDE_SDK_AGENT_TYPES


def requires_passive_terminal_delivery(agent_type: str) -> bool:
    return (agent_type or "").strip() in PASSIVE_TERMINAL_AGENT_TYPES


def canonical_answer(content: Any) -> str:
    return str(content or "").strip()


def _value(block: Any, name: str, default: Any = None) -> Any:
    if isinstance(block, dict):
        return block.get(name, default)
    return getattr(block, name, default)


def _kind(block: Any) -> str:
    if isinstance(block, dict):
        return str(block.get("type") or "")
    return block.__class__.__name__


@dataclass
class ClaudeSDKTerminalCollector:
    """Passively identify the terminal assistant answer from raw SDK messages.

    Claude SDK may emit text and ToolUse callbacks with different callback UUIDs
    but the same underlying message_id. A message that ever contains a tool is
    operational, so any text previously observed for that message is discarded.
    No prompt, tool, hook or model turn is added by this collector.
    """

    epoch: int = 0
    candidates: OrderedDict[str, str] = field(default_factory=OrderedDict)
    operational_message_ids: set[str] = field(default_factory=set)
    terminal_result: str = ""
    terminal_subtype: str = ""
    terminal_stop_reason: str = ""
    terminal_is_error: bool = False
    terminal_seen: bool = False
    frozen: bool = False
    assistant_matches_result: bool | None = None

    def observe(self, message: Any) -> None:
        message_type = message.__class__.__name__
        if message_type == "AssistantMessage":
            self._observe_assistant(message)
        elif message_type == "ResultMessage":
            self._observe_result(message)

    def _observe_assistant(self, message: Any) -> None:
        content = _value(message, "content", [])
        blocks = content if isinstance(content, list) else []
        text = "".join(
            str(_value(block, "text", "") or "")
            for block in blocks
            if _kind(block) in {"TextBlock", "text"}
        )
        has_tool = any(
            _kind(block)
            in {
                "ToolUseBlock",
                "ServerToolUseBlock",
                "tool_use",
                "server_tool_use",
            }
            for block in blocks
        )
        message_id = str(_value(message, "message_id", "") or "").strip()
        callback_uuid = str(_value(message, "uuid", "") or "").strip()
        message_key = message_id or callback_uuid or f"anonymous-{self.epoch}-{len(self.candidates) + 1}"

        if has_tool:
            self.epoch += 1
            self.operational_message_ids.add(message_key)
            self.candidates.clear()
            return

        # Some OpenAI/Anthropic compatibility gateways reuse one provider
        # message_id across several assistant turns.  The tool-bearing
        # callback above already clears every candidate observed before that
        # tool boundary, so permanently rejecting the reused id would also
        # discard the legitimate text-only answer from the next turn.  Keep
        # operational_message_ids as diagnostics, but scope invalidation to
        # the callback that actually contains the tool.
        if text:
            self.candidates[message_key] = self.candidates.get(message_key, "") + text

    def _observe_result(self, message: Any) -> None:
        self.terminal_seen = True
        self.terminal_subtype = str(_value(message, "subtype", "") or "").strip()
        self.terminal_stop_reason = str(_value(message, "stop_reason", "") or "").strip()
        self.terminal_is_error = bool(_value(message, "is_error", False))
        self.terminal_result = canonical_answer(_value(message, "result", ""))

        candidate = self.candidate_answer()
        self.assistant_matches_result = (
            candidate == self.terminal_result
            if candidate and self.terminal_result
            else None
        )
        self.frozen = (
            self.terminal_subtype == "success"
            and not self.terminal_is_error
            and self.terminal_stop_reason == "end_turn"
        )

    def candidate_answer(self) -> str:
        return canonical_answer("".join(self.candidates.values()))

    def answer(self) -> str:
        # ResultMessage.result is authoritative on a normal SDK completion.
        # A candidate fallback keeps an otherwise normal answer visible on
        # providers that omit the optional result field. Mismatches are
        # diagnostic only and never trigger a second model response.
        if self.terminal_result:
            return self.terminal_result
        return self.candidate_answer()
