# 文档处理多实例 API / E2E / 故障测试

该目录只包含测试工具，不修改服务实现。测试对象是“每个实例具备完整文档处理能力、共享文档队列、单实例并发受控，并在取得精确终止证明后安全接管故障实例文档”的部署。同一稳定实例的新 boot 可自动续领；跨实例硬故障必须先由 Docker/Kubernetes/STONITH 证明旧执行体已终止，网络分区或仅心跳超时会 fail closed，不能冒险双执行。

测试使用 Python 标准库，不依赖 `pytest` 或额外 pip 包。所有结果写入 `outputs/<时间>/events.jsonl` 和 `report.json`；`outputs/` 不应提交。

## 后端验收契约

测试默认把当前页面全部可见文档放进同一个请求：

```http
POST /api/v1/custom/document-queue/status
Content-Type: application/json

{"knowledge_ids":["id1","id2"]}
```

最小响应：

```json
{
  "success": true,
  "data": {
    "waiting_total": 498,
    "active_total": 2,
    "items": {
      "knowledge-id-1": { "state": "active", "position": 0 },
      "knowledge-id-2": { "state": "waiting", "position": 1 }
    }
  }
}
```

约束：

- `state` 为 `waiting | active | none`；兼容解析器也接受 `queued/pending/processing/running`。
- 等待位置从 1 开始，当前正在处理的文档不占等待位置。
- `waiting_total` 是整套系统的全局等待文档数，而不是当前用户的数量。
- `knowledge_ids` 只控制 `items` 返回范围，不能改变全局 `waiting_total`。
- 请求最多采用去重后的前 2000 个 ID。前端和测试器必须一次提交当前可见 ID，不能把大列表拆成多个顺序请求后合并排名。
- `waiting_total` 和所有 waiting 文档的 `position` 由同一条 CTE 快照产生；`active_total`、active 文档详情和 `capacity_total` 是独立观测查询，不承诺整个响应跨字段的事务级同一时刻。
- 若响应 `ahead_count`，必须等于 `position - 1`。
- 同一时刻两个等待文档不能得到同一位置。
- 前端知识卡片从该状态 API 合并位置，不要求污染 Knowledge DTO；waiting 卡片必须显示
  `data-testid="document-queue-badge"`，内容为“排队 position/waiting_total”。

可选的故障观测接口：

```http
GET /api/v1/custom/document-queue/instances
```

建议响应：

```json
{
  "success": true,
  "data": {
    "instances": [
      {
        "instance_id": "worker-a",
        "boot_id": "process-incarnation-uuid",
        "state": "ready",
        "healthy": true,
        "capacity": 2,
        "active_count": 2,
        "active_documents": ["knowledge-id-1", "knowledge-id-2"],
        "last_heartbeat_at": "2026-07-22T12:00:00Z"
      }
    ]
  }
}
```

该接口不是功能正确性的硬依赖，但提供后可以严格验证每实例 `active_count <= capacity`、实例上下线和文档归属分布。`state` 是持久生命周期状态（`ready | draining | stopped`），`healthy` 另外表示心跳是否仍在新鲜窗口内；测试不得把 `state=ready, healthy=false` 的历史行算作可用实例。队列 active item 还会返回 `owner_instance_id`、`owner_boot_id`、`execution_epoch`、`lease_until` 和 `last_progress_at`，用于逐文档证明续领、fencing 和接管。

## 测试矩阵

