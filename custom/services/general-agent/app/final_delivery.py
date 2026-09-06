"""Identify SDK completion from typed messages without rewriting answer text."""
from dataclasses import dataclass
from typing import Any

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
    terminal_seen: bool = False
    frozen: bool = False
    terminal_result: str = ""
    terminal_stop_reason: str = ""
    answer_source: str = ""
    answer_integrity_reason: str = ""

    def observe(self, message: Any) -> None:
        if message.__class__.__name__ != "ResultMessage":
            return
        self.terminal_seen = True
        self.terminal_stop_reason = str(_value(message, "stop_reason", "") or "")
        self.terminal_result = str(_value(message, "result", "") or "")
        self.frozen = (_value(message, "subtype") == "success"
            and not _value(message, "is_error", False)
            and self.terminal_stop_reason in {"end_turn", "stop"})

    def answer(self) -> str:
        if not self.frozen:
            self.answer_integrity_reason = "incomplete_sdk_result"
            return ""
        if not self.terminal_result.strip():
            self.answer_integrity_reason = "empty_sdk_result"
            return ""
        self.answer_source = "typed_result"
        return self.terminal_result
