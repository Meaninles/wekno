# WeKnora 智能体 Eval 框架

这套框架只评测本项目内的 WeKnora 智能体，覆盖 RAG 召回与引用、文档处理、工具安全和超多轮长上下文。它不把 WeKnora 变成“裁判”：WeKnora 只作为被测系统（SUT）、记录器和隔离实验环境；数据集策划、运行、分析、改进决策和 sealed holdout 由 Codex 执行。

> 当前新实验与发布结论使用 [ANTI-OVERFITTING-AUDIT-v2.md](ANTI-OVERFITTING-AUDIT-v2.md)
> 中的双轨产物和 Codex 完整对话审核。下文的 v1/v10 contract、确定性 scorer 和
> Judge 流程保留用于历史结果复现，不得作为新 v2 生产质量结论。v2 允许轻微
> 表达/格式问题，三次独立会话至少两次由 Codex 判断“实质正确且有用”即可；固定
> 短语和 required claims 只作为诊断，不再决定质量 PASS。

## 当前 v2 工作流

1. runner 原样保存 `production_candidate`，可选地另存 Eval-only
   `eval_assisted_answer`；两条轨道各有引用、分数和修复成本。
2. 分别导出两条轨道的完整对话，由 Codex 逐个审核；审核包不含 contract、参考
   答案、required claims 或 Judge 反馈。
3. 将审核结果按完整对话 SHA 绑定回 run artifact，再运行版本化 gate。

首次运行未见分布矩阵前，用独立 fixture 创建四个隔离知识库并更新本地、已忽略的
`runner.env`；脚本不会输出 API key：

```powershell
custom/services/agent-eval/prepare-unseen-capability-kbs.ps1
```

```powershell
python -m weknora_eval codex-review-export --dataset <dataset.jsonl> --run <run.json> --answer-track production_candidate --output <production-review.json>
python -m weknora_eval codex-review-export --dataset <dataset.jsonl> --run <run.json> --answer-track eval_assisted_answer --output <assisted-review.json>
python -m weknora_eval codex-review-apply --dataset <dataset.jsonl> --run <run.json> --reviews <completed-reviews.json> --output <reviewed-run.json>
python -m weknora_eval gate --dataset <dataset.jsonl> --candidate <reviewed-run.json> --baseline <reviewed-baseline.json> --policy policies/production-multiturn-release-gate.v3.json --output <gate.json>
```

正式 production gate 同时要求 `gate` 与 `sealed_holdout`。sealed 正文不存在或未由
Codex 低频授权执行时，结果必须是 `INVALID/NOT_READY`，不能用可见矩阵补齐。

## 设计边界

| 维度 | production | eval |
|---|---|---|
| 允许行为 | 仅采样记录元数据 | 全量记录、数据集回放、评分、实验 |
| 正文/推理内容 | 强制不记录 | 可记录，且有单属性大小上限 |
| 默认采样 | 1%，且不能超过生产上限 | 100% |
| 发送路径 | 有界 OTel BatchSpanProcessor；满队列丢弃 | 同一异步路径，可调更高容量 |
| 业务失败策略 | fail-open；配置、导出和 Langfuse 故障均不阻断业务 | 单 case 失败记 INVALID，继续其它 case |
| Eval runner | `doctor` 明确拒绝 | 必须同时满足 `mode=eval` 与 `recorder_enabled=true` |

生产模式未采样的请求会把该采样决定沿请求上下文传播。模型、流式响应、Embedding、Rerank、VLM 和 ASR 适配器直接调用原实现，不生成摘要、不缓存正文，也不增加流式转发 channel。采中的少量请求只做元数据统计，导出在有界异步队列完成；退出时的 flush 也不能改变业务请求结果。

## 组件

```text
Codex
  ├─ 真实对话采样 -> Quarantine -> 去敏/去重/契约化 -> family split/freeze
  ├─ eval-loop.ps1 -> WeKnora eval API -> production / assisted 双轨产物
  ├─ Codex 逐个审核完整对话 -> 绑定 exact-track SHA-256 verdict
  └─ 客观完整性/隔离/性能检查 + Codex 质量结论 -> Markdown/JSON report
                                  │
                                  └─ Langfuse v4：trace、dataset、experiment、score 记录
```

