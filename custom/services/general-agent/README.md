# WeKnora 通用智能体旁路服务

本服务承载 `agent_type=general-agent`、`agent_type=data-analysis` 和 `agent_type=table-analysis` 的 Claude Agent SDK 推理循环。

边界：

- 旁路服务不直接连接 WeKnora 数据库、对象存储、MCP 服务或业务数据源。
- WeKnora Go 后端会为每次运行下发允许使用的工具 schema。
- 工具执行始终通过 `/api/v1/custom/general-agent/internal/tools/call` 回调 Go 后端。
- 产物生成只由 `enable_artifacts` 控制。提示词要求最多 5 个产物、总大小小于 128MB，并优先生成重要文件；旁路服务在前端持久化前按同一规则保留产物。
- `agent_type=document-processing-agent` 使用相邻的文档处理镜像，不使用本容器。

本地 Docker：

```bash
docker compose -f custom/docker-compose.general-agent.yml up -d --build
```

健康检查：

```bash
curl http://127.0.0.1:8091/health
```

关键环境变量：

- `CUSTOM_GENERAL_AGENT_API_KEY`：Go 后端与旁路服务之间的共享密钥。
- `GENERAL_AGENT_RUN_ROOT`：旁路服务运行目录。
- `CUSTOM_GENERAL_AGENT_CLAUDE_API_TIMEOUT_MS`：LLM 请求超时。
- `CUSTOM_GENERAL_AGENT_CLAUDE_IDLE_TIMEOUT_MS`：流式响应空闲超时。
- `CUSTOM_GENERAL_AGENT_MAX_TURNS`：智能体配置未设置 `max_iterations` 时的兜底最大轮数。
- Claude SDK 的模型名、Anthropic 兼容端点和 API key 都从 WeKnora 模型管理中当前选中的模型解析。普通或本地模型如需独立 Anthropic 兼容端点，应在模型 `extra_config.general_agent_claude_base_url` 中配置；配置了加密 API key 时会复用该 key。若模型有意不配置 API key，Go 会把本次运行标记为 `api_key_helper` 认证，旁路服务通过 Claude Code 的 `apiKeyHelper` 传入无认证占位值，使 SDK 可以启动。

验收时可在 WeKnora 中配置的 MCP 测试服务：

- 时间 MCP。
- 限定安全临时目录的文件系统 MCP。
- Fetch MCP。
- 内存或 KV MCP。
- SQLite MCP。

通用智能体只能通过 WeKnora 现有 MCP 配置（`mcp_selection_mode` / `mcp_services`）看到这些服务，旁路服务中不硬编码测试 MCP。