| 场景 | 主要断言 |
|---|---|
| API smoke | 系统 API 可用；队列响应结构正确；计数非负 |
| 20 文档功能批次 | 全部进入同一文档队列；位置唯一；每实例不超过自身并发；最终全部完成 |
| 500 文档负载 | 无丢失、无失败、无永久等待；记录吞吐、排队/处理 P50/P95 |
| Worker 硬退出 | 公共 API 保持可用；剩余实例继续完成文档；退出实例可恢复 |
| 租约/重投竞态 | Worker 在接管窗口附近恢复；文档最终只完成一次；epoch 若暴露则只能递增 |
| 单点故障 | 任意一个 Worker 下线不影响集群完成；至少需要两个 Worker 和独立 API/LB |
| 解析与向量 | completed 文档有文本 chunk；唯一 marker 可从 hybrid-search 召回 |
| 重试幂等 | terminal 后 chunk ID 唯一且计数稳定，不因迟到旧任务再次增长 |
| 摘要/问题/图谱/Wiki | 按开启能力检查 summary、generated_questions、graph span、Wiki source_refs |
| 图片/表格 | 使用对应 fixture 时检查 image OCR/caption、table summary/column chunk |
| 水平扩展性能 | 可用单实例报告做 baseline，计算 N 实例 speedup 与 scaling efficiency |

## 运行顺序

先运行纯单元测试：

```powershell
python -m unittest custom.tests.document_processing_cluster_e2e.test_cluster_e2e -v
```

再运行协调器并发契约测试。当前 Windows 主机 Go 环境缺少项目 `pg_query` 的 CGO 生成符号，因此使用开发容器内与应用一致的工具链：

```powershell
docker exec WeKnora-app-dev bash -lc `
  'cd /workspace && /usr/local/go/bin/go test -count=1 ./custom/tests/document_processing_cluster_e2e'
```

默认组使用隔离 SQLite 状态机，覆盖全局排名后再做租户过滤、重复首次注册收敛、两个实例并发 Claim 只有一个成功、排空状态不会被后续心跳改回 ready、旧 dispatch epoch 被拒绝，以及同一稳定实例新 boot 对旧租约的原子接管。

“两个首次写入事务真正并行”的行为必须由 PostgreSQL 验证；测试会在现有开发库中创建并自动删除一个随机隔离 schema，不接触业务表：

```powershell
docker exec -e WEKNORA_DOCUMENT_QUEUE_POSTGRES_CONTRACT=1 WeKnora-app-dev bash -lc `
  'cd /workspace && /usr/local/go/bin/go test -run TestPostgresConcurrentFirstRegistrationConvergesOnOneWorkflow -count=10 ./custom/tests/document_processing_cluster_e2e'
```

该用例每轮同时启动 64 个 producer，要求无唯一键异常、全部返回同一 workflow ID，且 `created=true` 与最终数据库行都严格只有一个。

遗留 `default/document_heavy` root task 的转发去重使用隔离 Redis DB 验证；默认 DB 15，若该 DB 已存在 `document` 队列测试会安全跳过，可用环境变量选择另一个空 DB：

```powershell
docker exec -e WEKNORA_DOCUMENT_QUEUE_REDIS_CONTRACT=1 WeKnora-app-dev bash -lc `
  'cd /workspace && /usr/local/go/bin/go test -run TestForwardLegacyRootIsIdempotent -count=3 ./internal/custom/modules/documentqueue'
```

该契约要求重复 legacy delivery 只生成一条 durable workflow 和一条 `QueueDocument` delivery；workflow 已被新队列租约后，迟到旧副本也只 ACK，不会重新执行解析。

然后做 API-first 功能测试。必须使用专用测试知识库；Token 和 KB ID 不写入命令历史时可使用环境变量：

```powershell
$env:WEKNORA_E2E_HOST='http://localhost:8080'
$env:WEKNORA_E2E_TOKEN='<test-api-key>'
$env:WEKNORA_E2E_KB_ID='<disposable-kb-id>'

python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 20 `
  --upload-concurrency 16 `
  --expected-instance-concurrency 4 `
  --timeout 1800
```

`asynq.concurrency` 在这里表示每个应用实例可同时持有的完整文档工作流数，修改管理员设置后必须重启各实例使其统一生效。一个 admission 槽会持续覆盖 DocReader、切块、向量化以及已开启的摘要、问题、图谱和 Wiki，直到该文档工作流终止；问题生成可以使用独立 task lane，但不会提前释放文档槽。`WEKNORA_ASYNQ_TASK_CONCURRENCY` 是实例内部子任务 worker 的并发，扩容实例时也会随副本数放大，不能把 `asynq.concurrency` 理解为所有下游模型请求的全局上限。

