# 当前生产部署文件入口

本目录服务于现有生产 CCE 的当前版本更新。生产侧即使没有开发过程的聊天上下文，
也应能只依赖仓库、现网权限和下面的执行顺序完成部署。

> 最终逐命令操作以
> [当前版本生产更新部署执行手册](../../docs/custom/当前版本生产更新部署执行手册.md)
> 为准。不要直接对 `helm/values-production-ha.yaml` 执行
> `helm upgrade --install`，现网当前不是可直接接管的活动 Helm release。

## 目标结果

| 组件 | 副本与分布 |
|---|---|
| app | 3，`10.14.201.1/.2/.7` 各 1；每实例完整文档并发 4 |
| DocReader | 3，`.1/.2/.7` 各 1；每实例 worker 4、PDF render 4 |
| general-agent | 2，`.1/.2` 各 1 |
| document-processing-agent | 2，`.1/.2` 各 1 |
| frontend | 2，`.1/.2` 各 1 |
| mobile-web | 2，`.1/.2` 各 1 |

PostgreSQL、Neo4j、Redis、LiteLLM、Ingress 和 OBS 复用现网，不在本次更新中另起
一套。生产不使用 RWX；持久文件进入私有 OBS，解析和 Agent 工作区使用每 Pod
独立的 `csi-local-topology` RWO 临时卷。

## 文件说明

| 文件 | 用途 |
|---|---|
| [`helm/values-production-ha.yaml`](../../helm/values-production-ha.yaml) | 已核对的生产拓扑、容量、OBS、并发和入口基线 |
| [`values-site.example.yaml`](./values-site.example.yaml) | 复制到受保护目录后填写 SWR namespace 和不可变镜像 tag |
| [`values-migration.example.yaml`](./values-migration.example.yaml) | 维护窗口内只启动一个迁移 app |
| [`storage-migration-job.yaml.tmpl`](./storage-migration-job.yaml.tmpl) | 迁移仍被数据库引用的历史本地知识对象 |
| [`capture-model-runtime-secret.sh`](./capture-model-runtime-secret.sh) | 从现网 app 捕获模型运行配置到 Secret，不打印 Key |
| [`tests/capture-model-runtime-secret_test.sh`](./tests/capture-model-runtime-secret_test.sh) | 捕获脚本的离线测试 |

## 上线顺序

1. 固定待发布 Git SHA，保证工作树无未纳入镜像的生产代码。
2. 备份现网 Kubernetes 清单/Secret 元数据、PostgreSQL、Neo4j 和 `/data/files`。
3. 构建同一 SHA 的 app、DocReader、两个 Agent、frontend、mobile-web 和 sandbox，
   推送批准的 SWR；记录 digest。
4. 给 `.1/.2/.7` 打标签，将 500G/现有数据盘加入 Everest 本地卷池，并实际测试
   `csi-local-topology` 创建、读写、删除和回收。
5. 停止新流量并排空在途文档/Agent；保留 PostgreSQL 和对象存储回滚点。
6. 为三个 app 节点预置与 app 镜像同版本的 sandbox 技能和 sandbox 镜像。
7. 只启动一个 `AUTO_MIGRATE=true` 的迁移 app，执行数据库迁移和旧 Agent 产物
   到 OBS 的幂等迁移；校验对象大小和 SHA256。
8. 运行历史知识对象迁移 Job，只有数据库已无有效 `/data/files` 引用且历史下载
   全部通过，才能释放 `.7` 的旧卷。
9. 渲染并检查最终清单，受控替换旧无状态 Deployment/Service；禁止 `--prune`。
10. 设置管理员“单实例解析任务并发”为 4，确认三个 app 读取一致。
11. 完成 API、浏览器、双副本分流、完整文档衍生任务、Agent 产物、故障注入和
    真实召回验收后再开放流量。

## 三个必须补的现场值

```bash
cp deploy/production/values-site.example.yaml /secure/values-site.yaml
```

生产侧只在受保护文件中替换：

- `REPLACE_SWR_NAMESPACE`
- `REPLACE_GIT_SHA`（建议使用不可变 SHA tag，并记录 digest）
- `REPLACE_LEGACY_NODE_NAME`（仅迁移覆盖文件，填写 `.7` 的真实 node name）

真实 Secret 值不写入 Git。`values-site.yaml` 不得提交。

## 迁移成功判据

迁移不是“Job 返回 0”就算成功，必须同时满足：

- 数据库迁移只由一个 app 执行，没有并发 DDL 或重复初始化。
- 旧 Agent 产物数据库行指向新的私有 OBS URI；对象 HEAD 大小和下载 SHA256
  与源文件一致。
- 所有仍被数据库引用的 `/data/files` 知识对象都已迁入 OBS 并回填。
- 历史原文、解析图片和 Agent 产物从任意 app 副本均可鉴权下载。
- 数据库不再存在有效本地持久路径引用；`/data/files` 只剩兼容临时目录。
- 在旧盘保留期内完成业务验收，之后才允许删除旧 PVC/PV。

## 入口与超时

- 后端知识源原文件：2048 MiB。
- frontend/mobile Nginx 知识源请求：2304 MiB。
- Ingress：`proxy-body-size=2304m`、request buffering 关闭、读写超时 7200 秒。
- 普通附件代理：80 MiB。
- Agent 内部 JSON/Base64 代理：至少 128 MiB。
- 云 ELB/WAF 同时核对 body 和 idle/upstream timeout；任何一层仍为默认 1 MiB
  都会在请求到达 app/Agent 前返回 413。

## 回滚原则

- 未迁移数据前可回滚旧清单；执行数据库/对象迁移后，必须使用同一个迁移回滚点，
  不能只回滚 Deployment 镜像。
- 旧 `/data/files` 盘保持只读备份，直到历史下载和新版本验收全部通过。
- 新版已经写入 OBS 后，回滚版本也必须能访问这些对象；否则先恢复数据库备份和
  旧盘，再恢复旧流量。
- 不删除现有 PostgreSQL、Neo4j、Redis、LiteLLM Service/Endpoints 或 PVC。
- 故障期间不要重新解析或删除保留的“公司制度”验收知识库。

详细回滚命令、检查 SQL 和部署清单命令全部在生产执行手册中。
