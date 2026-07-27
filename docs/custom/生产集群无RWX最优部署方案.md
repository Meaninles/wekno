# 生产集群无 RWX 最优部署方案

> 当前版本生产更新请直接使用[当前版本生产更新部署执行手册](./当前版本生产更新部署执行手册.md)。本文保留架构设计与容量依据；全系统 HA 等配套增强不作为本次上线前置。

本文给出当前 WeKnora 二开代码在茅台生产 CCE 集群上的目标部署形态。它不是“第一阶段/第二阶段”路线图，而是一套完整的最终拓扑。对应 Helm 覆盖文件为 [`helm/values-production-ha.yaml`](../../helm/values-production-ha.yaml)。

文档解析的状态机、重启续跑、跨实例接管及 effective-once 边界见[文档解析水平扩展与故障恢复](./文档解析水平扩展与故障恢复.md)。本文只补充生产环境的节点、容量、存储、入口和发布约束。

## 结论

- 现有 5 个节点足以部署目标拓扑，不需要因为本次解析扩容再增加节点。
- app、DocReader 各 3 副本，完整文档并发为每 app 4、集群 12。
- general-agent、document-processing-agent、frontend、mobile-web 各 2 副本。
- 不部署 RWX。原文、解析持久产物、Agent 最终产物和用户上传的专业技能包全部进入私有 OBS；Pod 只保留可丢弃的本地解析工作区。
- `.1/.2/.7` 的数据盘必须先加入 Everest 本地卷池，Pod 通过 `csi-local-topology` 获得独立 RWO 临时 PVC。不能把 80–100 GiB 的工作区放到仅约 9–10 GiB 的 kubelet 根盘 `emptyDir`。
- PostgreSQL、Neo4j、外置 Redis 和 LiteLLM 保留现有部署。它们仍分别存在单点或容量边界；解析层水平扩展不会自动消除这些单点。

## 目标拓扑

| 节点 | 目标工作负载 | 说明 |
|---|---|---|
| `10.14.201.1`，8C/32G | app、DocReader、general-agent、document-processing-agent、frontend、mobile-web 各 1 | 新解析节点，本地数据盘承担 Pod 临时卷 |
| `10.14.201.2`，8C/32G | 与 `.1` 相同 | 与 `.1` 互为 Agent、前端和移动端故障副本 |
| `10.14.201.7`，8C/32G | app、DocReader 各 1 | 迁出 `/data/files` 后释放原本的本地持久卷用途 |
| `10.14.201.6`，8C/16G | PostgreSQL、LiteLLM、Ingress controller 等现有服务 | 不再承载 general-agent |
| `10.14.201.54`，8C/16G | Neo4j、Ingress controller 等现有服务 | 不再作为 document-processing-agent 的唯一节点 |

对 `.1/.2/.7` 使用 hostname 拓扑打散，而不是把 Deployment 固定到某一个主机名：

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,IP:.status.addresses[0].address

kubectl label node <node-for-10.14.201.1> \
  weknora.io/document-worker=true \
  weknora.io/agent-worker=true \
  weknora.io/stateless-worker=true --overwrite

kubectl label node <node-for-10.14.201.2> \
  weknora.io/document-worker=true \
  weknora.io/agent-worker=true \
  weknora.io/stateless-worker=true --overwrite

kubectl label node <node-for-10.14.201.7> \
  weknora.io/document-worker=true --overwrite
