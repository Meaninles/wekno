# WeKnora 文档处理智能体旁路服务

本服务运行与通用智能体相同的 Claude Agent SDK 应用，但使用文档处理镜像。镜像内预装 LibreOffice、Pandoc、PDF 工具、中文字体以及常用 Word/Excel/PDF/PPT Python 库。

只有 `agent_type=document-processing-agent` 会路由到本旁路服务。`agent_type=general-agent` 和 `agent_type=data-analysis` 继续使用 `weknora-custom-general-agent`。

健康检查：

```bash
curl http://127.0.0.1:8093/health
```
