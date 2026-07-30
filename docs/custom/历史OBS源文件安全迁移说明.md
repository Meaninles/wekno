# 历史 OBS 源文件安全迁移说明

## 适用范围

`legacy-source-migrate` 用于修复已完成、非手工创建文档的历史 OBS
源文件引用。目标是让 `knowledges.file_path` 位于当前部署私有知识对象前缀，
并为每份源文件建立完整的 `knowledge:aux_object` 持久归属账本。

该命令仅处理以下记录：

- `knowledges.deleted_at IS NULL`；
- `parse_status = completed`；
- `type <> manual`；
- `file_path` 使用 `obs://`；
- 当前没有 `source_file` 或 `clone_source_file` 的有效归属账本。

迁移只支持当前进程的全局 OBS 配置。它不会猜测历史凭证，也不会跨桶复制。

## 安全边界

每份文档单独执行以下流程：

1. 从历史 `obs://` 路径流式读取到单个临时文件；
2. 同时核对数据库中的 `file_size` 和 MD5 `file_hash`；
3. 对不在规范归属路径中的对象，按“租户 / 文档 / 确定性对象名”写入当前部署前缀；
4. 目标写入 SHA-256 元数据，并用对象 HEAD 结果复核大小与 SHA-256；
5. 按“知识库锁 → 文档行锁”顺序重新核对旧路径和 processing generation；
6. 在同一数据库事务中写入源文件归属账本并更新 `file_path`；
7. 事务提交后再次核对文档行、generation、账本和存储绑定。

若历史路径已经位于当前前缀且满足精确的租户/文档归属结构，命令只校验
源文件并补账本，不复制对象。

下列任一情况都会使该文档失败且不切换引用：

- OBS 对象不存在或无法读取；
- 对象大小与数据库不一致；
- 对象 MD5 与数据库不一致；
- 路径不属于配置的 OBS 桶；
- 目标对象大小或 SHA-256 校验失败；
- 文档在上传期间被重新解析、移动或删除；
- 已存在另一份源文件账本或绑定不一致。

旧 OBS 对象始终保留。上传成功但数据库竞争失败时，确定性目标对象也保留，
后续重跑会先校验并复用它。

## 执行

生产 Job 模板为
`deploy/production/legacy-source-migration-job.yaml.tmpl`。必须先运行 `audit`，
确认报告中的失败数为 0，再运行 `apply`：

```bash
./WeKnora legacy-source-migrate audit
./WeKnora legacy-source-migrate apply --confirm-old-objects-retained
```

该迁移采用逐行锁和乐观 generation/path 栅栏，不要求停止应用副本。整个
apply 期间使用独立 PostgreSQL advisory lock，禁止两个迁移实例并行。

## 验收

apply 报告必须满足：

- `failed_documents = 0`；
- `remaining_candidates = 0`；
- `source_objects_retained = true`；
- `source_verified = candidate_documents`；
- `ledgers_created = candidate_documents`；
- `paths_switched + ledger_only_candidates = candidate_documents`。

随后至少按两个维度分层抽样：

- 文档类型：PDF、DOCX、DOC、XLSX、PPTX，以及图片/文本类；
- 知识库：文档量最大的知识库、不同租户知识库、问题用户所在知识库。

每个样本应核对数据库路径与账本绑定、OBS 对象大小/哈希，并通过应用签名
下载链路实际读取全部字节。旧路径删除不属于本迁移范围。
