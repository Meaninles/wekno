# WeKnora 通用智能体旁路服务

本服务承载 `agent_type=general-agent`、`agent_type=knowledge-base-manager`、`agent_type=data-analysis` 和 `agent_type=table-analysis` 的 Claude Agent SDK 推理循环。

边界：

- 旁路服务不直接连接 WeKnora 数据库、对象存储、MCP 服务或业务数据源。
- WeKnora Go 后端会为每次运行下发允许使用的工具 schema。
- 工具执行始终通过 `/api/v1/custom/general-agent/internal/tools/call` 回调 Go 后端。
- 产物生成只由 `enable_artifacts` 控制。通常最多返回 5 个产物；`knowledge-base-manager` 类型不限制产物数量。所有类型仍要求单次返回产物总大小小于 128MB，旁路服务会在前端持久化前应用对应规则。
- `agent_type=document-processing-agent` 使用相邻的文档处理镜像，不使用本容器。

## 多副本和产物

生产部署两个副本并按主机名打散。一次 Claude SDK 运行从开始到 terminal 事件固定
在同一个 Pod 的 `GENERAL_AGENT_RUN_ROOT` 内完成，不要求粘性会话或共享文件系统：

1. 用户原文件由 Go app 写入独立的 MinIO/OBS 临时前缀并向 Agent 提供短时引用。
2. Agent 使用本 Pod 的 RWO 临时目录进行随机读写和工具运行。
3. 最终产物通过
   `/api/v1/custom/general-agent/internal/artifacts/upload` 回传 Go app。
4. Go app 写入私有对象存储，校验大小/SHA256并返回 artifact ID。
5. 只有持久提交成功后 Agent 才发送 terminal 事件；之后任意 app 都能鉴权下载。
6. 成功、失败、取消或 panic 后临时原文件被幂等删除，生产另配最长 24 小时的
   前缀级生命周期兜底。

本地开发使用 MinIO，生产使用 OBS。Agent 运行目录不得放到 OBS CSI/S3FS，也不
使用 RWX。Pod 在产物提交前崩溃时，本轮运行失败/重试；半成品本地目录不被当作
持久状态。

对象键不包含用户名、原文件名或业务名称，并使用用途 + deployment + namespace
UUID + tenant/run/artifact 的唯一私有路径。内部上传代理要允许 128 MiB，以覆盖
协议允许的 `<128 MiB` 返回总量；面向用户的普通原附件业务上限仍为 50 MiB。

双副本验收必须覆盖 `general-agent`、`knowledge-base-manager`、`data-analysis`
和 `table-analysis`，并分别验证知识检索、MCP/Web、数据库只读分析、表格分析、
产物下载/删除以及停止一个 Pod 后的新运行可用。

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
- `CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_URL`：Go app 内部产物上传入口。
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

生产副本、临时卷、Secret 和 OBS 配置见
[生产更新部署执行手册](../../../docs/custom/当前版本生产更新部署执行手册.md)。
