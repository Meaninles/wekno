# VLM 多实例生产近似 E2E

该测试启动两个独立的 WeKnora 主服务进程，共享本地 PostgreSQL 和一个隔离的
Redis DB，两个实例均通过生产同款模型配置直接请求：

这是独立的 VLM 双副本测试编排，不是日常本地 runtime profile。运行普通/稳定性
场景时，Go runner 在 `weknora-vlm-replica-e2e-a` 容器内执行；运行后台 workflow
场景前，必须先停止本地 runtime profile，避免额外的解析实例同时消费同一数据库。

```text
https://llmgateway.moutai.com.cn/v1 -> Qwen3-VL-32B
```

测试不经过本地 LiteLLM。正常长流、跨实例准入、跨租户全局上限和取消均调用
真实网关；熔断故障阶段只在两个测试容器内把网关域名临时解析到容器 loopback，
模型配置、credential digest、Redis circuit domain 均保持不变。移除该 DNS
故障并重建容器后，half-open 探针重新访问真实网关。

> 该 E2E 会产生真实模型流量和计费，只能在明确获准后运行。

默认配置与生产一致：

- VLM 全局并发 4、单租户并发 2；
- admission lease/heartbeat 为 45/15 秒；
- 首 token/流空闲/总预算为 180/45/360 秒；
- circuit threshold/window/open/probe 为 3/600/60/300 秒。

故障注入阶段可压缩等待时间，但不得改变并发上限、失败阈值或共享 Redis
状态机。故障映射使用容器自身 loopback，保证不会在企业网络中被特殊路由。
报告只写入 `outputs/`。`workflow` 场景会临时写入本地 PostgreSQL、MinIO 和
隔离 Redis DB，并在报告完成前通过正式删除 API 验证业务数据已清理；Redis
专用 DB 仍应在整组测试结束后清空。

## 运行

先确认 `WeKnora-postgres-dev`、`WeKnora-redis-dev`、`WeKnora-docreader-dev`
健康，并确认 Redis DB 8 为空。启动双副本：

```powershell
docker compose -f custom/tests/vlm_replica_e2e/docker-compose.yml up -d
```

生产参数长流/并发/取消验收：

```powershell
docker exec `
  -e VLM_E2E_REDIS_DB=8 `
  -e VLM_E2E_KEY_PREFIX=weknora:e2e:vlm-replica: `
  weknora-vlm-replica-e2e-a bash -lc `
  'cd /workspace && go run ./custom/tests/vlm_replica_e2e/cmd/e2e -scenario normal'
```

40 请求、每轮填满 4 个真实网关槽位的稳定性验收：

```powershell
docker exec `
  -e VLM_E2E_REDIS_DB=8 `
  -e VLM_E2E_KEY_PREFIX=weknora:e2e:vlm-replica: `
  weknora-vlm-replica-e2e-a bash -lc `
  'cd /workspace && go run ./custom/tests/vlm_replica_e2e/cmd/e2e -scenario stability -rounds 10'
```

完整后台链路验收会创建一个一次性本地知识库，并发上传四张不同图片。VLM、
Embedding 和摘要模型均使用生产 `llmgateway` 上的模型；两个本地副本共同消费
共享 Redis 中的 durable document workflow。场景会核对 OCR/Caption chunk、
embedding 行、executor identity、队列原子终态诊断和分布式 VLM 槽位，并通过
正式删除 API 等待测试知识库的异步清理完成。

派生任务被 admission 延后后可能留下历史 failed span，再由 durable retry
成功收敛。验收以最新 attempt 中同名 span 的最新一行为准，历史失败仍写入报告，
但只有最新结果失败才判定工作流失败。

文档工作流的恢复源是共享 PostgreSQL，不是隔离 Redis。运行该场景前必须停止本地
runtime profile 的全部角色和其他连接同一数据库的文档 worker，避免第三个实例
参与；测试会严格要求 executor identity 只能是 A/B。先执行：

```powershell
docker compose -p weknora-runtime-profile-e2e `
  -f custom/tests/runtime_profile_e2e/docker-compose.yml down
```

然后 runner 直接在 A 中执行：

```powershell
docker exec `
  weknora-vlm-replica-e2e-a sh -lc `
  'cd /workspace && /usr/local/go/bin/go run ./custom/tests/vlm_replica_e2e/cmd/e2e -scenario workflow'
```

完成后先关闭 A/B、清空专用 Redis DB 8，再按[开发指南](../../../docs/开发指南.md)
重新 build/up 本地 runtime profile。不要恢复或启动单体 `app-dev`。

`fault-sequence` 必须只在同时加载 `docker-compose.blackhole.yml` 的容器上执行；
它会保留 circuit 状态。随后移除 fault compose、重建两个副本并执行
`half-open`，验证只有一个副本能探测恢复后的真实网关。不要在正常负载阶段
加载 fault compose。
