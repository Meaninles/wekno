# GrepChunks 同步检索投影

本模块只在本地实现和验证；生产尚未更新。

> 本文为本地实现/验收记录；当前代码、路由、生产部署和地址以
> [当前实现架构与文档索引](./当前实现架构与文档索引.md)为准。生产入口统一使用
> `https://knora.moutai.com.cn`；本文中的 `localhost` 仅表示本地验收。测试租户、业务数据、
> 工作树和证据文件不应作为生产信息使用。

## 2026-09-12 本地验收记录

- 已按分角色 compose 停止旧实例、构建共享镜像并 force-recreate 全部角色。
- Compose 新增 migration 成功完成依赖，修复首次启动时 API/worker 抢先读取未提交 schema 的竞态。
- 正式本地库有效切片 431,297 条；投影同数量；集合及所有投影字段双向 EXCEPT 差异为 0。
- 投影连同索引约389MB；版本1，8个同步触发器启用。
- 真实 PostgreSQL 集成测试及 race 通过：包含迁移幂等、完整原查询 payload/计数、600条并列截断、权限范围、批量生命周期、双向并发发布、取消、拒绝不安全写事务隔离及禁用触发器检查。
- 本地18库真实工具回归：合同组合157候选→12引用，管理500候选→30引用；去重、覆盖、MMR后来源引用与源表基线一致。
- 合同样本源表查询约420ms，新路径候选/回源195ms、计数12ms；单次样本非 SLA，也不表示所有查询都提速。
- 后端 `localhost:8080/health` 与前端 `localhost:5177` 均返回200，分角色服务健康。
- 未连接生产、未推送镜像、未执行生产迁移或部署；没有创建 Git 提交。
- 扩展执行整个 `internal/agent/tools` 测试包时，`TestTableAnalysisToolAllowsChartsOnlyForTableAnalysis` 失败，单独复跑同样失败；本轮未改动其 `data_analysis.go` / `data_analysis_test.go`。不将全工具包描述为通过，也未扩大范围修复图表逻辑。grep 专项、race、真实引用回归及 bootstrap 测试通过。

## 查询规则

- `grep_chunks` 在 PostgreSQL 的 `custom_grepsearch_chunks` 查正文/标题，仍使用 `~*` 正则。
- 全知识库 `(knowledge_base_id,tenant_id)`、授权文件 ID、标签 EXISTS 的 OR 范围保持；不按问题类型分流。
- 只收录 enabled、未软删除、非 summary 且文档 published/未删除的切片。
- 全局候选 500，按 `created_at DESC,id DESC` 唯一排序；不降低原 MMR/覆盖/最终输出数量。
- 候选与 source payload 在一条 SQL 中查询，文档计数与该 SQL 共用只读 repeatable-read 事务。
- 回源字段沿用原查询，包括 metadata/source_locator。计数仍来自源 chunks，包含有效 summary，不能用投影数量代替。
- 数据库/正则/计数错误明确报错；不会降级到旧扫描或把失败当作无匹配。SQLite/MySQL 不提供这个 PostgreSQL 投影路径。
- `[GrepProjection]` 日志分开记录连接/事务获取、候选和回源、计数耗时与候选数量；不强制全局或局部 plan_cache_mode。

## 同步与迁移

`internal/custom/modules/grepsearch` 负责查询和迁移；SQL 真源为
`migrations/custom/grepsearch/900001_grepsearch.up.sql`，由 Go embed 加载，避免手工 SQL 与运行时分叉。

一次性 migration 角色在 publication 字段迁移之后执行：

1. 取得迁移事务锁。
2. 在同一事务锁住 knowledges/chunks 的写入。
3. 创建投影、范围/排序与 knowledge_id 索引、8 个 statement-level 触发器。
4. 按知识库/租户/时间顺序回填全部有效切片，ANALYZE 后写入版本标记。
5. 提交；任一失败整批回滚。重复运行检查版本/触发器，不重复回填。

服务角色只做 readiness 检查；未安装或触发器禁用时拒绝启动，不在请求中临时修复。

源 chunks 批量插入/更新/删除、文档标题/发布/删除状态更新，都在原写事务中同步投影。
metadata 等不影响匹配字段的更新不重写投影。TRUNCATE 同步清空投影。
按受影响文档 ID 排序取得事务 advisory lock，覆盖批量和跨文档移动，避免全局串行写锁。
不同语句跨文档反向持锁仍可能像其他数据库写事务一样遇到 deadlock；必须重试整个失败事务，不能仅重放触发器，也不会保留半更新投影。

影响投影的源表写入要求 READ COMMITTED（项目当前写入默认值）；高隔离级别写事务明确报错，防止旧发布快照造成新切片误收录。检索自身的只读 repeatable-read 不受影响。

## 容量与维护

投影复制有效正文和标题，增加存储及写入成本；标题修改可能更新一个文档的很多行。
实验中 12.57 万有效切片约 93MiB（索引随真实文本分布变化），3.16 万有效切片文档改标题约489ms。
这不是生产容量估算或 SLA。真实安装后的表大小、回填和响应时延以本地/目标环境实测为准。

表采用 fillfactor=90，autovacuum vacuum/analyze scale factor=0.05/0.02。
初始回填物理连续不能保证永久聚簇，持续观察死元组、表大小与 shared reads；本模块不自动执行阻塞式 CLUSTER/VACUUM FULL。

## 本地更新与验证

遵循 AGENTS：停止 `weknora-runtime-profile-e2e` 编排；构建 `runtime-api-1` 共享镜像；确认开发 PostgreSQL、Redis、MinIO、Neo4j、基础 DocReader 正常，再以同一 compose `up -d --force-recreate` 拉起全部角色。

- `GREPSEARCH_TEST_POSTGRES=1 go test ./internal/custom/modules/grepsearch -count=1`：真实 PostgreSQL 临时 schema，测试后只删除该 schema。
- `go test -race` 同一模块：迁移、payload/计数、批量生命周期、并发发布、错误与就绪检查。
- `GREPSEARCH_TEST_LIVE=1 go test ./internal/agent/tools -run TestGrepProjectionLiveReferences -count=1`：本地迁移完成后，只读比较真实工具输出引用与源表基线（固定本地测试租户的有限库集合）。
- 既有 grep 排序、覆盖、来源引用测试继续执行。

实验脚本及历史评估在 `custom/tests/grep_plan_lab/`，其中实验触发器仅在独立实验 schema；不得当作正式迁移。完整对话 LLM 回答质量不是 SQL 集合一致测试的替代品。
