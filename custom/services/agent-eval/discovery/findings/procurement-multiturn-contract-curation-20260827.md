# 采购多轮对话稳定契约整理（2026-08-27）

## 本阶段边界

本阶段把 36 轮真实自适应问题发现记录整理为可重复的 `dev` 契约，不执行正式 eval、LLM judge、baseline 对比或发布门禁，也不修改三个智能体的业务实现。

整理结果为 `datasets/multiturn-dev.v1.jsonl`：12 个 case、4 个 family、57 个轮次。runner/gate 使用的规范化数据集 SHA-256 为 `dd90cb1afa2c819535d2f1dc7c08e4207d2ad86a158c79dede03f77ed6ffabad`；该值不受 JSONL 行顺序和工作树换行符影响。快速问答、RAG 推理、通用智能体分别覆盖每个 family。数据集不包含唯一标准答案，`provenance.reference_answers` 为空；评分只依据可接受事实范围、禁止事实、状态生命周期、证据锚点、引用邻接、决策前置条件、工具和输出边界。

12 个 case 全部固定使用 DeepSeek V4 Flash 的运行时占位符和同一份已选采购制度，避免因模型或语料选择不同造成伪差异。状态 family 明确禁止发起制度检索，因此仍选择知识文件是为了保持真实端点配置并暴露不必要检索，而不是让空知识配置使该指标天然通过。每个 profile 的 provenance 都记录对应 discovery artifact 的 SHA-256，原始文件是否漂移可独立核验。

## 从真实分支到稳定契约的转换规则

1. 删除依赖某次错误回答才成立的追问。例如“你刚才写了 30 台”不进入固定 case，改写为“未提供数量，不得补写数量”。
2. 后续轮次只依赖用户已经明确声明的权威事实，不依赖助手是否正确理解或复述。
3. 不要求拟合一篇最优回答。一个事实可用多种自然措辞表达，确定性 scorer 只检查必要事实、禁止事实和证据关系。
4. 用户事实、制度事实、模型推断、当前事实、废弃事实、待确认事实和行动边界分别建模。
5. 关键制度主张不仅要求存在 citation，还要求该主张所在行邻接的 citation 映射到包含目标 anchor 的持久化证据。
6. 关键条件未知时，契约要求明确延期定案并禁止具体方式的提前推荐。
7. 所有 family 目前只进入 `dev`。本批原始语义及近义变体不得再进入 gate 或 sealed holdout；后两者必须由新的 family 和新的事实组合构造。

## 契约矩阵

| family | case 数 | 每个智能体轮数 | 主要来源问题 | 硬契约 |
|---|---:|---:|---|---|
| `citation-claim-binding-auction-definition` | 3 | 1 | 快速问答第 8/10 轮无引用宣称找到；通用智能体无效标签 | 第三十六条精确 anchor、正文与持久化引用一致、定义句邻接正确证据、禁止伪原文 |
| `active-retired-state-window-plus-two` | 3 | 12 | 三者当前/废弃/推断混淆；RAG 窗口外目标丢失；通用长提示状态失焦 | 当前事实、废弃事实、待确认事实、来源归属、身份隔离、行动边界、禁止检索与定案 |
| `current-turn-alignment-topic-detour` | 3 | 5 | 通用智能体第 6/8 轮滞后一轮；旁支后无法回主任务 | 当前问题必需事实、上一任务禁止词、旁支证据邻接、停止检索和停止旧话题 |
| `decision-under-unknowns-procurement-path` | 3 | 1 | 三者在公开性、需求完整性、时间可行性未知时过早推荐 | 三项未知条件、延期定案语句、具体方式推荐禁区、公开采购与竞价证据 anchor |

## 多轮状态 case 的权威事实时间线

`active-retired-state-window-plus-two` 对三个智能体使用完全相同的 12 轮用户事实：

1. 项目名称、最早业务目标和永久行动边界；
2. 初始预算及构成；
3. 当前预算替换并废弃旧值；
4. 初始日期；
5. 当前日期替换并废弃旧值；
6. 供应商数量、方案差异和需求完整性未知；
7. A 的排他主张仅为待核验；
8. 核验后废弃 A 排他主张；
9. 标的类别和采购信息公开性未知；
10. 法务确认不涉密、不应急、无不可替代专利；
11. 项目负责人、事实确认来源和当前用户身份未知；
12. 不重述旧事实，要求四栏完整状态审计。

每一轮都是独立有效的真实业务输入，不包含“如果你上轮答错”一类分支前提。第 12 轮要求恢复最早事实，因此可以验证后续结构化状态接力，而不是只验证当前轮显式重述。

## 实际历史窗口口径

- 快速问答配置 5 轮，并按完整问答对裁剪；第 12 轮远超原始窗口。
- RAG 推理配置 10 轮，并按完整问答对裁剪；第 12 轮的 `turn-001` 不在原始窗口。
- 通用智能体配置为 10 轮，但当前实现读取 `turns*2+4` 条消息，过滤当前轮后没有再次裁剪完整问答对。按本次有效数据库记录，第 12 轮实际可带入 11 个历史问答对，`turn-001` 仍在。

因此，通用智能体原第 12 轮失败应归类为“配置与实际历史不一致 + 长提示注意力/状态提取失败”，不能单独作为严格窗口外失忆证据。新 case 保留这一现状作为基线问题，但在结果报告中必须同时记录“configured history”和“actual rendered history”，不得只依据 profile 配置推断可见范围。

## 新增一等契约字段

`TurnContract` 新增以下离线字段：

- `conversation_state.active_facts`
- `conversation_state.retired_facts`
- `conversation_state.unknown_facts`
- `conversation_state.forbidden_inferences`
- `conversation_state.action_boundaries`
- `conversation_state.require_scoped_sections`：需要状态审计时按常见 Markdown 标题或表格分栏评分，防止事实放错生命周期仍通过
- `decision.mode / required_unknowns / required_defer_claims / forbidden_recommendations`
- `evidence_claims`：把一个可接受主张绑定到允许的 evidence anchor，并检查邻接引用
- `max_tool_calls`
- `TextRule.unless_any_of`：为“暂不建议采用”等显式否定提供排除项，避免把延期决策误判为正向推荐

这些字段只由 eval runner 解析和评分，不进入 WeKnora 生产请求链路。

## 当前仍不能由本数据集证明的事项

1. **不能计算生产正确率。** 当前数据是定向压力测试，不是随机生产样本。
2. **不能把零持久化引用直接判为召回失败。** 目前正式 runner 仍缺少 eval-only 的完整候选 chunk 快照，暂时只能评价端到端证据成功和主张绑定。
3. **不能制定最终延迟门槛。** 当前缺少 token/block 级输入规模观测，工具调用上限先作为 dev 诊断约束，延迟继续记录但不在本批新增硬阈值。
4. **不能将本批语义复制到 gate/holdout。** 同一事实组合、同一条款、同义改写和跨智能体变体共享 family，只能在 dev 使用。

## 晋级前检查表

- 使用全新事实组合和全新 family 构建 gate、sealed holdout 和 grader calibration。
- 为 eval 模式补 actual rendered history、各 prompt block 字符/token 数、检索候选与最终引用的分层记录。
- 由 Codex 对 dev 首轮结果逐 case 复核硬规则误报和漏报；只调整契约表达，不根据某篇具体回答拟合措辞。
- 为每个硬规则准备正例、反例和边界例；LLM judge 只能补充语义解释，不能覆盖硬失败。
- gate family 固定后冻结 JSONL 与 SHA-256；任何契约改动都产生新版本。