```

所有多副本 Deployment 使用 `topology.kubernetes.io/hostname` 等价的 `kubernetes.io/hostname` spread constraint 和 `DoNotSchedule`，避免两个同类副本落到同一节点。app、DocReader PDB 为 `minAvailable: 2`；两个 Agent、frontend、mobile-web 的 PDB 为 `minAvailable: 1`。

## 容量为什么这样设置

### 文档吞吐

已完成的同工作量测试结果：

| 配置 | 结果 | 原始吞吐 |
|---|---:|---:|
| 1 个 app，单实例完整文档并发 4 | 100/100 | 约 1.23 docs/s |
| 3 个 app，每实例完整文档并发 4 | 100/100 | 约 4.50 docs/s |
| 3 个 app，500 文档 | 500/500 | 约 4.10 docs/s |

因此生产固定为：

```text
app 副本数                   3
单实例完整文档并发           4
集群完整文档 admission       12
每 app 普通后台任务 worker   16
每 app Wiki Map worker       4
每 DocReader gRPC worker     4
DocReader 总 parser worker   12
```

`asynq.concurrency` 是系统管理员界面的单实例完整文档并发；修改后需要滚动重启所有 app，不能长期混用不同值。后台 worker 数不是额外的完整文档槽位，所有衍生任务仍属于相应文档工作流，必须完成向量、摘要、问题、图谱、Wiki 等启用项后，文档才进入完成终态。

### 单节点资源

`.1/.2` 在正常分布下每台各有一套 app、DocReader、两个 Agent、frontend 和 mobile-web：

| 资源 | requests 合计 | limits 合计 | 节点可分配/物理资源 |
|---|---:|---:|---:|
| CPU | 约 4.65 核 | 约 15.8 核（可突发、允许超卖） | 约 7.6 核 |
| 内存 | 约 8.7 GiB | 约 25 GiB | 约 27.4 GiB 可分配、32 GiB 物理 |

requests 后仍保留约 2.9 核和 18 GiB 调度余量；limits 后仍保留约 2.4 GiB 可分配内存余量。CPU limits 有意允许突发超卖，因为 DocReader 的 CPU 峰值与 app/Agent 等待模型或 I/O 的时段通常不重合。内存不按这一假设超卖，避免节点级 OOM。

`.7` 仅放一套 app + DocReader，requests 约 3 核/6 GiB，limits 约 10 核/18 GiB，保留空间更大。这样既能使用现有三个 32G 节点，又不会把数据库所在的 `.6` 和图数据库所在的 `.54` 压到内存边缘。

### PostgreSQL 连接

每 app `max_open=8`、`max_idle=2`，三个 app 的主要连接预算约 24 条；加上迁移、LiteLLM 和运维连接，仍低于现网 PostgreSQL `max_connections=100`，保留超过 50% 的故障和运维余量。禁止把连接池大小按 Pod 扩容后仍当成集群总数。

### 模型准入

模型准入由所有 app 通过 Redis 共享，是集群总上限，不随副本数相乘：

| 类别 | 集群并发 | 单租户并发 | 依据 |
|---|---:|---:|---|
| Chat | 24 | 12 | DeepSeek V4 Flash 在 32 并发、每请求实际生成 160 token 时 32/32 成功，P95 5.655 秒；运行上限 24 保留 25% 并发余量，并另保留 2 个交互槽位 |
| Embedding | 32 | 16 | 独立模型、批处理快，避免向量化成为 12 文档槽位的瓶颈 |
| Rerank | 24 | 12 | 独立模型，保留多租户公平性 |
| VLM | 4 | 2 | 图像长调用，限制显存和超时放大 |
| ASR | 2 | 1 | 音频调用重且数量相对少 |
| Parser | 12 | 4 | 与 3×4 DocReader worker 对齐 |

该配置是在现有实测信息下最大化资源且保留冗余的生产值。Chat 原先的 `18`
没有可复核的落盘依据，已经用
[`20260726-deepseek-v4-flash.json`](../../custom/tests/model_capacity_reports/20260726-deepseek-v4-flash.json)
重新校准。报告同时包含 1～32 并发短输出和 18/24/32 并发、每请求 160 token
的受控结果；它不是任意长上下文的容量承诺，因此生产仍按 24 而不是实测边界 32。
后续调整必须用同一批固定文档、相同模型和 P95/P99 延迟重新测量，不能仅因为增加
app Pod 就提高模型上限。

## 无 RWX 存储设计

| 数据 | 位置 | 生命周期 |
|---|---|---|
| 原始上传文档 | OBS | 持久 |
| 解析产生的持久图片/对象 | OBS | 持久 |
| Agent 最终 Word/Excel/PDF/PPT 等产物 | 私有 OBS | 持久 |
| chunk、向量、任务、Wiki、问题和状态 | PostgreSQL | 持久 |
| 图谱节点和关系 | Neo4j | 持久 |
| app 分卷、转换和下载缓存 | Pod 本地 RWO 临时 PVC | Pod 生命周期 |
| DocReader PDF/Office/OCR 工作区 | Pod 本地 RWO 临时 PVC | Pod 生命周期 |
| Agent SDK、Office/Python 工作目录 | Pod 本地 RWO 临时 PVC | Pod 生命周期 |
| `/data/files` | 1 GiB `emptyDir` 兼容目录 | 不允许存放持久数据 |

OBS 不能挂成 Agent 或 DocReader 的 POSIX 工作目录。Office 转换、解压、随机写和大量小文件必须在本地卷完成；只有校验后的最终对象进入 OBS。

### CCE 本地临时卷前置检查

华为 CCE 的本地 PV 由 Everest/LVM 提供，默认动态 StorageClass 为 `csi-local-topology`，绑定模式应为 `WaitForFirstConsumer`。`.1/.2/.7` 必须各有足够的数据盘空间加入 Everest 本地卷池。官方说明参见[动态本地 PV](https://support.huaweicloud.com/usermanual-cce/cce_10_0634.html)。

```bash
kubectl get sc csi-local-topology -o yaml
kubectl api-resources | grep -i nodelocal
kubectl get nodelocalvolumes -A -o yaml

