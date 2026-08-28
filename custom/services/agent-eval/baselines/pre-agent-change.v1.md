# 三智能体多轮 Eval：智能体改动前基线 v1

状态：`FROZEN_PRE_AGENT_CHANGE`。这份锁建立在任何智能体业务逻辑改动之前；从原始观测提交到锁准备提交，Git 变更仅位于 `custom/services/agent-eval/`。机器可校验的完整哈希和身份见 `pre-agent-change.v1.json`。

## 基线边界

- SUT：WeKnora 快速问答、RAG 推理、通用智能体。
- 模型：DeepSeek V4 Flash，精确 ID 为 `prod-deepseek-v4-flash-int8-chat`。
- 历史窗口：快速问答 5 轮，RAG 推理 10 轮，通用智能体 10 轮；长上下文用例均测试到 12 轮。
- 原始 SUT commit：`69e46f961ac4b111616940d365b2f228e8213cc6`，工作树干净。
- evaluator 可执行 tree：`d539c75939ba416c2fcb2dc86910fc6df1da849e`，范围固定为 `weknora_eval-tree-v1`。
- DEV：12 case × 3 session，171 个 turn observation；GATE：6 case × 3 session，117 个 turn observation；两侧均为 0 `INVALID`。
- sealed holdout 内容在建锁时没有读取，只锁定 manifest、case 数和 dataset hash。

`artifacts/` 被 Git 忽略以避免把真实回答和运行记录写进代码历史；三份正式 baseline 的 canonical JSON SHA-256 已提交到 JSON 锁。禁止覆盖这些文件，候选必须使用新路径。Langfuse 中对应 run ID 为 DEV `2c27c0f94b9c2011`、GATE `1465823d5d6c47eb`。

## 门禁为何分两级

当前基线本身并未达到目标质量。如果日常实验直接使用“所有 DEV case 必须 3/3”的晋级门禁，那么任何单变量修改都会因其他已知失败而永远失败，迫使一次修改多个模块并加剧过拟合。因此使用以下分层：

1. `multiturn-experiment-gate.v1.json`：候选必须至少让一个 case 的三次重复通过率严格提高；任何 case 通过率、受保护指标族或按智能体 P95 延迟回退都失败。原样 baseline 自对照只因 `minimum_case_improvement` 失败，证明门禁不会把随机重跑当收益。
2. `multiturn-optimization-gate.v1.json`：DEV 晋级要求每个 case 3/3，并执行关键确定性约束和绝对延迟预算。
3. `multiturn-release-gate.v2.json`：只有 DEV 晋级后才运行；GATE 每 case 至少 2/3，且关键约束零失败。
4. sealed holdout：Codex 低频里程碑执行，不参与日常提示词或实现拟合。

固定 pre-agent baseline 永不前移。实验门禁通过后，候选 judged artifact 可以成为下一轮的 active champion；下一轮必须与该 champion 比较，形成不可跳跃的晋级链，避免后续修改丢掉前一轮收益。

## 当前有效结果

这里的“有效”指确定性评分经过冻结 Judge 按策略裁决后的 case verdict，而不是 artifact 中尚未裁决的确定性 verdict。

| 数据层 | 快速问答 | RAG 推理 | 通用智能体 | 总计 |
|---|---:|---:|---:|---:|
| DEV | 9/12 | 8/12 | 4/12 | 21/36 |
| GATE | 3/6 | 3/6 | 3/6 | 9/18 |

DEV 的 case 级稳定性：

| 失败簇 | 快速问答 | RAG 推理 | 通用智能体 |
|---|---:|---:|---:|
| active/retired state window | 0/3 | 0/3 | 0/3 |
| citation claim binding | 3/3 | 3/3 | 3/3 |
| current-turn alignment | 3/3 | 3/3 | 0/3 |
| decision under unknowns | 3/3 | 2/3 | 1/3 |

GATE 的 public/invited definition 三个智能体均为 3/3；warehouse state window 三个智能体均为 0/3。因此当前发布门禁明确失败，不存在“框架准备完成就等于智能体已合格”的误解。

延迟基线：

| 数据层 / 智能体 | P95 | 最大值 |
|---|---:|---:|
| DEV / 快速问答 | 13,716 ms | 16,515 ms |
| DEV / RAG 推理 | 25,120 ms | 27,519 ms |
| DEV / 通用智能体 | 240,004 ms | 240,009 ms |
| GATE / 快速问答 | 9,412 ms | 10,016 ms |
| GATE / RAG 推理 | 15,621 ms | 16,713 ms |
| GATE / 通用智能体 | 69,315 ms | 71,712 ms |

DEV 晋级自对照失败项为 `hard_constraints`、`repetition_pass_rate` 和通用智能体绝对延迟；GATE 自对照失败项为 `hard_constraints` 与 `repetition_pass_rate`。这些是智能体优化目标，不是 evaluator `INVALID`。

## Judge 权限与已知分歧

Judge 校准为 10/10、最低置信度 1.0，且正式协议为 `single-turn-v1`。但真实回答上仍出现 DEV 3 个、GATE 7 个“确定性 `state.unknown.*` 失败而 Judge 给 PASS”的分歧。因此：

- `state.unknown` 继续由确定性 `TextRule` 最终裁决，并且不可被 Judge 覆盖。
- Judge 只处理 policy 白名单内的语义等价表达。
- Judge 网络失败、低置信度或缺失得到 `INVALID`，不会被记成智能体 `FAIL`，也不会中断生产业务。

这能保留 Judge 对自然语言多样性的容忍度，同时防止流畅但漏状态的回答穿过硬门禁。

## 开始修改智能体时的固定流程

1. 从报告选择一个失败簇，只提出一个机制假设；首选顺序为：通用智能体执行收敛/当前轮对齐 → 三智能体共享 unknown-state 持久化 → RAG 主张与证据绑定。
2. 使用 `-CaseId` 做聚焦诊断，不带 baseline，因此不产生门禁结论。
3. 恢复完整 12 × 3 DEV，使用实验 policy 与当前 champion 比较。
4. 实验 gate PASS 后才保留改动并更新 champion 指针；固定 pre-agent 文件和哈希永不覆盖。
5. 全部 DEV case 达到 3/3 后运行 promotion gate；只有 PASS 才允许运行 GATE。
6. GATE 只看结论，不据其具体措辞继续调参；里程碑时由 Codex 单独运行 sealed holdout。
7. 连续两轮无 case 级收益、只改善措辞、Judge 分歧增加或 holdout 退化时停止并回滚。

完整命令见同目录上级 `README.md` 的“一次 Eval loop”。
