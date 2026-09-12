# MCP Service API

[返回目录](./README.md)

MCP（Model Context Protocol）服务管理接口，提供 MCP 服务的 CRUD、连通性测试、工具/资源发现，以及工具人工审批策略配置。

| 方法   | 路径                                              | 描述                                          |
| ------ | ------------------------------------------------- | --------------------------------------------- |
| POST   | `/mcp-services`                                   | 创建 MCP 服务                                 |
| GET    | `/mcp-services`                                   | 获取当前租户的 MCP 服务列表                   |
| GET    | `/mcp-services/:id`                               | 获取 MCP 服务详情                             |
| PUT    | `/mcp-services/:id`                               | 更新 MCP 服务（部分字段更新）                 |
| DELETE | `/mcp-services/:id`                               | 删除 MCP 服务                                 |
| PUT    | `/mcp-services/:id/credentials`                  | 写入/替换 MCP 凭据（只返回 configured 状态）  |
| DELETE | `/mcp-services/:id/credentials/:field`            | 删除单个凭据字段                               |
| POST   | `/mcp-services/:id/test`                          | 测试 MCP 服务连通性                           |
| GET    | `/mcp-services/:id/tools`                         | 获取 MCP 服务工具列表                         |
| GET    | `/mcp-services/:id/resources`                     | 获取 MCP 服务资源列表                         |
| POST   | `/mcp-services/:id/oauth/authorize-url`           | 为当前用户发起 MCP OAuth 授权                 |
| GET    | `/mcp-services/:id/oauth/status`                  | 查询当前用户 OAuth 授权状态                   |
| DELETE | `/mcp-services/:id/oauth/token`                   | 撤销当前用户 OAuth 授权                       |
| GET    | `/mcp-oauth/callback`                             | OAuth 提供商回调（公开回调入口）              |
| GET    | `/mcp-services/:id/tool-approvals`                | 列出该服务下各工具的人工审批策略 |
| PUT    | `/mcp-services/:id/tool-approvals/:tool_name`     | 设置/更新某工具的人工审批策略  |
| POST   | `/agent/tool-approvals/:pending_id`               | 处理 Agent 工具调用待审批请求  |
| POST   | `/agent/mcp-oauth-resolutions/:pending_id`        | 完成对话内 MCP OAuth 授权                     |
| POST   | `/agent/mcp-oauth-resolutions/:pending_id/cancel` | 取消对话内 MCP OAuth 授权                     |

## POST `/mcp-services` - 创建 MCP 服务

**请求参数**:

| 字段             | 类型    | 必填 | 说明                                                                                          |
| ---------------- | ------- | ---- | --------------------------------------------------------------------------------------------- |
| name             | string  | 是   | 服务名称                                                                                      |
| description      | string  | 否   | 服务描述                                                                                      |
| transport_type   | string  | 是   | 当前可用传输：`sse`、`http-streamable`；`stdio` 仅保留为兼容字段，服务端拒绝其创建/连接 |
| url              | string  | 条件 | 服务地址；当 `transport_type` 为 `sse` / `http-streamable` 时必填（受 SSRF 安全校验约束）        |
| headers          | object  | 否   | 自定义请求头                                                                                  |
| auth_config      | object  | 否   | 非敏感认证配置，如 `auth_type`、`api_key_header`、OAuth scopes；密钥走凭据子资源           |
| advanced_config  | object  | 否   | 高级配置，支持 `timeout`、`retry_count`、`retry_delay`                                          |
| stdio_config     | object  | 否   | 兼容数据结构；当前服务端不接受 stdio 运行配置                                                  |
| env_vars         | object  | 否   | 兼容数据结构；当前可用的 SSE/HTTP Streamable 连接不使用此字段                                  |
| enabled          | boolean | 否   | 是否启用                                                                                      |

**请求**:

