# 生产派生三智能体多轮 Eval 准备说明

这套数据只用于隔离的 Eval worktree。生产环境仅执行只读查询和文档下载；不会创建会话、修改知识库、上传文件或发送评测请求。生产回答仅用于理解真实交互方式，不作为标准答案。

## 数据分层

| 层级 | 用途 | 三智能体场景 | 是否参与日常调优 |
|---|---|---|---|
| DEV | 发现问题、单变量实验、形成 champion | 无知识库业务台账；建设阶段制度纠错；WeKnora Skill 使用与行动边界 | 是 |
| GATE | 验证未参与 DEV 的业务族与知识库 | 智慧茅台跨年度报告；专利与技术文档证据校准；茅台云多来源综合 | 仅晋级时 |
| SEALED | 最终泛化检查 | 系统建设验收考核；灾备决策台账；供应商评审筹备 | 否，低频显式授权 |

DEV、GATE、SEALED 的 `family_id` 和知识库来源均不交叉。系统建设制度知识库只进入 SEALED；密封数据内容保存在 Git 忽略目录，Git 仅保存数据哈希、数量和依赖哈希。

这里的 DEV/GATE/SEALED 分别对应“调优反馈集/验证集/最终测试集”。它们不是用于训练模型权重的传统 train/validation/test；目的是避免同一问题族、同一文档或同一措辞同时驱动修改和证明修改有效。

## 知识库规则

本地创建六个物理隔离知识库：系统建设制度、数字化制度、智慧茅台、知识产权证据、茅台云、WeKnora 使用指南。前五组来自生产只读下载的 26 个真实文档；WeKnora 指南来自当前 worktree 的 8 个项目文档。所有文档均校验 SHA-256，知识库沿用生产的 512/80 分块、父子块和向量加关键词索引配置。

每个正式 RAG case 只能满足以下两种设置之一：

- `knowledge_base_ids` 恰好一个，`knowledge_ids=[]`；
- 两者都为空，表示不选择知识库。

不允许选择多个知识库，也不允许直接选择单个或多个文档。知识库绑定的稳定哈希与完整语料版本写入每次 run 的 `execution_contract`，防止同名数据悄悄变化。

## 固定用例与自适应发现

生产多轮对话经 Codex 逐轮审核后才进入正式集。固定用例必须分支安全：后续问题只依赖用户已经给出的事实和明确知识范围，不依赖上一轮助手是否列对、排序是否稳定或结论是否正确。每一轮都有可执行的状态、证据、引用、工具只读或行动边界契约，但没有唯一参考回答。

真正的问题发现使用 `discovery/scenarios/production-derived-multiturn-problem-finding.v1.json`。场景对每个智能体只保存一个起始问题；Codex审阅上一轮实际回答后，才能创建一个指向该轮的 continuation。框架拒绝一次排队多个固定后续问题，失败请求会隔离并回滚请求对，避免污染后续历史。

## 门禁与停止条件

每个正式 case 独立重复三次：

1. experiment：完整 DEV 相比当前 champion 至少改善一个 case，任何 case、保护指标或延迟不得回退。
2. optimization：DEV 每个 case 必须 3/3 通过，才允许进入 GATE。
3. release：GATE 每个 case 至少 2/3，通过率、关键确定性指标和延迟均不得回退。
4. sealed：仅在低频里程碑显式执行，内容不得用于定位和继续调参。

连续两轮没有 case 级实质增益、只改善措辞、Judge 与确定性指标分歧上升、GATE/SEALED 退化或必须修改测试来让候选通过时，停止当前方向并回滚。这样不要求拟合一份“完全最优回答”，而是要求在多种可接受回答下满足事实、证据、状态和安全边界。

## 一键准备（不发起对话）

```powershell
# 默认准备 GATE/release；会幂等核对知识库后执行只读 preflight
custom/services/agent-eval/prepare-production-multiturn-eval.ps1

# DEV 优化门禁准备
custom/services/agent-eval/prepare-production-multiturn-eval.ps1 -Stage optimization

# SEALED 只在明确需要时准备；仍不会执行对话
custom/services/agent-eval/prepare-production-multiturn-eval.ps1 -Stage sealed
```

成功输出必须同时包含 `READY`、`formal_eval_executed=false` 和 `PRODUCTION_MULTITURN_EVAL_READY`。预检只校验环境、数据、依赖、模型、知识库绑定和门禁契约，不创建 session、不发送 chat 请求、不发布 Langfuse experiment。

下一步开始正式基线时，先运行 DEV 全量三次并让 Codex审核，不能直接拿生产回答或单次运行作为 baseline。
