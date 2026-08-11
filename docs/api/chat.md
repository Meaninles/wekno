# 聊天功能 API

[返回目录](./README.md)

| 方法 | 路径                          | 描述                     |
| ---- | ----------------------------- | ------------------------ |
| POST | `/knowledge-chat/:session_id` | 基于知识库的问答         |
| POST | `/agent-chat/:session_id`     | 基于 Agent 的智能问答    |
| POST | `/knowledge-search`           | 基于知识库的搜索知识     |

## POST `/knowledge-chat/:session_id` - 基于知识库的问答

基于知识库的 RAG 问答，支持 SSE 流式响应。

这三个接口都挂在 `/api/v1` 下，并通过 `Viewer` 认证；可以使用登录态 Bearer
Token 或 `X-API-Key`。`session_id` 必须是调用者可访问的会话。

**请求参数**：

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `query` | string | 是 | 查询文本 |
| `knowledge_base_ids` | string[] | 否 | 知识库 ID 列表 |
| `knowledge_ids` | string[] | 否 | 知识文件 ID 列表，指定具体文件进行检索 |
| `agent_id` | string | 否 | 自定义 Agent ID，指定使用的智能体 |
| `agent_enabled` | bool | 否 | 本次请求是否启用 Agent 模式 |
| `web_search_enabled` | bool | 否 | 本次请求是否启用网络搜索 |
| `summary_model_id` | string | 否 | 覆盖默认的摘要模型 ID |
| `mcp_service_ids` | string[] | 否 | 本次通过 @提及选择的 MCP 服务 |
| `skill_names` | string[] | 否 | 本次通过 @提及选择的轻量 Skill |
| `professional_skill_names` | string[] | 否 | 输入栏选择的专业 Skill 名称 |
| `tag_ids` | string[] | 否 | 本次 @提及的标签 ID；具体作用域由 `mentioned_items` 提供 |
| `mentioned_items` | object[] | 否 | @提及的知识库、文件、标签、MCP 或 Skill |
| `disable_title` | bool | 否 | 是否禁用自动标题生成（默认 false） |
| `enable_memory` | bool | 否 | 显式覆盖本次请求的记忆开关；省略时使用当前用户偏好 |
| `images` | object[] | 否 | 附带的图片，客户端发送 base64 数据 |
| `attachment_uploads` | object[] | 否 | 附带的文件，客户端发送 base64 数据、文件名和字节数 |
| `channel` | string | 否 | 来源渠道标识：`web`、`api`、`im`、`browser_extension` |

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/knowledge-chat/ceb9babb-1e30-41d7-817d-fd584954304b' \
--header 'X-API-Key: sk-xxxxx' \
--header 'Content-Type: application/json' \
--data '{
    "query": "彗尾的形状",
    "knowledge_base_ids": ["kb-00000001"],
    "agent_id": "builtin-quick-answer"
}'
```

**响应格式**:
服务器端事件流（Server-Sent Events，Content-Type: text/event-stream）

**响应**:

```
event: message
data: {"id":"3475c004-0ada-4306-9d30-d7f5efce50d2","response_type":"references","content":"","done":false,"knowledge_references":[{"id":"c8347bef-...","content":"彗星xxx。","knowledge_id":"a6790b93-...","chunk_index":0,"knowledge_title":"彗星.txt","score":4.04,"match_type":3,"chunk_type":"text","knowledge_filename":"彗星.txt"}]}

event: message
data: {"id":"3475c004-0ada-4306-9d30-d7f5efce50d2","response_type":"answer","content":"彗尾的形状主要表现为...","done":false,"knowledge_references":null}

