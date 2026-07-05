# 智能体技能文档

## 概述

当前项目中“技能”分为三类，运行时和配置方式不同：

| 类型 | 来源 | 运行时用途 |
| --- | --- | --- |
| 轻量技能 | `internal/custom/modules/skillhub` 管理，前端“技能”页创建 | 作为提示词/上下文片段注入智能体，可共享给空间或用户，也可在对话中临时选择。 |
| 预加载运行时技能 | `skills/preloaded/` | 原生智能体技能目录，按渐进式披露方式读取 `SKILL.md` 和资源文件，可通过沙箱执行脚本。 |
| 专业技能 | `internal/custom/modules/skillhub` 管理，通常从技能包导入 | Claude SDK 运行时会把技能包落到 `.claude/skills/<name>`，供 `general-agent` 和 `document-processing-agent` 按需使用。 |

轻量技能不要求 `SKILL.md` 格式；预加载运行时技能和专业技能通常包含 `SKILL.md`。前端会分别展示轻量技能和专业技能。

## 渐进式披露

预加载运行时技能和专业技能都遵循渐进式披露思想：先让模型看到技能名称和描述，只有任务需要时再读取详细指令或附加资源。

```text
第 1 层：元数据
  - 技能名称
  - 简短描述

第 2 层：主指令
  - SKILL.md

第 3 层：附加资源
  - references/
  - templates/
  - scripts/
  - 其他技能包文件
```

原生运行时技能通过 `read_skill` 读取内容，通过 `execute_skill_script` 在沙箱中执行脚本。Claude SDK 专业技能由旁路服务放到 `.claude/skills/<name>` 后交给 Claude SDK 按需读取。

## SKILL.md 格式

专业技能和预加载运行时技能建议包含 `SKILL.md`，并使用 YAML frontmatter 描述元数据：

```markdown
---
name: pdf-processing
description: 从 PDF 文件中提取文本和表格。用户要求分析或转换 PDF 文档时使用。
---

# PDF 处理

这里编写详细指令和工作流程。
```

字段建议：

| 字段 | 要求 |
| --- | --- |
| `name` | 稳定唯一，建议使用小写英文、数字和连字符。 |
| `description` | 描述能力边界和触发场景，便于模型判断何时使用。 |

## 轻量技能

轻量技能由二开技能中心管理，适合固定写作风格、术语约束、输出模板、审校规则等场景。它们不会作为文件技能包挂载到 `.claude/skills`，也不依赖沙箱脚本。

相关配置字段：

| 字段 | 说明 |
| --- | --- |
| `lightweight_skills_selection_mode` | `all` / `selected` / `none` |
| `selected_lightweight_skills` | `selected` 模式下的技能名称列表 |
| `skills_selection_mode` | 旧字段，当前作为轻量技能选择模式的兼容回退 |
| `selected_skills` | 旧字段，当前作为轻量技能列表的兼容回退 |

轻量技能可以：

- 在前端“技能”页创建、更新、删除。
- 共享给空间或用户。
- 固定配置到智能体。
- 在对话输入区临时选择。

## 专业技能

专业技能是可导入的技能包，适合复杂工作法、行业规范、模板、脚本或多文件说明。管理接口位于 `/api/v1/custom/skills/professional*`。

相关配置字段：

| 字段 | 说明 |
| --- | --- |
| `professional_skills_selection_mode` | `all` / `selected` / `none` |
| `selected_professional_skills` | `selected` 模式下的专业技能名称列表 |

当前前端主要在 `general-agent` 和 `document-processing-agent` 的技能配置页展示专业技能。运行时会把选中的专业技能传给 Claude SDK 旁路服务，旁路服务在运行目录中准备 `.claude/skills/<name>`。

当前仓库内置专业技能目录位于 `skills/professional/`，包括：

- `anysearch-skill`
- `find-skill-skillhub`

## 预加载运行时技能

预加载技能位于 `skills/preloaded/`，当前目录为：

| 技能 | 用途 |
| --- | --- |
| `citation-generator` | 引用生成和来源标注。 |
| `data-processor` | 数据处理、格式转换、结构化提取。 |
| `doc-coauthoring` | 引导结构化文档协作创作。 |
| `document-analyzer` | 分析文档结构、主题、质量和关键信息。 |
| `openmaic-classroom` | OpenMAIC 课堂相关预加载技能。 |

目录结构示例：

```text
skills/preloaded/
├── citation-generator/
│   └── SKILL.md
├── data-processor/
│   ├── SKILL.md
│   └── scripts/
├── doc-coauthoring/
│   └── SKILL.md
├── document-analyzer/
│   └── SKILL.md
└── openmaic-classroom/
    └── SKILL.md
```

## 沙箱执行

预加载运行时技能中的脚本通过沙箱执行。相关环境变量：

| 环境变量 | 说明 | 默认值 |
| --- | --- | --- |
| `WEKNORA_SANDBOX_MODE` | `docker`、`local` 或 `disabled` | `disabled` |
| `WEKNORA_SANDBOX_TIMEOUT` | 脚本执行超时秒数 | `60` |
| `WEKNORA_SANDBOX_DOCKER_IMAGE` | Docker 沙箱镜像 | `wechatopenai/weknora-sandbox:latest` |

轻量技能不执行脚本，不受沙箱模式影响。专业技能在 Claude SDK 旁路服务中作为技能包使用，具体脚本能力取决于旁路服务能力和技能包内容。

## 接口

`GET /api/v1/skills` 返回当前可见的轻量技能和专业技能元数据：

```json
{
  "success": true,
  "data": [
    {
      "name": "policy-style",
      "display_name": "制度写作风格",
      "description": "按公司制度文档风格输出",
      "kind": "lightweight"
    }
  ],
  "professional_data": [
    {
      "name": "word-docx",
      "display_name": "Word 文档处理",
      "description": "处理和生成 docx 文档",
      "kind": "professional"
    }
  ],
  "skills_available": true,
  "professional_skills_available": true
}
```

管理接口：

- `/api/v1/custom/skills`：轻量技能 CRUD、共享。
- `/api/v1/custom/skills/professional`：专业技能导入、更新、删除、下载、共享。

## 使用建议

- 简单规则、输出格式、固定话术优先做轻量技能。
- 多文件说明、模板、复杂工作法优先做专业技能。
- 需要脚本执行的能力必须明确沙箱边界，不要把任意代码执行能力混入普通提示词技能。
- 专业技能名称和描述要清楚描述触发条件，避免模型在无关任务中误用。
