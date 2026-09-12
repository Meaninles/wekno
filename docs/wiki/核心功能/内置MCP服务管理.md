---
title: 内置MCP服务管理
tags: [核心功能, MCP, 系统管理, 内置服务]
aliases: [内置MCP, BuiltinMCP, BUILTIN_MCP_SERVICES]
source: BUILTIN_MCP_SERVICES.md
---

# 内置 MCP 服务管理指南

> 当前代码核对日期：2026-09-13。生产 API 基地址为
> `https://knora.moutai.com.cn/api/v1`。内置服务的上游地址、认证配置、租户值和凭据
> 属于受保护的系统部署数据，本文不展示。

`is_builtin=true` 的 MCP 服务是系统级配置。当前代码让它们对所有租户可见，同时对
普通租户隐藏 URL、Headers、环境变量、stdio 配置和认证细节，并禁止普通 MCP CRUD、
凭据子资源修改和删除。具体条目由受保护的系统初始化/运维流程维护，不在公开文档中列出。

系统管理员维护时应使用受保护发布流程，使用 `sse` 或 `http-streamable`，把认证放入
Secret/密钥管理系统，并通过生产测试接口验证工具/资源发现；`stdio` 当前会被服务端
拒绝。普通租户只能管理自己的非内置服务。

完整接口见 [MCP Service API](../../api/mcp-service.md) 和根目录
[内置 MCP 服务管理指南](../../BUILTIN_MCP_SERVICES.md)。

---

## 反向链接

- [Home](../Home.md)
- [MCP功能使用说明](MCP功能使用说明.md)
- [内置模型管理](内置模型管理.md)