> 当前服务端只创建 `sse` 或 `http-streamable` 服务；`stdio` 仅存在于兼容 DTO，不能用于
> 新建、更新为可运行服务或连接测试。

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "name": "天气查询服务",
    "description": "提供全球天气信息查询",
    "transport_type": "sse",
    "url": "https://mcp.example.com/weather/sse",
    "headers": {
        "X-Custom-Header": "value"
    },
    "auth_config": {
        "api_key": "<EXTERNAL_SERVICE_KEY>"
    },
    "advanced_config": {
        "timeout": 30,
        "retry_count": 3,
        "retry_delay": 1
    }
}'
```

> 安全约定：请求可以在 `auth_config` 中配置非敏感结构字段，但 `api_key` 和 `token`
> 不应再通过主资源 PUT 传递。请使用下方的 `/credentials` 子资源。任何响应都不会返回
> 凭据明文，只返回 `credentials.*.configured` 布尔值。

## PUT `/mcp-services/:id/credentials` - 写入 MCP 凭据

仅管理员可调用。请求体可以包含 `api_key`、`token` 的任意子集；省略字段保留原值，
空字符串不用于删除。成功响应只返回是否已配置，不返回密钥。

```curl
curl --location --request PUT 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/credentials' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "api_key": "<EXTERNAL_SERVICE_KEY>"
}'
```

```json
{
    "success": true,
    "data": {
        "fields": {
            "api_key": { "configured": true },
            "token": { "configured": false }
        }
    }
}
```

## DELETE `/mcp-services/:id/credentials/:field` - 删除单个凭据

`field` 只能是 `api_key` 或 `token`。操作幂等，成功返回 HTTP 204：

```curl
curl --location --request DELETE 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/credentials/api_key' \
--header 'X-API-Key: <TENANT_API_KEY>'
```

**响应**:

```json
{
    "data": {
        "id": "mcp-00000001",
        "tenant_id": 1,
        "name": "天气查询服务",
        "description": "提供全球天气信息查询",
        "enabled": true,
        "transport_type": "sse",
        "url": "https://mcp.example.com/weather/sse",
        "headers": {
            "X-Custom-Header": "value"
        },
        "auth_config": {
            "auth_type": "api_key",
            "api_key_header": "X-API-Key"
        },
        "credentials": {
            "api_key": { "configured": true },
            "token": { "configured": false }
        },
        "advanced_config": {
            "timeout": 30,
            "retry_count": 3,
            "retry_delay": 1
        },
        "is_builtin": false,
        "created_at": "2025-08-12T10:00:00+08:00",
        "updated_at": "2025-08-12T10:00:00+08:00"
    },
    "success": true
}
```

**stdio 兼容性说明（不支持的请求示例）**:

以下请求仅用于说明服务端会拒绝 `stdio`；外围应用不要提交此配置，生产只使用
`sse` 或 `http-streamable`。

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "name": "本地文件服务",
    "description": "通过 stdio 访问本地文件系统",
    "transport_type": "stdio",
    "stdio_config": {
        "command": "/usr/local/bin/mcp-file-server",
        "args": ["--root", "/data"]
    },
    "env_vars": {
        "MCP_LOG_LEVEL": "info"
    }
}'
```

## GET `/mcp-services` - 获取 MCP 服务列表

返回当前租户已配置的所有 MCP 服务。历史数据可能携带 `stdio` 兼容字段，但当前服务端
不会为其建立运行连接；新建和更新时只应使用 `sse` 或 `http-streamable`。