运行器严格按照以下顺序执行：

1. 调用系统和队列 API，先验证契约。
2. 并发上传带唯一 marker 的 Markdown 文档。
3. 检查队列位置和知识卡片字段。
4. 持续采集 waiting/active/instance 并发。
5. 等待全部 terminal。
6. 检查 chunk、向量召回和幂等性。
7. 删除本轮上传文档；传 `--keep-data` 可保留。

## 持久队列、重启续跑与竞态专项

这部分有独立入口 `run_durable_failover.py`，不与普通解析功能通过率混算。默认命令不会停止任何容器，只运行状态机、并发和 race detector：

```powershell
python custom/tests/document_processing_cluster_e2e/run_durable_failover.py `
  --go-container WeKnora-app-dev
```

专项边界如下：

| 独立崩溃/竞态边界 | 断言 |
|---|---|
| prepare 后、业务未绑定 | workflow 保持 `preparing`，队列不可见、不可 claim、不可恢复投递 |
| 业务绑定后、activate 前 | 多实例 recovery 竞争时只有一次 CAS，最终进入 `queued` |
| activate 后、Redis 接收前 | Redis 不可用不会丢失 PostgreSQL 接受状态，stable TaskID 不变 |
| 同 stable instance 新 boot | 只接管自身旧 boot 的租约，epoch 递增，旧 boot 被 fence |
| 不同实例接管 | 必须同时满足 owner heartbeat 过期、workflow lease 过期、Redis delivery inactive，以及旧 boot 已优雅停止或由运行时提供精确终止证明 |
| 仅 heartbeat/lease 超时 | 不允许接管；冻结或网络分区的旧进程仍可能复活，超时本身不是死亡证明 |
| 多实例并发 claim | 32 个竞争者严格只有一个获得租约 |
| recovery 阻塞 | 独立 heartbeat 仍前进，不把健康实例误判为失联 |
| 无终止证明的旧租约超过扫描预算 | keyset 游标跨周期继续，后续可恢复文档不会被永久挡住 |
| 过期租约尾部持续增长 | 每轮固定高水位并最终回绕；旧租约后来获得终止证明后会在有限轮内被重访 |
| handler 忽略取消 | 在 grace 后 liveness 失败，返回后恢复，不允许静默双执行 |

要把契约升级到真实 PostgreSQL 行锁和隔离 Redis DB，可显式开启。PostgreSQL 使用随机 schema 并自动删除；Redis 默认 DB 14，若已有 `document` 数据会安全跳过：

```powershell
python custom/tests/document_processing_cluster_e2e/run_durable_failover.py `
  --go-container WeKnora-app-dev `
  --postgres-contract `
  --redis-contract `
  --redis-contract-db 14
```

这条命令额外执行 64 producer 并发 prepare、16 replica 并发 activate、64 contender 并发 claim、16 个真实 PostgreSQL coordinator 同时回收同一过期租约，以及“8 个 recovery 同时补投只产生一个 Redis delivery”和“Redis 仍为 active 时禁止跨实例接管”。它还会让 survivor recovery、旧 boot 终止确认和同 stable instance 新 boot 接管三方同时竞争，验证无死锁、只发生一次 epoch/version 递增，并拒绝旧 boot 与旧 epoch delivery。报告和完整日志单独写入 `durable_failover_outputs/<时间>/`。

真实容器故障必须同时给 `--allow-chaos`，并明确提供稳定实例 ID 到容器名的映射。以下命令分别生成新文档批次，因此一个场景失败不会被下一场景掩盖：

```powershell
python custom/tests/document_processing_cluster_e2e/run_durable_failover.py `
  --skip-contracts --allow-chaos `
  --scenario stable-reboot `
  --scenario cross-instance-takeover `
  --scenario paused-old-owner `
  --token '<test-api-key>' --kb-id '<disposable-kb-id>' `
  --worker worker-cluster-e2e-a=WeKnora-worker-cluster-e2e-a `
  --worker worker-cluster-e2e-b=WeKnora-worker-cluster-e2e-b `
  --documents 8 --generated-size-kib 64 --pause-seconds 90