# 分别在 .1/.2/.7 核对，不允许只看 kubelet 上报的 ephemeral-storage。
lsblk
vgs
lvs
df -h
```

要求：

- StorageClass provisioner 为 Everest local CSI，`volumeBindingMode=WaitForFirstConsumer`，`reclaimPolicy=Delete`。
- `.1/.2` 每台正常运行占 240 GiB，`.7` 正常占 180 GiB。生产配置已将 app、DocReader 和两个 Agent 固定为 `maxSurge=0,maxUnavailable=1`，一次只替换一个 Pod，避免 500 GB 数据盘在滚动时额外申请整套临时卷。
- 禁止同时手工滚动四类大临时卷 Deployment；500 GB 十进制磁盘格式化后约 465 GiB，正常态仍需保留 CSI、文件系统和故障处理余量。
- 数据盘若仍作为 `/mnt/weknora-data` 普通文件系统或旧 `weknora-data-files` local PV 使用，不能同时假定它已属于 Everest LVM 池。必须先迁移、校验、卸载旧用途，再按 CCE 存储池方式配置。
- 本地 PVC 只保存可重算数据。节点永久损坏时新 Pod 在其他节点创建新临时 PVC，由持久文档工作流重新执行未提交阶段。

## 对象键规范

所有长期对象都使用“用途标记 + 部署标识 + 固定命名空间 UUID”的私有根。
三个用途根必须互不相同，即使它们位于同一个 OBS bucket：

```text
知识库与解析衍生对象：
weknora/__weknora_private_knowledge_objects_v1__/
  deployment/prod-cce-wk-6a9d12b0/
  namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  {tenant_id}/{knowledge_uuid}/{object_uuid}.{ext}

临时对象：
weknora/__weknora_private_knowledge_objects_v1__/
  deployment/prod-cce-wk-6a9d12b0/
  namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  temp/{tenant_id}/{object_uuid}.{ext}
```

生产 Agent 最终产物使用独立私有根：

```text
weknora/__weknora_private_agent_artifacts_v1__/
  deployment/prod-cce-wk-6a9d12b0/
  namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  tenant/{tenant_id}/
  session/{session_uuid}/
  run/{run_id}/
  artifact/{artifact_uuid}/{artifact_uuid}.{ext}
