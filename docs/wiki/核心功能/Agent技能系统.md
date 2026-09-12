---
title: Agent技能系统
tags: [核心功能, Agent, Skills, 技能, 沙箱]
aliases: [智能体技能, 技能系统, agent-skills]
source: agent-skills.md
---

# 智能体技能系统

## 概述

当前项目中“技能”分为三类：

| 类型 | 来源 | 用途 |
|------|------|------|
| 轻量技能 | 二开技能中心创建 | 提示词/上下文片段，可配置到智能体或在对话中临时选择。 |
| 预加载运行时技能 | `skills/preloaded/` | 原生 Agent skills 目录，通过 `read_skill` / `execute_skill_script` 渐进读取和执行。 |
| 专业技能 | 二开技能中心导入 | 技能包形式，由统一 Agent Runtime 准备到受控工作区 `/workspace/skills/<name>`。 |

轻量技能不要求 `SKILL.md`；预加载运行时技能和专业技能通常包含 `SKILL.md`。

## 渐进式披露

技能通过渐进式披露减少上下文占用：

```text
第 1 层：元数据，名称和描述
第 2 层：SKILL.md 主指令
第 3 层：references、templates、scripts 等附加资源
```

原生运行时技能通过工具读取内容和执行脚本；专业技能由统一 Agent Runtime 挂载给 AgentScope Harness 按需读取。

## 预加载技能

当前仓库不再内置默认预加载轻量技能。需要增加预加载技能时，在 `skills/preloaded/` 下添加包含 `SKILL.md` 的技能目录即可。

## 配置字段

轻量技能：

```json
{
  "lightweight_skills_selection_mode": "selected",
  "selected_lightweight_skills": ["policy-style"]
}
```

专业技能：

```json
{
  "professional_skills_selection_mode": "selected",
  "selected_professional_skills": ["word-docx"]
}
```

旧字段 `skills_selection_mode` 和 `selected_skills` 当前作为轻量技能兼容回退。

## 接口

`GET /api/v1/skills` 返回轻量技能 `data`、专业技能 `professional_data`、`skills_available` 和 `professional_skills_available`。

管理接口位于：

- `/api/v1/custom/skills`
- `/api/v1/custom/skills/professional`

## 沙箱

预加载运行时技能的脚本执行受 `WEKNORA_SANDBOX_MODE`、`WEKNORA_SANDBOX_TIMEOUT` 和 `WEKNORA_SANDBOX_DOCKER_IMAGE` 控制。轻量技能不执行脚本。

## 相关主题

- [MCP功能使用说明](MCP功能使用说明.md) — 另一种 Agent 扩展机制
- [开发指南](../开发部署/开发指南.md) — 沙箱镜像的构建

---

## 反向链接

- [Home](../Home.md) — Wiki 首页导航
- [MCP功能使用说明](MCP功能使用说明.md) — 与 Skills 并列的 Agent 扩展机制
