# 二开测试索引

二开验收强调 API、浏览器、持久层和真实结果的交叉验证。脚本返回 `passed` 只是
证据之一，不能代替 PostgreSQL、向量、Neo4j、Wiki、对象存储和前端状态实查。

| 测试 | 目录/报告 | 重点 |
|---|---|---|
| 文档处理集群 | [`document_processing_cluster_e2e/`](./document_processing_cluster_e2e/README.md) | 多实例、完整衍生、排队、竞态、恢复、删除、召回 |
| 知识库文件夹 | [`knowledge_folders_e2e/`](./knowledge_folders_e2e/README.md) | 分页、搜索、移动、并发和权限 |
| 模型容量 | `model_capacity_reports/` | DeepSeek V4 Flash 分档并发与 P95 |
| 生产最终验收 | [`final_acceptance_report.json`](./document_processing_cluster_e2e/final_acceptance_outputs/20260726-0107/final_acceptance_report.json) | 当前综合通过证据 |

## 成功判据

- 每份文档启用的主解析、向量、多模态、摘要、问题、图谱和 Wiki 全部成功。
- 队列/实例故障测试验证没有任务丢失、旧 epoch 越权写入或重复业务结果。
- 同用户/多用户、同知识库/多知识库并发均覆盖。
- 解析中删除、已完成删除和重建清理所有关系库、图谱、Wiki、对象和在途任务。
- app、DocReader、Agent、frontend、mobile-web 的多副本确实被请求命中。
- 召回测试只验证检索与来源，不通过修改 Agent 提示词掩盖召回问题。
- 资源使用受限，测试过程中持续检查容器 restart/OOM/磁盘和队列。

保留的“公司制度”知识库是后续真实使用资产，不应在重复测试中删除或无理由重建。
