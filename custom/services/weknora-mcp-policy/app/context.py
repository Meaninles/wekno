"""Per-request authentication context.

HTTP requests are stateless.  The incoming tenant API key is kept only in a
ContextVar for the duration of the MCP request and is never put in logs or in
the process-wide configuration.
"""

from __future__ import annotations

from contextvars import ContextVar
from dataclasses import dataclass


@dataclass(frozen=True)
class RequestContext:
    api_key: str
    source: str


_request_context: ContextVar[RequestContext | None] = ContextVar(
    "weknora_mcp_request_context", default=None
)


def set_request_context(context: RequestContext):
    return _request_context.set(context)


def reset_request_context(token) -> None:
    _request_context.reset(token)


def current_request_context() -> RequestContext | None:
    return _request_context.get()


def require_api_key() -> str:
    context = current_request_context()
    if context is None or not context.api_key:
        raise PermissionError("a tenant API key is required")
    return context.api_key

