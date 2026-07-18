"""Normalize malformed tool-call suffixes from the private DeepSeek stream.

The upstream may emit a valid tool call followed by a text newline and a
second arguments fragment containing ``\"\"``.  That sequence cannot be
represented as one valid OpenAI tool call and makes LiteLLM create extra
Anthropic content blocks.  Claude Agent SDK then rejects the stream.

This callback runs on LiteLLM's OpenAI-format streaming iterator.  It keeps
all content before the first tool call and all arguments through the first
complete JSON object for every tool call.  Only content and argument suffixes
after that point are discarded.
"""

from __future__ import annotations

import json
from typing import Any, AsyncGenerator, Dict, Iterable, Optional, Tuple

from litellm.integrations.custom_logger import CustomLogger


_TARGET_MODEL_MARKERS = (
    "deepseek-v4-flash-int8-anthropic-upstream",
    "/models/deepseek-v4-flash-int8",
)


class _ToolCallState:
    def __init__(self) -> None:
        self.arguments = ""
        self.complete = False


def _value(obj: Any, name: str, default: Any = None) -> Any:
    if isinstance(obj, dict):
        return obj.get(name, default)
    return getattr(obj, name, default)


def _assign(obj: Any, name: str, value: Any) -> None:
    if isinstance(obj, dict):
        obj[name] = value
    else:
        setattr(obj, name, value)


def _model_candidates(request_data: dict, chunk: Any = None) -> Iterable[str]:
    metadata = request_data.get("metadata") or {}
    litellm_params = request_data.get("litellm_params") or {}
    for candidate in (
        request_data.get("model"),
        request_data.get("litellm_model_name"),
        metadata.get("model_group"),
        metadata.get("model_info", {}).get("id") if isinstance(metadata.get("model_info"), dict) else None,
        litellm_params.get("model"),
        _value(chunk, "model"),
    ):
        if candidate is not None:
            yield str(candidate).lower()


def _is_target_stream(request_data: dict, chunk: Any = None) -> bool:
    return any(
        marker in candidate
        for candidate in _model_candidates(request_data, chunk)
        for marker in _TARGET_MODEL_MARKERS
    )


def _complete_json_prefix(value: str) -> Optional[int]:
    """Return the end offset of one complete JSON object, ignoring whitespace."""

    leading = len(value) - len(value.lstrip())
    stripped = value[leading:]
    if not stripped:
        return None
    try:
        parsed, end = json.JSONDecoder().raw_decode(stripped)
    except json.JSONDecodeError:
        return None
    # OpenAI function arguments are required to be a JSON object.  Waiting for
    # an object also avoids treating a transient string token as completion.
    if not isinstance(parsed, dict):
        return None
    return leading + end


def _normalize_argument_fragment(state: _ToolCallState, fragment: str) -> Optional[str]:
    if not fragment:
        return fragment
    if state.complete:
        return None

    previous_length = len(state.arguments)
    combined = state.arguments + fragment
    complete_at = _complete_json_prefix(combined)
    if complete_at is None:
        state.arguments = combined
        return fragment

    state.arguments = combined[:complete_at]
    state.complete = True
    keep_from_fragment = max(0, complete_at - previous_length)
    return fragment[:keep_from_fragment]


def _normalize_chunk(
    chunk: Any,
    states: Dict[Tuple[int, int], _ToolCallState],
) -> Any:
    for choice_position, choice in enumerate(_value(chunk, "choices", []) or []):
        choice_index = int(_value(choice, "index", choice_position) or 0)
        delta = _value(choice, "delta")
        if delta is None:
            continue

        had_tool_call = any(key[0] == choice_index for key in states)
        content = _value(delta, "content")
        if had_tool_call and content:
            _assign(delta, "content", None)

        tool_calls = _value(delta, "tool_calls") or []
        normalized_calls = []
        for tool_position, tool_call in enumerate(tool_calls):
            tool_index = int(_value(tool_call, "index", tool_position) or 0)
            key = (choice_index, tool_index)
            state = states.setdefault(key, _ToolCallState())
            function = _value(tool_call, "function")
            if function is None:
                normalized_calls.append(tool_call)
                continue

            fragment = _value(function, "arguments", "") or ""
            normalized_fragment = _normalize_argument_fragment(state, str(fragment))
            if normalized_fragment is None:
                continue
            if normalized_fragment != fragment:
                _assign(function, "arguments", normalized_fragment)
            normalized_calls.append(tool_call)

        if tool_calls and len(normalized_calls) != len(tool_calls):
            _assign(delta, "tool_calls", normalized_calls)

    return chunk


def _has_stream_payload(chunk: Any) -> bool:
    """Keep data-bearing chunks and discard deltas emptied by normalization."""

    usage = _value(chunk, "usage")
    if usage:
        return True
    choices = _value(chunk, "choices", []) or []
    if not choices:
        return True
    for choice in choices:
        if _value(choice, "finish_reason") is not None or _value(choice, "logprobs") is not None:
            return True
        delta = _value(choice, "delta")
        if delta is None:
            continue
        for field in (
            "role",
            "content",
            "tool_calls",
            "function_call",
            "reasoning_content",
            "thinking_blocks",
        ):
            if _value(delta, field):
                return True
    return False


class DeepSeekToolStreamNormalizer(CustomLogger):
    async def async_post_call_streaming_iterator_hook(
        self,
        user_api_key_dict: Any,
        response: Any,
        request_data: dict,
    ) -> AsyncGenerator[Any, None]:
        states: Dict[Tuple[int, int], _ToolCallState] = {}
        target_stream = _is_target_stream(request_data)

        async for chunk in response:
            target_stream = target_stream or _is_target_stream(request_data, chunk)
            if target_stream:
                normalized = _normalize_chunk(chunk, states)
                if _has_stream_payload(normalized):
                    yield normalized
            else:
                yield chunk


stream_normalizer = DeepSeekToolStreamNormalizer()
