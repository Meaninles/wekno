# 技能 API

[返回目录](./README.md)

## API 列表

| 方法 | 路径 | 描述 |
| ---- | ---- | ---- |
| GET | `/skills` | 获取当前可见的轻量技能和专业技能元数据 |
| GET | `/custom/skills` | 管理页获取轻量技能列表 |
| POST | `/custom/skills` | 创建轻量技能 |
| PUT | `/custom/skills/:id` | 更新轻量技能 |
| DELETE | `/custom/skills/:id` | 删除轻量技能 |
| GET | `/custom/skills/professional` | 管理页获取专业技能列表 |
| POST | `/custom/skills/professional` | 导入专业技能包 |
| PUT | `/custom/skills/professional/:id` | 更新专业技能 |
| DELETE | `/custom/skills/professional/:id` | 删除专业技能 |
| GET | `/custom/skills/professional/:id/download` | 下载专业技能包 |

共享相关接口挂在 `/custom/skills/:id/shares/*` 和 `/custom/skills/professional/:id/shares/*` 下，用于共享给组织或用户。

## GET `/skills` - 获取技能元数据

该接口供前端智能体编辑器和对话技能选择器读取技能元数据。响应分为两组：

- `data`：轻量技能。轻量技能是提示词/上下文片段，不要求 `SKILL.md` 格式。
- `professional_data`：专业技能。专业技能是可导入的技能包，通常包含 `SKILL.md`、引用文件、脚本或模板。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/skills' \
--header 'X-API-Key: sk-xxxxx'
```

**响应**:

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

当没有可用轻量技能时，`skills_available` 为 `false`；当没有可用专业技能时，`professional_skills_available` 为 `false`。两个字段互不代表对方是否可用。

## 管理接口说明

轻量技能和专业技能的创建、更新、删除、导入、下载和共享由 `/api/v1/custom/skills/*` 提供。接口实现位于 `internal/custom/modules/skillhub`。

当前运行时边界：

- 轻量技能可配置到智能体，也可在对话中临时选择。
- 专业技能当前主要用于 `general-agent` 和 `document-processing-agent`。运行时会把专业技能包落到 Claude SDK 旁路服务的 `.claude/skills/<name>` 目录。
- `data-analysis` 当前不展示工具、MCP、技能配置页。
