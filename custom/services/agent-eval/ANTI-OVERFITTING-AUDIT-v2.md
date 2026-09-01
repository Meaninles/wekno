# WeKnora 三智能体防过拟合整改审计（v3，文件名沿用历史入口）

更新时间：2026-09-01
适用分支：`codex/agent-eval-loop`
语义门禁：`codex-conversation-minimum-v1`

## 结论先行

新门禁不再把固定短语、固定栏目、required claims、参考答案或单项 Judge
分数当成生产质量结论。快速问答、RAG 推理和通用智能体的语义质量均由 Codex
逐个审核完整对话；表达、格式或局部完整性的小瑕疵可以标为 `minor_issue` 并
通过，只有影响核心任务的 `major_issue` 或 `critical finding` 才判失败。

确定性评分仍保留为诊断信号，但 v3 质量策略的
`critical_metric_prefixes=[]`、`max_metric_rate_regression={}` 且
`require_judge=false`，所以固定词命中与否不能决定发布。硬检查只覆盖不能靠主观
判断放宽的产物完整性和运行风险：数据/代码哈希、split 与智能体覆盖、独立重复、
SSE/数据库/历史答案一致、生产与 Eval 答案轨道隔离、延迟和修复成本。

当前正式 production release 仍需要 `gate + sealed_holdout`。仓库内没有可供日常
调优读取的 sealed holdout 正文，因此在低频 sealed 验收完成前，结论必须保持
`INVALID/NOT_READY`，不能用可见集替代或伪造通过。

## 一、现有修改分类

| 分类 | 处理结果 | 代表实现 |
|---|---|---|
| WeKnora 通用能力改进 | 保留，但要求跨领域反例与延迟验证 | 通用对话状态维度、较旧用户原文的有界归档、引用证据谱系、跨 chunk 深读、流式终态一致性 |
| Eval 专用诊断或补救 | 移出 SUT，只存在于外部 runner | 双答案轨道、有限补充检索/改写、修复 trace、Codex 审核导出与绑定、版本化 gate |
| 针对固定数据/短语/采购/Skill/答案形态的规则 | 从生产公共路径删除；旧数据和旧策略仅为历史复现资产 | 删除 runtime response contract、固定终态重写、采购/Skill 路由补丁、固定行动边界模板；旧 v1 gate/manifest 不覆盖 |
| 可能影响生产性能或语义的修改 | 明确列为发布风险，不因单测通过自动放行 | 每轮通用提示增加、最多 256 条的一次历史读取、grep 排序、精确 chunk 邻接读取、引用 registry 收窄 |

审计中发现并移除的生产内 Eval 行为包括：在 SUT 中接收或持久化 Eval
contract、按 required claims/Judge 反馈重写答案、因 Eval 失败补充检索、按采购或
Skill 场景短语改变路由/工具、把 completion 阶段过滤后的文本覆盖已流式返回答案。

## 二、双轨答案模型

每个完成轮次保存两份不可混淆的 snapshot：

- `production_candidate`：用户通过 SSE 看到、数据库持久化并由历史加载返回的
  同一份答案及其引用 registry。终态只做引用记账，不重写正文。
- `eval_assisted_answer`：仅外部 Eval runner 可以生成。最多一次补充检索、最多两次
  无工具改写；失败开放，永远不回写普通会话。

每轮同时记录：是否触发、触发原因、修复类型、尝试次数、失败原因、增加的模型/
工具调用与耗时。每个 case 同时保存 production/assisted 的确定性诊断分数和两份
独立 Codex 完整对话审核。Markdown 报告对有差异的轮次展开显示修复前后正文、
引用、分数和 repair trace；JSON artifact 保存所有轮次的完整字段。

Codex 审核包只包含用户原始轮次和选定的观测答案轨道，并绑定完整对话 SHA-256。
包中不包含 turn contract、required claims、reference answers、Judge rubric、Judge
反馈或另一条答案轨道。答案变化后旧审核会因 hash 不一致而失效。

## 三、v3 门禁设计

### Production release gate

策略：`policies/production-multiturn-release-gate.v3.json`

