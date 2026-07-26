# 项目文档入口

本目录同时包含上游 WeKnora 通用说明和当前企业二开文档。由于当前实现已经加入
文档级水平扩展、无 RWX 对象存储、Agent 多副本、完整工作流状态和知识库大数据量
交互，阅读时以[当前实现架构与文档索引](./custom/当前实现架构与文档索引.md)为
总入口。

## 按角色阅读

| 角色 | 首要文档 | 后续文档 |
|---|---|---|
| 生产运维 | [当前版本生产更新部署执行手册](./custom/当前版本生产更新部署执行手册.md) | [生产无 RWX 方案](./custom/生产集群无RWX最优部署方案.md)、[Helm](../helm/README.md) |
| 后端/架构 | [当前实现架构](./custom/当前实现架构与文档索引.md) | [文档水平扩展](./custom/文档解析水平扩展与故障恢复.md)、[二开规范](./custom/二开目录结构规范.md) |
| 前端 | [用户使用指南](./custom/使用指南/用户使用指南.md) | [知识图谱](./KnowledgeGraph.md)、[API](./api/README.md) |
| 智能体开发 | [智能体开发指南](./custom/使用指南/智能体开发指南.md) | [通用运行时](./custom/通用智能体方案.md) |
| 测试/验收 | [多实例文档验收](../custom/tests/document_processing_cluster_e2e/README.md) | [常见问题](./QA.md) |
| API 使用方 | [API 概览](./api/README.md) | [知识工作流 API](./api/document-workflow.md)、[文件夹 API](./api/knowledge-folders.md) |

## 核心主题

- 架构与实现：
  [当前实现架构](./custom/当前实现架构与文档索引.md)、
  [二开目录规范](./custom/二开目录结构规范.md)。
- 文档解析：
  [水平扩展与故障恢复](./custom/文档解析水平扩展与故障恢复.md)、
  [分块](./CHUNKING.md)。
- Wiki/图谱：
  [知识图谱与渐进展示](./KnowledgeGraph.md)、
  [开启图谱](./开启知识图谱功能.md)。
- Agent：
  [通用运行时](./custom/通用智能体方案.md)、
  [知识库管理智能体](./custom/知识库管理智能体.md)。
- 企业集成：
  [统一身份与默认配置](./custom/统一身份认证与默认配置实现说明.md)、
  [对话分享](./custom/对话分享功能实现说明.md)。
- 运维：
  [生产执行手册](./custom/当前版本生产更新部署执行手册.md)、
  [常见问题](./QA.md)。

## 综合交付文档

- [系统介绍说明](./custom/系统介绍说明.md)
- [系统使用说明](./custom/系统使用说明.md)
- [WeKnora 智能体能力使用文档](./custom/WeKnora智能体能力使用文档.md)

三份综合说明直接使用 Markdown 维护。修改架构、部署、状态或主要用户交互时，
需要同步更新并执行链接、配置值和实现一致性检查。

## 维护边界

`testdata/`、`fixtures/` 和测试上传目录中的 Markdown/TXT 是测试数据，不是说明
文档；`skills/*/SKILL.md` 和 `custom/document-templates/` 是运行时指令/模板。
这些文件不能为了“全量更新文档”而被批量改写。CHANGELOG 历史段落保持不变，
新变化写在顶部未发布部分。
