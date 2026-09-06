"""Explicit agent routes; existing public routes retain their generation defaults.

This module has no transport, credentials, prompts, or model calls. Both API
protocols resolve their controls here before the existing provider adapters run.
Signature profiles deliberately remain owned by the existing protocol adapter.
"""
from copy import deepcopy
from contextvars import ContextVar

# LiteLLM rewrites the public alias to an internal deployment model before the
# Anthropic translator. Preserve request identity separately from that model.
route_context = ContextVar("agent_generation_route", default=None)


def anthropic_route(request):
    model = request.get("model")
    return model if model in ROUTES else route_context.get()


ROUTES = {
    "DeepSeek-V4-Flash-Agent": ("deepseek0731", "DeepSeek-V4-Flash", "high"),
    "Qwen3.8-27B-Agent": ("qwen38", "Qwen3.6-27B", "xhigh"),
}


def apply(model, kwargs):
    profile = ROUTES.get(model)
    if profile is None:
        return False
    kind, _, default_effort = profile
    extra = deepcopy(kwargs.get("extra_body") or {})
    template = deepcopy(extra.get("chat_template_kwargs") or {})
    explicit_template = kwargs.get("chat_template_kwargs") or {}
    for key, value in explicit_template.items():
        if key in template and template[key] != value:
            raise ValueError(f"Conflicting chat_template_kwargs.{key}")
        template[key] = value
    switches = [template[k] for k in ("thinking", "enable_thinking") if k in template]
    thinking = kwargs.get("thinking")
    if thinking is not None:
        if not isinstance(thinking, dict) or thinking.get("type") not in {"enabled", "adaptive", "disabled"}:
            raise ValueError("thinking.type must be enabled, adaptive, or disabled")
        switches.append(thinking["type"] != "disabled")
    if any(type(v) is not bool for v in switches) or len(set(switches)) > 1:
        raise ValueError("Conflicting or invalid thinking controls")
    efforts = [v for v in (kwargs.get("reasoning_effort"), template.get("reasoning_effort")) if v is not None]
    if len(set(efforts)) > 1:
        raise ValueError("Conflicting reasoning_effort controls")
    effort = efforts[0] if efforts else None
    enabled = switches[0] if switches else effort != "none"
    if effort == "none" and enabled:
        raise ValueError("reasoning_effort=none conflicts with thinking enabled")
    if not enabled and effort not in (None, "none"):
        raise ValueError("reasoning_effort requires thinking enabled")
    allowed = {"low", "high", "max"} if kind == "deepseek0731" else {"low", "medium", "xhigh"}
    if enabled:
        effort = effort or default_effort
        if effort not in allowed:
            raise ValueError(f"Unsupported {kind} reasoning_effort: {effort}")
    for key in ("thinking", "enable_thinking", "reasoning_effort"):
        template.pop(key, None)
    template["thinking" if kind == "deepseek0731" else "enable_thinking"] = enabled
    if enabled:
        # Keep the model's native effort in its chat template parameters,
        # independently of the provider's top-level reasoning_effort enum.
        template["reasoning_effort"] = effort
    if kind == "qwen38":
        template.setdefault("preserve_thinking", True)
        kwargs.update(temperature=1.0 if enabled else 0.7,
                      top_p=0.95 if enabled else 0.8, top_k=20, min_p=0.0,
                      presence_penalty=0.0 if enabled else 1.5, repetition_penalty=1.0)
    else:
        kwargs.update(temperature=1.0, top_p=0.95)
    extra["chat_template_kwargs"] = template
    kwargs["extra_body"] = extra
    for key in ("thinking", "reasoning_effort", "chat_template_kwargs"):
        kwargs.pop(key, None)
    return True


def translate_anthropic(request, kwargs):
    model = anthropic_route(request)
    if model not in ROUTES:
        return False
    controls = {key: deepcopy(request[key]) for key in ("thinking", "reasoning_effort", "chat_template_kwargs") if key in request}
    effort = (request.get("output_config") or {}).get("effort")
    if effort is not None:
        if "reasoning_effort" in controls and controls["reasoning_effort"] != effort:
            raise ValueError("Conflicting Anthropic output_config.effort")
        controls["reasoning_effort"] = effort
    # Native SDK effort names are preserved. No DS normalizer is applied to Qwen.
    controls["extra_body"] = deepcopy(kwargs.get("extra_body") or {})
    apply(model, controls)
    kwargs.update(controls)
    kwargs.pop("thinking", None)
    kwargs.pop("reasoning_effort", None)
    return True


def validate(request, anthropic=False):
    if not isinstance(request, dict):
        return
    model = anthropic_route(request) if anthropic else request.get("model")
    if model not in ROUTES:
        return
    if anthropic:
        translate_anthropic(request, {})
    else:
        apply(request["model"], deepcopy(request))


class TerminalEvidence:
    """Validate delivery without parsing or grading the answer's prose."""
    def __init__(self):
        self.text = False
        self.tool = False
        self.refusal = False

    def observe(self, event):
        block = event.get("content_block") or {}
        delta = event.get("delta") or {}
        self.text |= bool(str(block.get("text") or "").strip())
        self.text |= bool(str(delta.get("text") or "").strip())
        self.tool |= block.get("type") in {"tool_use", "server_tool_use"}
        self.refusal |= delta.get("type") == "refusal_delta"
        if event.get("type") == "message_delta" and delta.get("stop_reason") == "end_turn":
            if not (self.text or self.tool or self.refusal):
                raise ValueError("upstream_empty_terminal: model ended with no answer or tool call")


def observe_terminal(stream, event):
    if not isinstance(event, dict):
        return
    model = getattr(stream, "_llmgateway_requested_model", None) or getattr(stream, "model", "")
    if model not in ROUTES and model not in {"DeepSeek-V4-Flash", "DeepSeek-V4-Flash-INT8", "Qwen3.6-27B", "Qwen3.6-27B-tool", "Qwen3.8-27B"}:
        return
    evidence = getattr(stream, "_agent_terminal_evidence", None)
    if evidence is None:
        evidence = TerminalEvidence()
        stream._agent_terminal_evidence = evidence
    try:
        evidence.observe(event)
    except ValueError as exc:
        from litellm.exceptions import InternalServerError
        raise InternalServerError(message=str(exc), model=model, llm_provider="hosted_vllm") from exc


def stream_terminal_error(stream, event):
    """SSE is already HTTP 200: report errors in the Anthropic event protocol.

    Raising here makes LiteLLM terminate the body with an OpenAI-shaped error,
    which Claude SDK may interpret as a successful, empty stream.
    """
    try:
        observe_terminal(stream, event)
    except Exception as exc:
        if "upstream_empty_terminal" not in str(exc):
            raise
        stream._agent_terminal_failed = True
        return {"type": "error", "error": {"type": "api_error",
                "message": "upstream_empty_terminal: model ended with no answer or tool call"}}
    return None


def check_anthropic_response(response):
    data = response if isinstance(response, dict) else response.model_dump()
    class Response:
        model = data.get("model", "")
    state = Response()
    for block in data.get("content") or []:
        observe_terminal(state, {"type": "content_block_start", "content_block": block})
    observe_terminal(state, {"type": "message_delta", "delta": {"stop_reason": data.get("stop_reason")}})
