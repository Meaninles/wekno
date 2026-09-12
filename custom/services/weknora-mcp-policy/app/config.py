"""JSONC configuration and hot-reloadable tool policy.

The policy is deliberately a small JSONC file instead of an environment
variable.  Operators can remove one tool per line, keep the change under
version control, and have both stateless MCP replicas observe it without a
restart.
"""

from __future__ import annotations

import json
import os
from dataclasses import dataclass
from pathlib import Path
from threading import RLock
from typing import FrozenSet, Iterable


class ConfigError(ValueError):
    """Raised when the MCP policy file is missing or invalid."""


def strip_jsonc(text: str) -> str:
    """Remove // and /* */ comments while preserving string contents."""

    result: list[str] = []
    index = 0
    in_string = False
    escaped = False
    in_line_comment = False
    in_block_comment = False

    while index < len(text):
        char = text[index]
        next_char = text[index + 1] if index + 1 < len(text) else ""

        if in_line_comment:
            if char in "\r\n":
                in_line_comment = False
                result.append(char)
            else:
                result.append(" ")
            index += 1
            continue

        if in_block_comment:
            if char == "*" and next_char == "/":
                in_block_comment = False
                result.extend((" ", " "))
                index += 2
            else:
                result.append(char if char in "\r\n" else " ")
                index += 1
            continue

        if in_string:
            result.append(char)
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
            index += 1
            continue

        if char == '"':
            in_string = True
            result.append(char)
            index += 1
            continue

        if char == "/" and next_char == "/":
            in_line_comment = True
            result.extend((" ", " "))
            index += 2
            continue

        if char == "/" and next_char == "*":
            in_block_comment = True
            result.extend((" ", " "))
            index += 2
            continue

        result.append(char)
        index += 1

    if in_block_comment:
        raise ConfigError("unterminated block comment")
    if in_string:
        raise ConfigError("unterminated JSON string")
    return "".join(result)


def load_enabled_tools(path: Path, known_tools: Iterable[str]) -> FrozenSet[str]:
    """Load and validate the enabled tool names from a JSONC file."""

    try:
        raw_text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise ConfigError(f"cannot read policy file {path}: {exc.strerror or exc}") from exc

    try:
        document = json.loads(strip_jsonc(raw_text))
    except json.JSONDecodeError as exc:
        raise ConfigError(f"invalid JSONC policy file {path}: {exc.msg} at line {exc.lineno}") from exc

    if not isinstance(document, dict):
        raise ConfigError("policy root must be a JSON object")
    enabled = document.get("enabled_tools")
    if not isinstance(enabled, list):
        raise ConfigError("policy field 'enabled_tools' must be an array")
    if any(not isinstance(name, str) or not name.strip() for name in enabled):
        raise ConfigError("every enabled_tools item must be a non-empty string")

    normalized = frozenset(name.strip() for name in enabled)
    unknown = sorted(normalized.difference(set(known_tools)))
    if unknown:
        raise ConfigError("unknown tool(s) in enabled_tools: " + ", ".join(unknown))
    return normalized


def default_policy_path() -> Path:
    configured = os.getenv("WEKNORA_MCP_TOOLS_FILE") or os.getenv("WEKNORA_MCP_CONFIG")
    if configured:
        return Path(configured)
    return Path("/app/config/weknora-mcp.config.jsonc")


@dataclass(frozen=True)
class PolicySnapshot:
    enabled_tools: FrozenSet[str]
    path: Path
    mtime_ns: int
    size: int


class ToolPolicy:
    """Thread-safe, mtime-based policy cache shared by one MCP replica."""

    def __init__(self, path: Path, known_tools: Iterable[str]):
        self.path = path
        self.known_tools = frozenset(known_tools)
        self._lock = RLock()
        self._snapshot: PolicySnapshot | None = None

    def snapshot(self) -> PolicySnapshot:
        try:
            stat = self.path.stat()
        except OSError as exc:
            raise ConfigError(f"cannot stat policy file {self.path}: {exc.strerror or exc}") from exc

        with self._lock:
            cached = self._snapshot
            if cached and cached.mtime_ns == stat.st_mtime_ns and cached.size == stat.st_size:
                return cached

            enabled = load_enabled_tools(self.path, self.known_tools)
            self._snapshot = PolicySnapshot(
                enabled_tools=enabled,
                path=self.path,
                mtime_ns=stat.st_mtime_ns,
                size=stat.st_size,
            )
            return self._snapshot

    def is_enabled(self, name: str) -> bool:
        return name in self.snapshot().enabled_tools

    def enabled_count(self) -> int:
        return len(self.snapshot().enabled_tools)

