from __future__ import annotations

from collections import OrderedDict
from dataclasses import dataclass, field
import re
from typing import Any


CLAUDE_SDK_TERMINAL_CONTRACT = "claude-sdk-terminal-v3"
TERMINAL_ANSWER_OPEN = "<<<WEKNORA_USER_VISIBLE>>>"
TERMINAL_ANSWER_CLOSE = "<<<WEKNORA_USER_VISIBLE_END>>>"
LEGACY_TERMINAL_ANSWER_OPEN = "<weknora_final_response>"
LEGACY_TERMINAL_ANSWER_CLOSE = "</weknora_final_response>"
TERMINAL_CITATION_PATTERN = re.compile(r'<src\s+id="S[1-9][0-9]*"\s*/>')
TERMINAL_BINDING_PATTERN = re.compile(
    r'<!--\s*weknora-run-binding:([A-Za-z0-9._-]{1,128})\s*-->',
    re.IGNORECASE,
)
TERMINAL_PROTOCOL_COMMENT_PATTERN = re.compile(
    r'<!--\s*/?\s*weknora[_-]final[_-](?:response|answer)\s*-->',
    re.IGNORECASE,
)
PRIVATE_CONTEXT_TAG_PATTERN = re.compile(
    r"(?is)</?(?:historical_(?:assistant_output|user_input)|user_source_ledger|user_request|current_task_priority)\b[^>]*>"
)
GENERIC_FINAL_ENVELOPE_PATTERN = re.compile(
    r'(?is)^\s*<((?:weknora|provider|gateway)[_.:-]final[_-](?:response|answer))\b[^>]*>'
    r'(.*?)</\1\s*>\s*$',
)

CLAUDE_SDK_AGENT_TYPES = frozenset(
    {
        "general-agent",
        "knowledge-base-manager",
        "document-processing-agent",
        "data-analysis",
        "table-analysis",
    }
)

PASSIVE_TERMINAL_AGENT_TYPES = CLAUDE_SDK_AGENT_TYPES

def uses_claude_sdk_terminal_projection(agent_type: str) -> bool:
    return (agent_type or "").strip() in CLAUDE_SDK_AGENT_TYPES


def requires_passive_terminal_delivery(agent_type: str) -> bool:
    return (agent_type or "").strip() in PASSIVE_TERMINAL_AGENT_TYPES


def canonical_answer(content: Any) -> str:
    return str(content or "").strip()


def terminal_binding_marker(binding: str) -> str:
    value = str(binding or "").strip()
    if not value or not re.fullmatch(r"[A-Za-z0-9._-]{1,128}", value):
        return ""
    return f"<!-- weknora-run-binding:{value} -->"


def terminal_binding_integrity_reason(content: Any, expected_binding: str) -> str:
    expected = str(expected_binding or "").strip()
    if not expected:
        return ""
    matches = TERMINAL_BINDING_PATTERN.findall(str(content or ""))
    if not matches:
        return "terminal_binding_missing"
    if len(matches) != 1 or matches[0] != expected:
        return "terminal_binding_mismatch"
    return ""


def project_terminal_answer(content: Any) -> str:
    """Remove the model-only answer envelope without adding a repair turn.

    Providers that ignore the contract fail open to their normal terminal text.
    When the envelope is present, any planning or self-talk around it remains
    private and only its body can reach SSE, persistence, or later history.
    """

    raw = str(content or "")
    start = raw.find(TERMINAL_ANSWER_OPEN)
    if start < 0:
        legacy_start = raw.find(LEGACY_TERMINAL_ANSWER_OPEN)
        if legacy_start >= 0:
            body = raw[legacy_start + len(LEGACY_TERMINAL_ANSWER_OPEN) :]
            legacy_end = body.find(LEGACY_TERMINAL_ANSWER_CLOSE)
            if legacy_end >= 0:
                body = body[:legacy_end]
        else:
            # Compatibility gateways occasionally alter only the retired XML
            # wrapper namespace while preserving a valid complete response.
            generic = GENERIC_FINAL_ENVELOPE_PATTERN.fullmatch(raw)
            body = generic.group(2) if generic else raw
    else:
        body = raw[start + len(TERMINAL_ANSWER_OPEN) :]
        end = body.find(TERMINAL_ANSWER_CLOSE)
        if end >= 0:
            body = body[:end]
    body = TERMINAL_BINDING_PATTERN.sub("", body, count=1)
    # A few OpenAI-compatible gateways have rendered the private envelope as
    # an HTML comment.  It carries no user content, so remove it structurally
    # instead of leaking protocol text into chat/history.
    body = TERMINAL_PROTOCOL_COMMENT_PATTERN.sub("", body)
    private_context = PRIVATE_CONTEXT_TAG_PATTERN.search(body)
    if private_context:
        body = body[: private_context.start()]
    return body.strip()


def terminal_answer_integrity_reason(content: Any) -> str:
    """Return a protocol-only failure reason, never a semantic judgment."""

    answer = canonical_answer(content)
    if not answer:
        return "empty_terminal_answer"
    lowered = answer.lower()
    if any(
        marker in lowered
        for marker in (
            TERMINAL_ANSWER_OPEN.lower(),
            TERMINAL_ANSWER_CLOSE.lower(),
            "<weknora_",
            "</weknora_",
            "<historical_",
            "</historical_",
            "<user_source_ledger",
            "</user_source_ledger",
            "<user_request",
            "</user_request",
            "<current_task_priority",
            "</current_task_priority",
            "weknora_final_placeholder",
        )
    ):
        return "terminal_protocol_residue"
    without_valid_citations = TERMINAL_CITATION_PATTERN.sub("", answer)
    if "<src" in without_valid_citations.lower():
        return "malformed_source_handle"
    compact = "".join(answer.split())
    if _has_dominant_consecutive_repeat(compact):
        return "degenerate_repetition"
    if _has_degenerate_layout(answer):
        return "degenerate_layout"
    return ""


