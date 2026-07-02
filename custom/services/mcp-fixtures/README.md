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

In the Docker dev app, `app-dev` automatically registers this service as a
built-in development MCP row when:

```text
CUSTOM_GENERAL_AGENT_REGISTER_MCP_FIXTURES=true
```

The dev compose default enables it and points to:

```text
CUSTOM_GENERAL_AGENT_MCP_FIXTURE_URL=http://weknora-custom-mcp-fixtures:8092/mcp
```

If you are not using `app-dev`, register it manually through the native MCP API:

```bash
WEKNORA_API_KEY=<tenant-api-key> \
python custom/scripts/register_general_agent_mcp_fixtures.py
```

Default MCP URL from `app-dev` is:

```text
http://weknora-custom-mcp-fixtures:8092/mcp
```

Use it in the agent editor under `MCP 服务`:

- `全部 MCP 服务`: fixture tools should appear automatically.
- `指定 MCP 服务`: select "General Agent MCP Fixtures"; only selected services should be exposed.
- `不使用 MCP`: fixture tools must not be exposed to the general agent.
