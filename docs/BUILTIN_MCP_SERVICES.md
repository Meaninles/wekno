# 内置 MCP 服务管理指南

> 当前代码核对日期：2026-09-13。生产 API 基地址为
> `https://knora.moutai.com.cn/api/v1`。内置服务的上游地址、认证配置、租户值和凭据
> 属于受保护的系统部署数据，本文不展示。

## 作用与可见性

`is_builtin=true` 的 MCP 服务是系统级配置。仓库当前代码会：

- 在列表、按 ID 查询和 Agent 服务选择时让内置服务对所有租户可见；
- 对普通租户隐藏内置服务的 URL、Headers、环境变量、stdio 配置和认证细节；
- 禁止通过普通 MCP CRUD 修改、删除或通过凭据子资源改写内置服务；
- 只允许使用系统维护的配置进行连接测试和运行时调用。

内置服务不是普通租户服务的复制品，不能通过修改租户 ID、服务 ID 或请求体伪造为内置
服务。具体内置条目由受保护的系统初始化/运维流程维护，不在公开文档中列出。

## 管理边界

普通用户通过 `https://knora.moutai.com.cn` 的“设置 → MCP 服务”只能管理自己的普通
服务。系统管理员维护内置服务时，应使用受保护的发布流程并完成：

1. 配置上游 `sse` 或 `http-streamable` 服务和最小权限认证；当前 `stdio` 传输会被服务
   端拒绝，不应作为内置服务发布。
2. 将敏感配置放入 Kubernetes Secret/密钥管理系统，不写进 values、Git、日志或交付包。
3. 用系统级数据库/初始化工具写入 `is_builtin=true`，并确认唯一标识、租户可见性和
   服务启用状态；不要把真实 SQL、ID、URL 或凭据复制进文档。
4. 通过生产 API 的测试接口验证工具/资源发现，再由 Agent 权限和人工审批策略控制调用。

## 前端与 API 的安全约定

内置服务的详情响应不应向跨租户调用方返回上游连接细节。普通服务的密钥使用：

```http
PUT    /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials
DELETE /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials/api_key
DELETE /api/v1/mcp-services/<MCP_SERVICE_ID>/credentials/token
```

这些操作对内置服务会被拒绝；普通服务响应只返回 `credentials.*.configured`，不返回
`api_key` 或 `token` 明文。完整接口以 [MCP Service API](./api/mcp-service.md) 为准。

## 核对清单

- [ ] 上游地址通过 SSRF 校验，并使用生产可达的 HTTPS 地址。
- [ ] 认证信息来自受保护 Secret，不出现在仓库、文档、日志和前端响应。
- [ ] 服务端/客户端仅使用 `sse` 或 `http-streamable`。
- [ ] 所有租户可见性和内置只读保护已通过生产 API 验证。
- [ ] 对有副作用的工具配置人工审批，并确认 Agent 只绑定需要的服务。
