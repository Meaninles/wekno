---
title: MCP功能使用说明
tags: [核心功能, MCP, 工具集成]
aliases: [MCP使用, MCP功能]
source: MCP功能使用说明.md
---

# MCP 功能使用说明

> 当前代码核对日期：2026-09-13。生产 API 基地址为
> `https://knora.moutai.com.cn/api/v1`；本地开发 API 仅使用
> `http://localhost:8080/api/v1`。本页与根目录的 [MCP 功能使用说明](../../MCP功能使用说明.md)
> 内容一致，外部地址、密钥和服务 ID 均为占位符。

MCP（Model Context Protocol）让 WeKnora 的 Agent 连接外部工具或数据源。MCP 服务在
“设置 → MCP 服务”中按当前租户管理，Agent 运行时只使用已启用且对当前租户可见的服务。

## 当前支持范围

- 可用传输：`sse`、`http-streamable`。
- `stdio` 字段仍存在于兼容数据模型，但服务端出于命令注入风险拒绝创建和连接。
- 认证策略：无认证、自定义请求头、API Key、Bearer Token、OAuth 2.0；OAuth 令牌按
  当前登录用户和 MCP 服务隔离保存。
- 支持连接测试、工具/资源发现和工具人工审批策略。

## 使用流程

1. 生产打开 [https://knora.moutai.com.cn](https://knora.moutai.com.cn)，进入“设置 → MCP 服务”；
   本地联调才使用 `http://localhost:5177`。
2. 新建服务时选择 `sse` 或 `http-streamable`，填写上游地址和非敏感配置。
3. API Key/Token 通过凭据子资源保存；列表和详情只显示是否已配置，不返回密钥明文。
4. 测试服务，确认工具/资源发现成功，再将服务绑定到需要的 Agent。
5. 对写入、删除等有副作用的工具配置人工审批。

内置服务对所有租户可见，但连接细节会隐藏，租户不能编辑、删除或修改凭据。完整字段与
OAuth 路由见 [MCP Service API](../../api/mcp-service.md) 和根目录说明。

---

## 反向链接

- [Home](../Home.md)
- [内置MCP服务管理](内置MCP服务管理.md)
- [Agent技能系统](Agent技能系统.md)
