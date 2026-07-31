# 后端二开模块

后端二开统一从 `internal/custom/bootstrap/` 注册。大段业务逻辑保留在本目录，
上游目录只放必要路由、Hook、接口和字段。总体架构见
[当前实现架构与文档索引](../../docs/custom/当前实现架构与文档索引.md)。

## 文档与知识主链路

| 模块 | 作用 |
|---|---|
| `documentqueue` | 文档级持久工作流、实例/boot、租约、epoch fencing、接管和队列位置 |
| `documentsplit` | 超大文档拆分、任务租约、重试、代表性采样 |
| `modeladmission` | Redis 集群级 Chat/Embedding/Rerank/VLM/ASR/Parser 准入 |
| `chatqueue` | 按实际聊天模型资源池的会话级 FIFO、个人等待上限、跨 API 租约和热配置 |
| `workloadbudget` | 问题、图谱和下游任务工作量上限 |
| `pipelineobs` | 文档阶段进度与运行观测 |
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
| 身份与治理 | `iam`、`authsecurity`、`admin`、`configcenter`、`connectiontls` |
| 协作 | `chatshare`、`sessionstate`、`answerfeedback`、`sourcerefs` |
| 安全/稳定性 | `fileguard`、`imageguard`、`logprivacy`、`taskretry`、`workretry`、`processownership`、`vlmguard` |
| 内容与缓存 | `contentcache`、`knowledgeaux`、`knowledgesearch`、`textencoding` |

## 关键语义

- `asynq.concurrency` 是每 app 的完整文档并发，不是内部任务线程总数。
- 聊天会话并发按实际模型资源池计算；API 副本数不会乘大上限，单用户等待上限跨模型池合计。
- PostgreSQL 是文档工作流事实来源，Redis 投递允许重复。
- `none` 是衍生状态初始值，不能直接解释为跳过。
- 多副本不得同时执行生产 DDL；正常 app 使用 `AUTO_MIGRATE=false`。
- `workretry` 统一长耗时模型任务的有限次数策略；真实供应商调用失败计数，准入/断路器拒绝只轮转等待。
- Python Agent 不直接连接数据库/MCP；工具与产物提交回到 Go。
- 生产持久文件进入私有 OBS，本目录实现不得依赖 RWX。

新增模块前先阅读
[二开目录结构规范](../../docs/custom/二开目录结构规范.md)，并为并发、租户隔离、
幂等、删除、重试和多副本行为补充测试。
