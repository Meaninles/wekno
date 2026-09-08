"""Classify private exceptions using the same public vocabulary as API and UI."""
from __future__ import annotations

import json
from pathlib import Path

from .budget import BudgetExhausted
from .control import control_cause
import httpx
import openai
import anthropic

_path = Path(__file__).with_name("public_error_catalog.json")
if not _path.exists():
    _path = Path(__file__).resolve().parents[4] / "internal/custom/modules/usererrors/catalog.json"
_catalog = json.loads(_path.read_text(encoding="utf-8"))


def error_code(error: BaseException) -> str:
    """Return only a known code; never serialize provider text or credentials."""
    seen: set[int] = set()
    cause = control_cause(error)
    if cause:
        if cause.code in ("finalization_required", "task_limit"):
            return "task_limit"
        if any(item['code'] == cause.code for item in _catalog):
            return cause.code

    def classify(exc: BaseException) -> str:
        if id(exc) in seen:
            return "unknown"
        seen.add(id(exc))
        if isinstance(exc, BudgetExhausted):
            return "task_limit"
        if isinstance(exc, (TimeoutError, httpx.TimeoutException, openai.APITimeoutError, anthropic.APITimeoutError)):
            return "timeout"
        if isinstance(exc, (httpx.NetworkError, openai.APIConnectionError, anthropic.APIConnectionError)):
            return "connection"
        for child in getattr(exc, "exceptions", ()):
            code = classify(child)
            if code != "unknown":
                return code
        detail = f"{type(exc).__name__} {exc}".lower()
        for item in _catalog:
            if any(pattern in detail for pattern in item["patterns"]):
                return item["code"]
        cause = exc.__cause__ or exc.__context__
        return classify(cause) if cause else "unknown"

    return classify(error)