只引入 Langfuse v4 和 OpenTelemetry 两个通用组件，WeKnora 侧保留一个薄适配层。Langfuse Web/Worker 固定在同一精确版本 `4.22.0`，使用官方推荐的分离形态；ClickHouse 固定为 v4 要求的 `25.12`，Redis 使用 `noeviction`。参考：[Langfuse v4 容器文档](https://langfuse.com/self-hosting/deployment/infrastructure/containers)、[ClickHouse 要求](https://langfuse.com/self-hosting/deployment/infrastructure/clickhouse)、[OpenAI Evals 指南](https://developers.openai.com/api/docs/guides/evals)。

## Worktree 与 Docker 物理隔离

实现分支为 `codex/agent-eval-framework`，工作树为 `C:\weknora-agent-eval-framework`。主工作树仍为 `C:\weknora`。

| 资源 | 主工作树 | Eval 工作树 |
|---|---|---|
| API | `localhost:8080` | `localhost:18080` |
| Langfuse | 主环境原端口 | `localhost:13001` |
| 容器前缀 | `WeKnora-*` / `weknora-runtime-*` | `WeKnora-agent-eval-*` / `weknora-agent-eval-runtime-*` |
| 网络 | `weknora_WeKnora-network-dev` | `weknora-agent-eval-network` |
| 基础设施卷 | Compose project `weknora_*` | Compose project `weknora-agent-eval-infra_*` |
| Langfuse ClickHouse 卷 | 主环境卷 | `weknora-agent-eval-platform_*` |
| 应用镜像 | 原镜像名 | `weknora-agent-eval-*:local` |

两边只 `stop/start`，不会删除卷，因此可以快速切换并保留各自状态。切换脚本先停另一侧，并会一并停止另一 worktree 的 Vite/esbuild 前端进程，避免 `5177` 页面仍代理到已经停止的旧后端；禁止同时启动两整套环境：

```powershell
# 开发 Eval 版本：先停主工作树，再启动隔离栈
custom/services/agent-eval/stack.ps1 -Action up -Target eval

# 后续回到老版本：先停 Eval，再从 C:\weknora 启动主栈
custom/services/agent-eval/stack.ps1 -Action up -Target main

# 修改后按项目固定拓扑完整重建当前 Eval 后端
custom/services/agent-eval/stack.ps1 -Action rebuild -Target eval

# 查看两侧残留状态
custom/services/agent-eval/stack.ps1 -Action status
```

后端切换完成后，只启动当前 worktree 的一个前端进程。Eval 前端必须显式代理到隔离 API；主前端代理回主 API：

```powershell
# Eval 前端（首次进入新 worktree 时先执行一次 npm ci）
cd C:\weknora-agent-eval-framework\frontend
npm ci
$env:VITE_DEV_PROXY_TARGET = "http://localhost:18080"
npm run dev -- --host 0.0.0.0 --port 5177

# 主前端（切回主环境后使用；不要与上面的进程同时运行）
cd C:\weknora\frontend
$env:VITE_DEV_PROXY_TARGET = "http://localhost:8080"
npm run dev -- --host 0.0.0.0 --port 5177
```

普通 `rebuild` 只重建包含当前 worktree 代码的镜像。两个 Python 智能体串行构建，Go module/build 使用持久 BuildKit cache，避免同时抢占 CPU、内存和磁盘。通用开发基础镜像优先复用主环境的不可变 Docker 层，并额外使用独立 Eval 标签；仅当 `docker/Dockerfile.dev-app` 本身变化时显式增加 `-RebuildBase`。

```powershell
custom/services/agent-eval/stack.ps1 -Action rebuild -Target eval -RebuildBase
```

Eval 栈使用项目规定的分角色拓扑，而不是单体 `app-dev`：API 3、解析 2、衍生 2、Wiki 2、维护 2、一次性迁移 1、DocReader 运行时 2，外加两个入口和基础 DocReader。

## 首次复制主环境数据

复制是“逻辑复制到新卷”，绝不挂载或共享主卷。脚本先停止两个应用面，只启动主环境 PostgreSQL/Redis/MinIO/Neo4j 进行导出；导出完成后停止源端，再启动目标基础设施恢复，因此不会同时运行两整套环境。

```powershell
# 默认：PG + Redis + Neo4j + .env 中声明的必要 MinIO 业务前缀
custom/services/agent-eval/seed-from-main.ps1

# 缓存不重要时进一步省空间
custom/services/agent-eval/seed-from-main.ps1 -SkipRedis

# 只有人工检查容量后才允许全量 MinIO
custom/services/agent-eval/seed-from-main.ps1 -FullMinio
```

容量门禁按 PostgreSQL、Redis、Neo4j 和实际选中 MinIO 前缀的体积估算，预留 `1.5 × 待复制数据 + 20 GB`；不足时在写入目标卷之前退出。可通过 `-MinimumFreeGB` 提高安全余量。`-Force` 只应在人工确认 Docker 虚拟磁盘容量后使用。临时快照默认校验 SHA-256 后删除，清单留在 `artifacts/`。

Redis 通过 portable RDB 导出，恢复前清掉目标卷自己的 AOF 目录，再由 Redis 从 RDB 重建 AOF。MinIO 默认仅镜像 `MINIO_PATH_PREFIX` 及 custom agent/skillhub 声明的业务前缀。克隆库保留源环境的 schema version，因此原生迁移默认关闭；独立的一次性角色仍执行二开迁移，且非零退出会阻止所有业务角色启动。旧 Langfuse v3 的物理存储不复制到 v4；v4 从空 trace 库开始，需要的 dataset 由 CLI 发布，避免跨大版本存储格式耦合。

## 数据集生命周期

真实历史对话只能自动进入 `quarantine`，旧回答只作为 provenance，不能自动成为“标准答案”。Codex 完成脱敏、事实核验、难例扩展和 acceptable-answer contract 后，才允许晋级。

多轮问题发现采用 Codex 主导的单轮检查点，框架不接受预写的 12 轮对话脚本。scenario 对每个智能体只能定义 1 个真实业务种子问题；`discover` 每次只能执行这一轮或一个经审核的自适应下一轮，不存在批量逃生参数。Codex 必须先核对回答中的当前请求对齐、用户事实、已废弃事实、引用支撑、工具行为、格式约束和会话状态，再决定继续、纠错、重问或状态重同步。

继续会话时必须提供单独的 `--next-turn-file`。该文件只能包含 1 轮，必须指向最新已审核 turn，并记录上轮 disposition、调整策略、理由以及四项显式语义检查：上轮已人工复核、当前问题符合 next action、不依赖未经核实的助手断言、已按用户事实核对状态。若上轮是 `rejected_answer`，框架禁止使用普通 `continue`；必须纠错、重问或重同步。这样脚本只负责可靠执行和留痕，下一问仍由 Codex 根据真实回答生成。

```powershell
# 首轮：只读取 scenario 中的 seed
python -m weknora_eval discover --scenario discovery/scenarios/procurement-multiturn-problem-finding.v1.json --profile general-agent --output artifacts/general.json

# Codex 查看回答后记录审核
python -m weknora_eval discover-review --artifact artifacts/general.json --profile general-agent --turn turn-001 --disposition accepted_with_findings --finding "..." --next-action "..."

# Codex 再编写仅含下一轮的 plan，框架验证关联关系后执行一次
python -m weknora_eval discover --scenario discovery/scenarios/procurement-multiturn-problem-finding.v1.json --profile general-agent --resume-from artifacts/general.json --next-turn-file discovery/adaptive-turns/general-agent-turn-002.json --output artifacts/general.json
```

流式错误、上游 403、超时或采集异常单独进入 `invalid_attempts`，用户状态账本回滚，并尽力软删除本次请求对，不能污染下一轮有效历史。失败重试仍使用同一个已审核 plan，不生成后续问题。

每个完成轮次还必须通过 `discover-review` 写入 Codex 审核结论，才能 resume：`accepted_observation`、`accepted_with_findings` 或 `rejected_answer`。三种状态都只是对观察数据可用性的判断，框架固定写入 `eligible_as_gold=false`；回答正确与否不能由模型自己宣布。`rejected_answer` 会保留为失败行为证据，下一问必须显式纠错或重新同步，而不是沿着错误回答自动生成。

三个会话智能体及其历史窗口由 `profiles/multiturn-agents.v1.json` 固定：快速问答为 5 轮、RAG 推理为 10 轮、通用智能体为 10 轮；问题发现至少执行到各自窗口之外 2 轮。当前发现模型固定为 DeepSeek V4 Flash。该 profile 矩阵是 WeKnora 内部智能体差异的配置层，数据集 schema、评分器和门禁仍共用一套实现。

首轮 36 个有效对话轮次的审核结论、问题族和后续用例拆分建议见 `discovery/findings/procurement-multiturn-discovery-20260827.md`；对应 gitignored 原始产物的校验值见同目录 manifest。

这 36 轮记录已经由 Codex 整理为第一版稳定、分支安全的多轮 DEV 契约：`datasets/multiturn-dev.v1.jsonl`。它包含 4 个 family、12 个 case、57 个轮次，快速问答、RAG 推理、通用智能体各覆盖全部 family；不包含唯一参考答案，也不依赖某次历史错误回答才成立。整理规则、事实时间线和当前能力边界见 `discovery/findings/procurement-multiturn-contract-curation-20260827.md`。

提交的数据集由可审查的确定性编译器生成，并有测试防止生成器与 JSONL 漂移：

```powershell
Push-Location custom/services/agent-eval
python -m curation.build_multiturn_dev_v1
python -m weknora_eval dataset validate --input datasets/multiturn-dev.v1.jsonl
Pop-Location
```

这一版全部固定为 `dev`，只用于定位失败和校准契约；不得直接改为 `gate` 或 `sealed_holdout`。后两者必须使用全新的 family、事实组合和证据问题，防止调试集泄漏与过拟合。

### 历史 v1/v10 冻结资产（仅用于复现旧结果）

正式执行默认使用 `datasets/multiturn-ready.v3.jsonl`。它保留 v2 的全部问题、case ID、split 和重复次数，只修订评分契约：接受已经人工确认的低风险等价表达，区分“明确标注已废弃”与真正的状态复活，并新增真实测试中出现的内部规划泄漏和 D 供应商陈旧状态回归检测。GATE 仍覆盖三个智能体，每个 case 固定执行 3 个独立 session，总计 18 个 session execution；当前评测器冻结身份见 `manifests/multiturn-ready.v3-evaluator-v10.manifest.json`。evaluator v5 把未出现在用户请求中的引用数量上限降为可观测软指标；evaluator v6 允许一个多要点证据锚点由多个有效引用共同覆盖，并扩充内部规划泄漏检测；evaluator v7 把 MCP 运行时别名规范化为原始工具名，避免将内部 `todo_write` 误判为业务写操作；evaluator v8 补充中文检索预算和当前轮证据失败等内部规划泄漏，并把带执行阶段的终止 SSE 错误固定识别为 SUT 失败，禁止持久化错误文案伪装成业务答案；evaluator v9 在保持语义反转保护的前提下，将“替用户/为用户发送”纳入否定动作的受控等价表达；evaluator v10 仅在禁词前的紧邻谓词位置识别裸“不”否定（如“不等同于”），避免把明确否认误判为违规，同时仍会捕获后续独立的肯定断言。引用完整性、缺失要点、来源范围和真正的行动越界仍是硬约束。旧 manifest 均保留为不可变历史记录并在依赖变化后 fail closed。

Evaluator v2 仍对每个观测轮生成完整 Judge 覆盖，但只在 turn 声明了 `judge_rubric`，或冻结确定性评分发现硬失败需要语义复核时调用远程 LLM。没有语义 rubric 且全部机械契约已通过的轮次生成可审计的 synthetic PASS；rubric 轮会把确定性检查一并交给 Judge，禁止其再次臆测“字符串缺失、栏目缺失或工具违规”。这减少了无意义的 LLM 调用和自相矛盾误判，同时保留 Judge 对关系发明、错误归类和可复核边界的降级权。协议身份为 `single-turn-v2`，v1 manifest 在当前代码下必须 fail-closed，而不是被原地改写。

智能体改动前的稳定性基线使用 `datasets/multiturn-optimization-dev.v1.jsonl`。它完整复制 v3 中 12 个已审核 DEV case 的问题和契约，不读取、不改写也不派生 GATE/SEALED 文案；只把每个 case 提升到 3 个独立 session，共 36 个 session execution。同一数据集绑定两级门禁：`policies/multiturn-experiment-gate.v1.json` 用于单变量实验，要求至少一个 case 的重复通过率严格提升且 case、指标族和延迟都不回退；`policies/multiturn-optimization-gate.v1.json` 用于 DEV 晋级，要求全部目标 case 达到 3/3。发布 GATE 仍保留独立的 2/3 抗随机门槛。优化集必须在任何智能体改动前冻结，后续不得为了候选答案改变 scorer、Judge、contract 或通过阈值。

sealed holdout 不进入 Git：本机文件为 `sealed/multiturn-holdout.v1.jsonl`，只提交哈希、数量和 split 信息到 `manifests/multiturn-holdout.v1.manifest.json`。普通开发、DEV 和 GATE 都不会挂载其内容到优化输入；只有 Codex 在低频里程碑验收时显式使用 `-AllowSealed`。这不是把某次模型回答藏起来当标准答案，holdout 仍使用可接受答案契约，只把未见业务事实和问题组合隔离出来。

数据分层对应关系如下：

| 层 | 可见性 | 用途 | 是否参与日常调优 |
|---|---|---|---:|
| DEV | Git 内可见 | 定位失败、改提示词/召回/历史策略 | 是 |
| GATE | Git 内可见但冻结 | 与 baseline 配对做提交门禁 | 否，只看是否通过 |
| sealed holdout | 本机 `sealed/`，Git 忽略 | 低频里程碑泛化验收 | 否 |
| judge calibration | Git 内固定边界样例 | 校准裁判的 PASS/FAIL/INVALID 区分 | 不评 SUT |

```text
生产/验收真实对话
  -> quarantine（needs_codex_review=true）
  -> 去敏、去重、失败聚类、对抗/变形样例扩展
  -> 按 family_id 分组切分
  -> dev / gate / sealed_holdout / grader_calibration
  -> JSONL + SHA-256 manifest 冻结
```

禁止逐条随机切分；同一来源文档、问题模板、同义改写和多轮变体必须共享 `family_id`，只能落入同一 split。推荐用途：

- `dev`：高频调试，可反复看答案。
- `gate`：提交/发布门禁，可看失败原因但不参与日常提示词拟合。
- `sealed_holdout`：低频里程碑验收，默认 CLI 拒绝，必须显式 `--allow-sealed`。
- `grader_calibration`：人工金标，用于校准正式门禁的 LLM Judge。
- `quarantine`：自动采集但未由 Codex 审核的数据。

构建与冻结命令：

```powershell
python -m weknora_eval dataset build --transcripts <真实会话.json> --suite <suite> --agent-id <agent> --output quarantine.jsonl
python -m weknora_eval dataset split --input reviewed.jsonl --output-dir datasets/frozen-v1 --salt <固定盐>
python -m weknora_eval dataset validate --input datasets/frozen-v1/all.jsonl
python -m weknora_eval dataset freeze --input datasets/frozen-v1/all.jsonl --output artifacts/frozen-v1.manifest.json
```

`dataset build` 能直接读取 discovery artifact 的 `sessions[].turns[]`；任何未写入 Codex 审核的完成轮次都会让构建失败。它会忽略 `invalid_attempts`，从每个 session 推断真实 `agent_id`/endpoint，把观察到的助手回答放入 `provenance.reference_answers`，并把每轮 disposition、findings、next action、被拒回答和分支整理需求写入 provenance。生成 case 始终为 `enabled=false`、`split=quarantine`、`needs_codex_review=true`，因此错误回答只能作为失败行为证据，不会自动成为训练目标、标准答案或发布门禁标准。

真实自适应对话不能原样晋级为固定 gate：例如后续用户说“你刚才写了 30 台”只在被测回答确实犯过该错误时成立；优化后的智能体若没有犯错，这个固定下一问反而会制造伪失败。只要 provenance 中 `requires_branch_curation=true`，数据校验会硬性禁止进入非 quarantine split。Codex 必须把它整理成不依赖某个错误回答的稳定状态测试，或将其保留为由 Codex 根据当轮回答选择分支的 discovery/actor 用例；清除该标记前需要重新核对整条分支。

示例数据集可通过 `prepare-runner-env.ps1` 从隔离库解析 profile 冻结的 DeepSeek V4 Flash、指定知识文件和同一模型的 OpenAI-compatible Judge 连接，并在 eval-only 握手成功后生成 `runner.env`。租户与模型 API key 只在进程内解密并写入被 Git 忽略的本地文件，不打印到日志；脚本不会跟随数据库里后来变更的默认模型，而是按 profile 的精确模型 ID fail-closed。自定义数据集可复制 `runner.env.example` 后改用自己的绑定。数据集只保留 `${ENV}` 占位符，数据集 hash 不受运行时 ID 替换影响。

```powershell
custom/services/agent-eval/prepare-runner-env.ps1
```

## 当前 v2 质量判断与客观门禁

回答“是否足够好”的唯一裁决者是 Codex：逐个读取一段完整对话，只判断用户的真实任务是否被实质完成、事实是否有依据、上下文状态是否正确、引用是否归属正确、行动边界是否守住以及表达是否可用。`minor_issue` 允许 PASS；只有影响核心任务的 `major_issue` 或 critical finding 才能 FAIL。三次独立会话中至少两次 PASS 即达到当前最低要求，不追求每次措辞和格式都完美。

确定性 scorer 继续保存为诊断信息和 Eval assistance 的可解释触发信号，但 v2 policy 不把任何固定词、required claim、栏目形态或 Judge 分数列为质量否决项。gate 只对以下客观事实 fail closed：

1. 数据集/依赖哈希、split、case、三智能体与重复次数是否完整。
2. Codex 审核是否覆盖每段完整对话、答案轨道是否正确、审核 SHA 是否仍与答案一致。
3. `production_candidate` 是否与 SSE、数据库持久化和历史加载逐字一致。
4. production 与 Eval assistance 是否隔离；assisted PASS 永远不替代 production FAIL。
5. 运行身份、基线可比性、每智能体 P95/最大延迟和修复依赖是否未恶化。

任一完整性故障得到 `INVALID`，Codex 判断质量不足得到 `FAIL`。业务运行始终 fail-open；发布 gate 始终 fail-closed，这两个失败域完全分开。历史 v1/v10 的 deterministic/Judge 规则仍可用旧 manifest 复现，但不再给新实验发放质量结论。

## 一键执行前准备（不会运行 Eval）

### 修改后独立语义路由回归

`datasets/semantic-routing-regression.v1.jsonl` 是提示词与状态边界修改后的独立回归集，不改写原有 DEV、GATE 或 sealed holdout。它使用单独的 Meridian 场地手册语料，覆盖三个智能体、每个 12 轮、3 次独立会话，成对验证“对话内状态/内容处理不调用检索或文件工具”和“否定表达出现在知识问题中时仍正常检索引用”。语义质量仅由 Codex 完整对话审核决定；机械契约只记录工具、引用、答案轨道与持久化完整性。

该 v1 文件保留用于历史复现，但其中 quick case 的空 `knowledge_base_ids` 不能再作为“无知识库”证据：内置智能体的 `kb_selection_mode=all` 会在请求没有显式目标时回退到租户全部知识库。当前框架要求新用例显式声明 `knowledge_selection_mode=explicit|none`；runner 会在创建 session 前读取真实 agent 配置，`none` 只有绑定到 `kb_selection_mode=none` 且绑定知识库为空的同运行模式 Eval clone 才有效。旧数据仍按 `agent_default` 原样加载，不会被静默改写。

准备独立知识库并执行：

```powershell
custom/services/agent-eval/prepare-semantic-routing-regression-kb.ps1
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/semantic-routing-regression.v1.jsonl `
  -Manifest /workspace/manifests/semantic-routing-regression.v1-production-gate-evaluator-v2.manifest.json `
  -Policy /workspace/policies/semantic-routing-regression-gate.v1.json `
  -Profiles /workspace/profiles/semantic-routing-regression.v1.json `
  -MaxConcurrency 3 `
  -EnableEvalAssistance
```

首轮只导出 production/assisted 两份 Codex 审核包；逐对话填写并绑定答案轨道哈希后，以 `-Run` 和两份 `-CodexReview` 重新进入无基线回归门禁。该门禁只评 `production_candidate`，assisted 通过不能挽救 production 失败。

### 提示词修改后的全新回归

`datasets/fresh-generalization-regression.v1.jsonl` 在终态投影修改完成后才创建，不读取 sealed holdout，也不复制 Meridian、采购或 Skill 用例。它包含四个 13 轮 case、每 case 三次独立会话：快速问答分别使用新建的 Northbank 音频交付知识库和真实无知识库配置，RAG 推理、通用智能体使用真实无知识库配置；领域另覆盖社区菜园、用户研究和展览彩排。三个无知识库智能体只在隔离 Eval 租户创建，除知识选择为 `none` 外复制完整生产配置，不改变生产内置智能体。

```powershell
custom/services/agent-eval/prepare-no-kb-agent-profiles.ps1
custom/services/agent-eval/prepare-fresh-generalization-kb.ps1
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/fresh-generalization-regression.v1.jsonl `
  -Manifest /workspace/manifests/fresh-generalization-regression.v1-production-gate.manifest.json `
  -Policy /workspace/policies/fresh-generalization-regression-gate.v1.json `
  -Profiles /workspace/profiles/fresh-generalization-regression.v1.json `
  -MaxConcurrency 3
```

这组用例没有 required claims、参考答案、Judge rubric 或固定状态字段评分；Codex 逐段审核“足够好”即可，三次中至少两次通过。机械门禁仍要求三智能体/全部 case 完整、SSE 与持久化一致、知识选择真实、双轨隔离、无无效会话并满足延迟上限。

在 `evidence_query` 与任务改写分离完成后，又创建了一个不参与调优的单次能力回归：
`post-change-evidence-canary.v1` 使用全新的 Helix 实验室样本交接规程，采用 10 轮而非
既有 13 轮结构，连续验证混合状态/文档请求、否定知识问答、属性更新后的行动边界、
零计数的生命周期不确定性、用户原文来源和严格一句英文草稿。它同样不包含语义
答案规则，由 Codex 审核三次完整会话：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/post-change-evidence-canary.v1.jsonl `
  -Manifest /workspace/manifests/post-change-evidence-canary.v1.manifest.json `
  -Policy /workspace/policies/post-change-evidence-canary-gate.v1.json `
  -Profiles /workspace/profiles/post-change-evidence-canary.v1.json `
  -MaxConcurrency 3
```

`eval-loop.ps1` 会在每次相关运行前校验对应隔离知识库，并刷新语料版本和知识绑定
哈希，避免 `runner.env` 中上一次数据集的身份残留污染本次产物。

终态协议与提示词再次修改后，新增 `post-prompt-lab-handover.v1`。它不是把既有
问题替换名词，而是重新组合了未决状态同义表达、规则与对象生命周期分离、聊天内容
与外部持久化分离、话题切换、来源摘录、英文改写和工具边界，共 14 轮、2 个独立
session。用例不含 required claims、参考答案、Judge rubric 或固定字段答案；语义质量
只由 Codex 阅读整段对话后判断是否达到可接受水平：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/post-prompt-lab-handover.v1.jsonl `
  -Manifest /workspace/manifests/post-prompt-lab-handover.v1.manifest.json `
  -Policy /workspace/policies/post-prompt-lab-handover-gate.v1.json `
  -Profiles /workspace/profiles/post-prompt-lab-handover.v1.json `
  -MaxConcurrency 2 `
  -EnableEvalAssistance
```

一键脚本现在会读取数据集声明：只要包含
`knowledge_selection_mode=none`，每次运行都从当前生产智能体配置重新生成 Eval 隔离
clone，并且只把知识选择改为 `none`。因此新提示词或运行配置不会被陈旧 clone 掩盖。

在隔离 Eval 栈已经启动、Main 栈完全停止后，先创建/校验四个未见分布知识库；脚本只更新被 Git 忽略的 `runner.env`，不会打印凭据：

```powershell
custom/services/agent-eval/prepare-unseen-capability-kbs.ps1
```

随后运行当前 v3 preflight：

```powershell
custom/services/agent-eval/eval-loop.ps1 -Split dev -PreflightOnly
```

它只执行 handshake 与静态校验：冻结哈希、模型和语料绑定、三智能体矩阵、12 轮以上窗口溢出、3 次独立重复、eval/full recorder、运行镜像身份和 clean-worktree provenance；不会创建 session 或发送 chat 请求。正式 production preflight 必须同时准备 `gate` 与 sealed 数据，sealed 仍需显式授权：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split gate,sealed_holdout `
  -AllowSealed `
  -PreflightOnly
```

如果 Main 的 `weknora` 或 `weknora-runtime-profile-e2e` Compose 项目仍有容器运行，准备和正式 eval 都会直接拒绝，避免两套工作树争抢端口、CPU、内存或写错存储。

## 一次 v2 Eval loop

第一阶段只执行三智能体对话并导出两条答案轨道的 Codex 审核包，不自动调用 Judge，也不发放质量结论：

```powershell
custom/services/agent-eval/eval-loop.ps1 -Split dev
```

脚本默认运行未见分布矩阵，单 case 三个独立 session，并分别生成 `production_candidate` 与 `eval_assisted_answer` 的完整对话审核包。默认 assistance 关闭，两条轨道相同；只有显式传入 `-EnableEvalAssistance` 才允许外部 runner 做一次补充检索和最多两次改写，且结果永不写回 SUT。

Codex 必须逐个填写导出包中的 verdict、六个维度、依据轮次和 findings。审核只能看到用户原始消息、选定答案轨道、真实引用与工具观测；包中没有 contract、required claims、参考答案或 Judge 反馈。完成后对原 run 绑定审核，不重新跑一组不同答案：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Run /workspace/artifacts/run-<timestamp>.json `
  -CodexReview `
    /workspace/artifacts/codex-review-<timestamp>-production.completed.json, `
    /workspace/artifacts/codex-review-<timestamp>-assisted.completed.json `
  -Baseline /workspace/artifacts/reviewed-baseline-dev.json
```

没有 `-CodexReview` 时脚本明确输出 `WAITING_FOR_CODEX_REVIEW`；有审核但没有 reviewed baseline 时只生成报告，不伪造 gate。baseline 也必须来自相同数据、相同模型/语料/框架配置并完成同轨 Codex 审核。答案或引用发生任何变化后，审核 SHA 不匹配，gate 直接 `INVALID`。

production gate 只能读取 production 审核，Eval optimization gate 只能读取 assisted 审核。每个 case 的三次会话至少两次由 Codex 判断“实质正确且有用”即可；固定短语诊断、单项分数或某一次采样失败不会被提升为新的质量规则。延迟、SSE/持久化一致性、运行身份、覆盖和修复依赖仍作为客观底线单独检查。

正式 production 运行必须把 `gate` 与独立 sealed family 放在同一冻结候选中，并显式授权 sealed。当前仓库不包含 sealed 正文，所以日常可见矩阵最多形成 DEV/GATE 证据，不能自行宣布 production PASS：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split gate,sealed_holdout `
  -Dataset /workspace/sealed/<frozen-production-suite>.jsonl `
  -Manifest /workspace/manifests/<frozen-production-suite>.manifest.json `
  -AllowSealed
```

`framework_commit`、policy/scorer/Codex rubric 哈希、答案轨道、模型/语料/profile、SUT commit 与实际运行镜像都会进入产物身份。正式 gate 要求 evaluator 与 SUT 都来自干净提交；修改 policy、scorer、审核 rubric 或回答后，旧审核和旧 baseline 不能静默复用。

## 本地验证

生产只读真实对话、隔离知识库、DEV/GATE/SEALED 数据分层和三智能体多轮门禁的当前实现见 `PRODUCTION-DERIVED-MULTITURN.md`。在不发送任何问答请求的前提下，一键核对知识库与 release 预检：

```powershell
custom/services/agent-eval/prepare-production-multiturn-eval.ps1
```

该路径固定使用 `profiles/production-derived-multiturn.v1.json` 和 DeepSeek V4 Flash；知识场景只选择一个知识库，绝不直接选择文档。

```powershell
# Python
Push-Location custom/services/agent-eval
python -m unittest discover -s tests -v
python -m weknora_eval dataset validate --input datasets/multiturn-ready.v3.jsonl
python -m weknora_eval calibration validate --input calibration/judge-multiturn.v1.json
Pop-Location

# Go（Windows 宿主受项目 pg_query/CGO 限制，最终以 Linux runtime 镜像为准）
go test ./internal/custom/modules/agenteval

# Compose 静态展开
docker compose --env-file C:/weknora/.env --env-file custom/services/agent-eval/eval.env `
  -p weknora-agent-eval-platform -f custom/services/agent-eval/docker-compose.yml config --quiet
```

## 关键文件

- `stack.ps1`：两工作树互斥切换和固定分角色重建。
- `seed-from-main.ps1`：容量预检、顺序导出/恢复和物理卷隔离。
- `prepare-unseen-capability-kbs.ps1`：幂等创建四个独立未见分布知识库并冻结绑定。
- `eval-loop.ps1`：v3 preflight、双轨 run、Codex 审核导出/绑定、gate 与 report；不自动 Judge。
- `weknora_eval/codex_review.py`：完整对话、exact-track、SHA 绑定的 Codex 最低质量标准。
- `policies/production-multiturn-release-gate.v3.json`：只读 production candidate 的当前发布策略。
- `policies/eval-optimization-gate.v3.json`：只读 assisted answer 的当前恢复能力策略。
- `policies/repair-dependency-gate.v2.json`：修复触发、成功、repair-only、调用与延迟依赖策略。
- `datasets/unseen-capability-matrix.v1.jsonl`：7 个跨领域、12 轮、三智能体、每 case 三次的历史能力矩阵；保留不改以复现旧结果，其中空知识库请求不能作为真实无知识库证据。
- `datasets/unseen-capability-matrix.v2.jsonl`：保持 v1 问题文本不变，只将每个知识边界显式升级为 `explicit` 或真实 `none` clone。`unseen-capability-visible-readiness-gate.v1` 由 Codex 逐个完整对话判断 dev+gate 是否达到 2/3；它不读取 sealed holdout，也不能替代正式 production release gate。
- `datasets/post-prompt-lab-handover.v1.jsonl`：提示词修改后新造的 14 轮 RAG 无知识库反例回归，2 次独立运行，语义只由 Codex 整段审核。
- `ANTI-OVERFITTING-AUDIT-v2.md`：整改分类、双轨边界、验收证据和剩余风险。

以下文件只用于复现历史 v1/v10 结果，不参与当前质量结论：

- `prepare-eval.ps1`、Judge calibration 与旧 `multiturn-*` policy/manifest。
- `weknora_eval/` 中的旧 contract scorer/Judge 命令仍可读取历史产物，但当前 policy 不让其决定语义 PASS。
- `policies/production-multiturn-release-gate.v2.json`、`policies/eval-optimization-gate.v2.json`、`policies/repair-dependency-gate.v1.json` 及对应 manifest 固定复现 complete-event 投影修正前的历史运行。
- `policies/multiturn-release-gate.v2.json`：三智能体多轮 GATE 门禁策略。
- `policies/multiturn-experiment-gate.v1.json`：单变量候选至少改善一个 case、同时禁止 case/指标族/延迟回退的 DEV 实验门禁。
- `policies/multiturn-optimization-gate.v1.json`：智能体改动前冻结、目标 case 必须 3/3 的 DEV 优化门禁。
- `policies/multiturn-sealed-gate.v1.json`：低频 sealed holdout 门禁策略。
- `datasets/examples.v1.jsonl`：RAG、文档处理和长对话契约示例。
- `datasets/multiturn-dev.v1.jsonl`：由真实发现记录整理出的三智能体多轮 DEV 契约。
- `datasets/multiturn-ready.v3.jsonl`：冻结的 DEV + GATE 正式数据集（保持 v2 观察数据兼容）。
- `datasets/multiturn-optimization-dev.v1.jsonl`：不派生 GATE 文案、每个 DEV case 重复 3 次的优化基线集。
- `baselines/pre-agent-change.v1.json`：三套正式 baseline、原始观测、门禁自对照、preflight、依赖和 sealed manifest 的机器可校验不可覆盖哈希锁。
- `baselines/pre-agent-change.v1.md`：改智能体前的有效失败矩阵、Judge 权限边界和固定四级 loop 操作说明。
- `manifests/`：dataset、profile、policy、Judge calibration、scorer、gate 和 Judge prompt 的联合冻结哈希。
- `calibration/judge-multiturn.v1.json`：judge 正例、负例、边界例和 INVALID 校准集。
- `curation/build_multiturn_dev_v1.py`：上述数据集的可审查、确定性编译器。
- `curation/build_multiturn_ready_v1.py`：正式 DEV + GATE 数据集编译器。
- `curation/build_multiturn_ready_v3.py`：保持观察数据兼容的 eval 契约可靠性修订编译器。
- `curation/build_multiturn_optimization_dev_v1.py`：在任何智能体改动前冻结完整 DEV 稳定性矩阵。
