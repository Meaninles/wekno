# 文档完整工作流 API

当前文档处理按文档工作流排队。接口中的总状态包含主解析、索引和启用的摘要、
问题/图谱、Wiki 等衍生阶段；不能只使用 `parse_status` 判断最终完成。

基础 URL：`/api/v1`。普通接口要求当前 tenant 的 Bearer/API Key；实例管理接口
仅系统管理员可用。

## 查询文档队列

### POST `/custom/document-queue/status`

一次查询当前页面可见文档的全局位置。推荐 POST，避免多个 GET 快照合并后位置
重复，也避免代理请求行过长。

```json
{
  "knowledge_ids": [
    "knowledge-uuid-1",
    "knowledge-uuid-2"
  ]
}
```

单次最多处理 2000 个去重后的 ID。服务先计算全局排名，再只返回当前 tenant 有权
看到的文档，因此 `position` 是真实全局位置，但不会泄露其他租户文档详情。

响应：

```json
{
  "success": true,
  "data": {
    "waiting_total": 487,
    "active_total": 12,
    "capacity_total": 12,
    "items": {
      "knowledge-uuid-1": {
        "position": 31,
        "state": "waiting",
        "stage": "parse"
      },
      "knowledge-uuid-2": {
        "position": 0,
        "state": "active",
        "stage": "wiki",
        "owner_instance_id": "app-1",
        "owner_boot_id": "boot-uuid",
        "execution_epoch": 2,
        "lease_until": "2026-07-26T01:02:03Z",
        "last_progress_at": "2026-07-26T01:01:50Z"
      }
    }
  }
}
```

字段：

| 字段 | 含义 |
|---|---|
| `waiting_total` | 全系统等待文档数 |
| `active_total` | 当前被健康实例接纳的完整文档数 |
| `capacity_total` | 健康 app 的完整文档容量合计 |
| `position` | 等待时从 1 开始的全局位置；active/terminal 通常为 0 |
| `state` | 工作流队列态 |
| `stage` | 当前外层阶段，用于提示，不是独立排队队列 |
| `owner_*` / `execution_epoch` | 当前执行所有权和 fencing 信息 |

### GET `/custom/document-queue/status`

兼容入口：

```http
GET /api/v1/custom/document-queue/status?knowledge_ids=id1,id2
```

大量卡片应使用 POST。

## 完整状态筛选

### GET `/knowledge-bases/{kb_id}/knowledge`

在原列表接口增加：

```http
?workflow_status=failed&page=1&page_size=20
```

允许值：

```text
pending
processing
cancelling
deleting
completed
failed
cancelled
draft
```

投影规则：

- `completed`：主解析完成，所有启用衍生状态也成功/明确不适用。
- `failed`：主解析失败，或主解析完成后任一衍生失败/降级且已无仍在恢复的运行项。
- `processing`：主解析或任一启用衍生正在运行。
- `none` 不是筛选状态。衍生列为 `none` 时要结合功能开关和主解析判断“尚未开始”
  或“不适用”。

详情里的显式 `skipped`，或阶段 span 的 `output.skipped`，表示有原因的 no-op，
例如无 source chunk、空白/装饰性图片或旧 generation 已被 fencing。模型/解析器/
Neo4j/Wiki 异常、超时和部分失败不得写成跳过。

同一参数也可用于知识库文件夹的 `/nodes` 和 `/search` 接口。

## 预览策略

### GET `/knowledge/{id}/preview-policy`

在打开原始对象前查询浏览器准入：

```json
{
  "success": true,
  "data": {
    "mode": "paged_chunks",
    "reason": "file_too_large",
    "file_type": "pdf",
    "file_size": 52428800,
    "chunk_count": 680,
    "max_original_bytes": 25165824
  }
}
```

`mode=original` 时才可请求 `/knowledge/{id}/preview`。`paged_chunks` 时使用分页的
解析内容；强行请求原始预览返回 `413 PREVIEW_REQUIRES_PAGED_CHUNKS`。

主要原始预览上限：PDF 24 MiB、DOCX 4 MiB、PPTX 8 MiB、XLSX/CSV 2 MiB、常见
图片 5 MiB、音频 20 MiB。结构复杂、超过 240 个 chunk、拆分过或解码器无界的
文件即使体积小也可被强制为分页预览。

## 管理员实例 API

### GET `/custom/document-queue/instances`

系统管理员查看每个 app 的稳定实例、boot、心跳、容量、active 文档和健康状态。
扩容验收必须确认实例实际 active，而不是只看 Deployment 副本数。

### POST `/custom/document-queue/instances/termination-attestation`

```json
{
  "instance_id": "stable-app-id",
  "boot_id": "exact-boot-id",
  "proof": "operator/orchestrator proof text"
}
```

这是安全边界，不是普通健康探测。只有确切容器/Pod boot 已终止，或节点已完成
隔离后才能调用。心跳超时、租约超时或 Redis 无活动都不能单独作为 proof。boot
已经变化或证据冲突时返回 409。

## 生命周期接口

- `POST /knowledge/{id}/reparse`：创建新 generation；旧 generation 被 fencing。
- `POST /knowledge/{id}/cancel-parse`：进入取消边界。
- `DELETE /knowledge/{id}`：取消在途任务并清理 chunk、索引、问题、图谱、Wiki
  来源和对象。

客户端在这些操作后应刷新完整状态和队列 POST 快照，不应继续沿用旧卡片位置。

状态机和故障语义见
[文档解析水平扩展与故障恢复](../custom/文档解析水平扩展与故障恢复.md)。
