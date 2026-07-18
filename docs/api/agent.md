# 智能体（Agent）管理 API

[返回目录](./README.md)

## 概述

智能体 API 用于管理自定义智能体（Custom Agent）。系统提供了内置智能体，同时支持用户创建自定义智能体来满足不同的业务场景需求。

> 智能体的共享与跨组织分发（`/agents/:id/shares` 等）属于组织协作能力，文档见 [组织管理 API](./organization.md)。本文件只覆盖智能体自身的 CRUD、复制、占位符、类型预设以及推荐问题接口。

### 内置智能体

系统默认从 `config/builtin_agents.yaml` 加载以下内置智能体：

| ID | 名称 | 描述 | 模式 |
|----|------|------|------|
| `builtin-quick-answer` | 快速问答 | 基于知识库的 RAG 问答 | `quick-answer` |
| `builtin-simple-chat` | 简单对话 | 不默认绑定知识库，支持联网搜索和文件、图片、音频上传 | `quick-answer` |
| `builtin-smart-reasoning` | 智能推理 | ReAct 推理框架，支持多步思考和工具调用 | `smart-reasoning` / `rag-qa` |
| `builtin-data-analyst` | 数据分析 | Claude SDK 数据分析智能体，绑定 MySQL/PostgreSQL 数据源后使用 SQL 和图表分析数据 | `smart-reasoning` / `data-analysis` |
| `builtin-table-analyst` | 表格分析 | Claude SDK 表格分析智能体，分析 CSV/Excel 知识库文件或对话附件并生成图表 | `smart-reasoning` / `table-analysis` |
| `builtin-general-agent` | 通用智能体 | Claude SDK 通用智能体，复用知识库、联网、MCP、技能、数据库源和产物生成 | `smart-reasoning` / `general-agent` |
| `builtin-document-processing` | 文档处理 | Claude SDK 文档处理智能体，生成或修改 Word、Excel、PDF、PPT 文档 | `smart-reasoning` / `document-processing-agent` |
| `builtin-wiki-researcher` | 维基问答 | 面向 Wiki 知识库的问答智能体 | `smart-reasoning` / `wiki-qa` |
| `builtin-wiki-fixer` | 维基修订 | 面向 Wiki 巡检问题的页面修订智能体，配置中存在但普通排序列表不默认展示 | `custom` |

### 智能体模式

| 模式 | 说明 |
|------|------|
| `quick-answer` | RAG 模式，快速问答，直接基于知识库检索结果生成回答 |
| `smart-reasoning` | ReAct 模式，支持多步推理和工具调用 |

`smart-reasoning` 下还可以通过 `agent_type` 选择类型预设：`rag-qa`、`wiki-qa`、`hybrid-rag-wiki`、`data-analysis`、`table-analysis`、`document-processing-agent`、`general-agent`、`knowledge-base-manager`、`custom`。其中 `general-agent`、`knowledge-base-manager`、`document-processing-agent`、`data-analysis`、`table-analysis` 会进入 Claude SDK sidecar 运行时。

## API 列表

| 方法   | 路径                       | 描述                       |
| ------ | -------------------------- | -------------------------- |
| POST   | `/agents`                  | 创建智能体                 |
| GET    | `/agents`                  | 获取智能体列表             |
| GET    | `/agents/:id`              | 获取智能体详情             |
| PUT    | `/agents/:id`              | 更新智能体                 |
| DELETE | `/agents/:id`              | 删除智能体                 |
| POST   | `/agents/:id/copy`         | 复制智能体                 |
| GET    | `/agents/placeholders`     | 获取占位符定义             |

---

## POST `/agents` - 创建智能体

创建新的自定义智能体。成功返回 HTTP 201。

**请求体参数**:

| 参数          | 类型   | 必填 | 说明                                              |
| ------------- | ------ | ---- | ------------------------------------------------- |
| `name`        | string | 是   | 智能体名称                                        |
| `description` | string | 否   | 智能体描述                                        |
| `avatar`      | string | 否   | 头像（emoji 或图标名称）                          |
| `config`      | object | 否   | 智能体配置，详见 [配置参数](#配置参数)            |

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/agents' \
--header 'X-API-Key: sk-xxxxx' \
--header 'Content-Type: application/json' \
--data '{
    "name": "我的智能体",
    "description": "自定义智能体描述",
    "avatar": "🤖",
    "config": {
        "agent_mode": "smart-reasoning",
        "system_prompt": "你是一个专业的助手...",
        "temperature": 0.7,
        "max_iterations": 10,
        "kb_selection_mode": "all",
        "web_search_enabled": true,
        "multi_turn_enabled": true,
        "history_turns": 5
    }
}'
```

**响应**:

```json
{
    "success": true,
    "data": {
        "id": "550e8400-e29b-41d4-a716-446655440000",
        "name": "我的智能体",
        "description": "自定义智能体描述",
        "avatar": "🤖",
        "is_builtin": false,
        "tenant_id": 1,
        "created_by": "user-123",
        "config": {
            "agent_mode": "smart-reasoning",
            "system_prompt": "你是一个专业的助手...",
            "temperature": 0.7,
            "max_iterations": 10
        },
        "created_at": "2025-01-19T10:00:00Z",
        "updated_at": "2025-01-19T10:00:00Z"
    }
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明                              |
| ------ | ------ | --------------------- | --------------------------------- |
| 400    | 1000   | Bad Request           | 请求参数错误或智能体名称为空      |
| 500    | 1007   | Internal Server Error | 服务器内部错误                    |

---

## GET `/agents` - 获取智能体列表

获取当前租户的所有智能体，包括内置智能体和自定义智能体。响应中额外返回 `disabled_own_agent_ids`，指示当前租户在前端对话下拉框中主动隐藏的本租户自有智能体 ID 列表（不影响其他租户）。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/agents' \
--header 'X-API-Key: sk-xxxxx'
```

**响应**:

```json
{
    "success": true,
    "data": [
        {
            "id": "builtin-quick-answer",
            "name": "快速问答",
            "description": "基于知识库的 RAG 问答，快速准确地回答问题",
            "avatar": "💬",
            "is_builtin": true,
            "tenant_id": 10000,
            "created_by": "",
            "config": {
                "agent_mode": "quick-answer",
                "temperature": 0.3,
                "max_completion_tokens": 2048,
                "kb_selection_mode": "all",
                "web_search_enabled": false,
                "multi_turn_enabled": true,
                "history_turns": 5
            },
            "created_at": "2025-12-29T20:06:01.696308+08:00",
            "updated_at": "2025-12-29T20:06:01.696308+08:00",
            "deleted_at": null
        },
        {
            "id": "550e8400-e29b-41d4-a716-446655440000",
            "name": "我的智能体",
            "is_builtin": false,
            "config": {
                "agent_mode": "smart-reasoning"
            }
        }
    ],
    "disabled_own_agent_ids": []
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明               |
| ------ | ------ | --------------------- | ------------------ |
| 401    | 1001   | Unauthorized          | 缺少租户上下文     |
| 500    | 1007   | Internal Server Error | 服务器内部错误     |

---

## GET `/agents/:id` - 获取智能体详情

根据 ID 获取智能体的详细信息。

**路径参数**:

| 参数 | 类型   | 说明     |
| ---- | ------ | -------- |
| `id` | string | 智能体 ID |

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/agents/builtin-quick-answer' \
--header 'X-API-Key: sk-xxxxx'
```

**响应**:

```json
{
    "success": true,
    "data": {
        "id": "builtin-quick-answer",
        "name": "快速问答",
        "description": "基于知识库的 RAG 问答，快速准确地回答问题",
        "is_builtin": true,
        "tenant_id": 1,
        "config": {
            "agent_mode": "quick-answer",
            "system_prompt": "",
            "context_template": "请根据以下参考资料回答用户问题...",
            "temperature": 0.7,
            "max_completion_tokens": 2048,
            "kb_selection_mode": "all",
            "web_search_enabled": true,
            "multi_turn_enabled": true,
            "history_turns": 5
        },
        "created_at": "2025-01-01T00:00:00Z",
        "updated_at": "2025-01-01T00:00:00Z"
    }
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明               |
| ------ | ------ | --------------------- | ------------------ |
| 400    | 1000   | Bad Request           | 智能体 ID 为空     |
| 404    | 1003   | Not Found             | 智能体不存在       |
| 500    | 1007   | Internal Server Error | 服务器内部错误     |

---

## PUT `/agents/:id` - 更新智能体

更新智能体的名称、描述、头像和配置。内置智能体不可修改。

**路径参数**:

| 参数 | 类型   | 说明     |
| ---- | ------ | -------- |
| `id` | string | 智能体 ID |

**请求体参数**:

| 参数          | 类型   | 必填 | 说明         |
| ------------- | ------ | ---- | ------------ |
| `name`        | string | 否   | 智能体名称   |
| `description` | string | 否   | 智能体描述   |
| `avatar`      | string | 否   | 智能体头像   |
| `config`      | object | 否   | 智能体配置   |

**请求**:

```curl
curl --location --request PUT 'http://localhost:8080/api/v1/agents/550e8400-e29b-41d4-a716-446655440000' \
--header 'X-API-Key: sk-xxxxx' \
--header 'Content-Type: application/json' \
--data '{
    "name": "更新后的智能体",
    "description": "更新后的描述",
    "config": {
        "agent_mode": "smart-reasoning",
        "temperature": 0.8,
        "max_iterations": 20
    }
}'
```

**响应**:

```json
{
    "success": true,
    "data": {
        "id": "550e8400-e29b-41d4-a716-446655440000",
        "name": "更新后的智能体",
        "description": "更新后的描述",
        "config": {
            "agent_mode": "smart-reasoning",
            "temperature": 0.8,
            "max_iterations": 20
        },
        "updated_at": "2025-01-19T11:00:00Z"
    }
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明                              |
| ------ | ------ | --------------------- | --------------------------------- |
| 400    | 1000   | Bad Request           | 请求参数错误或智能体名称为空      |
| 403    | 1002   | Forbidden             | 无法修改内置智能体                |
| 404    | 1003   | Not Found             | 智能体不存在                      |
| 500    | 1007   | Internal Server Error | 服务器内部错误                    |

---

## DELETE `/agents/:id` - 删除智能体

删除指定的自定义智能体。内置智能体不可删除。

**路径参数**:

| 参数 | 类型   | 说明     |
| ---- | ------ | -------- |
| `id` | string | 智能体 ID |

**请求**:

```curl
curl --location --request DELETE 'http://localhost:8080/api/v1/agents/550e8400-e29b-41d4-a716-446655440000' \
--header 'X-API-Key: sk-xxxxx'
```

**响应**:

```json
{
    "success": true,
    "message": "Agent deleted successfully"
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明                  |
| ------ | ------ | --------------------- | --------------------- |
| 400    | 1000   | Bad Request           | 智能体 ID 为空        |
| 403    | 1002   | Forbidden             | 无法删除内置智能体    |
| 404    | 1003   | Not Found             | 智能体不存在          |
| 500    | 1007   | Internal Server Error | 服务器内部错误        |

---

## POST `/agents/:id/copy` - 复制智能体

复制指定的智能体，创建一个新的副本，副本始终为自定义智能体。支持复制内置智能体。成功返回 HTTP 201。

**路径参数**:

| 参数 | 类型   | 说明           |
| ---- | ------ | -------------- |
| `id` | string | 源智能体 ID    |

**请求**:

```curl
curl --location --request POST 'http://localhost:8080/api/v1/agents/builtin-smart-reasoning/copy' \
--header 'X-API-Key: sk-xxxxx'
```

**响应**:

```json
{
    "success": true,
    "data": {
        "id": "660e8400-e29b-41d4-a716-446655440001",
        "name": "智能推理 (副本)",
        "description": "ReAct 推理框架，支持多步思考和工具调用",
        "is_builtin": false,
        "config": {
            "agent_mode": "smart-reasoning",
            "max_iterations": 50
        },
        "created_at": "2025-01-19T12:00:00Z",
        "updated_at": "2025-01-19T12:00:00Z"
    }
}
```

**错误响应**:

| 状态码 | 错误码 | 错误                  | 说明               |
| ------ | ------ | --------------------- | ------------------ |
| 400    | 1000   | Bad Request           | 智能体 ID 为空     |
| 404    | 1003   | Not Found             | 智能体不存在       |
| 500    | 1007   | Internal Server Error | 服务器内部错误     |

---

## GET `/agents/placeholders` - 获取占位符定义

获取所有可用的提示词占位符定义，按字段类型分组。这些占位符可用于系统提示词和上下文模板中。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/agents/placeholders' \
--header 'X-API-Key: your_api_key'
```

**响应**:

```json
{
    "success": true,
    "data": {
        "all": [...],
        "system_prompt": [...],
        "agent_system_prompt": [...],
        "context_template": [...],
        "rewrite_system_prompt": [...],
        "rewrite_prompt": [...],
        "fallback_prompt": [...]
    }
}
```

---

## 配置参数

智能体的 `config` 对象支持以下配置项：

### 基础设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `agent_mode` | string | - | 智能体模式：`quick-answer`（RAG）或 `smart-reasoning`（ReAct） |
| `agent_type` | string | `custom` | 智能推理类型预设：`rag-qa`、`wiki-qa`、`hybrid-rag-wiki`、`data-analysis`、`table-analysis`、`document-processing-agent`、`general-agent`、`custom` |
| `system_prompt` | string | - | 系统提示词，支持使用占位符 |
| `system_prompt_id` | string | - | 系统提示词模板 ID（引用 `prompt_templates/` YAML 文件中的模板） |
| `context_template` | string | - | 上下文模板（仅 quick-answer 模式使用） |
| `context_template_id` | string | - | 上下文模板 ID（引用 `prompt_templates/` YAML 文件中的模板） |
| `document_template` | object | - | 文档处理智能体的 Word/Excel/PDF/PPT 要求文件和模板文件配置 |

### 模型设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `model_id` | string | - | 对话模型 ID |
| `rerank_model_id` | string | - | 重排序模型 ID |
| `temperature` | float | 0.7 | 温度参数（0-1） |
| `max_completion_tokens` | int | 2048 | 最大生成 token 数 |
| `thinking` | *bool | nil | 是否启用思考模式（适用于支持扩展思考的模型） |
| `enable_artifacts` | bool | false | Claude SDK 通用/文档处理运行时是否允许生成可下载产物 |

### 智能体模式设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `max_iterations` | int | 10 | ReAct 最大迭代次数 |
| `llm_call_timeout` | int | - | 单次 LLM 调用超时秒数，0 或空使用全局默认 |
| `allowed_tools` | []string | - | 允许使用的工具列表 |
| `mcp_selection_mode` | string | - | MCP 服务选择模式：`all`/`selected`/`none` |
| `mcp_services` | []string | - | 选中的 MCP 服务 ID 列表 |
| `mcp_auth_wait_timeout` | int | - | MCP OAuth 授权等待超时秒数 |
| `skills_selection_mode` | string | - | 旧字段，当前作为轻量技能选择模式的兼容回退 |
| `selected_skills` | []string | - | 旧字段，当前作为轻量技能列表的兼容回退 |
| `lightweight_skills_selection_mode` | string | - | 轻量技能选择模式：`all`/`selected`/`none` |
| `selected_lightweight_skills` | []string | - | 选中的轻量技能名称列表 |
| `professional_skills_selection_mode` | string | - | 专业技能选择模式：`all`/`selected`/`none`，当前主要用于 `general-agent` 和 `document-processing-agent` |
| `selected_professional_skills` | []string | - | 选中的专业技能名称列表 |

### 数据库数据源设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `db_data_sources` | []string | - | 绑定的 MySQL/PostgreSQL 数据源 ID。`data-analysis` 必填，`general-agent` 和 `document-processing-agent` 可选，`table-analysis` 不使用 |

### 知识库设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `kb_selection_mode` | string | - | 知识库选择模式：`all`/`selected`/`none` |
| `knowledge_bases` | []string | - | 关联的知识库 ID 列表 |
| `retrieve_kb_only_when_mentioned` | bool | false | 仅在用户通过 @ 显式提及时才检索知识库 |
| `retain_retrieval_history` | bool | false | 是否保留检索历史供后续轮次参考 |
| `supported_file_types` | []string | - | 支持的文件类型（如 `["csv", "xlsx"]`） |

### 图片上传 / 多模态设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `image_upload_enabled` | bool | false | 是否允许上传图片 |
| `vlm_model_id` | string | - | 图片分析所用的 VLM 模型 ID |
| `image_storage_provider` | string | - | 图片存储提供者：`local`/`minio`/`cos`/`tos`/`oss`，为空使用全局默认 |
| `audio_upload_enabled` | bool | false | 是否允许上传音频 |
| `asr_model_id` | string | - | 音频转写所用的 ASR 模型 ID |

### FAQ 策略设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `faq_priority_enabled` | bool | true | FAQ 优先策略开关 |
| `faq_direct_answer_threshold` | float | 0.9 | FAQ 直接回答阈值 |
| `faq_score_boost` | float | 1.2 | FAQ 分数加成系数 |

### 网络搜索设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `web_search_enabled` | bool | true | 是否启用网络搜索 |
| `claude_sdk_web_search_enabled` | bool | false | Claude SDK 运行时是否启用原生 web search 能力 |
| `web_search_max_results` | int | 5 | 网络搜索最大结果数 |
| `web_search_provider_id` | string | - | 网络搜索提供者 ID，为空使用租户默认提供者 |
| `web_fetch_enabled` | bool | false | 是否自动获取重排后的搜索结果页面全文 |
| `web_fetch_top_n` | int | 3 | 重排后获取全文的最大页面数 |

### 多轮对话设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `multi_turn_enabled` | bool | true | 是否启用多轮对话 |
| `history_turns` | int | 5 | 保留的历史轮次数 |

### 检索策略设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `embedding_top_k` | int | 10 | 向量检索 TopK |
| `keyword_threshold` | float | 0.3 | 关键词检索阈值 |
| `vector_threshold` | float | 0.5 | 向量检索阈值 |
| `rerank_top_k` | int | 5 | 重排序 TopK |
| `rerank_threshold` | float | 0.5 | 重排序阈值 |

### 推荐问题设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `suggested_prompts` | []string | - | 推荐问题列表，用于在前端对话面板展示快捷提问 |

### 高级设置

| 参数 | 类型 | 默认值 | 说明 |
|------|------|--------|------|
| `enable_query_expansion` | bool | true | 是否启用查询扩展 |
| `enable_rewrite` | bool | true | 是否启用多轮对话查询改写 |
| `rewrite_prompt_system` | string | - | 改写系统提示词 |
| `rewrite_prompt_user` | string | - | 改写用户提示词模板 |
| `query_understand_model_id` | string | - | 查询理解、改写等辅助 LLM 任务使用的模型 ID |
| `fallback_strategy` | string | `model` | 回退策略：`fixed`（固定回复）或 `model`（模型生成）；未设置时在服务端默认为 `model` |
| `fallback_response` | string | - | 固定回退回复（`fallback_strategy` 为 `fixed` 时使用） |
| `fallback_prompt` | string | - | 回退提示词（`fallback_strategy` 为 `model` 时使用） |
| `intent_prompts` | []object | - | 非检索意图、问候、闲聊等场景的提示词覆盖 |

---

## 使用智能体进行问答

创建或获取智能体后，可以通过 `/agent-chat/:session_id` 接口使用智能体进行问答。详情请参考 [聊天功能 API](./chat.md)。

在问答请求中使用 `agent_id` 参数指定要使用的智能体：

```curl
curl --location 'http://localhost:8080/api/v1/agent-chat/session-123' \
--header 'X-API-Key: sk-xxxxx' \
--header 'Content-Type: application/json' \
--data '{
    "query": "帮我分析一下这份数据",
    "agent_enabled": true,
    "agent_id": "builtin-data-analyst"
}'
```

## 相关文档

- 智能体的组织共享、跨租户分发与禁用（`/agents/:id/shares`、`/shared-agents` 等）：见 [组织管理 API](./organization.md)
- 智能体绑定 IM 渠道（`/agents/:id/im-channels`）：见组织/IM 渠道相关文档
- 网络搜索提供者配置（被 `web_search_provider_id` 引用）：见 [Web Search API](./web-search.md)