```

用户上传的专业技能使用独立私有根，数据库保存对象路径、大小、SHA256 和文件数；镜像内两个系统预置技能不进入该根：

```text
weknora/__weknora_private_professional_skills_v1__/
  deployment/prod-cce-wk-6a9d12b0/
  namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  tenant/{tenant_id}/
  skill/{skill_uuid}/
  revision/{revision_uuid}/package.zip
```

设计约束：

- deployment 名只使用小写 DNS label；生产值 `prod-cce-wk-6a9d12b0` 是不含客户或业务名称的基础设施标识，其中短后缀仅便于人工审计。namespace 才是该生产部署一次生成、终身稳定的 RFC 4122 UUID。
- 测试、容灾演练、恢复验证和另一个集群必须生成不同 namespace UUID。
- 普通升级不得旋转 UUID，否则应用会把旧对象视为待迁移对象。
- 键中不放用户名、原文件名、客户名称、密钥或其他个人/业务敏感文本。
- `artifact_uuid` 使不同产物不冲突；同一个 `(tenant, run, file_token)` 重试复用数据库预留的精确键，实现幂等。
- 普通知识对象和临时对象使用随机 UUID 文件名；同名上传不会覆盖，跨租户、跨知识库也不会碰撞。
- 历史本地对象迁移保留其 `tenant/相对路径` 并写入本部署的唯一知识对象根；相同旧路径表示同一逻辑对象，重试只校验并复用该键。
- 各用途允许共享同一个部署 UUID，因为用途根已经完全隔离；禁止让两个独立部署、测试集群或恢复演练复用同一个 `(bucket, purpose, deployment, namespace UUID)` 组合。
- bucket 必须为 private，禁止 `public-read`。通过 app 校验 tenant/session/user 权限后流式下载。
- OBS 应在 bucket policy 开启默认服务端加密，访问日志和对象生命周期策略按公司合规要求配置。

原始文件临时交付使用另一个独立前缀：

```text
weknora/__weknora_claude_sdk_original_inputs_v1__/
  deployment/prod-cce-wk-6a9d12b0/
  namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  temp/{tenant_id}/{transfer_uuid}.{ext}
```

- `transfer_uuid` 每次物理传输独立生成，原文件名不进入对象键；并发上传同名文件不会覆盖。
- Agent 运行完成、失败、取消或发生 panic 后，app 都会幂等删除本次临时对象；生成预签名 URL 失败时立即补偿删除。
- Pod 被强制终止可能来不及执行应用清理，因此生产 OBS 还必须只针对
  `__weknora_claude_sdk_original_inputs_v1__` 前缀配置最长 24 小时自动过期兜底。
- 该生命周期规则严禁覆盖知识对象根和 Agent 最终产物根。

## 历史 `/data/files` 迁移与取消共享持久卷

取消原 `weknora-data-files` PVC 前必须完成以下不可跳过的校验：

1. 冻结旧版 app 的新写入，只允许已部署的迁移实例执行。
2. 查询 `custom_general_agent_artifacts.file_path`、知识文档 `file_path`、消息附件和其他持久文件引用，列出所有本地路径。
3. 对每个文件计算大小和 SHA256，写入生产私有 OBS 唯一键。
4. 对 OBS 对象执行 HEAD/Stat 和实际下载 SHA256 双校验。
5. 在数据库事务中把路径更新为 `obs://bucket/key`，状态只允许从 `uploading` 转为 `ready`。
6. 多 app 启动由 PostgreSQL advisory lock 串行迁移；后启动副本只验证现有 OBS 对象，不能再次生成不同键。
7. 验证历史文档预览/下载、历史 Agent 产物下载、删除清理和租户越权拒绝。
8. 确认有效记录中不存在 `/data/files/...`、裸本地绝对路径或旧对象前缀。
9. 保留旧盘只读备份直至备份窗口结束；确认无回滚需求后再删除 PVC/PV。删除后不可依赖应用回滚到旧本地路径。

