# 对话分享 API（二开）

[返回目录](./README.md)

对话分享 API 用于把 Web 对话会话导出为只读分享链接，并在登录态下查看分享快照。接口前缀为 `/api/v1/custom/chat-share`。

该能力面向 WeKnora Web 登录用户，不面向匿名用户、API Key Principal 或嵌入合成用户。请求需要携带 Web 登录后的 Bearer Token：

```http
Authorization: Bearer <jwt>
```

分享链接本身不授予继续对话、访问原会话或跳转来源引用的能力。查看者只需具备登录态，不需要拥有原会话或原租户权限。

| 方法 | 路径 | 描述 |
| --- | --- | --- |
| POST | `/custom/chat-share/sessions/:session_id` | 为当前可访问的会话创建分享链接 |
| GET | `/custom/chat-share/:token` | 读取分享消息快照 |
| GET | `/custom/chat-share/:token/files?file_path=...` | 通过分享 token 读取快照中的图片或附件 |
| GET | `/custom/chat-share/:token/artifacts/:artifact_id/download` | 通过分享 token 下载分享会话中的通用智能体产物 |

## POST `/custom/chat-share/sessions/:session_id` - 创建分享链接

创建分享链接。服务端会校验当前登录用户对原会话的访问权限，然后把当前消息写入分享快照。

**请求**:

```curl
curl --location --request POST 'http://localhost:8080/api/v1/custom/chat-share/sessions/411d6b70-9a85-4d03-bb74-aab0fd8bd12f' \
--header 'Authorization: Bearer <jwt>' \
--header 'Content-Type: application/json' \
--data '{}'
```

**路径参数**:

| 字段 | 类型 | 必填 | 描述 |
| --- | --- | --- | --- |
| `session_id` | string | 是 | 要分享的会话 ID |

**响应**:

```json
{
  "success": true,
  "data": {
    "id": "f7b37cb4-1877-48da-9c90-bde11f73e43f",
    "session_id": "411d6b70-9a85-4d03-bb74-aab0fd8bd12f",
    "token": "MrYLBGnBbvIxnAadFKbCLlzpLxIGSTRW7g05R9KCThU",
    "url": "/share/chat/MrYLBGnBbvIxnAadFKbCLlzpLxIGSTRW7g05R9KCThU",
    "title": "我的对话",
    "created_at": "2026-07-09T10:20:30+08:00"
  }
}
```

明文 `token` 只在创建响应中返回，数据库中只保存 token 的 SHA-256 hash。`url` 字段在配置 `FRONTEND_BASE_URL` 时返回公网绝对地址，未配置时返回 `/share/chat/:token` 相对路径；前端通常使用该字段复制分享链接。

快照会保留消息内容、图片、附件、提及项和结构化图表渲染数据，但会清空知识库引用，避免分享页出现引用跳转。通用智能体产物不会把 agent steps 暴露给查看者，查看分享时会按源会话和源消息补充只读产物下载信息。

## GET `/custom/chat-share/:token` - 读取分享快照

