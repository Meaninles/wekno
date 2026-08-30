# WeKnora 智能体 Eval 框架

这套框架只评测本项目内的 WeKnora 智能体，覆盖 RAG 召回与引用、文档处理、工具安全和超多轮长上下文。它不把 WeKnora 变成“裁判”：WeKnora 只作为被测系统（SUT）、记录器和隔离实验环境；数据集策划、运行、分析、改进决策和 sealed holdout 由 Codex 执行。

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
  ├─ eval-loop.ps1 -> WeKnora eval API -> deterministic scorer -> calibrated semantic judge
  └─ paired gate (PASS / FAIL / INVALID) -> Markdown/JSON report
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

正式执行默认使用 `datasets/multiturn-ready.v3.jsonl`。它保留 v2 的全部问题、case ID、split 和重复次数，只修订评分契约：接受已经人工确认的低风险等价表达，区分“明确标注已废弃”与真正的状态复活，并新增真实测试中出现的内部规划泄漏和 D 供应商陈旧状态回归检测。GATE 仍覆盖三个智能体，每个 case 固定执行 3 个独立 session，总计 18 个 session execution；当前评测器冻结身份见 `manifests/multiturn-ready.v3-evaluator-v3.manifest.json`。evaluator v3 进一步统一跨平台文本哈希、语义行动边界、未知状态和否定示例判定；旧 manifest 均保留为不可变历史记录并在依赖变化后 fail closed。

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

## 指标、评分与门禁

确定性指标按 case 契约判定，不要求拟合一篇唯一参考答案：必需/禁止事实、当前/废弃/待确认状态及其分栏归属、禁止推断与行动边界、决策延期条件、关键主张与正确证据的邻接绑定、证据 anchor、正文引用与持久化 reference 一致性、检索来源下限、必需/禁止/只读工具、工具调用上限、多轮引用清零、响应和延迟边界。正式 gate 必须先通过冻结校准集，再由 Judge 复核语义型边界；Judge 只能裁决 policy 明确列出的语义指标，不能覆盖执行、引用证据、工具安全、长度或内部规划泄漏等关键失败。校准集包含“必填状态被省略”和“负责人/当前用户必须分离”的关键样例；关键样例任一错判都会让整次校准失败，即使总准确率仍超过阈值，防止 Judge 用流畅但不完整的回答覆盖确定性失败。

门禁不计算一个容易掩盖问题的加权总分，而是依次检查：

1. 数据集 hash、case/capability 覆盖与 baseline 完整性；缺失或执行故障为 `INVALID`。
2. 校准 Judge 覆盖率与置信度；缺失、低置信度或无效裁决为 `INVALID`。
3. 关键约束与每个 case 的三次独立 session 通过率；低于当前阶段的绝对阈值为 `FAIL`。
4. 按 case 聚合后的 baseline → candidate 通过率回归，而不是把随机的 attempt-1/2/3 强行一一配对；实验阶段还要求至少一个 case 严格改善，原样重跑不能伪装成收益。
5. 指定指标族通过率、按智能体的 P95/最大绝对延迟和按智能体的相对延迟回归预算。指标族按前缀聚合，例如 `state.unknown` 覆盖所有具体未知项。
6. 任一 `INVALID` 优先得到整体 `INVALID`，不能把“没测成”伪装成质量下降或通过。

业务运行始终 fail-open；发布 gate 始终 fail-closed。这两个失败域完全分开。

## 一键执行前准备（不会运行 Eval）

在 Eval 栈已经启动、Main 栈完全停止后执行：

```powershell
custom/services/agent-eval/prepare-eval.ps1 -Split gate
```

该命令只做只读 handshake 与静态校验：重建轻量 runner、校验 JSONL、校验 judge calibration、核对冻结哈希、DeepSeek V4 Flash 精确模型 ID、知识文档/语料变量、三个智能体绑定、历史窗口外 2 轮覆盖、3 次独立重复计划、门禁能力覆盖以及 eval/full recorder。它不会创建 session，不会发送 chat 请求，不会生成 baseline，也不会发布 Langfuse experiment；输出中的 `formal_eval_executed` 必须为 `false`。