```

- 所有场景先从容器环境变量/hostname 和 instances API 双向核对 `instance_id=container` 映射；每次 kill 都用 `docker inspect` 确认 stopped，每次 start 都确认 running，随后要求目标实例发布新 `boot_id` 并进入 ready。
- `stable-reboot` 会剔除 kill 边界已经 terminal 的文档，但要求至少一个未完成文档；kill 边界的每个文档都必须在新 boot 上重新成为 active、出现 owner/progress 证据或在新 boot ready 后明确 terminal，epoch 字段缺失不能空通过。
- `cross-instance-takeover` 硬杀当前 owner，确认容器 stopped 后才提交精确 `instance_id + boot_id` 终止证明；kill 边界的每个未完成文档都必须同时出现 owner 变化和 epoch 增长，或有被观察到的 survivor owner 明确完成，不能以任意一个文档成功代替整批成功。
- `paused-old-owner` 冻结旧进程跨过 heartbeat、lease 和 Asynq 恢复窗口；所有未 terminal 文档必须持续存在、owner 仍是旧实例且 epoch 不变化，item 消失或 owner 为空均直接失败。
- `fleet-restart` 会同时停止全部三个解析/API 实例，需提供至少三个 `--worker` 映射并另加 `--allow-full-worker-outage`；场景记录三个实例重启前后的 boot，容忍 API 冷启动 connection-refused，但最终要求三个实例全部 ready。
- `api-restart` 用 `--fault-instance` 指定 API 容器，要求 API boot 变化、冷启动恢复，并证明重启边界前由其他 worker 持有的文档在恢复后仍有进展或已完成，最终仍对整批做无丢失/无重复输出校验。
- `redis-restart` 会停止共享 Redis，需另加 `--allow-infrastructure-chaos --redis-container <name>`；场景确认容器确实 down/up、Redis 返回认证 `PONG` 且 API 恢复。这证明 PostgreSQL outbox 能在 Redis 重启后补投，不代表单副本 Redis 已经具备 HA。

所有 chaos 场景在 `finally` 中尽力恢复被 stop/pause 的容器；仍应只对可丢弃测试集群执行。

终止证明是分布式安全边界，不是额外的人工排队步骤：Docker/Kubernetes 运维控制器应先从运行时确认旧容器已经终止，再调用
`POST /api/v1/custom/document-queue/instances/termination-attestation`。同一个稳定实例内的容器重启无需该调用，新 boot 会自动 fence 旧 boot 并续跑；SIGTERM 优雅退出也会在所有本地 handler 返回后自动发布 `stopped`。节点失联但尚未完成 STONITH/fencing 时禁止提交证明，否则无法同时保证不重复执行和自动转移。

## 500 文档负载与水平扩展对比

先用单实例获得 baseline：

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 500 --generated-size-kib 64 --upload-concurrency 32 `
  --verify-sample 3 --instance-count 1 --timeout 7200
```

记录输出的 `report.json` 后，在五实例部署上执行：

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 500 --generated-size-kib 64 --upload-concurrency 32 `
  --verify-sample 3 --instance-count 5 `
  --baseline-report '<single-instance-report.json>' `
  --min-scaling-efficiency 0.65 `
  --min-throughput 0.20 `
  --max-p95-processing-seconds 900 `
  --timeout 7200
```

不要在 baseline 命令中传 `--keep-data`；默认清理只删除本轮文档，不删除报告，也可避免残留文档影响后续 scaled 轮次。性能门槛与模型、向量库和测试文件强相关，因此默认不硬编码；验收环境必须显式给出阈值。现在使用 API 实测拓扑计算：`scaling_efficiency = 实际 speedup / (scaled healthy-ready 数 / baseline healthy-ready 数)`，建议首版五实例目标不低于 0.65。