当前代码对 Agent 产物执行上述锁、状态机、大小/SHA 校验和幂等迁移。知识文档主存储在生产已经是 OBS，但仍必须对现网约 98 MiB 历史目录做数据库引用清单，不能仅凭目录大小判断它已经无引用。

专业技能切换时还必须让唯一的维护实例挂载或保留旧 `skills/professional` 目录，并配置
`CUSTOM_SKILLHUB_PROFESSIONAL_STORAGE_PROVIDER`、`CUSTOM_SKILLHUB_PROFESSIONAL_BUCKET`
和 `CUSTOM_SKILLHUB_PROFESSIONAL_PATH_PREFIX`。维护实例只迁移数据库中仍有效且本地包可验证的非预置技能；找不到旧包的记录保持不可用并记录明确告警，禁止从其他 Pod 的空目录伪造成功。迁移完成后应确认所有需要保留的记录均有非空 `object_path`，再启动多副本服务。此后上传、列表、下载和对话运行时均以数据库与该私有对象根为事实源，不再扫描 Pod 本地上传目录。

## 入口请求大小

入口不能继续使用 Nginx 默认 1 MiB：

- 后端公网知识源原文件最大为 2 GiB；云 ELB、Ingress controller、桌面/移动 Nginx
  和任何 WAF/反向代理的请求体上限使用至少 2304 MiB，为 multipart 边界与元数据
  留出余量，后端仍按 2 GiB 精确拒绝超限原文件。
- Ingress 使用 `proxy-body-size: 2304m` 且关闭 request buffering，避免超大上传
  先完整落到 ingress 节点临时盘。
- app 与 DocReader 的普通解析传输上限仍为 50 MiB；超大文档先在 app 分卷，再传给 DocReader。
- 桌面和移动 Nginx 的普通附件代理上限为 80 MiB，避免 50 MiB 原文件因 multipart
  包装越过同为 50 MiB 的入口阈值；UI 和后端原文件限制仍为 50 MiB。
- 如果未来在 app→Agent 的 ClusterIP 路径中插入 Nginx、Service Mesh sidecar 或 API gateway，该内部入口设置为 128 MiB。原因是单个 50 MiB 附件经过 Base64 和 JSON 后约 66.7 MiB，再加请求元数据、multipart/JSON 开销和合理余量。
- 128 MiB 只适用于内部 Agent 请求，不能替代外部知识源 2 GiB 上限。

发布前分别验证：

```bash
kubectl -n weknora get ingress weknora -o yaml
kubectl -n kube-system get deploy -l app=nginx-ingress -o yaml
# 同时核对云 ELB/WAF 控制台的 body、idle timeout 和 upstream timeout。
```

## 密钥、镜像与发布

创建独立的 Agent 内部密钥，不复用用户 JWT、OBS AK/SK 或 LiteLLM key：

```bash
kubectl -n weknora create secret generic weknora-agent-internal \
  --from-literal=apiKey="$(openssl rand -hex 32)" \
  --dry-run=client -o yaml | kubectl apply -f -
```

生产镜像必须使用不可变 Git SHA tag，并记录 repo digest。SWR 已配置，app、frontend、mobile、两个 Agent、DocReader 和 sandbox 必须统一推到获批的业务 SWR 项目；Kubernetes Pod 通过 `default-secret` 拉取。

每个 app 节点还必须有：

- 可用的 `/var/run/docker.sock`。
- 完全相同 digest 的本次 Git SHA sandbox SWR 镜像；宿主 Docker 需要单独认证或预拉取。
- 完全相同内容和权限的 `/app/skills/preloaded`。

sandbox 是 `docker run --rm` 的单次运行，不保存跨请求会话，因此 app Service 不需要 sticky session。

发布前渲染和校验：

```bash
helm lint ./helm -f ./helm/values-production-ha.yaml
helm template weknora ./helm \
  -n weknora \
  -f ./helm/values-production-ha.yaml \
  -f /secure/values-site.yaml > /secure/weknora-rendered.yaml

kubectl apply --dry-run=server -f /secure/weknora-rendered.yaml
kubectl diff -f /secure/weknora-rendered.yaml
```

