# WeKnora 生产停机更新包

> 文档核对日期：2026-09-13。本目录保存生产发布/回滚脚本和模板索引；具体生产值由
> 受保护的现场配置注入，本文不记录节点、内部服务、对象存储、模型端点、路径、租户
> 快照、Git SHA、镜像 digest 或任何凭据。生产访问统一为
> [https://knora.moutai.com.cn](https://knora.moutai.com.cn)。

仅文档变更不需要生产部署。执行发布前必须阅读
[当前版本生产更新部署执行手册](../../docs/custom/当前版本生产更新部署执行手册.md)
和[当前生产实现与部署基线](../../docs/custom/当前生产实现与部署基线.md)，并由有权限的
发布人员完成审批、备份、渲染、校验和回滚准备。

## 当前生产边界

- 生产使用角色化编排，不把单体 `app-dev` 当成生产拓扑；API、parse、derivative、Wiki、
  maintenance、migration、DocReader、Agent Runtime、frontend 和 mobileWeb 分别管理。
- 生产不依赖共享 RWX 文件系统：持久文件进入受保护对象存储，解析/Office/Agent 工作区
  为按任务隔离的临时本地目录。
- 数据库迁移只允许一次性 migration 角色执行；普通业务角色不重复迁移。
- 历史任务、对象和数据库是否回放、迁移或重建，必须以停机截点和受保护台账为准；本文不
  固化任何现场数量或租户数据。
- 公开知识源、普通文件、DocReader、入口请求体和内部产物代理均有独立大小边界，详见
  [生产实现与部署基线](../../docs/custom/当前生产实现与部署基线.md)。

## 目标拓扑

| 角色 | 生产副本/执行方式 |
|---|---:|
| API | 3 |
| parse | 3 |
| derivative | 2 |
| Wiki | 2 |
| maintenance | 2 |
| migration | 1 次性 |
| DocReader | 3 |
| Agent Runtime | 2 |
| frontend | 2 |
| mobileWeb | 2 |

精确资源、亲和/反亲和、探针、并发、镜像和受保护基础设施配置以
`helm/values-production-ha.yaml` 及经审批的现场覆盖文件为准；不要从公开文档推导内部
节点分布或网络地址。

## 文件索引

| 文件 | 作用 |
|---|---|
| `values-site.example.yaml` | 现场镜像、摘要和部署覆盖的示例结构 |
| `values-migration.example.yaml` | 一次性数据库迁移 Job 覆盖 |
| `render-and-validate-release.sh` | 渲染清单并执行 schema、API Server dry-run、拓扑和镜像校验 |
| `build-release-images.sh` | 基础镜像门禁、构建、smoke、推送和 digest/技能导出 |
| `preload-build-dependencies.sh` | 发布前准备基础镜像和构建依赖 |
| `concurrency-plan.json` | 受保护的队列、数据库池、流水线和模型容量基线 |
| `apply-capacity-plan.py` | 通过受控通道预验证并应用容量/调度策略 |
| `prepare-hostpaths.sh` | 准备角色化 scratch 和备份目录 |
| `switch-preloaded-skills.sh` | 原子切换预加载技能目录，失败可回滚 |
| `capture-release-cutoff.sh` | 截取 Kubernetes、数据库、任务和队列基线 |
| `backup-postgres.sh` | 生成 PostgreSQL 备份 |
| `verify-postgres-restore.sh` | 执行恢复演练并校验结果 |
| `restore-postgres-backup.sh` | 回滚时恢复数据库备份 |
| `verify-release.py` | 对平台和知识库重建执行 fail-closed 验收 |
| `sql/*.sql` | 停机截点的只读台账模板/查询 |

脚本所需的 `REPLACE_*`、镜像仓库、凭据、对象存储和服务地址必须从受保护环境提供，
不得写入 Git、日志或公共使用指南。提交前应检查构建上下文和生成物，避免把 `.env`、
密钥文件、现场 values、快照或恢复归档打包。

## 发布硬门槛

1. 生产代码、镜像 digest、受保护 values、技能 staging、渲染清单和数据库迁移方案均已
   审批。
2. 已完成最终停机截点、数据库备份和恢复演练；备份可读且回滚步骤经过演练。
3. API、parse、derivative、Wiki、maintenance、DocReader、Agent Runtime、前端和移动端
   的副本/资源/探针与模板一致。
4. 发布后通过生产域名验证 Web、API、移动端、MCP、分享、嵌入式、IM、文件上传、解析和
   产物下载。
5. 任何门槛未满足时只允许继续准备，不得切断业务入口或执行迁移。

禁止删除 `ingress-nginx`、Ingress Controller、其 Service 或其他集群级入口组件；入口、
数据库、缓存、对象存储和模型依赖的变更必须单独审批。