报告会写入 `workload_profile` 和其 SHA-256 指纹；使用 `--baseline-report` 时，在上传前强制比较以下字段，任一不同都直接拒绝计算 speedup：

- 文档数、上传并发、生成模板与每文档目标 KiB；使用 fixture 时则比较所选文件名、后缀、字节数和内容 SHA-256。
- `process_config` 指纹、要求完成的衍生能力、`--expect-chunk-text`、验证抽样数、Wiki 超时、轮询周期和卡片契约开关。
- KB ID，以及分块、图片处理、Embedding/Summary/VLM/ASR、存储 provider、向量库、抽取、问题生成、Wiki 和索引策略等 KB 配置的联合指纹。
- 故障注入模式及其时序参数；普通性能对照应保持 chaos 关闭。

`wall_seconds` 和吞吐计时从批量上传开始，包含上传、排队、完整处理与输出核验，但不包含 API/KB 预检、fixture 哈希及读取 baseline 报告，避免 scaled 命令独有的预检开销污染性能对照。

`--instance-count` 现在只是对 instances API 实测结果的断言，不再参与性能分母；默认值 `0` 表示自动发现。baseline/scaled 对照都会要求 instances API 在测量开始和结束时返回相同的一组实例：状态必须可运行且明确为 `healthy=true`，`instance_id`、`boot_id` 和正数 capacity 必须完整，期间不能发生扩缩容或重启。显式传入的 `--instance-count` 必须在两个边界都精确匹配；baseline 与 scaled 的每实例 capacity 也必须相同。旧报告若缺少 `workload_profile` 或起止拓扑证据，必须用当前 harness 重新生成，不能继续作为 baseline。

模型 ID 相同但其网关路由/配额被外部修改、DocReader 镜像或未由 API 暴露的运行参数变化，无法仅靠报告自动识别，正式对照仍需冻结并记录这些外部部署条件。不同规模 workload 只能分别报告原始吞吐，不能把算出的 speedup 当作严格线性扩展证据。`owner_distribution` 来自轮询采样，快速完成的 owner 可能没有被采到，因此只证明实例参与过处理，不等于精确的文档分配总数。

## Worker 异常退出和单点故障

故障测试会真实停止容器，只有指定 `--allow-chaos` 才运行。务必使用可丢弃的测试集群，至少提供两个仅承担 Worker 的容器；不要把唯一 API 容器作为故障目标。

本目录提供一个可选本地拓扑：两个同构全功能应用实例，各自绑定一个 DocReader，连接现有 dev PostgreSQL、Redis、MinIO/本地共享目录和向量库。先确保主应用已完成迁移，再启动测试 Worker：

```powershell
docker compose `
  -f custom/tests/document_processing_cluster_e2e/docker-compose.cluster-e2e.yml `
  --profile cluster-e2e up -d
```

测试 compose 为故障场景使用 2 秒心跳和 75 秒工作流租约；租约必须覆盖 Asynq 约 60 秒的 active-task 恢复窗口，不能为了缩短测试而降到该窗口以下。该配置只适用于本地验收，不是生产参数。它不发布 HTTP 端口，日常 API 仍调用 `http://localhost:8080`；两个应用实例只增加完整处理能力。每个应用实例使用自己的 DocReader，因此杀掉一个 Worker 时不会连带破坏另一个实例的本地解析器。

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 40 --timeout 2400 `
  --generated-size-kib 32 `
  --worker-container WeKnora-worker-cluster-e2e-a `
  --worker-container WeKnora-worker-cluster-e2e-b `
  --fault-target WeKnora-worker-cluster-e2e-a `
  --allow-chaos --hard-kill `
  --down-seconds 30 --takeover-timeout 180
