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
  ├─ eval-loop.ps1 -> WeKnora eval API -> deterministic scorer -> optional judge
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
- `grader_calibration`：人工金标，用于校准可选 LLM judge。
- `quarantine`：自动采集但未由 Codex 审核的数据。

构建与冻结命令：

```powershell
python -m weknora_eval dataset build --transcripts <真实会话.json> --suite <suite> --agent-id <agent> --output quarantine.jsonl
python -m weknora_eval dataset split --input reviewed.jsonl --output-dir datasets/frozen-v1 --salt <固定盐>
python -m weknora_eval dataset validate --input datasets/frozen-v1/all.jsonl
python -m weknora_eval dataset freeze --input datasets/frozen-v1/all.jsonl --output artifacts/frozen-v1.manifest.json
```

示例数据集可通过 `prepare-runner-env.ps1` 从隔离库解析默认模型和指定知识文件，并在 eval-only 握手成功后生成 `runner.env`。租户 API key 只在进程内解密并写入被 Git 忽略的本地文件，不打印到日志；自定义数据集可复制 `runner.env.example` 后改用自己的绑定。数据集只保留 `${ENV}` 占位符，数据集 hash 不受运行时 ID 替换影响。

```powershell
custom/services/agent-eval/prepare-runner-env.ps1
```

## 指标、评分与门禁

硬指标按 case 契约判定，不要求拟合一篇唯一参考答案：必需/禁止事实、证据 anchor、正文引用与持久化 reference 一致性、检索来源下限、必需/禁止/只读工具、多轮引用清零、响应和延迟边界。可选 LLM judge 只给软分和 pairwise 解释，不能覆盖硬失败。

门禁不计算一个容易掩盖问题的加权总分，而是依次检查：

1. 数据集 hash、case/capability 覆盖与 baseline 完整性；缺失或执行故障为 `INVALID`。
2. 硬约束；任一失败为 `FAIL`。
3. 同 case 的 baseline PASS → candidate 非 PASS 回归；为 `FAIL`。
4. 指定指标通过率与 P95 延迟回归预算。
5. 任一 `INVALID` 优先得到整体 `INVALID`，不能把“没测成”伪装成质量下降或通过。

业务运行始终 fail-open；发布 gate 始终 fail-closed。这两个失败域完全分开。

## 一次 Eval loop

先启动 Eval 栈；`eval-loop.ps1` 会在首次运行时准备 `runner.env` 并构建轻量 runner。无 baseline 的首次运行只生成实验与报告，不发放发布结论：

```powershell
custom/services/agent-eval/eval-loop.ps1 -Split gate
```

Codex 审核首轮结果、数据集冻结清单和人工抽样后，才将一个合格 run 固化为 baseline。之后每轮执行配对门禁：

```powershell
custom/services/agent-eval/eval-loop.ps1 `
  -Split gate `
  -Baseline /workspace/artifacts/baseline-gate-v1.json
```

需要软 judge 时增加 `-Judge`；它要求 `runner.env` 中配置 OpenAI-compatible judge。默认并发为 1，避免模型限流与本机抢占影响结果；调高 `-MaxConcurrency` 前先建立同并发基线。

标准循环是：观察失败簇 → 只在 `dev` 上提出一个可解释改动 → 固定 SUT、模型、语料和配置指纹运行 → 与同数据集 baseline 配对比较 → 通过 `gate` 才保留 → 周期性由 Codex 单独运行 sealed holdout。连续两轮无实质增益、只改善已知措辞、judge 与人工分歧升高或 holdout 退化时立即停止调优并回滚候选，防止无限拟合与过拟合。

## 本地验证

```powershell
# Python
Push-Location custom/services/agent-eval
python -m unittest discover -s tests -v
python -m weknora_eval dataset validate --input datasets/examples.v1.jsonl
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
- `eval-loop.ps1`：doctor、validate、freeze、run、可选 judge、gate、report。
- `weknora_eval/`：数据集、runner、确定性评分、三态门禁和 Langfuse experiment 适配器。
- `policies/release-gate.v1.json`：发布门禁策略。
- `datasets/examples.v1.jsonl`：RAG、文档处理和长对话契约示例。
