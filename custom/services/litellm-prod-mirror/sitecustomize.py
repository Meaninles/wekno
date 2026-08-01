try:
    from litellm.llms.anthropic.experimental_pass_through.adapters.transformation import LiteLLMAnthropicMessagesAdapter as A
    _orig = A._translate_thinking_to_openai
    def _patched(self, anthropic_message_request, new_kwargs):
        _orig(self, anthropic_message_request, new_kwargs)
        thinking = anthropic_message_request.get("thinking")
        if isinstance(thinking, dict) and thinking.get("type") == "enabled":
            # 注入 enable_thinking
            eb = dict(new_kwargs.get("extra_body") or {})
            ctk = dict(eb.get("chat_template_kwargs") or {})
            ctk["enable_thinking"] = True
            eb["chat_template_kwargs"] = ctk
            new_kwargs["extra_body"] = eb
            new_kwargs["chat_template_kwargs"] = ctk
            # 删除 reasoning_effort(vllm只认high/max,low会400报错)
            new_kwargs.pop("reasoning_effort", None)
    A._translate_thinking_to_openai = _patched
except Exception as _e:
    import sys; print(f"[patch] FAILED: {_e}", file=sys.stderr)
