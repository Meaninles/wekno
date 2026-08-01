# 二开目录

`custom/` 放置不适合写入上游目录的大段独立实现、运行时服务、测试、文档模板和
部署辅助内容。注册边界见
[二开目录结构规范](../docs/custom/二开目录结构规范.md)。

| 目录 | 内容 |
|---|---|
| `services/` | general-agent、document-processing-agent、文档拆分器、LiteLLM 生产非 GLM 镜像等独立服务 |
| `tests/` | 多实例文档、知识库文件夹、模型容量、迁移和 Agent 验收 |
| `document-templates/` | Word/Excel/PDF/PPT 生成模板与运行时指令 |

后端 Go 模块位于 [`internal/custom/`](../internal/custom/README.md)，前端二开位于
[`frontend/src/custom/`](../frontend/src/custom/README.md)。当前总体架构见
[当前实现架构与文档索引](../docs/custom/当前实现架构与文档索引.md)。

测试数据和模板不是普通说明文档，不能在全量文档更新时批量改写。独立服务的
README 应明确其多副本、临时盘、对象存储和 Go 控制边界。
