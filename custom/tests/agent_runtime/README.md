# 当前开发环境验证

这些脚本调用当前开发工作树正在运行的开发应用，使用可观察场景和人工核对功能行为。
`dev_client.py` 在内存中取得开发账户授权；不得打印或提交凭据。

- `scenario_matrix.py`：上传、公司制度检索与 18 轮长会话。
- `citation_continuity_probe.py --pass first` / `--pass second`：分别在新会话中重放反馈的五轮问题，再延伸到 16 轮；检查引用、跨话题复用、调用次数和没有生成后纠正。回归检查不进入在线问答流程。
- `uncited_history_probe.py --pass first`：复制原反馈会话的前四轮及已获取证据，验证未引用原文的后续复用；原会话保持不变。
- `conversation_boundary_probe.py --pass first`：复制截图中的失败轮次，检查失败后的压缩请求；同时检查通用智能体的制度问答与连续追问。
- `capability_probe.py`：独立类型、轻量与专业技能。
- `kbmanager_probe.py`：一次性测试库新增、替换、删除，结束清理测试对象。
- `integration_probe.py`、`im_probe.py`：本地 MCP／网页／IM 模拟。
- `failure_probe.py`：空回复、限流、超时和未知错误。
- `dev_workspace_probe.py`：真实隔离工作区文件、超时及进程行为。

报告写入 `.local-data/agent-runtime-validation`，含运行 ID、耗时、模型请求、
引用、事件与提交检查。成功状态不能代替内容核对；文件结果还需摘要、内容与渲染验证。
生产 Kubernetes 尚未执行本地 Docker 场景的同等实测。