- 只选择 `production_candidate`，并要求每个完整对话有独立 Codex 审核。
- 每个 case 运行 3 次，至少 2 次达到“实质正确且有用”的最低标准；不要求
  措辞、格式或每次采样都完美。
- baseline 的 Codex 通过率变化继续报告，但随机的 attempt 对位和固定诊断指标
  不再成为语义否决项。
- repair-only 成功只报告，绝不换算成 production PASS。若 production 的 Codex
  通过率未达到阈值，assisted 通过也不能救活发布结论。
- `gate` 与 `sealed_holdout`、三智能体、重复执行、SSE/DB 一致、运行身份、延迟
  和修复依赖完整性仍 fail closed。

### Eval optimization gate

策略：`policies/eval-optimization-gate.v3.json`

- 只选择 `eval_assisted_answer`，同样由 Codex 逐个完整对话判断。
- 用来判断有限检索或改写能否恢复失败，不等同于生产质量。
- 报告全部修复成本和依赖；assisted PASS 不写回 production verdict。

### Repair dependency gate

策略：`policies/repair-dependency-gate.v2.json`

报告并比较：trigger rate、success rate、repair-only pass rate、average attempts、
added model/tool calls、added latency。该 gate 是运行成本与依赖风险检查，不替代
Codex 的语义审核。

## 四、状态型轮次的通用结构

生产提示只提供六个领域无关的语义维度：

1. active facts
2. retired facts
3. unknown/pending facts
4. source attribution
5. output scope
6. action boundaries

实现不预先把某个短语判为“状态轮”或“检索轮”，也不包含采购字段、培训说明、
固定动作三元组或固定答案栏目。模型只能依据当前用户原文、正常用户历史、有界的
较旧用户原文和真实当轮检索证据组织回答；早期助手推断不能升级为持久事实。

反例覆盖：分析为何不能执行不等于登记状态；“不要检索”不反转为检索请求；修改
方案内容与修改文件/外部系统分开；知识问答中出现“不安装”不自动禁用检索；中文
同义改写、口语、英文和不同词序均不触发硬编码分支。

## 五、未见分布与数据隔离

`unseen-capability-matrix.v1` 按能力维度构建，而不是复制旧采购问题后替换名词。
可见矩阵包含 7 个 case、每个 12 轮、每个 3 次独立 session，覆盖人事、产品
手册、IT 运维、制度政策、项目管理、客户运营和组合决策；三个智能体均包含有知识
库和无知识库场景，并覆盖状态更新、话题切换、旧事实废弃、未知状态、原话追问、
综合总结、中文改写、否定句和对抗反例。

四个 fixture 由 `prepare-unseen-capability-kbs.ps1` 幂等创建为互相独立的知识库；
脚本校验模型、文档数、文件大小、解析状态和绑定哈希，并只把 ID 写入本地忽略的
`runner.env`，不把固定 ID 写进运行时代码。

DEV、GATE、sealed holdout 与 grader calibration 按 family 隔离。调优期间不得
读取 sealed 正文、具体答案或失败详情；当前整改没有读取 sealed 内容。正式发布
必须等低频 sealed 执行完成，不能把 DEV/GATE 结果外推为 sealed PASS。

## 六、生产隔离与答案一致性

生产模式的 Eval 能力只有失败开放的观测：

- 默认 metadata-only，采样率默认 1% 且受生产上限约束；
- OpenTelemetry `BatchSpanProcessor` 使用有界队列异步导出；队列/导出失败不改变
  业务回答、状态码或数据库结果；
- 不增加模型调用、检索、同步数据库写入或阻塞式修复；
- 不持久化 Eval contract、评分、修复或内部验证信息；
- Eval full-content 只有 `mode=eval + capture=full` 时才开启。

production candidate 是前端对完整 SSE 的最终公开投影：工具调用会废弃临时 answer
segment，`complete.data.final_answer` 与正式前端一样作为最终可见正文；缺少 complete
时才从仍有效的 answer segment 重建。停止请求仍持久化已经发送的精确字节。Eval
runner 按相同公开投影重放 SSE，再与历史 API 读取的助手正文逐字比较；正式 v3 gate
对不相等或无法证明相等的完成轮次判 `INVALID`。

