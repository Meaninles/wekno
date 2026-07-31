# Excel 解析分片与富化可靠性修复说明

## 1. 结论

本次问题不是单一的“文件太大”或“模型网关 403”，而是四条链路叠加：

1. Excel 空白样式把工作表物理使用范围扩展到 `XFB`，旧文件检查把样式尾部当成真实列宽。
2. 旧分片器把列违规得到的 `minimum_parts` 再用于压缩行窗口，形成行数乘列数的 part 膨胀。
3. Excel 无缓存公式在 `data_only=True` 读取后变成 `None`；即使修复了解析器，旧共享解析缓存仍会返回修复前的缺失结果。
4. 问题生成、Wiki 等派生任务的失败曾与正文解析状态耦合；同时旧的 provider-call 唯一索引和响应契约解析缺陷会阻断有效重试。

修复目标不是放大 `max_parts=10000`，而是：只按语义有效范围分片、保证每个语义单元格可重建、让缓存随解析语义版本失效，并把可用正文与派生处理状态彻底分离。

## 2. 已确认的根因

### 2.1 空白样式尾部导致伪超宽表

生产样本 `20231005-02.xlsx` 只有约 170 KB、约 92 行、1,344 个非空单元格，真实内容最远到 `BC`（第 55 列），但工作表 XML 中第 6 行记录到 `XFB`（第 16,382 列）。第 100 列以后有 16,284 个只带样式、值为空的单元格。

旧算法按 75 列安全窗口计算：

```text
ceil(16382 / 75) = 219 个列窗口
ceil(92 / 219) = 1 行/窗口
219 × 49 + 39 = 10,770 parts
```

因此它触发的是物理 part 数上限，而不是字节大小、LLM、全局锁或富化任务。

### 2.2 行列联动造成乘法放大

列数违规计算出的 `minimum_parts=219` 不应再次压缩行窗口。旧算法这样做后，每个真实行都会与全部列窗口组合，再叠加关键列重复，导致一个 92 行的小表生成一万多个文件。

对照样本“数据资源（物理）信息采集清单-NC系统”虽然有 16.24 MB、约 47 万行，但最多只有 11 列，是长窄表，只需 25 个物理 part，因此能够正常处理。

### 2.3 无缓存公式被静默丢弃

DocReader 为填充合并单元格使用 `openpyxl.load_workbook(..., data_only=True)`。对没有缓存计算值的公式，openpyxl 返回 `None`；后续 pandas 看不到该单元格，正文和 chunk 中也不会出现公式。

真实评分表中有 15 个这类公式，例如：

```text
=IF(COUNTA(F18:F22)=0,"",SUM(F18:F22))
=IF(C10="","",C10/B10)
```

### 2.4 共享解析缓存跨版本复用旧错误结果

修复 DocReader 后首次手动重建仍缺 15 个公式。parse-worker 日志给出了确定证据：

```text
[convert] shared parse cache hit
```

原缓存版本仍为 `document-parser-v3-vector-images`，文件哈希不变时重建会复用修复前的解析结果。必须把“可搜索语义投影版本”纳入缓存键；仅重启 DocReader 不够。

### 2.5 派生任务重试被旧唯一索引阻断

数据库同时存在：

- 正确约束：`(work_item_id, request_hash, attempt)`
- 旧 GORM 索引：`(work_item_id, request_hash)`

第二次 provider 尝试因此报 `duplicate key value violates unique constraint`，不是模型已经成功，也不是等待全局锁。旧索引实际阻止了 N+1 次尝试落账。

### 2.6 问题响应契约解析错误

模型常返回合法的裸 JSON 数组。旧解析器先截取第一个 `{` 到最后一个 `}`，会把数组外壳删除，随后误报 `invalid character ',' after top-level value`。另外，要求模型回传 36 字符 chunk UUID 容易被模型改写，导致响应无法与输入稳定对应。

当前日志也确认存在真正的模型空响应：同一批次连续 4 次均为 `content=""`、`completion_tokens=1`、`finish_reason=end_turn`。这不是排队拿锁；它应在有限重试后把富化标为 `degraded`，但正文仍为 `completed/ready`。

## 3. 已落地的修改

### 3.1 语义边界

- Go 文件检查和 Python 分片器统一使用语义边界。
- 只把非空值、公式和有意义的合并区域计入边界。
- 不再使用只带样式的尾部单元格扩大有效行列数。
- 扫描已存储 cell，避免因为 `XFB` 样式尾部反向创建 16K 列对象。

涉及：

- `internal/custom/modules/fileguard/validator.go`
- `custom/services/document-splitter/weknora_document_splitter/xlsx_semantic.py`

### 3.2 二维精确分片计划

- 行窗口和列窗口独立计算，不再让列违规数量压缩行窗口。
- 在单 part 行列和字节安全约束下选择二维窗口组合。
- 保留必要关键列，但不让关键列重复参与 part 数反推。
- 生成前做精确 preflight；超过硬限制时返回不可重试错误，不创建半套 plan/part/chunk。
- `retryable=false` 通过类型化远程错误向外围传播，不再作为普通字符串丢失。

涉及：

- `custom/services/document-splitter/weknora_document_splitter/service.py`
- `internal/custom/modules/documentsplit/errors.go`
- `internal/infrastructure/docparser/grpc_parser.go`

### 3.3 公式和合并单元格保真

- 同时读取值工作簿与公式工作簿。
- 有缓存值时保留缓存值，保证用户看到 Excel 的计算结果。
- 无缓存值时把公式表达式作为可搜索源文本写入预处理工作簿。
- 显式把回退公式设为字符串，避免 pandas 再以 `data_only=True` 打开时二次丢失。
- `DISPIMG`/`IMAGE` 等图片函数仍按原规则过滤，不把图片公式误当正文。