**请求**:

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "data": [
        {
            "id": "mcp-00000001",
            "tenant_id": 1,
            "name": "天气查询服务",
            "description": "提供全球天气信息查询",
            "enabled": true,
            "transport_type": "sse",
            "url": "https://mcp.example.com/weather/sse",
            "headers": {},
            "auth_config": {
                "auth_type": "api_key",
                "api_key_header": "X-API-Key"
            },
            "credentials": {
                "api_key": { "configured": true },
                "token": { "configured": false }
            },
            "advanced_config": {
                "timeout": 30,
                "retry_count": 3,
                "retry_delay": 1
            },
            "is_builtin": false,
            "created_at": "2025-08-12T10:00:00+08:00",
            "updated_at": "2025-08-12T10:00:00+08:00"
        },
        {
            "id": "mcp-00000002",
            "tenant_id": 1,
            "name": "本地文件服务",
            "description": "通过 stdio 访问本地文件系统",
            "enabled": true,
            "transport_type": "stdio",
            "headers": {},
            "auth_config": null,
            "advanced_config": null,
            "stdio_config": {
                "command": "/usr/local/bin/mcp-file-server",
                "args": ["--root", "/data"]
            },
            "env_vars": {
                "MCP_LOG_LEVEL": "info"
            },
            "is_builtin": false,
            "created_at": "2025-08-12T11:00:00+08:00",
            "updated_at": "2025-08-12T11:00:00+08:00"
        }
    ],
    "success": true
}
```

## GET `/mcp-services/:id` - 获取 MCP 服务详情

**路径参数**:

| 字段 | 类型   | 说明           |
| ---- | ------ | -------------- |
| id   | string | MCP 服务 ID    |

> 注：响应 DTO 永远不会包含 `api_key` 或 `token`。普通服务通过 `credentials` 返回
> configured 元数据；内置服务还会隐藏租户特定的连接细节。

**请求**:

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "data": {
        "id": "mcp-00000001",
        "tenant_id": 1,
        "name": "天气查询服务",
        "description": "提供全球天气信息查询",
        "enabled": true,
        "transport_type": "sse",
        "url": "https://mcp.example.com/weather/sse",
        "headers": {},
        "auth_config": {
            "auth_type": "api_key",
            "api_key_header": "X-API-Key"
        },
        "credentials": {
            "api_key": { "configured": true },
            "token": { "configured": false }
        },
        "advanced_config": {
            "timeout": 30,
            "retry_count": 3,
            "retry_delay": 1
        },
        "is_builtin": false,
        "created_at": "2025-08-12T10:00:00+08:00",
        "updated_at": "2025-08-12T10:00:00+08:00"
    },
    "success": true
}
```

## PUT `/mcp-services/:id` - 更新 MCP 服务

支持部分字段更新，可传入下列任意子集：`name`、`description`、`enabled`、`transport_type`、`url`、`headers`、`auth_config`、`advanced_config`。`transport_type` 只能使用 `sse` 或 `http-streamable`；`stdio_config` 和 `env_vars` 仅为兼容字段，不能启用 stdio。其中 `url` 若提供，会再次执行 SSRF 安全校验。`auth_config.api_key` 和 `auth_config.token` 在主资源更新中会被忽略；凭据必须通过 `/credentials` 子资源显式更新。

**请求**:

```curl
curl --location --request PUT 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "name": "天气查询服务（更新）",
    "description": "提供全球天气信息查询，支持实时数据",
    "enabled": false
}'
```

**响应**:

```json
{
    "data": {
        "id": "mcp-00000001",
        "tenant_id": 1,
        "name": "天气查询服务（更新）",
        "description": "提供全球天气信息查询，支持实时数据",
        "enabled": false,
        "transport_type": "sse",
        "url": "https://mcp.example.com/weather/sse",
        "headers": {},
        "auth_config": {
            "auth_type": "api_key",
            "api_key_header": "X-API-Key"
        },
        "credentials": {
            "api_key": { "configured": true },
            "token": { "configured": false }
        },
        "advanced_config": {
            "timeout": 30,
            "retry_count": 3,
            "retry_delay": 1
        },
        "is_builtin": false,
        "created_at": "2025-08-12T10:00:00+08:00",
        "updated_at": "2025-08-12T12:00:00+08:00"
    },
    "success": true
}
```

## DELETE `/mcp-services/:id` - 删除 MCP 服务

**请求**:

```curl
curl --location --request DELETE 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "success": true,
    "message": "MCP service deleted successfully"
}
```

## POST `/mcp-services/:id/test` - 测试 MCP 服务连通性

