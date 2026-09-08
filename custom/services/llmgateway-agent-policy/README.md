# Agent 模型参数与网关协议

本目录仅负责模型参数、协议适配和部署验证，不包含业务提示词或任务分类规则。WeKnora 传递思考开关和强度；网关根据明确的公开模型名选择采样参数。旧公开模型名保留既有采样行为。

| 新公开模型名 | 实际上游 | 思考开启 | 思考关闭 | 默认强度 |
|---|---|---|---|---|
| DeepSeek-V4-Flash-Agent | DS Flash 0731，原 dsv4-dspark 路由 | temperature=1、top_p=.95 | 同左 | high |
| Qwen3.8-27B-Agent | 原 Qwen3.6-27B 路由的 Qwen3.8-27B | temperature=1、top_p=.95、top_k=20、min_p=0、presence_penalty=0、repetition_penalty=1 | temperature=.7、top_p=.8、presence_penalty=1.5，其余同左 | xhigh |

参数依据：[DeepSeek 模型卡](https://huggingface.co/deepseek-ai/DeepSeek-V4-Flash-0731)、[Qwen 模型卡](https://huggingface.co/Qwen/Qwen3.8-27B)。强度合法值分别是 low/high/max 和 low/medium/xhigh。Qwen preserve_thinking 默认开启。强度不等于 temperature。

当前策略（v5）：Qwen 上游升级后移除统一关闭思考的临时限制。`Qwen3.8-27B-Agent` 按智能体请求启用或关闭思考，开启时默认强度为 `xhigh`；两种模式分别使用表中的官方采样参数。DS 和旧生产模型名保持既有行为，智能体级开关继续决定是否开启思考。

## 边界

- `generation_policy.py` 是两种协议共同使用的参数实现，无网络访问、凭据或业务提示词。显式冲突的参数会报错。
- `build_adapter.py` 对原网关适配器执行有 SHA 校验的源码变换，复用既有调用入口和签名逻辑；不重新叠加另一层模型客户端。
- `deploy.py` 创建不可变、带版本的 ConfigMap，保留部署镜像、密钥、上游地址和旧模型名。3 副本滚动更新，maxUnavailable=0；保留原 drain 钩子，给予正在执行的请求退出时间。
- Anthropic 流在已经发送 HTTP 200 后发生空终止，必须发送标准 `event: error`，不能抛异常让通用 OpenAI 错误格式截断流。非流响应通过异常返回失败。
- 不执行 reasoning_content 中的代码，不在网关自行拼接工具调用。Qwen 上游旧解析器的问题应在其模型服务中解决。
- 原适配器快照、生成结果、原始对话和部署备份留在私有目录，禁止提交凭据或原始业务日志。

## 验证

`test_generation_policy.py` 检查参数与冲突处理。`test_adapter_runtime.py` 在实际 LiteLLM 1.92 镜像内用本地 HTTP 服务记录最终上游请求，覆盖两种协议、同步/异步和旧模型名。`test_proxy_http.py` 启动完整 LiteLLM HTTP 服务，检查路由名称改写、流式错误及 Anthropic 客户端的错误识别。

运行后两项测试时，将生成的 `sitecustomize.py`、本目录 policy 和测试文件放在同一私有挂载目录，设置 `PYTHONPATH` 指向它，并设置 `LITELLM_LOCAL_MODEL_COST_MAP=True`。测试仅使用测试密钥；不接触真实模型。

`canary.py` 和 `smoke_http.py` 会调用真实上游，应在明确授权的验证窗口使用。前者验证两轮工具调用，后者验证新旧模型名、协议和思考开关；不会修改生产 WeKnora。

`replay_upstream.py` 仅用于原始请求诊断，不执行模型返回的工具。

最新部署记录、流程回归和未解决边界见 [v5 思考恢复与历史失败回归](../../../docs/custom/Qwen思考恢复与历史失败回归-20260906.md)。此前 v1–v4 过程见 [模型参数与网关协议修复验证](../../../docs/custom/模型参数与网关协议修复验证-20260906.md)。