现网不是活动 Helm release，首次切换不能直接 `helm upgrade --install`。必须按完整运行手册备份、迁移、删除不可变 selector 的旧无状态 Deployment、重建 headless DocReader Service，再对渲染清单执行受控 `kubectl apply`；禁止 `--prune`。

## 部署后必须完成的验收

### 副本和分布

- app、DocReader 各有 3 个 Ready Pod，且分别落在 `.1/.2/.7`。
- general-agent、document-processing-agent、frontend、mobile-web 各有 2 个 Ready Pod，且各自跨 `.1/.2`。
- app instances API 显示 3 个 `healthy=true` 实例、每实例 capacity=4、集群 capacity=12。
- DocReader 请求实际命中过 3 个 Pod；五类智能体的请求实际命中过两个 Agent Pod，而不是仅检查副本数。

### API 与浏览器

- API 上传、状态、重建、删除、召回和下载都经过多 app Service。
- 浏览器桌面端和移动端分别经过两个副本，登录、租户切换、知识库、聊天、上传和下载正常。
- 通用、知识库管理、数据分析、表格分析、文档处理五类智能体均覆盖其特有工具和产物。
- 同一 Agent 运行在一个 Pod 内完成，最终产物在 terminal 事件前进入 OBS；随后由任意 app 副本下载。

### 文档主链路

- 至少以不同用户、同/不同知识库并行上传混合大小和格式的批量文档。
- 每份文档都要直接查 PostgreSQL 确认主解析、chunk、向量、摘要、问题、图谱和 Wiki 的启用项均达到成功终态。
- 直接查向量字段/召回结果、Neo4j 节点关系、Wiki 页面和问题记录；不得只相信测试脚本的“completed”。
- 前端卡片状态、全部状态筛选、失败筛选、等待位置与后台一致。
- 解析中删除和完成后删除都要确认取消工作流、清理 chunk/向量/图谱/Wiki/问题及对象。
- “公司制度”知识库作为保留数据，不得在异常测试中删除或无理由重新解析。

### 故障注入

- 单 app 优雅退出、容器原地重启、replacement Pod、单节点故障。
- 单 DocReader 退出以及 PDF/Office 原生进程异常。
- 一个和多个 Agent Pod 退出。
- pause/网络分区超过 heartbeat/lease，确认没有仅凭超时发生双 owner。
- 精确旧 Pod termination proof 后，确认 owner/boot/epoch 转移且旧 boot 写入被 fence。
- Redis、PostgreSQL、Neo4j、OBS、LiteLLM 分别短时不可用后的重试/恢复边界。
- 同一个 `(tenant, run, file_token)` 并发上传到多个 app，只允许一行 ready 记录和一个精确对象键。

## 仍存在的单点

| 组件 | 现状 | 影响 |
|---|---|---|
| Redis | master + replica，但无 Sentinel/Cluster/VIP 自动切换 | master 故障期间队列 delivery、流式状态和模型准入不可用 |
| PostgreSQL | 单实例、本地 RWO、单节点 | 所有状态、向量和工作流停止；节点永久故障需人工恢复 |
| Neo4j | 单实例、本地 RWO、单节点 | 图谱写入和图查询不可用 |
| LiteLLM | 单实例 | Chat/Wiki/问题/图谱等模型调用受影响 |
| 集群 | 全部节点同一 AZ | 不能抵抗 AZ 级故障 |

因此本方案实现的是：解析和 Agent/前端层的水平扩展、单 Pod/单解析节点故障恢复，以及无 RWX 的持久文件共享。它不能被表述为整套系统已经没有单点；要达到整系统 HA，仍需分别完成 Redis 自动切换、PostgreSQL HA、Neo4j HA、LiteLLM 多副本/外置以及跨 AZ 节点部署。