sealed holdout 的准备也需要明确授权：

```powershell
custom/services/agent-eval/prepare-eval.ps1 -Split sealed_holdout -AllowSealed
```

如果 Main 的 `weknora` 或 `weknora-runtime-profile-e2e` Compose 项目仍有容器运行，准备和正式 eval 都会直接拒绝，避免两套工作树争抢端口、CPU、内存或写错存储。

## 一次 Eval loop

先启动 Eval 栈；`eval-loop.ps1` 会在首次运行时准备 `runner.env` 并构建轻量 runner。必须先让 `prepare-eval.ps1` 返回 READY。无 baseline 的首次正式运行只生成实验与报告，不发放发布结论：

```powershell
custom/services/agent-eval/eval-loop.ps1 -Split gate
```

Codex 审核首轮结果、数据集冻结清单和人工抽样后，才将一个合格 run 固化为 baseline。之后每轮执行配对门禁：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split gate `
  -Baseline /workspace/artifacts/baseline-gate-v1.json
```

无 baseline 的 DEV/探索运行可用 `-Judge` 主动生成语义评分。只要提供 `-Baseline`，脚本就会自动先实时运行冻结的 Judge calibration，未达准确率直接停止；随后用当前冻结契约重算 baseline、重新裁决 baseline，再对 candidate 和同 attempt baseline 做语义复核，因此旧 baseline 不会因缺少 Judge 字段而变成伪 `INVALID`。不能跳过 Judge 后仍获得正式 gate 结论。它要求 `runner.env` 中配置 OpenAI-compatible judge。默认并发为 1，避免模型限流与本机抢占影响结果；调高 `-MaxConcurrency` 前先建立同并发基线。

`eval-loop.ps1` 默认给每个回答 240 秒墙钟总截止时间，可用 `-ResponseDeadlineSeconds` 显式调整。该值会写入 `execution_contract` 并参与 baseline/candidate 身份比对，不能靠放宽超时获得伪提升。持续 SSE 心跳不再能绕过截止时间：超时、流结束后无完整持久化回答，以及由此跳过的后续轮次都会记为可复现的 SUT `FAIL`；如果流中某一步报错但最终完整回答已经持久化，则以最终回答为准继续评分。WAF、HTTP/网络、记录器和 evaluator 故障仍为 `INVALID`。两者不会互相污染，也都只发生在隔离 Eval 环境。

改智能体前的完整优化基线命令为：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/multiturn-optimization-dev.v1.jsonl `
  -Manifest /workspace/manifests/multiturn-optimization-dev.v1-evaluator-v3.manifest.json `
  -Policy /workspace/policies/multiturn-optimization-gate.v1.json `
  -Judge `
  -MaxConcurrency 1
```

正式修改智能体时使用四级循环，不能跳级：

1. 聚焦诊断：只选一个失败簇和一个可解释变量，以 `-CaseId` 无 baseline 运行；结果只用于定位，不产生门禁结论。
2. 全量 DEV 实验门禁：恢复 12 case × 3 session 的完整矩阵，与当前 champion 配对；必须至少改善一个 case，任何 case 通过率、受保护指标族或 P95 延迟回退都会失败。通过后该 judged artifact 才能成为下一轮 champion。
3. DEV 晋级门禁：使用 `multiturn-optimization-gate.v1.json` 全量复测，全部 case 必须 3/3，才允许进入未参与调优的 GATE。
4. 发布与泛化：GATE 要求每 case 至少 2/3 且零关键失败；sealed holdout 只由 Codex 在低频里程碑显式运行，内容不进入日常调优上下文。

聚焦诊断示例：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/multiturn-optimization-dev.v1.jsonl `
  -Manifest /workspace/manifests/multiturn-optimization-dev-experiment.v1-evaluator-v3.manifest.json `
  -Policy /workspace/policies/multiturn-experiment-gate.v1.json `
  -CaseId <case-id> `
  -Judge
```