```

这个通用 hard-kill 场景只验证 Worker 退出后幸存实例仍能服务，不是严格的“被杀 owner 跨实例接管”证明：它不保证 fault target 在 kill 时持有文档，也不提交精确终止证明。要证明被杀实例持有的每个文档都转移到其他实例，必须使用前文独立的 `run_durable_failover.py --scenario cross-instance-takeover`。

该场景验证：

- 注入前确实存在 active 文档。
- 停掉一个 Worker 后，公共 API 不出现不可用窗口。
- 其他实例仍有完成进度或实例接口证明有健康 survivor。
- 被停止实例可以重新启动。
- 最终所有文档完成，terminal 后 chunk 不重复。

接管窗口竞态：

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 40 --timeout 2400 `
  --generated-size-kib 32 `
  --worker-container WeKnora-worker-cluster-e2e-a `
  --worker-container WeKnora-worker-cluster-e2e-b `
  --fault-target WeKnora-worker-cluster-e2e-a `
  --allow-chaos --restart-race --race-pause-seconds 25
```

应至少分别在“租约到期前恢复”和“租约到期后恢复”两个时间点运行一次。若状态 API暴露 `execution_epoch`，测试会验证观测到的 epoch 不会回退。

更严格的“旧实例回魂”测试使用 `docker pause`：旧进程内存和旧 handler 都保留，跨过 75 秒工作流租约后再恢复。测试随后检查 terminal 后 chunk ID 唯一且数量稳定。这比 kill/restart 更容易暴露旧执行与新 owner 同 generation 双写的问题：

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 40 --timeout 2400 --generated-size-kib 32 `
  --worker-container WeKnora-worker-cluster-e2e-a `
  --worker-container WeKnora-worker-cluster-e2e-b `
  --fault-target WeKnora-worker-cluster-e2e-a `
  --allow-chaos --pause-race --down-seconds 90
```

停止测试 Worker（不删除现有开发基础设施）：

```powershell
docker compose `
  -f custom/tests/document_processing_cluster_e2e/docker-compose.cluster-e2e.yml `
  --profile cluster-e2e down
```

## 全部衍生能力

`process_config.full.example.json` 会开启问题生成和图谱覆盖；使用前确认测试 KB 已配置可用 Chat 模型、图谱模型/Neo4j，并在 KB 设置中开启 Wiki。摘要沿用 KB 的 summary model。

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 3 `
  --process-config custom/tests/document_processing_cluster_e2e/process_config.full.example.json `
  --expect-derived summary,questions,graph,wiki `
  --verify-sample 3 --wiki-timeout 2400 --timeout 2400
```

图片和表格建议另开小批次，传真实文件一次一份，避免文件哈希去重：

```powershell
python custom/tests/document_processing_cluster_e2e/run_e2e.py `
  --documents 2 `
  --fixture '<image-with-text.png>' `
  --fixture '<table.xlsx>' `
  --expect-derived multimodal,table `
  --expect-chunk-text '<known fixture text>' `
  --verify-sample 2 --timeout 2400
```

若图片和表格分属不同文档，当前命令会对两个 sample 都要求两种能力；正式验收应分别运行图片批次和表格批次，以便断言精确。

只有生成了 `expect_derived=multimodal` / `table` 的成功报告，才能宣称相应真实格式已覆盖；固定 fixture 还应重复传入 `--expect-chunk-text`，校验 OCR/表格关键文本确实进入持久 chunk。Markdown 的完整衍生能力报告不能替代 OCR、图片描述、Excel、PDF、Word 或 PPT 的格式验收。

## 不能由 Worker 测试消除的基础设施单点

多 Worker 只能消除文档处理实例的单点。若 PostgreSQL、Redis、对象存储、向量库、Neo4j、模型网关、Ingress/负载均衡器仍是单副本，它们依然是系统单点；本地唯一的 `localhost:8080` 也只有恢复能力，没有无中断 API HA。DocReader 至少需要两个副本和可负载均衡的 gRPC 服务；多应用副本若使用本地文件存储则必须是 RWX，共享对象存储更适合生产。Kubernetes API/运行时无法提供旧 Pod 精确终止证明时，跨 Pod 接管会安全阻塞。该套件会证明 Worker 下线后的连续处理能力，但不会把“单节点 Redis 重启后最终恢复”误报成“Redis 无单点”。生产验收还需要各基础设施自身的 HA/切主测试。