def _has_dominant_consecutive_repeat(value: str) -> bool:
    if len(value) < 24:
        return False
    for unit in range(1, min(64, len(value) // 3) + 1):
        required_repeats = 3 if unit >= 10 else 4 if unit >= 4 else 8
        for start in range(0, len(value) - unit * required_repeats + 1):
            fragment = value[start : start + unit]
            if unit >= 4 and len({char.casefold() for char in fragment if char.isalnum()}) < 2:
                continue
            repeats = 1
            while (
                start + (repeats + 1) * unit <= len(value)
                and value[start + repeats * unit : start + (repeats + 1) * unit]
                == fragment
            ):
                repeats += 1
            if repeats >= required_repeats and repeats * unit * 2 >= len(value):
                return True
    return False


def _has_degenerate_layout(value: str) -> bool:
    """Detect transport-corrupted padding/markup without judging semantics.

    A compatibility gateway can occasionally return hundreds of Markdown
    separators and large whitespace runs around a few broken token fragments.
    Normal prose, tables, code blocks, and intentionally sparse formatting do
    not contain several forty-character horizontal padding runs or dozens of
    formatting-only lines, so this remains a narrow transport integrity check.
    """

    if len(value) < 300:
        return False
    if len(re.findall(r"[ \t]{40,}", value)) >= 3:
        return True
    nonempty = [line.strip() for line in value.splitlines() if line.strip()]
    if len(nonempty) < 24:
        return False
    formatting_only = sum(
        1
        for line in nonempty
        if not re.search(r"[A-Za-z0-9\u3400-\u9fff]", line)
    )
    meaningful = sum(
        1
        for line in nonempty
        if len(re.findall(r"[A-Za-z0-9\u3400-\u9fff]", line)) >= 3
    )
    return formatting_only >= 12 and formatting_only * 2 >= len(nonempty) and meaningful * 3 < len(nonempty)


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
    answer_integrity_reason: str = ""
    answer_source: str = ""
    expected_binding: str = ""

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
        # ResultMessage.result remains authoritative when it is usable. Some
        # compatibility gateways occasionally corrupt only that aggregate while
        # the final text-only AssistantMessage is intact, so prefer that passive
        # candidate before asking the provider for another model turn.
        result_raw = self.terminal_result
        candidate_raw = self.candidate_answer()
        binding_mismatch = any(
            terminal_binding_integrity_reason(raw, self.expected_binding)
            == "terminal_binding_mismatch"
            for raw in (result_raw, candidate_raw)
            if raw
        )
        if binding_mismatch:
            self.answer_integrity_reason = "terminal_binding_mismatch"
            self.answer_source = "result" if result_raw else "assistant_candidate"
            return ""
        for source, raw in (
            ("result", result_raw),
            ("assistant_candidate", candidate_raw),
        ):
            binding_reason = terminal_binding_integrity_reason(raw, self.expected_binding)
            # The provider end_turn closes the answer. The optional close
            # delimiter only excludes trailing text when it is present.
            if self.expected_binding and (binding_reason or TERMINAL_ANSWER_OPEN not in raw):
                continue
            answer = project_terminal_answer(raw)
            reason = terminal_answer_integrity_reason(answer)
            if answer and not reason:
                self.answer_integrity_reason = ""
                self.answer_source = source
                return answer

        # The SDK stream itself owns this request. A successful provider result
        # need not reproduce an application marker to be deliverable. Reject an
        # explicitly different binding above, but do not turn optional framing
        # into a new model call or discard the provider's completed answer.
        if self.frozen:
            for source, raw in (("native_result", result_raw), ("native_assistant", candidate_raw)):
                answer = project_terminal_answer(raw)
                if answer and not terminal_answer_integrity_reason(answer):
                    self.answer_integrity_reason = ""
                    self.answer_source = source
                    return answer

        selected_raw = result_raw or candidate_raw
        selected = project_terminal_answer(selected_raw)
        if self.expected_binding:
            self.answer_integrity_reason = "terminal_binding_missing"
        else:
            self.answer_integrity_reason = terminal_answer_integrity_reason(selected)
        self.answer_source = "result" if result_raw else "assistant_candidate"
        # An incomplete response cannot be promoted to a completed answer.
        if self.answer_integrity_reason in {"terminal_binding_missing", "terminal_binding_mismatch"}:
            return ""
        return selected


class TerminalTextStream:
    """Project only the bound terminal text, including markers split across deltas."""
    def __init__(self, binding: str) -> None:
        self.binding = terminal_binding_marker(binding)
        self.pending = ""
        self.opened = False
        self.closed = False
        self.started = False

    def push(self, delta: str, final: bool = False) -> str:
        if self.closed:
            return ""
        self.pending += delta
        if not self.opened:
            start = self.pending.find(TERMINAL_ANSWER_OPEN)
            if start < 0 or self.binding not in self.pending[:start]:
                return ""
            self.pending = self.pending[start + len(TERMINAL_ANSWER_OPEN):]
            self.opened = True
        end = self.pending.find(TERMINAL_ANSWER_CLOSE)
        if end >= 0:
            self.pending = self.pending[:end]
            final = True
            self.closed = True
        # Withhold a possible closing-marker prefix and trailing whitespace.
        keep = 0 if final else len(TERMINAL_ANSWER_CLOSE) - 1
        split = max(0, len(self.pending) - keep)
        candidate = self.pending[:split].rstrip()
        self.pending = self.pending[len(candidate):]
        if not self.started:
            candidate = candidate.lstrip()
        if candidate:
            self.started = True
        return candidate
