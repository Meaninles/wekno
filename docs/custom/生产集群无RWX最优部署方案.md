# 生产集群无 RWX 部署方案

> 已落地基线，核对日期：2026-08-06。本文说明为什么现有五个节点无需新增资源也能
> 承载当前拓扑。精确模型并发和发布边界以
> [当前生产实现与部署基线](./当前生产实现与部署基线.md)为准。

## 1. 结论

生产不使用 RWX，也不新增磁盘、节点、数据库或模型实例。持久文件进入私有 OBS，
需要 POSIX 随机读写的解析/Office 工作区使用 `.1/.2/.7` 现有数据盘上的隔离
hostPath scratch。PostgreSQL、Neo4j 与 Redis 保存各自持久或可恢复状态。

当前不是“单体 app ×3”：同一个 app 镜像已经拆为 API、parse、derivative、wiki 和
maintenance 角色。每个角色按真实 CPU、内存、连接和下游并发单独配置，避免 API
副本数无意放大解析和模型并发。

## 2. 现有节点分配

| 节点 | 主要工作负载 | request 合计 | request 占用 |
|---|---|---:|---:|
| `10.14.201.1` | API、parse、derivative、maintenance、DocReader、两个 Agent、两种 Web | 4130m / 8400Mi | 52.2% CPU / 29.5% 内存 |
| `10.14.201.2` | API、parse、wiki、maintenance、DocReader、两个 Agent、两种 Web | 4130m / 8400Mi | 52.2% / 29.5% |
| `10.14.201.7` | API、parse、derivative、wiki、DocReader | 3580m / 6800Mi | 45.3% / 23.9% |
| `10.14.201.6` | PostgreSQL 与集群基础设施 | 5110m / 11316Mi | 64.6% / 86% |
| `10.14.201.54` | Neo4j 与集群基础设施 | 2610m / 3636Mi | 现有固定预算 |

`.6` 内存 request 已接近 86%，不能再放置新应用 worker。主要弹性和离线任务使用
`.1/.2/.7` 的余量。limit 可以高于节点容量以允许短时突发，但 request 必须保证
调度可落地；压测时监控实际 CPU、RSS、OOM 和节点 eviction。

## 3. 工作负载与资源

| 组件 | 副本与分布 | request/副本 | limit/副本 | 本地并发 |
|---|---|---:|---:|---:|
| API | `.1/.2/.7` | 500m / 1280Mi | 1500m / 3Gi | DB 6 |
| parse-worker | `.1/.2/.7` | 1C / 2Gi | 3C / 5Gi | 文档 4，多模态 4，embedding 12 |
| DocReader | `.1/.2/.7` | 750m / 1Gi | 4C / 4Gi | gRPC worker 4 |
| derivative-worker | `.1/.7` | 500m / 1Gi | 1500m / 2Gi | consumer 18 |
| wiki-worker | `.2/.7` | 500m / 1Gi | 1500m / 2Gi | consumer 6 |
| maintenance | `.1/.2` | 150m / 384Mi | 750m / 1Gi | DB 3，主备 |
| general-agent | `.1/.2` | 250m / 768Mi | 1500m / 2Gi | 运行固定单 Pod |
| document-agent | `.1/.2` | 500m / 1280Mi | 2500m / 4Gi | 运行固定单 Pod |
| frontend | `.1/.2` | 100m / 128Mi | 500m / 512Mi | 无状态 |
| mobile-web | `.1/.2` | 50m / 64Mi | 300m / 512Mi | 无状态 |

API、parse-worker 和 DocReader 在 `.1/.2/.7` 各一个；双副本角色跨主机。调度以
hostname topology spread、anti-affinity 和节点亲和表达，不用不可恢复的 `nodeName`
硬绑定。大工作区角色滚动时使用 `maxSurge=0,maxUnavailable=1`，避免瞬时重复占盘。

## 4. 容量匹配

### 4.1 文档链路

```text
parse-worker:       3 × 4  = 12 个完整文档工作流
DocReader:          3 × 4  = 12 个 gRPC 解析请求
multimodal:         3 × 4  = 12 个本地 consumer
embedding local:    3 × 12 = 36 个本地执行窗口
derivative:         2 × 18 = 36 个 consumer
wiki:               2 × 6  = 12 个 consumer
```

1000 份文档可以持久排队，但核心链路只并行 12 份完整工作流。DocReader、VLM、
Embedding、Rerank 和聊天模型再受实际资源池全集群上限控制；不能通过增加线程绕过。

DocReader 每个请求在独立进程组运行，600 秒硬超时后终止整个进程树。单个异常 PDF、
Java/Office 子进程或已有 gRPC 长连接不能永久占住解析服务。readiness 摘除异常 Pod
后仍要让调用端旧连接失效，验收以健康 endpoint 收到新请求和队列持续推进为准。

### 4.2 模型和队列

主要聊天池为 Qwen 27B `32 running + 36 waiting`、V4 Flash INT8
`16 running + 16 waiting`，合计 48 执行、52 等待、100 接纳。Embedding/Rerank
各 64，VLM 16，Omni 8。衍生/Wiki 权重 3:1、prefetch 2、容量等待 30 秒、派发
租约 120 秒。精确字段见 `deploy/production/concurrency-plan.json`。

### 4.3 PostgreSQL