后端会以已保存配置建立一次 MCP 连接，返回连接结果及探测到的工具/资源列表。连接失败时 HTTP 仍为 200，但 `data.success` 为 `false`，错误原因在 `data.message` 中。

**请求**:

```curl
curl --location --request POST 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/test' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "data": {
        "success": true,
        "message": "连接成功",
        "description": "提供全球天气信息查询",
        "tools": [
            {
                "name": "get_weather",
                "description": "获取指定城市的天气信息",
                "inputSchema": {
                    "type": "object",
                    "properties": {
                        "city": {
                            "type": "string",
                            "description": "城市名称"
                        }
                    },
                    "required": ["city"]
                }
            }
        ],
        "resources": [
            {
                "uri": "weather://cities",
                "name": "城市列表",
                "description": "支持查询的城市列表",
                "mimeType": "application/json"
            }
        ]
    },
    "success": true
}
```

## GET `/mcp-services/:id/tools` - 获取 MCP 服务工具列表

**请求**:

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/tools' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "data": [
        {
            "name": "get_weather",
            "description": "获取指定城市的天气信息",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "city": {
                        "type": "string",
                        "description": "城市名称"
                    }
                },
                "required": ["city"]
            }
        },
        {
            "name": "get_forecast",
            "description": "获取未来天气预报",
            "inputSchema": {
                "type": "object",
                "properties": {
                    "city": {
                        "type": "string",
                        "description": "城市名称"
                    },
                    "days": {
                        "type": "integer",
                        "description": "预报天数"
                    }
                },
                "required": ["city"]
            }
        }
    ],
    "success": true
}
```

## GET `/mcp-services/:id/resources` - 获取 MCP 服务资源列表

**请求**:

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/resources' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json'
```

**响应**:

```json
{
    "data": [
        {
            "uri": "weather://cities",
            "name": "城市列表",
            "description": "支持查询的城市列表",
            "mimeType": "application/json"
        },
        {
            "uri": "weather://config",
            "name": "服务配置",
            "description": "当前服务配置信息",
            "mimeType": "application/json"
        }
    ],
    "success": true
}
```

## MCP OAuth 授权

当 MCP 服务的非敏感 `auth_config.auth_type` 配置为 OAuth 时，当前登录用户可以按用户
维度完成授权。授权令牌不会通过服务 CRUD 响应返回。

### POST `/mcp-services/:id/oauth/authorize-url`

请求体：

```json
{
    "redirect_uri": "https://knora.moutai.com.cn/api/v1/mcp-oauth/callback",
    "frontend_redirect": "/settings/mcp"
}
```

成功响应返回一次性浏览器授权地址：

```json
{
    "success": true,
    "data": { "authorization_url": "https://mcp.example.com/oauth/authorize?..." }
}
```

授权服务完成回调后访问生产地址 `https://knora.moutai.com.cn/api/v1/mcp-oauth/callback`。
`state` 为一次性状态参数；不要在文档、日志或客户端持久化真实授权码和令牌。

### GET `/mcp-services/:id/oauth/status`

返回当前登录用户是否已完成该服务授权：

```json
{ "success": true, "data": { "authorized": true } }
```

### DELETE `/mcp-services/:id/oauth/token`

撤销当前登录用户对该 MCP 服务的授权，成功返回 HTTP 204。

对话内 OAuth 暂停还需要调用 `/agent/mcp-oauth-resolutions/:pending_id`，请求体至少
包含 `service_id`；用户跳过授权可调用同路径的 `/cancel`。

## GET `/mcp-services/:id/tool-approvals` - 列出工具人工审批策略

返回该 MCP 服务下各工具持久化的 `require_approval` 标记。仅返回数据库中已显式配置过的工具记录；未出现在列表中的工具默认无需审批。

**路径参数**:

| 字段 | 类型   | 说明        |
| ---- | ------ | ----------- |
| id   | string | MCP 服务 ID |

**请求**:

```curl
curl --location 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/tool-approvals' \
--header 'X-API-Key: <TENANT_API_KEY>'
```

