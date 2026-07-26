# WeKnora 知识图谱与大规模关联浏览

WeKnora 中有两类容易混淆的图：

| 类型 | 数据来源 | 存储 | 主要用途 |
|---|---|---|---|
| 实体关系图谱 | LLM 从文档 chunk 抽取实体和关系 | Neo4j | GraphRAG、智能体关系查询 |
| Wiki 页面链接图 | Wiki 页面之间的入链/出链 | PostgreSQL | 分类浏览、页面导航和关联阅读 |

两者可以在同一个知识库中同时启用。文档只有在启用的图谱抽取和 Wiki 阶段都达到
成功终态后，完整工作流才显示“已完成”。

## 启用实体关系图谱

开发环境：

```env
NEO4J_ENABLE=true
NEO4J_URI=bolt://neo4j:7687
NEO4J_USERNAME=neo4j
NEO4J_PASSWORD=your_strong_password
# NEO4J_DATABASE=neo4j
```

```bash
docker compose --profile neo4j up -d
```

生产复用现有 Neo4j Service，密码必须从 Kubernetes Secret 注入，不能写进 values
或 Git。当前生产 Neo4j 是单实例；app 多副本不会自动让它高可用。

在知识库设置中：

1. 启用图谱索引。
2. 启用实体/关系抽取并配置允许的实体、关系类型。
3. 选择可用的对话模型。
4. 保存后上传或重建测试文档。

## 完成性验证

前端“已完成”只是第一层证据。图谱验收同时检查：

1. 文档 `parse_status=completed`。
2. `enrichment_status=completed` 且没有图谱批任务仍在 pending/running。
3. Neo4j 中该知识库/文档对应的节点和关系确实存在。
4. 使用图谱查询工具或已知关系问题能返回预期节点、关系和来源。

Neo4j 控制台只用于运维抽查，查询必须带知识库范围和 `LIMIT`，不要在大库执行
无界 `MATCH (n) RETURN n`：

```cypher
MATCH (n)
RETURN labels(n), count(*) AS nodes
ORDER BY nodes DESC
LIMIT 50;

MATCH ()-[r]->()
RETURN type(r), count(*) AS relations
ORDER BY relations DESC
LIMIT 50;
```

实际租户/知识库过滤字段以当前 schema 为准，不要把控制台查询结果跨租户展示给
普通用户。

## 大规模 Wiki 图界面

知识库节点多时，浏览器不再一次性请求并渲染全库图。当前交互为：

1. 节点列表按 Wiki `page_type` 分类。
2. 列表和搜索由服务端分页/渐进加载，页面正文不进入图目录响应。
3. 初次打开只返回连接度靠前的有界 overview。
4. 从列表选择节点后，加载该节点的一跳 ego 图。
5. 点击图中的关联节点，会把该节点设为新中心并请求它的一跳关系，等同于从列表
   重新选择该节点。
6. 邻居过多时使用分页继续加载；画布不会无限追加整个知识库。

这样可以通过“分类/搜索 → 选节点 → 沿关系逐点跳转”访问每个节点，同时把前端
内存、布局计算和网络响应控制在有界范围。

### Wiki 图 API

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/graph
    ?mode=overview
    &types=entity,concept
    &limit=500
```

```http
GET /api/v1/knowledgebase/{kb_id}/wiki/graph
    ?mode=ego
    &center={page_slug}
    &depth=1
    &types=entity,concept
    &limit=500
    &page=1
```

边界：

- `mode`：`overview` 或 `ego`。
- `ego` 必须提供 `center`。
- `depth` 为 1–3，界面默认 1；推荐通过点击节点逐跳探索。
- `limit` 默认 500、最大 2000。
- `page` 是 ego 邻居页码。
- 返回 `meta` 中包含 `total/returned/truncated` 及邻居分页信息。

Wiki 分类目录使用 Wiki pages 的轻量 `projection=graph`、`page/page_size/query`
接口，不下载正文后再在浏览器过滤。

## 关联节点切换

用户点击关联节点时，前端应：

1. 记录新中心 slug。
2. 清除旧中心的瞬时布局状态，但保留返回导航历史。
3. 请求 `mode=ego&center=<new-slug>&depth=1&page=1`。
4. 用新中心、其直接邻居和关系替换主画布。
5. 同步选中左侧列表项；如果该节点尚未出现在已加载列表页，仍允许图正常展示。

“展开邻居”用于继续分页或围绕当前节点扩展，不应退化为一次加载全图。

## 常见问题

### 图谱任务失败

检查模型、Neo4j 连通性、图谱批任务和文档解析时间线。启用图谱但抽取/写入异常
应显示失败或降级，不能显示跳过或完成。

### Neo4j 有节点但前端 Wiki 图为空

确认查看的是哪一类图。Neo4j 实体图不等于 Wiki 页面链接图；Wiki 图需要启用 Wiki
并生成已发布页面与页面链接。

### 图打开很慢

检查请求是否使用 `mode=overview/ego` 和有界 `limit`，以及前端是否错误地循环
加载所有分页。不要通过把 `limit` 提高到最大来修复；先确认分类列表、搜索和中心
节点跳转正常。

### 删除后仍有关系

文档删除是异步完整清理。等待 `deleting` 结束后核对 PostgreSQL、Neo4j 和 Wiki
来源；如果关系仍存在，应按删除失败处理，不要手工把前端状态改成完成。

完整启用步骤见[开启知识图谱功能](./开启知识图谱功能.md)，API 字段见
[Wiki 图 API](./api/knowledge-graph.md)。
