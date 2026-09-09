# 智能体两阶段引用

主工作树的知识问答、通用智能体共用此链路。实现集中于运行时 `app/citations.py` 和 Go `sourcerefs/structured.go`，不增加业务工具、不按问题关键词判断。

## 固定正文，再补引用

1. 智能体按原流程检索、读取证据，最终调用 `GenerateStructuredOutput`，仅提交 `{"answer":"完整连贯的 Markdown 正文"}`。不拆段生成，不提交引用字段，不自行嵌入引用协议标签。
2. 代码对原始正文生成 `A1…` 锚点，保留精确文本及 UTF-8 字节结束位置。重复段落具有不同位置；跳过标题、代码块；Markdown 表格的数据行在末列内部插入引用。
3. 有本轮有效证据时，同一配置的模型额外调用一次，仅允许 `SubmitCitations`，返回 `{"citations":[{"anchor_id":"A1","source_ids":["S1"]}]}`。它能看到固定正文、锚点和本轮证据正文，不能重写正文、调用业务工具或额外检索。
   提示词仅约束“固定正文、按语境核对证据、只选择已有 ID”，不包含业务关键词或特例。工具 schema 将锚点和来源 ID 限定为本次枚举；正文文本和字节位置由代码生成，不交给模型填写。
4. 代码验证 ID，将映射转换成原始文本和位置；Go 再验证来源和位置，从后向前插入 `<src id="S1" />`。前端沿用原来的引用编号、来源面板机制。正文只发布最终结果一次，工具过程仍正常显示。

第二次调用负责判断证据是否支持具体陈述，代码负责精确定位：可保证插入位置不受模型抄写正文或 Markdown 格式差异影响，但不能承诺大模型永远没有语义判断错误。没有证据支持的陈述不得硬凑引用；一个来源支持多个段落是合法的。

## 证据、安全及恢复

- 来源来自本轮 `RunRecord.References` 的有效证据，不从历史回答猜来源。历史追问仍先通过原有工具按需取回证据并注册为本轮来源。
- `runs/budget` 默认只给 `current_run_sources` 索引；显式 `include_evidence: true` 才返回 `citation_evidence`（ID、标题、证据正文），沿用内部认证、租户和运行所有权校验，不返回任意来源元数据。
- 第二次调用使用原有模型准入、并发与总预算账本，不开启新的智能体循环。独立限制最多 120 秒，并为正文提交留下时间；上下文装不下时明确失败，不截断证据后伪装完整。
- 无本轮证据时不增加模型调用。证据和回答均作为不可信数据传入引用模型。
- 正文完成后、引用完成后分别保存检查点；恢复复用 SDK 已提交正文，已完成的引用通过正文哈希复用，不重复生成正文。
- 引用格式非法、未知 ID、模型故障、超时或预算不足：保留正文，`citation_status=failed`、`failure_code=citation_generation_failed`，运行标记 incomplete。电脑和手机显示“引用补充失败，正文已保留”。取消或所有权丢失仍遵循原来的取消/恢复机制。
- `citation_status` 写入消息现有 `retrieval_stats` JSON，同时经完成事件传给前端，无新增数据库表或列迁移。

## 回归入口

- `custom/services/agent-runtime/tests/test_citations.py`：独立引用调用、重复中文/emoji 段落、表格和代码块、非法 ID、失败保留正文、检查点恢复和取消。
- `custom/services/agent-runtime/tests/test_answer.py`：第一阶段只接受正文契约，不为错误引用触发修复循环。
- `internal/custom/modules/sourcerefs/structured_test.go`：精确字节位置及有效来源过滤。
- `custom/tests/agent_runtime/citation_regression.py`：在指定真实本地会话追加问题，校验两阶段结果和正文一致性；`--inspect-only` 只复查已保存结果。
- `custom/tests/agent_runtime/citation_render_regression.mjs`：使用实际桌面/移动共用渲染器重放真实结果。

业务内容与运行报告只保存至 `.local-data/agent-runtime-validation/`，不提交凭据或业务内容。

## 本地回归记录（2026-09-09）

- 运行时全套 184 项测试通过；Go agentruntime、sourcerefs、session 包在 Linux 容器测试通过。PostgreSQL 隔离 schema 验证证据读取权限、提交幂等、引用失败及错误偏移时的正文保留。
- 前端类型检查及 18 项引用/会话展示相关测试通过；实际桌面和 `/mobile/` 页面验证列表、表格引用显示。
- 在原会话 `10f6b571-6e0f-4ffe-802d-2e5499ae3ac4` 追加真实问题：主数据流程 9 个引用标记，表格追问 8 个，依据开发总结报告创建智能体 31 个。对应报告名为 `citation-json-qa-two-pass-final-master`、`citation-json-qa-two-pass-final-table`、`citation-json-qa-two-pass-final-create-kb`。
- 三个最终用例均为 3 次实际模型请求（检索/读取决策、完整正文、引用映射），正文去掉生成标签后逐字等于第一次提交；耗时分别约 16.6、16.5、26.2 秒。
- 过程中发现重复问题直接复述历史答案的缺口，已在历史导航通用指令中要求先恢复原始证据，复测使用 `read_conversation` 正常补回引用。
- 不应将“没有引用”全部归因于锚点：限定错误知识库而检索为空，或走技能文件读取但未注册可引用来源时，第二阶段不会伪造引用。这些诊断结果也留在原会话中，未删除或改写旧消息。