2026-09-01 的追加审计发现，历史数据把空 `knowledge_base_ids` 标注为 no-KB，
但三个内置智能体均配置为 `kb_selection_mode=all`；后端在没有显式目标时会回退到
租户全部知识库。因此旧运行仍可复现，但不再被当作无知识库覆盖证据。新框架加入
显式 `knowledge_selection_mode`：`explicit` 必须携带 KB/文档目标，`none` 禁止目标，
且 runner 会在创建对话前从 API 校验实际 agent 必须为 `kb_selection_mode=none`、
绑定 KB 为空。隔离 Eval 租户中的三个 clone 复制对应内置 agent 的完整配置，只改
这两个知识选择字段；生产内置配置、普通会话和 main 基线均不修改。

新增 `fresh-generalization-regression.v1` 作为提示词修改后的独立回归：一个全新
Northbank 音频交付知识库用例，加上快速问答、RAG 推理、通用智能体各一个真实
no-KB 用例；四个 case 均为 13 轮、3 次独立运行，覆盖社区、研究与展览等新领域。
它不含参考答案、required claims、Judge rubric 或可注入 SUT 的评分反馈，语义结论
仍由 Codex 对完整对话逐一判断。

## 七、验收证据与剩余风险

自动化测试覆盖以下关键性质：

- production FAIL + assisted PASS 时，production gate 仍按 production Codex
  结论失败；repair-only 只报告、不计为 production PASS。
- 固定短语诊断失败但 Codex 判断完整对话实质合格且仅有 minor issue 时，v3 gate
  可以通过。
- 缺失、过期、轨道错误或维度不完整的 Codex 审核 fail closed。
- 双轨正文、引用、分数、修复原因/次数/成本可同时在产物与报告中查看。
- completion、停止、fallback、引用异常场景不再造成 SSE 与历史正文分叉。
- 跨领域、英文、不同词序和否定反例不触发生产短语分类器。

仍需在每个候选提交上重新证明而不能由代码静态宣称的项目：真实模型的三智能体
3 次稳定性、四个新知识库的召回质量、Linux 容器集成测试、相对 baseline 的 P95
延迟和 sealed holdout。任一项缺失时，最终结论必须写为“不适合进入生产”，不能
通过改 scorer、阈值、参考答案或移除失败指标掩盖。

## 八、2026-09-01 正式可见矩阵实测

本次候选运行 ID 为 `run-03f45b16-2838-47cf-89ea-79b40d1060f0`，对应产物：

- `artifacts/run-20260901-032346-reviewed.json`
- `artifacts/codex-review-20260901-032346-production.completed.json`
- `artifacts/codex-review-20260901-032346-assisted.completed.json`
- `artifacts/gate-20260901-032346-production-v3.json`
- `artifacts/gate-20260901-032346-eval-optimization-v3.json`
- `artifacts/gate-20260901-032346-repair-dependency-v2.json`
- `artifacts/report-20260901-032346-final.md`

Codex 按完整 12 轮对话逐个审核 21 个 production conversation 和 21 个 assisted
conversation。小的措辞、格式和完整性问题允许以 `minor_issue` 通过；错误事实、
旧状态复活、错误来源、行动越界和未完成核心请求才作为 `major_issue`。结果为：

| 轨道 | PASS | FAIL | 说明 |
|---|---:|---:|---|
| production candidate | 4 | 17 | 生产真实可见答案 |
| eval assisted | 5 | 16 | 仅 Eval 外部有限修复 |

唯一 repair-only PASS 是 `unseen-dev-quick-hr` 第 2 次运行：生产第 4 轮发生大段
重复退化，Codex 判 production FAIL；Eval 改写去除退化后判 assisted PASS。报告将
该项明确标为 `Repair-only pass=yes`，production verdict 仍为 FAIL。这一真实样本和
`test_assisted_codex_pass_cannot_rescue_production_codex_failure` 回归测试共同证明：
修复答案不能挽救生产门禁。

### 三类门禁结果

