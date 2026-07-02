# WeKnora MCP Fixtures

Optional HTTP Streamable MCP service used to validate `general-agent` can reuse WeKnora's native MCP selection and tool execution pipeline.

It exposes deterministic tools:

- `time_now`
- `echo`
- `calculator`
- `kv_put`
- `kv_get`
- `fetch_stub`
- `fs_list`
- `fs_write_text`
- `fs_read_text`
- `sqlite_query`

Start it:

```bash
docker compose -f custom/docker-compose.mcp-fixtures.yml up -d --build
```

The app does not register this fixture as a built-in MCP service by default.
For local validation, register it manually through the native MCP API:

```bash
WEKNORA_API_KEY=<tenant-api-key> \
MCP_FIXTURE_URL=http://weknora-custom-mcp-fixtures:8092/mcp \
python custom/scripts/register_general_agent_mcp_fixtures.py
```

Default MCP URL in the dev compose network is:

```text
http://weknora-custom-mcp-fixtures:8092/mcp
```

Use it in the agent editor under `MCP 服务`:

- `全部 MCP 服务`: fixture tools should appear automatically.
- `指定 MCP 服务`: select "General Agent MCP Fixtures"; only selected services should be exposed.
- `不使用 MCP`: fixture tools must not be exposed to the general agent.
