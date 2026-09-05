# 智能体根因诊断实验

这些是诊断实验，不是业务修复。测试直接调用当前工作树的方法，断言能否复现缺陷；因此测试通过表示观察到预期行为，并不表示缺陷已经修复。

所有干预均针对通用机制：事务/批量写入、输入完整性、消息状态与位置、候选到达顺序、错误传播、原文件传递、SDK 持久化与流式生命周期。没有修改业务问题、注入标准答案或增加场景关键词规则。

## Go 组件实验

在 eval 工作树执行：

```powershell
docker build --progress plain -f custom/tests/agent_root_cause/Dockerfile --target results --output type=local,dest=.local-data/agent-audit-20260905/root-cause .
```

镜像将 `*.go.txt` 复制到相应包中，只在测试镜像里编译运行。工作树中的应用源码和正在运行的服务不改变。文件使用 `.txt` 后缀，是为了避免仓库的 `go test ./...` 把测试夹具当作独立业务包编译。

`AUDIT_TEST_PATTERN` 构建参数可选择某项实验。完整运行包含 5 万行物化实验，耗时约一分钟以上。

## SDK 生命周期实验

```powershell
docker cp custom/tests/agent_root_cause/runtime_probe.py weknora-agent-eval-general-agent:/tmp/weknora-root-cause-runtime.py
docker exec weknora-agent-eval-general-agent python /tmp/weknora-root-cause-runtime.py /tmp/weknora-root-cause-lifecycle
docker cp weknora-agent-eval-general-agent:/tmp/weknora-root-cause-lifecycle .local-data/agent-audit-20260905/root-cause-sdk-lifecycle
```

使用容器里实际安装的 Claude Agent SDK 和未修改的 runner，模型端为仅监听容器回环地址的定时服务。该服务固定每 0.35 秒返回一个片段，用来排除模型推理和外网抖动，不测量模型准确率，不调用真实模型，也不使用真实密钥。对照仅改变 SDK 的会话文件持久化选项。脚本退出时关闭定时服务，恢复临时替换的方法，清理实验工作目录。

## 文档原文件传递 A/B

`document_ab.py` 使用上一轮审计创建的 Word 和测试会话作为固定夹具。它会创建两个新的本地测试会话，复制相同的首轮文本和工具记录，再发送完全一致的第二轮请求。B 通过现有 `attachment_uploads` 通道附带相同原文件；A 仅使用历史。它不会修改智能体配置或提示词。

```powershell
docker cp custom/tests/agent_root_cause/watch_run.py weknora-agent-eval-document-processing-agent:/tmp/weknora-root-cause-watch.py
python custom/tests/agent_root_cause/document_ab.py
python custom/tests/agent_root_cause/summarize_transcripts.py
```

观察器从当前测试请求的进度事件取得 run ID，仅复制该次运行的诊断文件；不是修改 runner 或关闭清理逻辑。结果保存在 `.local-data`，不提交真实会话内容、运行时配置或认证信息。

文档用例只用于验证通用的会话文件传递机制。基于上传原文件的干预不是要求最终用户每轮重新上传：实际改造应由平台按会话恢复版本明确的文件句柄。

## 当前结果

详见 `docs/custom/智能体根因验证与结构精简方案-2026-09-05.md`。此目录未部署任何业务优化；真实服务继续运行 eval 编排。
