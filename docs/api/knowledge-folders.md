# 知识库文件夹 API

文件夹是知识库内的管理层级，不改变文档检索内容、向量或租户归属。列表、搜索和
统计均为服务端分页/渐进加载，客户端不能先拉全库再过滤。

基础前缀：

```text
/api/v1/custom/knowledge-folders/knowledge-bases/{kb_id}
```

读取要求 viewer + 知识库读权限；写操作要求知识库拥有者/系统管理员和写权限。

## 渐进列表

### GET `/{prefix}/nodes`

参数：

| 参数 | 说明 |
|---|---|
| `folder_id` | 空表示根目录 |
| `page` / `page_size` | 正整数分页 |
| `keyword` | 文档关键字 |
| `tag_ids` | 逗号分隔 |
| `file_type` | 文件类型 |
| `parse_status` | 主解析兼容筛选 |
| `workflow_status` | 完整状态筛选 |
| `source` | 来源 |
| `start_time` / `end_time` | RFC3339、`YYYY-MM-DD HH:mm:ss` 或日期 |

响应同时返回 folder/document 混合节点、总数、当前文件夹和 breadcrumbs：

```json
{
  "success": true,
  "data": [
    {"node_type": "folder", "folder": {"id": "...", "name": "制度"}},
    {"node_type": "document", "document": {"id": "...", "name": "示例.pdf"}}
  ],
  "total": 201,
  "page": 1,
  "page_size": 20,
  "current": null,
  "breadcrumbs": []
}
```

文件夹统计为异步维护的紧凑计数，包括子树文档、等待/运行解析、衍生/Wiki 待办
和异常文档；列表请求不会扫描整棵子树。

## 搜索

### GET `/{prefix}/search`

在当前知识库递归搜索文件夹和文档。支持 `folder_id`、`keyword`、分页及同一组
文档筛选。

### GET `/api/v1/custom/knowledge-folders/search`

在当前用户可访问的文档知识库中搜索：

```http
?keyword=财务制度
&knowledge_base_ids=kb1,kb2
&page=1
&page_size=20
```

结果包含 `knowledge_base_id/name`，仍不会返回无权限知识库。

## 文件夹操作

### POST `/{prefix}/folders`

```json
{
  "parent_id": "",
  "name": "公司制度",
  "description": "制度分类",
  "sort_order": 0
}
```

### PATCH/PUT `/{prefix}/folders/{folder_id}`

字段均可选：

```json
{
  "name": "人力制度",
  "parent_id": "new-parent-id",
  "sort_order": 10
}
```

禁止移动到自身/子孙、超过深度限制或在同一父级创建规范化同名目录。

### DELETE `/{prefix}/folders/{folder_id}?mode=...`

- `reject`（默认）：非空则返回 409。
- `move_to_parent`：把直接内容移到上级后删除文件夹，不删除文档。

### GET `/{prefix}/folders/{folder_id}`

返回文件夹、路径、深度和统计。

### GET `/{prefix}/folders/options`

返回轻量文件夹选项树，供移动/上传选择器使用；不包含文档正文。

## 移动文档

### PUT `/{prefix}/documents/locations`

```json
{
  "knowledge_ids": ["id-1", "id-2"],
  "target_folder_id": "folder-id"
}
```

移动只改变管理位置，不重建文档、不复制向量。文档必须属于当前知识库。

## 定向导入

### POST `/{prefix}/files`

`multipart/form-data`：

| 字段 | 说明 |
|---|---|
| `file` | 必填，知识源上限由后端控制，生产 2048 MiB |
| `folder_id` | 目标文件夹 |
| `relative_path` | 文件夹上传相对路径，服务端安全创建中间目录 |
| `fileName` | 自定义文件名，不能含路径分隔符 |
| `metadata` | JSON |
| `enable_multimodel` | boolean |
| `process_config` | 本文档解析覆盖 JSON |
| `tag_ids` / `channel` | 标签与来源 |

`relative_path` 必须包含目录和文件名，禁止 `..`、盘符和空路径段。

### POST `/{prefix}/urls`

请求包含 `url`、`folder_id`、文件名/类型、标签和可选 `process_config`。URL 仍受
SSRF 校验。

### POST `/{prefix}/manual`

在原手工 Markdown payload 上增加 `folder_id`。

上传成功只表示文档已持久接收。客户端随后使用
[文档完整工作流 API](./document-workflow.md)显示队列和所有衍生阶段。