| 门禁 | 结果 | 独立失败原因 |
|---|---|---|
| production release v3 | `INVALID/NOT_READY` | 缺少 sealed holdout 和冻结 baseline；4 个可见 gate case 的 production 重复通过率分别为 0/3、1/3、1/3、1/3，均低于 2/3；general-agent gate P95 169,212ms、最大 240,054ms，超过 120,000/180,000ms 上限 |
| eval optimization v3 | `INVALID/NOT_READY` | 缺少冻结 baseline；assisted 轨中 quick HR 为 2/3，RAG product 与 general project 均为 0/3，重复稳定性失败 |
| repair dependency v2 | `INVALID/NOT_READY` | 缺少用于判断“不得恶化”的冻结 baseline；当前依赖数据完整报告且不被当作生产质量 |

三个 gate 都成功校验 42 份审核的对话哈希、答案轨道和六个维度；production gate
的 `answer_track_contract` 明确为 `production_candidate`，并且
`eval_assistance_cannot_rescue_release` 为 PASS、`counted_as_production_pass=false`。
门禁没有修改参考答案、required claims、Judge 或 scorer，也没有降低 2/3 重复标准、
延迟上限或 sealed/baseline 要求。

### 修复依赖与答案表面

全 21 个 case、252 轮范围内：

- repair trigger rate：9/252（3.6%）
- repair success rate：9/9（100%）
- repair-only pass rate：1/21（4.8%）
- average repair attempts：1.00
- added model calls：9
- added tool calls：0
- added latency：16,056ms（每次平均 1,784ms）

全部 242 个完成轮次均证明 `complete.data.final_answer` 的公开 SSE 投影与持久化/
历史加载一致。其余 10 轮均来自同一个 general-agent session：第 3 轮
`sut_response_deadline_exceeded`，后续 9 轮被明确标为 skipped；框架没有把缺失答案
伪装成表面一致。

### 修改前后客观对比

对比前一轮 `run-20260901-013110.json` 与当前 v3 运行：

| 指标 | 前一轮 | 当前 | 变化 |
|---|---:|---:|---:|
| 不完整/错误轮次 | 55 | 10 | -45 |
| 无法证明 production surface 一致的轮次 | 107 | 10 | -97 |
| quick IT 完成轮次 | 0/36 | 36/36 | +36 |
| general project 完成轮次 | 28/36 | 36/36 | +8 |
| repair trigger / success | 10 / 9 | 9 / 9 | 触发减少且全部成功 |
| repair attempts / model calls | 11 / 11 | 9 / 9 | 各减少 2 |
| repair added latency | 28,093ms | 16,056ms | -12,037ms |

前后 `surface` 统计不是严格同口径：旧 runner 没有按正式前端的
`complete.data.final_answer` 投影公开终态，当前 v3 已修正。因此该表只证明框架和
传输故障减少，不能冒充语义质量的配对 baseline。旧运行也没有完成同一套 Codex
整段审核，所以这里不声称 production 语义通过率相对旧版本提升。

### 剩余失败与生产结论

人工整段审核识别出的主要剩余失败是：

1. 把知识问答或概念示例升级成已经发生的业务事实，例如“询问校准方法”变成
   “校准已完成”、补偿券语义分析变成客户提案。
2. 为满足模板完整性而补写用户没有提供的负责人、申请人、复核日期、网络事件或
   恢复条件。
3. 最终摘要事实值正确但来源轮次错标，或把手册限定场景扩张成更广的业务规则。
4. 明确“不要检索”时仍调用检索工具，以及“只列范围”时输出跨话题内容。
5. 通用智能体仍有一次 240 秒截止超时，且整体 gate 延迟超过发布上限。
6. 多轮暴露内部分析式独白，虽有些对话仍可按 `minor_issue` 通过，但生产表达需继续
   收敛。

运行时代码静态扫描未发现 `case_id`、可见 case 名称、固定知识库/文档名、参考答案、
required claims 或 Judge feedback 分支。当前失败来自模型通用状态与证据行为，不能
再用场景短语补丁处理。

**最终结论：本候选不适合进入生产。** 原因不是固定短语评分器，而是 Codex 完整
对话审核下的生产重复稳定性不足、通用智能体超时/延迟超限，以及 sealed holdout
和冻结 baseline 尚未完成。后续应针对“助理推断不得升级为事实、精确来源归属、
跨话题隔离和通用智能体时延”做领域无关改进，再用新的不可覆盖产物重跑；不得围绕
上述可见 case 增加专用短语或固定答案分支。
