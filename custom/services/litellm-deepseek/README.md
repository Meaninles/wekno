# DeepSeek LiteLLM protocol normalizer

This LiteLLM deployment exposes both the short diagnostic name
`deepseek-v4-flash-int8` and WeKnora's canonical name
`/models/DeepSeek-V4-Flash-INT8` to Anthropic `/v1/messages` clients such as
Claude Agent SDK.

The private upstream currently exposes an Anthropic-compatible endpoint whose
stream can mix `text_delta` events into an open `tool_use` block. Passing that
stream through directly makes strict Anthropic clients fail with
`Content block is not a text block`.

The configuration therefore uses two LiteLLM deployments and a LiteLLM
streaming callback in one proxy process:

1. `deepseek-v4-flash-int8-anthropic-upstream` consumes the private Anthropic
   endpoint through LiteLLM's Chat Completions API and produces normalized
   OpenAI chunks.
2. `deepseek-v4-flash-int8` calls that internal deployment over loopback. The
   `/v1/messages` adapter then rebuilds a standards-compliant Anthropic stream.
3. `stream_normalizer.py` runs on LiteLLM's streaming-iterator extension point.
   For each tool call it retains arguments through the first complete JSON
   object, while dropping only the upstream's illegal text/argument suffix.

The callback must be mounted next to `/app/config.yaml` as
`/app/stream_normalizer.py`; LiteLLM loads it from
`litellm_settings.callbacks`.

The container must set
`LITELLM_USE_CHAT_COMPLETIONS_URL_FOR_ANTHROPIC_MESSAGES=true`; otherwise an
OpenAI-backed model may be routed through the Responses API instead of the
Chat Completions normalization path.

Required environment variables:

- `LITELLM_MASTER_KEY`
- `DEEPSEEK_ANTHROPIC_API_BASE`
- `DEEPSEEK_ANTHROPIC_API_KEY`
- `DEEPSEEK_OPENAI_API_BASE`
- `DEEPSEEK_OPENAI_API_KEY`
- `QWEN_API_BASE`
- `QWEN_API_KEY`
