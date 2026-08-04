# WeKnora 生产停机更新包

本目录是现有 CCE 生产环境的发布入口。最终逐命令操作以
[当前版本生产更新部署执行手册](../../docs/custom/当前版本生产更新部署执行手册.md)
为准。现网不是可直接由 Helm 接管的活动 release，禁止直接执行
`helm upgrade --install`，也禁止对命名空间使用 `--prune`。

## 已确认的生产边界

- 只使用现有 5 个节点，不新增节点、磁盘、数据库、Redis 或模型实例。
- 采用完全停机切换；Ingress 关闭后才截取最终基线、备份和迁移。
- 只备份 PostgreSQL。5,033 个已引用源对象均在私有 OBS，另有 10 条空路径；
  `/data/files` 没有持久业务文件，因此不备份、不迁移文件。
- 重建过程只新增目标 OBS 对象，所有原知识库及原 `obs://` 路径保持不变，作为
  业务级回退副本。
- 历史 Asynq/衍生任务不逐条回放。失败或未完成文档所属知识库整体重建，旧
  payload、旧 generation 和已删除目标任务一律不重放。

2026-08-04 16:32 在线预检：58 个有效知识库、2,633 个文档、20 个租户；32 个库曾启用
或生成过 Wiki，其中 30 个存在未完成文档，必须新建纯文档目标库。另有 8 个纯
文档库原地全量重解析，15 个库全部完成保留，5 个空库保留。任务台账 16,043 条，
逐条人工补跑数为 0。业务仍在线，正式停机 cutoff 重新计算的数量才是执行依据。

## 目标拓扑

| 角色 | 副本 | 固定分布 | 单副本请求/上限 |
|---|---:|---|---|
| API | 3 | `.1/.2/.7` | `500m/1280Mi`；`1500m/3Gi` |
| parse-worker | 3 | `.1/.2/.7` | `1C/2Gi`；`3C/5Gi` |
| DocReader | 3 | `.1/.2/.7` | `750m/1Gi`；`4C/4Gi` |
| derivative-worker | 2 | `.1/.7` | `500m/1Gi`；`1500m/2Gi` |
| wiki-worker | 2 | `.2/.7` | `500m/1Gi`；`1500m/2Gi` |
| maintenance | 2 | `.1/.2` | `150m/384Mi`；`750m/1Gi` |
| general-agent | 2 | `.1/.2` | `250m/768Mi`；`1500m/2Gi` |
| document-agent | 2 | `.1/.2` | `500m/1280Mi`；`2500m/4Gi` |
| frontend/mobile | 各 2 | `.1/.2` | 见 values |
| PostgreSQL | 1 | `.6` | `3C/8Gi`；`6C/12Gi`，`/dev/shm=2Gi` |

Neo4j 保持在 `.54`，LiteLLM/Ingress 保持现状。目标节点 request 占用为：`.1/.2`
约 `52.2% CPU / 29.5% RAM`，`.7` 约 `45.3% / 23.9%`，`.6` 约
`64.6% / 86.0%`；不需要扩容。

## 文件索引

| 文件 | 作用 |
|---|---|
| `values-site.example.yaml` | 受保护目录中的现场镜像仓库、SHA 和 digest 覆盖 |
| `values-migration.example.yaml` | 唯一数据库迁移 Job 覆盖 |
| `render-and-validate-release.sh` | 渲染三套清单，使用哈希锁定的离线 schema、API Server dry-run 和拓扑/镜像校验 |
| `build-release-images.sh` | 离线基础镜像门禁、受限串行构建、smoke、推送和 digest/技能导出 |
| `preload-build-dependencies.sh` | 国内镜像源探测、基础镜像入 SWR、GitHub 二进制和 DuckDB 扩展预置 |
| `concurrency-plan.json` | 节点、数据库池、流水线和七个生产模型并发的唯一容量基线 |
| `llmgateway-capacity-evidence-20260804.json` | 七个生产模型经生产 llmgateway 的无密钥容量证据 |
| `apply-capacity-plan.py` | 通过本机 port-forward 全量预验证并应用七个模型池和 scheduler 策略 |
| `prepare-hostpaths.sh` | 检查或创建现有节点 scratch/数据库备份目录 |
| `switch-preloaded-skills.sh` | 停机后原子启用三节点 staging 技能目录，失败自动恢复并支持显式回滚 |
| `capture-release-cutoff.sh` | 截取 K8s、数据库、知识库、任务和 Redis 元数据基线 |
| `backup-postgres.sh` | 停机后把自包含 PostgreSQL custom archive 写到 `.7` |
| `verify-postgres-restore.sh` | 在现有 PG 中建立临时库做一次完整恢复演练后删除 |
| `restore-postgres-backup.sh` | 回滚时保留新库并原子换名恢复发布前数据库 |
| `postgres-runtime.strategic-merge.yaml` | 只补 PostgreSQL 资源和 2Gi tmpfs `/dev/shm` |
| `verify-release.py` | 平台和知识库重建的最终 fail-closed 验收 |
| `sql/*.sql` | 同一停机截点的知识库、文档和任务只读台账 |

## 发布硬门槛

正式停机前必须同时具备：批准的 Git SHA；七个镜像的不可变 digest；填写完成且无
`REPLACE_*` 的现场 values；三节点同版本 sandbox 镜像、发布技能 staging 目录及
便携哈希清单；生产机渲染通过的三套清单；最终停机 cutoff；数据库备份和完整恢复
演练 PASS。技能 staging 只能在业务 Pod 全部退出后原子切换。

任何一个门槛不满足，都只允许继续准备，不允许删除 Ingress 或执行迁移。
