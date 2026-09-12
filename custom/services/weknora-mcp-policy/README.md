# WeKnora 可配置外部 MCP 服务

这是一个独立的外部 MCP 协议适配层，供 WorkBuddy、Codex 或其他 MCP 客户端访问 WeKnora。它不保存租户 API Key：HTTP 请求必须携带 `X-API-Key`，服务只在当前请求内把该 Key 转发到 `WEKNORA_BASE_URL`。

## 能力

- Streamable HTTP：`/mcp` 和 `/mcp/`（两者等价，不依赖重定向）
- stdio：可用同一个镜像以 `MCP_TRANSPORT=stdio` 启动
- 健康检查：`/healthz`
- 32 个工具，工具定义和输入 schema 位于 `app/tools.py`
- 服务端 JSONC 工具白名单：删除 `enabled_tools` 中的一行即可关闭工具；文件挂载为只读时，修改宿主机文件后下一个请求自动重新加载
- 写入知识、文件、URL 的返回值只包含 ID、名称和处理状态，不回显正文或文件内容
- 模型返回中的 API Key、Token、Secret、Password 等字段递归脱敏

`download_knowledge` 是唯一显式返回原始文件 Base64 的工具，并受 `MCP_MAX_DOWNLOAD_BYTES` 限制；如果不需要下载能力，直接从 JSONC 白名单删除它。

## 配置白名单

默认文件：`config/weknora-mcp.config.jsonc`。它是 JSONC，不是严格 JSON，因此可以保留注释：

```jsonc
{
  "enabled_tools": [
    "list_knowledge_bases",
    "hybrid_search",
    "create_knowledge_from_content"
  ]
}
```

服务启动或请求 `tools/list` 时会校验工具名。缺文件、格式错误或出现未知工具名时，`/healthz` 返回 503，避免错误配置意外开放全部能力。

## WorkBuddy 配置

本地双实例入口由 Compose 的 `runtime-mcp-entry` 提供，宿主机默认地址为 `http://127.0.0.1:8000/mcp`。WorkBuddy 的 MCP 配置可写成：

```json
{
  "mcpServers": {
    "weknora-local": {
      "type": "http",
      "url": "http://127.0.0.1:8000/mcp/",
      "headers": {
        "X-API-Key": "填写当前租户 API Key"
      }
    }
  }
}
```

不要把 API Key 写进服务镜像、Compose 文件或 Git。生产环境只需要把 URL 换成生产 MCP 入口，并在生产机器挂载生产白名单；本次实现不修改生产部署。

工具名称保持稳定的英文标识，便于 MCP 客户端调用；每个工具同时提供中文标题和中文描述，支持客户端在工具列表中显示中文。客户端若不显示 MCP `title` 字段，仍会显示协议要求的英文工具名，这不影响调用。

服务初始化响应还会提供通用的 WeKnora 使用说明；工具发现仍使用标准 MCP `tools/list`，不依赖额外的特殊能力工具。

## 本地启动

先确保 `docker-compose.dev.yml` 中的 PostgreSQL、Redis、MinIO、Neo4j 和基础 DocReader 已启动，再使用 runtime profile 编排：

```powershell
docker compose -p weknora-runtime-profile-e2e -f custom/tests/runtime_profile_e2e/docker-compose.yml build runtime-mcp-1 runtime-mcp-2
docker compose -p weknora-runtime-profile-e2e -f custom/tests/runtime_profile_e2e/docker-compose.yml up -d --force-recreate runtime-mcp-1 runtime-mcp-2 runtime-mcp-entry
docker compose -p weknora-runtime-profile-e2e -f custom/tests/runtime_profile_e2e/docker-compose.yml ps runtime-mcp-1 runtime-mcp-2 runtime-mcp-entry
```

默认实例和入口如下：

| 组件 | 容器内端口 | 宿主机端口 | 作用 |
| --- | ---: | ---: | --- |
| `runtime-mcp-1` | 8000 | - | MCP 实例 1 |
| `runtime-mcp-2` | 8000 | - | MCP 实例 2 |
| `runtime-mcp-entry` | 8000 | 8000 | HAProxy 入口 |

可通过 `WEKNORA_RUNTIME_MCP_PORT` 修改宿主机端口，但 WorkBuddy URL 也要同步修改。两个实例均访问 Compose 内的 `runtime-entry:8080/api/v1`，不依赖本地回环地址访问后端。

## 工具与安全边界

白名单是 MCP 层的能力裁剪，不是后端 RBAC 的替代品。后端仍会按租户 API Key、租户角色和知识库权限校验；但拥有该 API Key 的调用者如果能修改服务配置或直接访问 WeKnora REST API，就可以绕过 MCP 白名单。因此生产环境应把 JSONC 配置文件设为只有部署管理员可写，并结合网关、网络 ACL 和 API Key 轮换使用。

“只写不读”配置至少保留 `create_knowledge_base` 以及一个或多个 `create_knowledge_from_*` 工具，并删除 `list_knowledge_bases`、`get_knowledge_base`、`hybrid_search`、`list_knowledge`、`get_knowledge`、`download_knowledge`、`chat`、`agent_chat`、Wiki 和分块读取工具。创建知识的成功响应仍只会返回新条目的 ID 和处理状态。

文件写入通过 `content_base64` 传入，默认禁止服务端任意 `file_path` 读取；只有显式设置 `MCP_ALLOW_SERVER_FILE_PATH=true` 才会启用后者。本地 Compose 固定为 `false`。

## 验证

`custom/tests/mcp_policy_e2e/protocol_probe.py` 使用官方 `mcp.client` 的 `ClientSession` 模拟 WorkBuddy，验证初始化、`tools/list`、白名单拒绝和实际 API 调用。脚本不会输出 API Key：

```powershell
$env:WEKNORA_E2E_TENANT_API_KEY = "当前租户 API Key"
python custom/tests/mcp_policy_e2e/protocol_probe.py `
  --url http://127.0.0.1:8000/mcp/ `
  --api-key-env WEKNORA_E2E_TENANT_API_KEY `
  --expect-tool list_knowledge_bases `
  --call-list-knowledge-bases
```

`WEKNORA_E2E_TENANT_API_KEY` 只存在于当前 PowerShell 进程，不要把它写入文件或命令历史。
