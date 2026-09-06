"""Build a reviewed adapter from the production snapshot; fail on source drift.

The snapshot and generated file stay outside Git. This is a source transformation,
not another runtime monkey-patch layer. Existing protocol and signature code stays
in place; generation policy is imported at its existing request boundaries.
"""
import argparse
import ast
import hashlib
from pathlib import Path

BASELINE_SHA256 = "b30a4a8f9b547906a906ed0914eb286daffd4ae9f1a4820be11a25c77afee326"


def replace_once(source, old, new):
    if source.count(old) != 1:
        raise ValueError(f"Expected exactly one integration point: {old[:100]!r}")
    return source.replace(old, new, 1)


def build(source):
    if hashlib.sha256(source.encode()).hexdigest() != BASELINE_SHA256:
        raise ValueError("Production adapter changed; review the new snapshot before building")
    source = replace_once(source, 'def _is_ds_v4_model(model):',
        'import generation_policy as _agent_policy\n\n'
        '_DS_V4_MODELS.update(name for name, profile in _agent_policy.ROUTES.items() if profile[0] == "deepseek0731")\n'
        '_QWEN36_27B_MODELS.update(name for name, profile in _agent_policy.ROUTES.items() if profile[0] == "qwen38")\n'
        '_QWEN36_27B_TOOL_WORKFLOW_MODELS.update(name for name, profile in _agent_policy.ROUTES.items() if profile[0] == "qwen38")\n\n'
        'def _is_ds_v4_model(model):')
    for signature in (
        '        def _patched_router_completion(self, model, messages, **kwargs):',
        '        async def _patched_router_acompletion(self, model, messages, stream=False, **kwargs):',
    ):
        source = replace_once(source, signature, signature + '\n            _agent_policy.apply(model, kwargs)')
    signature = '        def _patched_translate_thinking(self, anthropic_message_request, new_kwargs):'
    source = replace_once(source, signature, signature +
        '\n            if _agent_policy.translate_anthropic(anthropic_message_request, new_kwargs):\n                return')
    signature = '        def _process_anthropic_event(stream, event):'
    source = replace_once(source, signature, signature + '\n            terminal_error = _agent_policy.stream_terminal_error(stream, event)\n            if terminal_error is not None:\n                return terminal_error')
    for signature, stop in (("        def _patched_anthropic_next(self):", "StopIteration"),
                            ("        async def _patched_anthropic_anext(self):", "StopAsyncIteration")):
        source = replace_once(source, signature, signature +
            "\n            if getattr(self, '_agent_terminal_failed', False):\n                raise " + stop)
    source = replace_once(source,
        '            and output_config.get("effort") not in {"low", "medium", "high", "max"}',
        '            and not _agent_policy.anthropic_route(data)\n'
        '            and output_config.get("effort") not in {"low", "medium", "high", "max"}')
    source = replace_once(source,
        '                        try:\n                            return await endpoint(*endpoint_args, **endpoint_kwargs)\n                        finally:\n                            if qwen25omni_context_token is not None:',
        '                        policy_token = _agent_policy.route_context.set(data.get("model") if request is not None and data.get("model") in _agent_policy.ROUTES else None)\n'
        '                        try:\n                            return await endpoint(*endpoint_args, **endpoint_kwargs)\n                        finally:\n'
        '                            _agent_policy.route_context.reset(policy_token)\n'
        '                            if qwen25omni_context_token is not None:')
    source = replace_once(source,
        '            return translated\n\n        A.translate_anthropic_to_openai',
        '            _agent_policy.check_anthropic_response(translated)\n'
        '            return translated\n\n        A.translate_anthropic_to_openai')
    for signature in ('def _validate_ds_v4_anthropic_request(data):',
                      'def _validate_qwen36_27b_anthropic_request(data):'):
        source = replace_once(source, signature, signature + '\n    _agent_policy.validate(data, anthropic=True)')
    for signature in ('def _validate_ds_v4_openai_request(data, responses=False):',
                      'def _validate_qwen36_27b_openai_request(data, responses=False):'):
        source = replace_once(source, signature, signature + '\n    _agent_policy.validate(data)')
    ast.parse(source)
    return source


if __name__ == "__main__":
    parser = argparse.ArgumentParser()
    parser.add_argument("baseline", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    args.output.write_text(build(args.baseline.read_text(encoding="utf-8")), encoding="utf-8", newline="\n")
    print("Built adapter", hashlib.sha256(args.output.read_bytes()).hexdigest())
