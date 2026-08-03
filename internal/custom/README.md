# 后端二开模块

后端二开统一从 `internal/custom/bootstrap/` 注册。大段业务逻辑保留在本目录，
上游目录只放必要路由、Hook、接口和字段。总体架构见
[当前实现架构与文档索引](../../docs/custom/当前实现架构与文档索引.md)。

## 文档与知识主链路

| 模块 | 作用 |
|---|---|
| `documentqueue` | 文档级持久工作流、实例/boot、租约、epoch fencing、接管和队列位置 |
| `documentsplit` | 超大文档拆分、投递/执行双 epoch、重启恢复、业务失败预算和代表性采样 |
| `dependencycontrol` | PostgreSQL/ParadeDB 共享能力断路、持久健康状态、索引校验和自动修复 |
| `modeladmission` | Redis 集群级模型准入、后台执行窗口、衍生/Wiki 公平调度及 Wiki Map/Commit 自动分配 |
| `capacitycontrol` | 实际模型资源池有效值编译、跨模块冲突检查和独立管理 API |
| `runtimeinstances` | 通用运行角色/能力心跳和在线实例可观测性 |
| `derivativequeue` | 衍生任务 PostgreSQL outbox、派发租约、provider checkpoint 和恢复 |
| `wikiqueue` | Wiki Map PostgreSQL outbox、Commit-aware Map 派发上限、epoch/lease 和缺失 wake 恢复 |
| `chatqueue` | 按实际聊天模型资源池的会话级 FIFO、个人等待上限、跨 API 租约和热配置 |
| `workloadbudget` | 问题、图谱和下游任务工作量上限 |
| `pipelineobs` | 文档阶段进度与运行观测 |
| `processingtrace` | V2 逻辑业务 span 唯一存储、稳定逻辑键与尝试分配 |
| `enrichmentoutcome` / `terminalrepair` | 衍生结果收敛和终态修复 |
| `knowledgeworkflowfilter` | 完整工作流状态筛选 |
| `knowledgefolders` | 文件夹、渐进列表、搜索、移动和导入 |
| `documentpreview` | 大文件预览策略 |
| `mobiledocument` | 移动端短时签名下载与企业微信原生文件响应 |
| `knowledgepurge` / `wikidelete` | 删除时清理关系库、对象、图谱和 Wiki |
| `artifactstore` / `objectnamespace` / `storagemigration` | 私有对象、唯一前缀、校验和迁移 |

## Agent 与企业能力

| 模块组 | 模块 |
|---|---|
| Agent | `generalagent`、`builtinagentdefaults`、`kbmanager`、`chatretrieval` |
| 数据和技能 | `dbanalytics`、`skillhub`、`scheduledchat` |
| 身份与治理 | `iam`、`authsecurity`、`admin`、`configcenter`、`wikiaccess`、`connectiontls` |
| 协作 | `chatshare`、`sessionstate`、`answerfeedback`、`sourcerefs`、`imoutput`、`impreview` |
| 安全/稳定性 | `fileguard`、`imageguard`、`logprivacy`、`taskretry`、`workretry`、`processownership`、`vlmguard` |
| 内容与缓存 | `contentcache`、`knowledgeaux`、`knowledgesearch`、`textencoding` |

## 关键语义

- `asynq.concurrency` 是每 app 的完整文档并发，不是内部任务线程总数。
- 实际模型资源池是模型调用、聊天会话和后台文档 fan-out 的唯一并发真源；后台执行窗口按资源池容量计算，与 API/worker 副本数无关。
- Wiki Map/Commit 的 task/provider 份额从有效资源池容量自动派生并空闲借用，只作为只读有效值展示，不增加管理员配置项。
- Wiki 单任务内部的 Map/Reduce LLM fan-out 自动裁剪到阶段保底；知识库并发值只是期望上限，不需要随资源池容量手工同步。
- derivative/Wiki 容量等待是无损调度延期，不增加解析次数、provider attempt 或业务失败预算。
- 模型工作在准入成功、即将调用 provider 时才启动子 span；排队、限速、断路和重投等待对业务 span 零写入。
- `custom_processing_spans_v2` 是唯一权威表；每个 `(knowledge, attempt, logical_key)` 只保留一行，重试只累加真实执行次数。
- `parse_status=completed` 只等待核心索引和全部可选分支的持久化意图；表格元数据、摘要、问题、图谱和 Wiki 的运行/失败只更新独立富化状态。
- PostgreSQL 是文档工作流事实来源，Redis 投递允许重复。
- 物理 part 的执行次数、业务失败次数和 Redis 投递代次是三个独立计数；进程重启、租约恢复和依赖等待不消耗业务失败预算。
- `none` 是衍生状态初始值，不能直接解释为跳过。
- 多副本不得同时执行生产 DDL；正常 app 使用 `AUTO_MIGRATE=false`。
- `workretry` 统一长耗时模型任务的有限次数策略；真实供应商调用失败计数，准入/断路器拒绝只轮转等待。
- Python Agent 不直接连接数据库/MCP；工具与产物提交回到 Go。
- 生产持久文件进入私有 OBS，本目录实现不得依赖 RWX。

新增模块前先阅读
[二开目录结构规范](../../docs/custom/二开目录结构规范.md)，并为并发、租户隔离、
幂等、删除、重试和多副本行为补充测试。