**响应**:

```json
{
    "data": [
        {
            "tool_name": "delete_file",
            "require_approval": true,
            "updated_at": "2025-09-20T15:30:00+08:00"
        },
        {
            "tool_name": "get_weather",
            "require_approval": false,
            "updated_at": "2025-09-20T15:31:00+08:00"
        }
    ],
    "success": true
}
```

## PUT `/mcp-services/:id/tool-approvals/:tool_name` - 设置工具人工审批策略

为指定 MCP 服务下的某个工具设置/更新人工审批要求。当 `require_approval` 为 `true` 时，Agent 在调用该工具前会阻塞并产生一条待审批记录，需要前端调用 `POST /agent/tool-approvals/:pending_id` 完成审批。

**路径参数**:

| 字段       | 类型   | 说明                                                                |
| ---------- | ------ | ------------------------------------------------------------------- |
| id         | string | MCP 服务 ID                                                         |
| tool_name  | string | 工具名（由 Gin 自动 URL 解码，调用方需对名称中的 `%`、`/` 做 URL 编码） |

**请求体**:

| 字段              | 类型    | 必填 | 说明                                |
| ----------------- | ------- | ---- | ----------------------------------- |
| require_approval  | boolean | 是   | 是否要求人工审批后才能执行该工具    |

**请求**:

```curl
curl --location --request PUT 'https://knora.moutai.com.cn/api/v1/mcp-services/mcp-00000001/tool-approvals/delete_file' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "require_approval": true
}'
```

**响应**:

```json
{
    "success": true
}
```

## POST `/agent/tool-approvals/:pending_id` - 处理待审批工具调用

用于 Agent 在执行过程中阻塞等待人工审批的场景：当 Agent 命中一个 `require_approval = true` 的工具时会生成一条 `pending_id`，前端拿到这个 ID 后调用此接口将审批结果回传给 Agent，Agent 才会继续执行（或终止）。

**鉴权要求**：请求上下文中必须有已认证用户（`user_id`），且该用户必须是当前 pending 会话的所有者；租户与用户两层都会做 fail-close 校验。

**路径参数**:

| 字段        | 类型   | 说明                |
| ----------- | ------ | ------------------- |
| pending_id  | string | 待审批记录 ID       |

**请求体**:

| 字段           | 类型   | 必填 | 说明                                                                                                              |
| -------------- | ------ | ---- | ----------------------------------------------------------------------------------------------------------------- |
| decision       | string | 是   | 审批结论，必须为 `approve` 或 `reject`                                                                            |
| modified_args  | object | 否   | 仅在 `approve` 时生效，允许人工修改本次工具调用的参数；必须是非 null 的 JSON 对象，否则返回 400                    |
| reason         | string | 否   | 审批理由（任意，便于审计）                                                                                        |

**请求（通过）**:

```curl
curl --location --request POST 'https://knora.moutai.com.cn/api/v1/agent/tool-approvals/pending-abcdef123456' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "decision": "approve",
    "modified_args": {
        "path": "/tmp/safe-target.txt"
    },
    "reason": "已确认目标路径安全"
}'
```

**请求（驳回）**:

```curl
curl --location --request POST 'https://knora.moutai.com.cn/api/v1/agent/tool-approvals/pending-abcdef123456' \
--header 'X-API-Key: <TENANT_API_KEY>' \
--header 'Content-Type: application/json' \
--data '{
    "decision": "reject",
    "reason": "目标路径在受保护目录"
}'
```

**响应**:

```json
{
    "success": true
}
```

**错误码说明**:

| HTTP | 触发条件                                                                                |
| ---- | --------------------------------------------------------------------------------------- |
| 400  | `decision` 不是 `approve`/`reject`；或 `modified_args` 是 `null`/非对象；或租户/用户错配 |
| 401  | 上下文缺失认证用户（中间件未注入 `user_id`）                                            |
| 404  | `pending_id` 不存在或已完成（超时/取消已先一步消费）                                    |
