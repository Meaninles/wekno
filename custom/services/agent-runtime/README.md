# Agent Runtime

AgentScope 2.0.7.post1 是唯一问答运行循环；Go 提供授权业务工具和持久 Run 状态。
各类型独立保留。知识问答固定 15 次，其他类型固定 50 次。

- `app/run.py`：SDK 组装与执行。
- `app/budget.py`、`delivery.py`：预算、收尾、交付与最终提交。
- `app/control.py`、`state.py`、`events.py`：Go 状态协议、检查点和事件。
- `app/reasoning.py`：模型声明的内联思考协议到 SDK 思考/正文块的转换。
- `app/workspace.py`、`kubernetes_workspace.py`：隔离文件与专业技能运行环境。
- `uv.lock`：固定依赖。普通问答的工作区延迟创建。

开发编排为 `custom/docker-compose.agent-runtime.yml`，复用当前 API 和基础设施。
运行 `uv run pytest` 检查 SDK 协议；真实开发验证脚本见 `custom/tests/agent_runtime`。
不要将工作区临时目录视为最终产物存储。最终文件必须经 `publish_artifact` 上传，
由 Go 校验并提供私有鉴权下载。Worker 不持有业务数据库或对象存储账户凭据。

模型 `parameters.extra_config.reasoning_format` 默认为 `native`，使用服务返回的
原生思考字段。对已确认把 `<think>…</think>`（或聊天模板预填开启标记后仅返回
关闭标记）放在 `content` 中的 OpenAI Chat 服务，设置为 `think-tags`。
这是提供方输出协议，不按模型名称、智能体类型、问题或对话 ID 推断。
转换在 SDK 累积消息、生成事件与检查点之前完成，不使用答案去重、二次生成或前端遮盖。
该协议的首段在结束标记出现前有歧义，需要暂存；出现标记后正文正常流式输出。
没有标记的普通正文在响应结束时释放。原生协议的流式时延不受影响。
代码块、引用及行内代码中的字面标记保留；显式开启但未关闭的思考块按不完整响应处理。

完整实现与验证边界见 [说明](../../../docs/custom/统一AgentHarness实现方案.md)。
