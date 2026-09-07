# Agent Runtime

AgentScope 2.0.7.post1 是唯一问答运行循环；Go 提供授权业务工具和持久 Run 状态。
各类型独立保留。知识问答固定 15 次，其他类型固定 50 次。

- `app/run.py`：SDK 组装与执行。
- `app/budget.py`、`delivery.py`：预算、收尾、交付与最终提交。
- `app/control.py`、`state.py`、`events.py`：Go 状态协议、检查点和事件。
- `app/workspace.py`、`kubernetes_workspace.py`：隔离文件与专业技能运行环境。
- `uv.lock`：固定依赖。普通问答的工作区延迟创建。

开发编排为 `custom/docker-compose.agent-runtime.yml`，复用当前 API 和基础设施。
运行 `uv run pytest` 检查 SDK 协议；真实开发验证脚本见 `custom/tests/agent_runtime`。
不要将工作区临时目录视为最终产物存储。最终文件必须经 `publish_artifact` 上传，
由 Go 校验并提供私有鉴权下载。Worker 不持有业务数据库或对象存储账户凭据。

完整实现与验证边界见 [说明](../../../docs/custom/统一AgentHarness实现方案.md)。
