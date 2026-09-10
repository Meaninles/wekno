# Agent 运行时静态 PVC 与目录隔离

Kubernetes 后端使用一个预先绑定的静态 PVC，不再动态创建或删除运行级 PVC。
Docker 开发后端继续沿用其现有命名卷，不改变文件工具、历史上下文、输入下载、
输出基线、对象存储交付、执行回执及故障恢复协议。本改造无数据库 schema 变更。

## 配置与存储

`AGENT_WORKSPACE_PVC_NAME` 必填；`AGENT_WORKSPACE_PV_NAME` 可指定预期 PV。
对应 Helm 值为 `agentRuntime.workspace.existingClaim`、`existingVolume`。
旧 `storageClass/storageSize` 配置已移除，缺少 PVC 或 PVC 未 Bound 时拒绝启动工作区，
不回退到动态供给。应用不创建 StorageClass、PV 或 PVC。

生产现有对象是 `weknora-agent-workspaces/agent-runtime-shared` →
`weknora-agent-runtime-pv`，local 路径 `/mnt/weknora-data/weknora-agent-runtime`，
节点 `10.14.201.2`，RWO、Retain、100Gi。参见
[独立存储清单](../../deploy/production/agent-runtime-storage.example.yaml)。
该清单不进入业务 workload 渲染和删除流程；namespace 和存储应提前准备。
工作区及清理 Pod 使用 namespace 内的 `default-secret` 拉取 SWR 镜像。

一个运行对应 `sha256(run_id)[:32]`，与副本数、Worker 编号、并发配置无关：

```text
PVC root/
  workspaces/<key>/   → 业务容器 /workspace，uid 1000，0700
  receipts/<key>/     → 业务容器 /control，root，0700
  lifecycle/<key>/    → 仅受信任管理程序可见，锁与生命周期状态
```

业务容器只得到自己的两个 subPath；受信任初始化容器准备目录，清理 Pod 回收目录。
二者使用 runtime 镜像内固定的管理代码通过 Python 参数执行，管理代码不来自模型，
无网络访问或数据库凭据需求。目录遍历使用 FD 与 O_NOFOLLOW，清理使用抗符号链接
攻击的 rmtree。禁止递归 chown 整卷，禁止按用户输入路径清理。

## 生命周期与回收

创建工作区前先验证 PVC 绑定，创建 `agent-storage-<key>` ConfigMap，记录 run ID、
key、claim 名称和 UID。Pod 和 Worker 消失不会丢失回收索引。发现同名异属记录、
PVC 被重新创建或绑定错误时拒绝操作。

每次 close 只移除执行 Pod，保留文件和回执。交付流程可以重新挂载同一目录扫描输出。
现有 60 秒 housekeeping 查询 Go 权威运行状态；非终态或查询失败不清理。
终态集合保持现有 completed/failed/cancelled/incomplete 规则。
清理前删除对应执行 Pod并等待，使用同名、标记为 cleanup 的受信任 Pod串行回收。
清理失败保留 ConfigMap 供重试；Succeeded 后才删除清理 Pod 和对应 UID 的 ConfigMap。

目录管理锁串行化同一运行的初始化与回收；先写 deleting 标记再删除两个运行目录，
完成后保留 deleted 标记和锁文件。延迟的旧初始化不能复活已清理目录。
这些小型生命周期标记有意保留，不自动按时间删除；输入、输出和回执内容被回收。
本方案增加每运行一个 ConfigMap 和短生命周期清理 Pod，不增加 PV/PVC 或永久 Service。

历史文件依然由消息/上传记录和已发布制品清单恢复到新运行的独立工作区。
同名制品选择、租户/用户/会话边界、按需下载与 SHA256 校验保持原有实现。
同运行恢复时不覆盖已经修改的输入。终态清理不触及持久化原文件、制品和旧生产目录。

## 本地验证和上线边界

Linux 容器运行 `tests/test_static_workspace*.py` 验证真实权限、符号链接、重试、并发
清理与终态防复活；运行原工作区、文件问答和交付测试验证能力回归。
标准本地应用仍运行角色化 Compose 与独立 agent-runtime Compose。
没有本地 Kubernetes 时不能用 Docker 测试宣称 CCE 挂载和网络策略验证通过。

上线前必须在目标版本验证两个工作区 Pod 同时挂载、首次目录初始化、同运行恢复、
清理只影响目标目录、SWR 拉取与 pods/exec。工作区 NetworkPolicy 仅禁止入站连接；
工作区保留直接出站能力，并须验证 DNS、HTTPS、GitHub、依赖下载源及所需外部 API。
控制 API 查询在外部 Agent Runtime 完成，清理 Pod不主动联网。

生产目前为旧 general-agent/document-processing-agent hostPath 架构。
整体升级须先验证旧制品/上传记录可被新版读取、旧任务完成交付，再停止旧服务。
旧目录保持，不由新回收器管理。不要直接将旧运行任务挂载到新的空目录。

本次不实现磁盘耗尽、配额或新的文件大小限制；100Gi 不是文件系统硬配额。
local PV 的 workspace Pod 固定在 .2，节点不可用时不能自动跨节点访问数据。
不要强制删除失联节点上的执行 Pod并把它视为已停止；等待节点恢复和实际进程退出。