涉及：

- `docreader/parser/xlsx_merge.py`
- `docreader/tests/test_excel_parser.py`

### 3.4 解析缓存语义版本

- 缓存版本升级为 `document-parser-v4-xlsx-formula-fallback`。
- 文件哈希不变的手动重建也不会命中 v3 旧解析结果。
- 缓存仍可在相同语义版本内复用，不需要粗暴关闭共享缓存。

涉及：

- `internal/application/service/knowledge_process.go`
- `internal/application/service/knowledge_parse_cache_test.go`

以后任何会改变正文语义投影的 DocReader 修改都必须同步提升该版本；性能优化但不改变输出时无需提升。

### 3.5 派生任务可靠性和状态分离

- summary、question batch、finalizer 使用持久化 work item 和 provider-call 尝试账本。
- provider 成功响应先 checkpoint，再物化，避免工作进程退出后重复付费调用。
- admission/全局锁等待属于排队，可重新调度；不会消耗为真正 provider 失败准备的次数。
- 合同错误、真实空响应和基础设施错误有独立错误类别。
- 删除旧二列唯一索引，保留三列 `(work_item_id, request_hash, attempt)` 唯一约束。
- 问题解析接受规范对象、裸数组、Markdown fence 和可证明安全的截断恢复。
- 使用 `r_` 加 16 个十六进制字符的稳定请求 ID，在当前 batch 内做碰撞检查，不要求模型复述 UUID。
- 正文可用状态为 `core_status=ready`；派生任务最终失败显示 `enrichment_status=degraded`，不再把整篇文档显示为解析失败。

涉及：

- `internal/custom/modules/derivativequeue/`
- `internal/custom/modules/questioncontract/`
- `migrations/custom/derivativequeue/900002_provider_call_contract_attempts.up.sql`
- `frontend/src/custom/modules/knowledgeWorkflowStatus/`

### 3.6 生产运行角色隔离

- API、parse-worker、derivative-worker、wiki-worker、maintenance、migration 使用独立运行角色。
- parse 全局锁和模型 admission 仍保留，但等待与失败分开记录。
- Helm 对副本数、连接池预算和非法角色配置做渲染期校验，避免扩容后挤爆 PostgreSQL 连接。

涉及：

- `helm/templates/_app-runtime.tpl`
- `helm/templates/app-runtime-roles.yaml`
- `helm/values-production-ha.yaml`
- `internal/custom/modules/runtimeprofile/`

## 4. 无损验收结果

### 4.1 分片前后逐语义单元格重建

对三个差异很大的真实 Excel 重新执行逐 part 投影并逐单元格对比：

| 样本 | 结构特征 |
|---|---|
| NC 系统清单 | 16.24 MB，约 47 万行，长窄表 |
| 项目服务清单 | 1,793 个公式，1,051 个合并区域 |
| 云数网安评分表 | 15 个无缓存公式 |

汇总结果：

- 3/3 通过
- 28 个物理 part
- 475,281 行完成重建比较
- 2,045,261 个非空语义单元格完成比较
- 1,808 个公式
- 15 个无缓存公式
- 0 个语义不一致
- 66 个浮点单元格只有 Excel 序列化末位差异，最大相对误差 `3.98088997406327e-16`

另对 Downloads 下 68 个本地 XLSX 做过批量回归：68/68 通过，共 224 个 part、9,033 行、85,396 个非空单元格、4,045 个公式；无语义不一致。

### 4.2 生产失败样本 dry-run

- `20231005-02.xlsx`：旧算法 10,770 parts，新算法 5 parts。
- 另一个旧算法 17,308 parts 的样本：新算法 2 parts。
- 其余两个生产失败 Excel：分别为 1 part、1 part。

part 数下降来自忽略纯样式尾部和纠正二维规划，不是删除真实内容。

### 4.3 本地真实 API 重建

在隔离知识库中使用真实上传、对象存储、DocReader、parse-worker、embedding 和派生队列链路重建。缓存版本提升后：

- 项目服务清单：2,773/2,773 个可审计语义单元格覆盖，缺失 0。
- 云数网安评分表：162/162 个可审计语义单元格覆盖，缺失 0；15 个无缓存公式全部可检索。

父子分块可能把一个多行单元格切到相邻 child window。真实检索会由 child 回扩到 `parent_text`，因此完整性审计同时检查 `text + parent_text`；问题生成数量仍只按 child text 统计。

## 5. 上线顺序

1. 备份并执行 derivativequeue migration，确认旧二列唯一索引已删除、三列唯一约束存在。
2. 先部署 migration 角色，再部署 DocReader 和 parse-worker。
3. 部署 derivative/wiki worker，最后部署 API 和前端。
4. 校验运行角色、副本数和数据库连接预算。
5. 选择生产失败样本手动重建；缓存 v4 会自动绕开旧结果，无需删除全部缓存。
6. 观察 split plan/part、parse cache hit version、provider disposition、enrichment degraded 和 Wiki terminal 指标。

## 6. 不采用的方案

- 不提高 `max_parts=10000` 来掩盖问题：会让 92 行表产生一万多个无意义文件。
- 不按文件字节大小判断复杂度：长窄大表与稀疏超宽小表的物理 part 数完全不同。
- 不无限重试模型：真实空响应、合同错误必须有限重试并可观测；无限重试会永久占用队列和模型容量。
- 不把富化失败写回正文解析失败：正文、摘要/问题、Wiki 各自有独立终态。
- 不关闭整个解析缓存：用语义版本精确失效旧结果，保留正确缓存带来的吞吐收益。
