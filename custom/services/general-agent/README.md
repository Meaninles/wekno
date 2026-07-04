# WeKnora General Agent Sidecar

This service hosts the Claude Agent SDK loop for `agent_type=general-agent`.

Boundary:

- The sidecar does not connect to WeKnora databases, object storage, MCP services, or data sources directly.
- WeKnora Go backend sends the allowed tool schemas for each run.
- Tool execution is always called back to Go through `/api/v1/custom/general-agent/internal/tools/call`.
- Artifact creation is controlled only by `enable_artifacts`. Claude is told: at most 5 artifacts, total size < 128MB, important files first. The sidecar enforces the same rule by ordered truncation before frontend persistence.

Local Docker:

```bash
docker compose -f custom/docker-compose.general-agent.yml up -d --build
```

Health:

```bash
curl http://127.0.0.1:8091/health
```

Key environment variables:

- `CUSTOM_GENERAL_AGENT_API_KEY`: shared secret between Go and sidecar.
- `GENERAL_AGENT_RUN_ROOT`: sidecar run directory.
- `CUSTOM_GENERAL_AGENT_CLAUDE_API_TIMEOUT_MS`: LLM request timeout.
- `CUSTOM_GENERAL_AGENT_CLAUDE_IDLE_TIMEOUT_MS`: streaming idle timeout.
- `CUSTOM_GENERAL_AGENT_MAX_TURNS`: fallback max turns when agent config does not set `max_iterations`.
- Claude SDK model name, Anthropic-compatible endpoint, and API key are resolved from the selected model in WeKnora model management. Configure `extra_config.general_agent_claude_base_url` on generic/local models that need a dedicated Anthropic-compatible endpoint; the selected model's encrypted API key is reused when configured. If that model intentionally has no API key, Go marks the run as `api_key_helper` auth and the sidecar passes a no-auth placeholder through Claude Code's `apiKeyHelper` setting so the SDK can start.

MCP test fixtures to configure in WeKnora for acceptance:

- Time MCP
- Filesystem MCP scoped to a safe temp directory
- Fetch MCP
- Memory / KV MCP
- SQLite MCP

The general agent should see these only through WeKnora's existing MCP configuration (`mcp_selection_mode` / `mcp_services`) and never through sidecar hardcoding.
