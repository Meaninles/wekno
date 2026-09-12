# MCP 功能使用说明

> 当前代码核对日期：2026-09-13。生产 API 基地址为
> `https://knora.moutai.com.cn/api/v1`；本地开发 API 仅使用
> `http://localhost:8080/api/v1`。文档中的外部 MCP 地址、密钥和服务 ID 均为占位符。

MCP（Model Context Protocol）让 WeKnora 的 Agent 连接外部工具或数据源。MCP 服务在
“设置 → MCP 服务”中按当前租户管理，Agent 运行时只使用已启用且对当前租户可见的服务。

## 当前支持范围

- 可用传输：`sse`、`http-streamable`。
- `stdio` 字段仍存在于数据模型/API 兼容结构中，但当前服务端出于命令注入风险会拒绝
  创建和连接，不应在生产配置中使用。
- 认证策略：无认证、自定义请求头、API Key、Bearer Token、OAuth 2.0；OAuth 授权令牌
  按当前登录用户和 MCP 服务隔离保存。
- 每个服务可设置超时、重试次数、重试间隔，并可测试工具和资源发现。
- MCP 服务可以配置工具人工审批策略；Agent 调用被要求审批的工具时会暂停等待用户处理。

## 管理操作

1. 打开生产控制台 [https://knora.moutai.com.cn](https://knora.moutai.com.cn)，进入
   “设置 → MCP 服务”。本地联调才使用 `http://localhost:5177`。
2. 点击“添加服务”，填写名称、描述、`sse` 或 `http-streamable` 地址，以及非敏感
   连接配置。
3. 保存 API Key 或 Bearer Token 时，使用“认证配置”对应的凭据操作；密钥不会在服务
   列表、详情或更新响应中返回，只显示是否已配置。
4. 点击“测试”检查初始化、工具列表和资源列表；失败时先检查 URL、SSRF 策略、认证和
   上游 MCP 服务可达性。
5. 编辑/启停/删除只影响当前租户自己的普通服务。系统级内置服务对所有租户可见，
   连接细节会隐藏，不能由租户编辑、删除或修改凭据。

## API 地址与凭据边界

生产 API 示例：

```text
https://knora.moutai.com.cn/api/v1/mcp-services
```

普通 MCP 服务的主资源用于名称、传输方式、地址、Headers、OAuth 非敏感配置和高级
策略。API Key/Token 使用以下子资源，成功响应只返回 `configured` 状态：

```http
PUT    /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials
DELETE /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials/api_key
DELETE /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials/token
```

完整请求/响应字段见 [MCP Service API](./api/mcp-service.md)。任何真实密钥只能通过
密钥管理系统、环境变量或受保护的 API 请求注入，不得写进 Markdown、日志、镜像或前端
静态文件。

## OAuth 2.0

将普通 MCP 服务的 `auth_config.auth_type` 设为 `oauth` 后，由当前用户发起授权：

1. 调用 `POST /mcp-services/<MCP_SERVICE_ID>/oauth/authorize-url` 获取一次性授权地址。
2. OAuth 服务回调生产地址
   `https://knora.moutai.com.cn/api/v1/mcp-oauth/callback`。
3. 用 `GET /mcp-services/<MCP_SERVICE_ID>/oauth/status` 查询状态，或用
   `DELETE /mcp-services/<MCP_SERVICE_ID>/oauth/token` 撤销授权。

不要记录授权码、access token 或 refresh token。对话内授权暂停/继续使用 API 文档中
列出的 `mcp-oauth-resolutions` 路由。

## 安全建议

- 生产上游地址使用 HTTPS，并让服务端 SSRF 校验决定是否允许访问；不要通过白名单绕过
  校验暴露不必要的内网目标。
- 外部服务使用最小权限凭据并定期轮换；更换凭据后服务端会关闭旧连接，下次调用重新建立。
- 对可能产生写入或删除的工具启用人工审批，并限制 Agent 的服务/工具选择范围。
- 测试响应只用于诊断，不代表 Agent 已获得某个工具的业务权限；仍需按租户和用户权限
  检查实际调用。

相关文档：[Agent 技能 API](./api/skill.md)、[IM 集成](./IM集成开发文档.md)、
[API 概览](./api/README.md)。
