# 当前开发环境验证

这些脚本调用 eval 工作树正在运行的开发应用，不使用旧 eval 评分器或 gate。
`dev_client.py` 在内存中取得开发账户授权；不得打印或提交凭据。

- `scenario_matrix.py`：上传、公司制度检索与 18 轮长会话。
- `capability_probe.py`：独立类型、轻量与专业技能。
- `kbmanager_probe.py`：一次性测试库新增、替换、删除，结束清理测试对象。
- `integration_probe.py`、`im_probe.py`：本地 MCP／网页／IM 模拟。
- `failure_probe.py`：空回复、限流、超时和未知错误。
- `dev_workspace_probe.py`：真实隔离工作区文件、超时及进程行为。

报告写入 `.local-data/agent-runtime-validation`，含运行 ID、耗时、模型请求、
引用、事件与提交检查。成功状态不能代替内容核对；文件结果还需摘要、内容与渲染验证。
生产 Kubernetes 尚未执行本地 Docker 场景的同等实测。
