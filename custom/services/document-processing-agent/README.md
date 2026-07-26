# WeKnora 文档处理智能体旁路服务

本服务运行与通用智能体相同的 Claude Agent SDK 应用，但使用文档处理镜像。镜像内预装 LibreOffice、Pandoc、PDF 工具、中文字体以及常用 Word/Excel/PDF/PPT Python 库。

只有 `agent_type=document-processing-agent` 会路由到本旁路服务。`agent_type=general-agent`、`agent_type=data-analysis` 和 `agent_type=table-analysis` 继续使用 `weknora-custom-general-agent`。

## 多副本与工作目录

生产部署两个副本，分别位于两个 Agent 节点。每次运行固定在一个 Pod，Word、
Excel、PDF、PPT、LibreOffice、Pandoc 和 Python 中间文件只写本 Pod 的 40Gi RWO
临时卷。对象存储不能替代 POSIX 工作目录，也不需要 RWX。

最终产物在 terminal 事件前通过 Go app 上传私有 OBS，校验大小/SHA256并持久化
artifact ID；后续下载请求可以落到任意 app。Pod 崩溃时只丢失尚未提交的本地
半成品，本轮运行按失败/重试处理，不会损坏已完成产物。

双副本验收至少覆盖：

- Word 创建/修改和模板。
- Excel 公式/样式或数据报告。
- PDF 生成/转换。
- PPT 创建/转换。
- 带中文字体的渲染。
- 原附件输入、最终产物鉴权下载和删除。
- 同时运行、停止一个 Pod、新运行由另一 Pod 接收、恢复后重新分流。

对象存储和内部 API 边界与
[general-agent README](../general-agent/README.md#多副本和产物)一致；完整生产配置见
[生产更新部署执行手册](../../../docs/custom/当前版本生产更新部署执行手册.md)。

健康检查：

```bash
curl http://127.0.0.1:8093/health
```