```text
API             3 × 6 = 18
parse           3 × 5 = 15
derivative      2 × 4 =  8
wiki            2 × 4 =  8
maintenance     2 × 3 =  6
steady                  55
migration overlap        3
maximum                 58 / 100
```

普通 Deployment 必须 `AUTO_MIGRATE=false`。唯一迁移 Job 最多使用 3 个连接，完成并
归档证据后清理。

## 5. 无 RWX 存储设计

| 数据 | 位置 | 语义 |
|---|---|---|
| 原始知识文件 | 私有 OBS | 持久、停机迁移时不变化 |
| 解析图片与衍生对象 | 私有 OBS | 持久、租户隔离 |
| Agent 最终产物 | 私有 OBS | 持久、鉴权下载 |
| 业务/工作流/chunk/向量 | PostgreSQL/ParadeDB | 持久事实来源 |
| 实体关系 | Neo4j | 持久图谱 |
| 投递/流/准入 | Redis | 可恢复运行状态 |
| 解析/Office 工作目录 | hostPath scratch | 可丢弃、按角色和 Pod 隔离 |

当前 scratch 根目录：

```text
/mnt/weknora-data/weknora-v2-scratch/api
/mnt/weknora-data/weknora-v2-scratch/parse
/mnt/weknora-data/weknora-v2-scratch/docreader
/mnt/weknora-data/weknora-v2-scratch/derivative
/mnt/weknora-data/weknora-v2-scratch/wiki
/mnt/weknora-data/weknora-v2-scratch/general-agent
/mnt/weknora-data/weknora-v2-scratch/document-agent
```

具体 Pod 路径继续包含 namespace、角色或 Pod UID，禁止多个运行实例写同一工作目录。
目录只用于可重建中间文件：PDF 渲染、Office 转换、解压和 SDK workspace。完成产物
必须先提交 OBS，随后任一 Pod 才能读取。

对象存储不能挂载成 POSIX 工作目录。S3/OBS 对随机小文件、rename、文件锁和目录遍历
语义不同，用它替代本地 scratch 会造成性能与正确性问题。

## 6. 对象与回滚

私有对象按用途使用互不重叠的部署/namespace UUID 前缀：

```text
weknora/__weknora_private_knowledge_objects_v1__/deployment/<deployment>/namespace/<uuid>/
weknora/__weknora_private_agent_artifacts_v1__/deployment/<deployment>/namespace/<uuid>/
weknora/__weknora_claude_sdk_original_inputs_v1__/deployment/<deployment>/namespace/<uuid>/
```

对象默认私有，下载必须先由 API 校验 tenant/session/artifact 权限。测试和容灾环境
不得复用生产 UUID 前缀。

停机迁移只备份 PostgreSQL：在线低优先级全量备份，停机后增量备份，两者必须验证能
组合恢复到原库。文件在停机窗口不变化，不重复备份、不移动；回滚继续引用原对象。

## 7. 入口与静态资源

- 知识源原文件上限 2048 MiB；Nginx/Ingress 入口上限 2304 MiB，关闭 request
  buffering，读写超时 7200 秒。
- 普通附件和 DocReader 单次 gRPC 传输保持 50 MiB；大知识文件先拆分。
- Agent 内部 JSON/Base64 代理上限至少 128 MiB，并受膨胀比和任务预算限制。
- TDesign 图标、PDF worker/WASM/CMap/font/ICC 全部本站托管，不依赖公共 CDN。
- 生产镜像与构建依赖提前通过现有内部/国内镜像源缓存，不在发布窗口临时下载。

## 8. 发布与清理

当前生产不是可直接接管的活动 Helm release。`helm/values-production-ha.yaml` 用于机器
渲染基线，不授权直接 `helm upgrade --install`。必须离线渲染、校验、固定镜像 digest，
再审查后受控 `kubectl apply`；禁止 namespace `--prune`。

不得删除 `ingress-nginx`、Ingress Controller、其 Service 或集群级入口组件。停机只
切断业务 Ingress 路由。一次性迁移、预拉取、验证 Pod/Job 在证据归档后清理；失败项
先保留现场，不能为了“看起来干净”提前删除。

## 9. 验收

- 长期 Pod Ready、零异常重启、endpoint 与计划一致；
- `.1/.2/.7` scratch 独立、可写、可清理，不出现跨 Pod 工作目录复用；
- 三个 parse-worker 和三个 DocReader 都收到真实任务；异常文档超时后新任务继续；
- 48 执行、52 等待、100 接纳及七个模型资源池有效值无冲突；
- PostgreSQL 连接峰值、节点 RSS/CPU、队列 P95 与磁盘空间有余量；
- 小/大/复杂文档、PDF/Office/图片/音频、知识检索与真实引用正常；
- 企业微信电脑/移动端引用、原文和 PDF 本地静态资源正常；
- 原 OBS 对象和回滚数据库备份点保持可用；
- 一次性 Pod/Job 清理完成，Ingress Controller 未改变。

## 10. 当前单点

PostgreSQL、Neo4j 仍是单实例，Redis 主从缺少自动切换，外部 llmgateway 是独立共享
依赖，集群也仍为单 AZ。应用多副本和无 RWX 设计解决的是应用/解析 Pod 故障、水平
执行与文件共享边界，不能据此宣称整个平台无单点。
