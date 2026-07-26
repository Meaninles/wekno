# Wiki 关联图 API

该接口返回 Wiki 页面链接图，不是 Neo4j 实体关系图。它面向大知识库的有界概览、
分类目录和以某节点为中心的渐进式邻接浏览。

## GET `/knowledgebase/{kb_id}/wiki/graph`

### 概览

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/graph
  ?mode=overview
  &types=entity,concept
  &limit=500
```

按连接度返回有界 overview。`limit` 默认 500、最大 2000。

### 中心节点图

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/graph
  ?mode=ego
  &center=page-slug
  &depth=1
  &types=entity,concept
  &limit=500
  &page=1
```

| 参数 | 约束 |
|---|---|
| `mode` | `overview`（默认）或 `ego` |
| `center` | ego 必填，Wiki page slug |
| `depth` | 1–3，默认 1 |
| `types` | 可选 page_type 白名单 |
| `limit` | 返回节点上限，默认 500、最大 2000 |
| `page` | ego 的邻居页码，从 1 开始 |

推荐界面固定 `depth=1`：用户点击任一关联节点后，把它作为新 `center` 重新请求，
而不是把所有跳数不断累积到旧画布。

响应：

```json
{
  "nodes": [
    {"slug": "center", "title": "中心", "page_type": "concept", "link_count": 21}
  ],
  "edges": [
    {"source": "center", "target": "neighbor"}
  ],
  "meta": {
    "mode": "ego",
    "total": 10240,
    "returned": 101,
    "truncated": true,
    "center": "center",
    "depth": 1,
    "page": 1,
    "page_size": 100,
    "total_pages": 3,
    "neighbor_total": 280,
    "neighbor_returned": 100,
    "has_previous": false,
    "has_more": true
  }
}
```

`total` 与 `neighbor_total` 是服务端统计，不表示响应已经带回全部节点。

## 分类目录和搜索

Wiki 页面列表接口支持：

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/pages
  ?projection=graph
  &page_type=entity,concept
  &query=关键词
  &page=1
  &page_size=50
```

`projection=graph` 只返回图目录所需的轻量字段，不返回页面正文。客户端按类型分页，
不要循环抓完所有页。

搜索入口：

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/search?q=关键词&limit=10
```

## 前端契约

- 初次打开加载 overview 和第一页分类列表。
- 从列表选择节点时请求 ego。
- 点击关联节点时替换中心并重新请求 ego，同时同步列表选中态。
- `has_more=true` 时按 page 继续加载邻居。
- 不允许为了“展示完整”自动循环到最后一页或把 `limit` 固定为 2000。
- 已删除、被类型过滤或死链节点不应伪装成“仍未加载”的有效邻居。

图界面说明和实体图区别见[知识图谱与大规模关联浏览](../KnowledgeGraph.md)。
