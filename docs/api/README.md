# WeKnora API 文档

## 目录

- [概述](#概述)
- [最权威参考：Swagger UI](#最权威参考swagger-ui)
- [基础信息](#基础信息)
- [认证机制](#认证机制)
- [错误处理](#错误处理)
- [API 概览](#api-概览)

## 概述

WeKnora 提供了一系列 RESTful API，用于创建和管理知识库、检索知识，以及进行基于知识的问答。本文档详细描述了这些 API 的使用方式。

## 最权威参考：Swagger 生成文件与运行时路由

生产 API 基地址为 `https://knora.moutai.com.cn/api/v1`。生产部署使用 release 模式时不
挂载 Swagger UI；请以当前生产接口响应、本文档和平台发布的接口版本说明交叉核对。

仅在允许调试的非 release 本地环境中，启动服务后访问
`http://localhost:8080/swagger/index.html` 查看交互式 Swagger UI。该地址仅用于本地
开发/验收，不是生产入口；扩展接口仍应以当前接口响应和对应文档为准。

本目录下的 Markdown 提供场景、状态机和扩展接口说明。字段冲突时优先核对当前
生产接口响应；`/custom/*`、完整工作流、文件夹和渐进 Wiki 图还要以对应 Markdown
和实际请求结果共同确认。

> Swagger UI 仅在非 release 模式（`GIN_MODE != release`）下挂载；生产部署默认关闭。

## 基础信息

 - **生产基础 URL**: `https://knora.moutai.com.cn/api/v1`
- **本地开发基础 URL**（仅本地验收）: `http://localhost:8080/api/v1`
- **响应格式**: JSON
- **认证方式**: API Key

## 认证机制

所有 API 请求需要在 HTTP 请求头中包含 `X-API-Key` 进行身份认证：

```
X-API-Key: <TENANT_API_KEY>
```

为便于问题追踪和调试，建议每个请求的 HTTP 请求头中添加 `X-Request-ID`：

```
X-Request-ID: unique_request_id
```

### 获取 API Key

在 web 页面完成账户注册后，请前往账户信息页面获取您的 API Key。

请妥善保管您的 API Key，避免泄露。API Key 代表您的账户身份，拥有完整的 API 访问权限。

部分扩展 Web 端接口不使用 API Key 认证，而要求 Web 登录态 Bearer Token，例如对话分享。具体认证方式以各接口文档为准。

## 错误处理

所有 API 使用标准的 HTTP 状态码表示请求状态，并返回统一的错误响应格式：

```json
{
  "success": false,
  "error": {
    "code": "错误代码",
    "message": "错误信息",
    "details": "错误详情"
  }
}
```

## API 概览

WeKnora API 按功能分为以下几类：

| 分类 | 描述 | 文档链接 |
|------|------|----------|
| 认证管理 | 用户注册、登录、令牌管理；OIDC 流程 | [auth.md](./auth.md) · [OIDC认证调用流程.md](../OIDC认证调用流程.md) |
| 租户管理 | 创建和管理租户账户 | [tenant.md](./tenant.md) |
| 知识库管理 | 创建、查询和管理知识库 | [knowledge-base.md](./knowledge-base.md) |
| 知识管理 | 上传、检索和管理知识内容 | [knowledge.md](./knowledge.md) |
| 文档完整工作流（扩展接口） | 队列位置、完整状态筛选、实例与故障接管边界 | [document-workflow.md](./document-workflow.md) |
| 知识库文件夹（扩展接口） | 渐进列表、搜索、文件夹、移动和定向导入 | [knowledge-folders.md](./knowledge-folders.md) |
| Wiki 关联图 | 分类目录、overview、中心节点 ego 图和邻居分页 | [knowledge-graph.md](./knowledge-graph.md) |
| 模型管理 | 配置和管理各种AI模型 | [model.md](./model.md) |
| 分块管理 | 管理知识的分块内容 | [chunk.md](./chunk.md) |
| 标签管理 | 管理知识库的标签分类 | [tag.md](./tag.md) |
| FAQ管理 | 管理FAQ问答对 | [faq.md](./faq.md) |
| 智能体管理 | 创建和管理自定义智能体 | [agent.md](./agent.md) |
| 会话管理 | 创建和管理对话会话 | [session.md](./session.md) |
| 对话分享（扩展接口） | 生成和查看登录态对话分享快照 | [chat-share.md](./chat-share.md) |
| 知识搜索 | 在知识库中搜索内容 | [knowledge-search.md](./knowledge-search.md) |
| 聊天功能 | 基于知识库和 Agent 进行问答 | [chat.md](./chat.md) |
| 消息管理 | 获取和管理对话消息 | [message.md](./message.md) |
| 初始化管理 | 知识库模型配置与 Ollama 管理 | [initialization.md](./initialization.md) |
| 系统管理 | 系统信息、解析引擎、存储引擎 | [system.md](./system.md) |
| MCP 服务 | MCP 工具服务管理 | [mcp-service.md](./mcp-service.md) |
| 组织管理 | 组织、成员、知识库/智能体共享 | [organization.md](./organization.md) |
| Skills | 预装智能体技能 | [skill.md](./skill.md) |
| 网络搜索 | 网络搜索服务商 | [web-search.md](./web-search.md) |
| 向量存储 | 向量数据库连接管理 | [vector-store.md](./vector-store.md) |
| IM 渠道 | 企业微信 / 飞书 / Slack 等 IM 平台对接，含渠道 CRUD 与回调 | [../IM集成开发文档.md](../IM集成开发文档.md) |
| 数据源导入 | 当前实际注册的飞书 / Notion / 语雀 / RSS 数据源接入与同步 | [../数据源导入开发文档.md](../数据源导入开发文档.md) |