读取分享内容。查看者必须已登录，但不要求拥有原会话权限。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/custom/chat-share/MrYLBGnBbvIxnAadFKbCLlzpLxIGSTRW7g05R9KCThU' \
--header 'Authorization: Bearer <jwt>'
```

**路径参数**:

| 字段 | 类型 | 必填 | 描述 |
| --- | --- | --- | --- |
| `token` | string | 是 | 分享链接中的 token |

**响应**:

```json
{
  "success": true,
  "data": {
    "id": "f7b37cb4-1877-48da-9c90-bde11f73e43f",
    "session_id": "411d6b70-9a85-4d03-bb74-aab0fd8bd12f",
    "title": "我的对话",
    "created_at": "2026-07-09T10:20:30+08:00",
    "messages": [
      {
        "id": "7d6d1b46-c4a6-48c5-9bda-594bd9e9b16e",
        "share_id": "f7b37cb4-1877-48da-9c90-bde11f73e43f",
        "seq": 1,
        "original_message_id": "ebbf7e53-dfe6-44d5-882f-36a4104910b5",
        "session_id": "411d6b70-9a85-4d03-bb74-aab0fd8bd12f",
        "content": "你好",
        "role": "user",
        "knowledge_references": [],
        "mentioned_items": [],
        "images": [],
        "attachments": [],
        "tool_results": [],
        "is_completed": true,
        "created_at": "2026-07-09T10:19:00+08:00",
        "updated_at": "2026-07-09T10:19:00+08:00"
      }
    ]
  }
}
```

读取成功后，服务端会增加分享链接的访问次数并记录最近访问时间。响应中的 assistant 消息可能包含 `tool_results` 数组，用于渲染 `structured_analysis_result` ECharts 图表；它只包含图表所需的 rows、columns、chart 等安全字段，不包含 agent steps。为兼容旧快照，服务端可能按原消息补齐缺失的安全图表数据，但不会同步原会话新增或删除的消息。assistant 消息也可能包含 `artifacts` 数组；每个产物只包含文件名、类型、大小、SHA256 和分享下载地址。

## GET `/custom/chat-share/:token/files` - 读取分享文件

读取分享快照中的图片或附件。该接口用于分享页渲染受保护文件，通常由前端自动调用。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/custom/chat-share/MrYLBGnBbvIxnAadFKbCLlzpLxIGSTRW7g05R9KCThU/files?file_path=local%3A%2F%2F1%2Fchat%2Fimage.png' \
--header 'Authorization: Bearer <jwt>' \
--output image.png
```

**路径参数**:

| 字段 | 类型 | 必填 | 描述 |
| --- | --- | --- | --- |
| `token` | string | 是 | 分享链接中的 token |

**查询参数**:

| 字段 | 类型 | 必填 | 描述 |
| --- | --- | --- | --- |
| `file_path` | string | 是 | 快照消息中的存储路径 |

**响应**:

成功时直接返回文件流，并设置 `Content-Type`。图片、PDF、CSV 等常见格式会尽量返回对应 MIME 类型，其他格式返回 `application/octet-stream`。

服务端会校验 `file_path` 中的租户段必须等于分享链接的源租户，并拒绝包含 `..` 的路径。

## GET `/custom/chat-share/:token/artifacts/:artifact_id/download` - 下载分享产物

下载分享会话中的通用智能体产物。该接口用于分享页的产物下载按钮，不要求查看者拥有原会话或原租户权限。

**请求**:

```curl
curl --location 'http://localhost:8080/api/v1/custom/chat-share/MrYLBGnBbvIxnAadFKbCLlzpLxIGSTRW7g05R9KCThU/artifacts/2e4b6f20-4f2b-4026-bd64-4a5b6f9a8b71/download' \
--header 'Authorization: Bearer <jwt>' \
--output artifact.xlsx
```

**路径参数**:

| 字段 | 类型 | 必填 | 描述 |
| --- | --- | --- | --- |
| `token` | string | 是 | 分享链接中的 token |
| `artifact_id` | string | 是 | 分享快照响应里返回的产物 ID |

**响应**:

成功时直接返回文件流，并设置 `Content-Disposition: attachment`、`Content-Type` 和 `Content-Length`。服务端会校验产物必须属于该分享链接记录的源租户和源会话。

## 错误码

| 状态码 | 场景 |
| --- | --- |
| 400 | 缺少 token、session_id、file_path、artifact_id，或文件路径非法 |
| 401 | 未登录、登录态不是 Web 用户，或使用了匿名/合成 Principal |
| 404 | 会话不存在、分享链接不存在、分享链接已失效、文件不存在或产物不存在 |
| 500 | 分享服务不可用、存储服务异常或其他内部错误 |

## 前端链接

前端分享页路径为：

```text
/share/chat/:token
```

该路径由同一个分享页路由承载，并按视口渲染桌面或移动布局。未登录访问时会跳转 `/login?returnTo=/share/chat/:token`，登录或 SSO 完成后回到原分享链接。