event: message
data: {"id":"3475c004-0ada-4306-9d30-d7f5efce50d2","response_type":"answer","content":"","done":true,"knowledge_references":null}
```

## POST `/agent-chat/:session_id` - 基于 Agent 的智能问答

Agent 模式支持更智能的问答，包括工具调用、网络搜索、多知识库检索等能力。

**请求参数**：

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `query` | string | 是 | 查询文本 |
| `knowledge_base_ids` | string[] | 否 | 知识库 ID 列表，可动态指定本次查询使用的知识库 |
| `knowledge_ids` | string[] | 否 | 知识文件 ID 列表，可动态指定本次查询使用的具体文件 |
| `agent_enabled` | bool | 否 | 是否启用智能体模式（默认 false，优先使用智能体配置） |
| `agent_id` | string | 否 | 自定义 Agent ID，指定使用的智能体（支持共享 Agent） |
| `web_search_enabled` | bool | 否 | 是否启用网络搜索（默认 false） |
| `summary_model_id` | string | 否 | 覆盖默认的摘要模型 ID |
| `mcp_service_ids` | string[] | 否 | 本次选择的 MCP 服务 ID |
| `skill_names` | string[] | 否 | 本次选择的轻量 Skill 名称 |
| `professional_skill_names` | string[] | 否 | 本次选择的专业 Skill 名称 |
| `tag_ids` | string[] | 否 | 本次 @提及的标签 ID |
| `mentioned_items` | object[] | 否 | @提及的知识库、文件、标签、MCP 或 Skill |
| `disable_title` | bool | 否 | 是否禁用自动标题生成（默认 false） |
| `enable_memory` | bool | 否 | 显式覆盖本次请求的记忆开关；嵌入模式会强制关闭 |
| `images` | object[] | 否 | 附带的图片，客户端发送 base64 数据 |
| `attachment_uploads` | object[] | 否 | 附带的文件，客户端发送 base64 数据、文件名和字节数 |
| `channel` | string | 否 | 来源渠道标识：`web`、`api`、`im`、`browser_extension` |

**公共请求字段结构**：

`mentioned_items` 的 `type` 当前支持 `kb`、`file`、`tag`、`mcp`、`skill`。不同类型
使用的关联字段如下：

| 字段 | 类型 | 说明 |
|------|------|------|
| `id` | string | 被提及对象 ID |
| `name` | string | 显示名称 |
| `type` | string | `kb`、`file`、`tag`、`mcp` 或 `skill` |
| `kb_type` | string | `document` 或 `faq`，用于 `kb` |
| `kb_id` | string | 文件或标签所属知识库 ID |
| `kb_name` | string | 文件或标签所属知识库名称 |
| `service_id` | string | MCP 工具所属服务 ID |
| `skill_name` | string | 预加载 Agent Skill 名称 |

**images 结构**：

| 字段 | 类型 | 说明 |
|------|------|------|
| `data` | string | base64 编码的图片数据（`data:image/png;base64,...`） |
| `url` | string | 服务端保存后的图片 URL；请求时通常不需要填写 |
| `caption` | string | 服务端完成 VLM 分析后写入的描述；请求时通常不需要填写 |

`attachment_uploads` 结构如下：

| 字段 | 类型 | 说明 |
|------|------|------|
| `data` | string | Base64 编码的文件内容 |
| `file_name` | string | 原始文件名 |
| `file_size` | integer | 文件大小，单位为字节 |

附件大小由 `MAX_FILE_SIZE_MB` 控制，默认是 50 MiB；选择了自定义 Agent 时还会按
该 Agent 的 `supported_file_types` 校验扩展名。`enable_memory` 省略时使用调用用户
持久化的 `preferences.enable_memory`，未设置偏好时为关闭；显式传 `true` 或 `false`
时覆盖该偏好，嵌入模式会强制为 `false`。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/agent-chat/ceb9babb-1e30-41d7-817d-fd584954304b' \
--header 'X-API-Key: sk-xxxxx' \
--header 'Content-Type: application/json' \
--data '{
    "query": "帮我查询今天的天气",
    "agent_enabled": true,
    "web_search_enabled": true,
    "knowledge_base_ids": ["kb-00000001"],
    "agent_id": "builtin-smart-reasoning",
    "mentioned_items": [
        {
            "id": "kb-00000001",
            "name": "天气知识库",
            "type": "kb",
            "kb_type": "document"
        }
    ]
}'
```

**响应格式**:
服务器端事件流（Server-Sent Events，Content-Type: text/event-stream）

**响应类型说明**：

| response_type | 描述 |
|---------------|------|
| `agent_query` | Agent 开始处理查询 |
| `thinking` | Agent 思考过程 |
| `tool_call` | 工具调用信息 |
| `tool_result` | 工具调用结果 |
| `references` | 知识库检索引用 |
| `answer` | 最终回答内容 |
| `reflection` | Agent 反思内容 |
| `session_title` | 自动生成的会话标题 |
| `error` | 错误信息 |

**响应示例**:

```
event: message
data: {"id":"req-001","response_type":"thinking","content":"用户想查询天气，我需要使用网络搜索工具...","done":false}

event: message
data: {"id":"req-001","response_type":"tool_call","content":"","done":false,"data":{"tool_name":"web_search","arguments":{"query":"今天天气"}}}

event: message
data: {"id":"req-001","response_type":"tool_result","content":"搜索结果：今天晴，气温25°C...","done":false}

event: message
data: {"id":"req-001","response_type":"answer","content":"根据查询结果，今天天气晴朗，气温约25°C。","done":false}

event: message
data: {"id":"req-001","response_type":"answer","content":"","done":true}
```

## POST `/knowledge-search` - 直接搜索知识

该接口只执行知识检索，不调用 LLM 总结，响应为 JSON。请求体字段如下：

| 参数 | 类型 | 必填 | 说明 |
|------|------|------|------|
| `query` | string | 是 | 搜索文本 |
| `knowledge_base_id` | string | 否 | 单个知识库 ID，兼容旧调用 |
| `knowledge_base_ids` | string[] | 否 | 本次搜索的多个知识库 ID |
| `knowledge_ids` | string[] | 否 | 限定搜索的知识文件 ID |

```bash
curl --location 'http://localhost:8080/api/v1/knowledge-search' \
  --header 'X-API-Key: sk-xxxxx' \
  --header 'Content-Type: application/json' \
  --data '{
    "query": "彗尾的形状",
    "knowledge_base_ids": ["kb-00000001"],
    "knowledge_ids": []
  }'
```
