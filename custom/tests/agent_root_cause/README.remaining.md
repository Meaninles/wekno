# 剩余问题的只读诊断实验

基线提交：`1e669e6b1ac5401ea4216c669747fcd42a5a7487`。这些文件只分析和测试，不修改运行中的业务实现、智能体提示词或知识库配置，不重建索引。

报告：`docs/custom/智能体剩余问题代码根因分析-2026-09-05.md`。

| 文件 | 验证边界 |
|---|---|
| `Dockerfile.remaining`、`remaining_chunk_config.go.txt`、`remaining_citation.go.txt` | 在一次性镜像中调用实际 Go 函数，观察父子块配置传递、当前流式引用处理与旧辅助函数的差异。日志中的 PASS 只表示诊断运行完成，不是业务答案通过。 |
| `remaining_docreader_probe.py` | 在现有 eval DocReader 中用 builtin 解析器解析指定 Word，保存分块前文本；不调用 OCR/VLM，不写知识库。 |
| `remaining_chunk_probe.py` | 通过只读 `/chunker/preview` 端点重放实际父子块配置；对原文和通用长表格行改变 token 预算，检查结构损失。 |
| `remaining_retrieval_probe.py` | 四库原有自然问题和实际工具查询原样重放；比较候选预算 10/50，及重排输入位置与 511/4096 token。只改变测试请求参数，原始结果供人工审阅，不按关键词判答案通过。 |
| `remaining_context_probe.py` | 冻结终段输入，保留当前原话和证据，仅移除以前轮次的工具过程或助手消息。培训迭代未包含原始可调用工具 schema，属于不完整的探索性重放，不可据此归因线上失败或统计提速。 |
| `remaining_sdk_probe.py` | 实际已安装 SDK/runner 接入容器回环的协议服务；比较文本/typed tool_use、流式边界和 SDK `--name`，不调用真实 LLM。 |

私有原始材料在 `.local-data/agent-audit-20260905/remaining/`。模型认证使用当前本地配置；密钥只保留在进程内，不进入结果、报告或提交。候选扩大同时改变服务内部召回预算，不能解释为只改变最后一次数组切片。重排实验没有修改索引、嵌入模型或模型答案。

```powershell
docker build --progress plain -f custom/tests/agent_root_cause/Dockerfile.remaining --target results --output type=local,dest=.local-data/agent-audit-20260905/remaining/component-results .
python custom/tests/agent_root_cause/remaining_chunk_probe.py .local-data/agent-audit-20260905/remaining
python custom/tests/agent_root_cause/remaining_retrieval_probe.py .local-data/agent-audit-20260905
python custom/tests/agent_root_cause/remaining_context_probe.py .local-data/agent-audit-20260905/remaining
```

DocReader 和 SDK 脚本应复制到相应容器的 `/tmp` 后用容器 Python 运行，参见脚本开头说明。运行产生的测试工作目录由 SDK 探针清理；输出证据保留供复核。SDK 最初使用重复文本的夹具被现有重复检测拒绝，已改用不同句子重跑，初次失败证据未删除。