全量单变量实验门禁示例：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split dev `
  -Dataset /workspace/datasets/multiturn-optimization-dev.v1.jsonl `
  -Manifest /workspace/manifests/multiturn-optimization-dev-experiment.v1-evaluator-v3.manifest.json `
  -Policy /workspace/policies/multiturn-experiment-gate.v1.json `
  -Baseline /workspace/artifacts/baseline-pre-agent-change-dev-experiment.v1.json
```

标准循环是：观察失败簇 → 只提出一个改动 → 聚焦验证机制是否命中 → 全量 DEV 与 champion 比较 → 通过实验门禁才保留 → 达到 3/3 后晋级 GATE → 低频 sealed holdout。连续两轮没有 case 级实质增益、只改善已知措辞、Judge 与确定性指标分歧升高或 holdout 退化时立即停止并回滚候选，防止无限拟合与过拟合。固定的 pre-agent baseline 永不覆盖；champion 只保存“从哪个已通过 artifact 晋级”的链条。

正式 run artifact 会写入 `summary_model_id`、`corpus_version`、知识文档 ID、profile set 哈希、eval 目录自身的 Git tree identity/dirty 状态、scorer 哈希、Judge 模型、校准集哈希和 Judge prompt 哈希。SUT 单独记录源代码 commit、整个工作树 dirty 状态、实际运行的 Go runtime 镜像 ID 和 general-agent 镜像 ID。门禁要求 evaluator 在 baseline/candidate 之间完全一致且两侧 SUT 都来自干净提交，但允许候选 SUT commit 和镜像与 baseline 不同——这正是智能体改动需要比较的变量。Langfuse 发布模式下，每个 `case × attempt` 都是独立 dataset item，不会把声明的 3 次重复悄悄压成 1 次。

LLM Judge 的权力由 gate policy 白名单约束。它可以消除可接受措辞和语义表达造成的误杀，但不能覆盖关键确定性失败。校准样例本身使用与数据集完全一致的 `TurnContract` / `TextRule` 强类型协议和真实问题上下文，禁止用只在校准中成立的简写契约；关键正反例任一错判都会使整次校准失败。每次正式 gate 都实时运行 `calibration run`，同时达到 `calibration/judge-multiturn.v1.json` 的最低准确率和逐项最低置信度；日常 preflight 只执行结构校验，不调用 Judge。这样避免把“最优回答”误写成唯一措辞，也避免裁判漂移驱动无限拟合。

正式裁决固定使用 `single-turn-v1` 协议：每次请求只允许携带一个 turn 的契约和回答，与单项校准走同一代码路径，禁止后续回答替早期回答补齐漏项。即便如此，LLM 也无权覆盖可机器验证的 `state.unknown` 必填状态：该指标由含多种可接受表达的 `TextRule` 确定性执行，是不可审查、零回归硬信号；Judge 只处理策略白名单中的语义等价项。Judge 网络调用默认采用单次 180 秒、最多 2 次的有界重试，并把协议与两个参数写入 execution identity。某个 case 在重试后仍失败时，只把该 case 标为 `INVALID`，继续落盘其余裁决并生成 gate/report；它不会改写原始观测，不会把评测基础设施故障记成智能体质量 `FAIL`，也不会向业务请求路径传播异常。

`framework_commit` 固定为 `weknora_eval/` 可执行代码的 Git tree identity，并配合独立的 `gate_policy_sha256`。因此新增报告、基线锁或说明文档不会让候选与基线失去可比性，但评分/裁决代码或门禁策略的任何变化仍会触发身份不一致并失败关闭。

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
- `prepare-eval.ps1`：只读一键预检；成功也不会创建 session 或发送对话。
- `eval-loop.ps1`：preflight、实时 Judge 校准、baseline 重算/裁决、run、gate、report。
- `weknora_eval/`：数据集、校准、readiness、runner、确定性评分、三态门禁和 Langfuse experiment 适配器。
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
