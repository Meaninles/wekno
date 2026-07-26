# 常见问题

## 1. 如何查看日志？
```bash
docker compose logs -f app docreader postgres
```

## 2. 如何启动和停止服务？
```bash
# 启动服务
./scripts/start_all.sh

# 停止服务
./scripts/start_all.sh --stop

# 清空数据库
./scripts/start_all.sh --stop && make clean-db
```

## 3. 服务启动后无法正常上传文档？

通常是Embedding模型和对话模型没有正确被设置导致。按照以下步骤进行排查

1. 查看`.env`配置中的模型信息是否配置完整，其中如果使用ollama访问本地模型，需要确保本地ollama服务正常运行，同时在`.env`中的如下环境变量需要正确设置:
```bash
# LLM Model
INIT_LLM_MODEL_NAME=your_llm_model
# Embedding Model
INIT_EMBEDDING_MODEL_NAME=your_embedding_model
# Embedding模型向量维度
INIT_EMBEDDING_MODEL_DIMENSION=your_embedding_model_dimension
# Embedding模型的ID，通常是一个字符串
INIT_EMBEDDING_MODEL_ID=your_embedding_model_id
```

如果是通过remote api访问模型，则需要额外提供对应的`BASE_URL`和`API_KEY`:
```bash
# LLM模型的访问地址
INIT_LLM_MODEL_BASE_URL=your_llm_model_base_url
# LLM模型的API密钥，如果需要身份验证，可以设置
INIT_LLM_MODEL_API_KEY=your_llm_model_api_key
# Embedding模型的访问地址
INIT_EMBEDDING_MODEL_BASE_URL=your_embedding_model_base_url
# Embedding模型的API密钥，如果需要身份验证，可以设置
INIT_EMBEDDING_MODEL_API_KEY=your_embedding_model_api_key
```

当需要重排序功能时，需要额外配置Rerank模型，具体配置如下：
```bash
# 使用的Rerank模型名称
INIT_RERANK_MODEL_NAME=your_rerank_model_name
# Rerank模型的访问地址
INIT_RERANK_MODEL_BASE_URL=your_rerank_model_base_url
# Rerank模型的API密钥，如果需要身份验证，可以设置
INIT_RERANK_MODEL_API_KEY=your_rerank_model_api_key
```

2. 查看主服务日志，是否有`ERROR`日志输出

## 4. 没有图片或者显示无效的图片链接？

当使用多模态功能时，如果遇到图片无法显示或显示无效链接的问题，请按照以下步骤排查：

### 1. 确认多模态功能已正确配置

在知识库设置中开启**高级设置 - 多模态功能**，并在界面中配置相应的多模态模型。

### 2. 确认 MinIO 服务已启动

如果多模态功能配置使用的是 MinIO 存储，需要确保 MinIO 镜像已正确启动：

```bash
# 启动 MinIO 服务
docker-compose --profile minio up -d

# 或者启动完整服务（包括 MinIO、Neo4j、Qdrant）
docker-compose --profile full up -d
```

### 3. 检查 MinIO Bucket 权限

确保 MinIO 对应的 bucket 具有正确的读写权限：

1. 访问 MinIO 控制台：`http://localhost:9001`（默认端口）
2. 使用 `.env` 中配置的 `MINIO_ACCESS_KEY_ID` 和 `MINIO_SECRET_ACCESS_KEY` 登录
3. 进入对应的 bucket，检查并设置访问策略为**公开读取**或**公开读写**

**重要提示**：
- Bucket 名称不要包含特殊字符（包括中文），建议使用小写字母、数字和连字符
- 如果无法修改现有 bucket 的权限，可以在配置中填入一个不存在的 bucket 名称，本项目会自动创建对应的 bucket 并设置好正确的权限

### 4. 配置 MINIO_PUBLIC_ENDPOINT

在 `docker-compose.yml` 文件中，`MINIO_PUBLIC_ENDPOINT` 变量默认配置为 `http://localhost:9000`。

**重要提示**：如果你需要从其他设备或容器访问图片，`localhost` 可能无法正常工作，需要将其替换为本机的实际 IP 地址：


## 5. 平台兼容性说明

**重要提示**：`OCR_BACKEND=paddle` 模式在部分平台上可能无法正常运行。如果遇到 PaddleOCR 启动失败的问题，请选择以下解决方案

### 方案一：关闭 OCR 识别

在 `docker-compose.yml` 文件的 `docreader` 服务中删除 `OCR_BACKEND` 配置，然后重启 docreader 服务

**注意**：设置为 `no_ocr` 后，文档解析将不会使用 OCR 功能，这可能会影响图片和扫描文档的文字识别效果。

### 方案二：使用外部 OCR 模型（推荐）

如果需要 OCR 功能，可以使用外部的视觉语言模型（VLM）来替代 PaddleOCR。在 `docker-compose.yml` 文件的 `docreader` 服务中配置：

```yaml
environment:
  - OCR_BACKEND=vlm
  - OCR_API_BASE_URL=${OCR_API_BASE_URL:-}
  - OCR_API_KEY=${OCR_API_KEY:-}
  - OCR_MODEL=${OCR_MODEL:-}
```

然后重启 docreader 服务

**优势**：使用外部 OCR 模型可以获得更好的识别效果，且不受平台限制。

## 6. 如何使用数据分析功能？

当前有两条数据分析链路：

1. **文件/知识库表格分析**：上传或入库 CSV、Excel 等资料后，智能体通过知识库检索、`data_schema`、`data_analysis` 或 `database_query` 等工具分析已解析的数据。
2. **数据库数据源分析**：在左侧“数据源”中创建 MySQL/PostgreSQL 数据源，刷新元数据并启用表字段后，把数据源绑定到智能体。运行时使用 `db_catalog`、`db_schema`、`db_query` 查询。

数据分析智能体（`agent_type=data-analysis`）必须绑定至少一个数据库数据源。通用智能体和文档处理智能体也可按需绑定数据库源，但不是必填。

### 注意事项与使用规范

- 数据库查询仅支持只读查询，禁止 `INSERT`、`UPDATE`、`DELETE`、`CREATE`、`DROP` 等写入或结构变更操作。
- 使用数据库源时，应先查看 catalog/schema，再生成 SQL；建议限制表范围、返回行数、扫描行数和查询超时。
- 文件表格分析依赖文档解析质量。复杂 Excel 如果解析失败，可先转成结构更清晰的 CSV 或拆分工作表后重新上传。

## 7. 页面里刚保存的配置几秒后又消失了？

这类问题通常不是配置真的被系统清掉了，而是浏览器代理、缓存或插件干扰导致前端读到了异常响应，页面随后又被旧状态覆盖。

建议按下面顺序排查：

1. 先关闭浏览器代理、抓包工具、自动改写请求的插件，再重新打开页面。
2. 确认浏览器没有把 `localhost` 或当前访问域名走代理；如果配置了 PAC，请将 `localhost`、`127.0.0.1` 和实际部署域名加入直连名单。
3. 强制刷新页面，或直接使用无痕窗口重新登录后再保存一次配置。
4. 打开浏览器开发者工具的 `Network` 面板，确认保存配置相关请求返回的是最新内容，且没有被代理改写、缓存命中或重定向到其他环境。
5. 如果是调试模式部署，可尝试重启 `app` 服务后再验证一次：

```bash
docker compose restart app
```

如果重启后短时间恢复正常，但再次访问又出现相同现象，仍应优先检查浏览器代理、缓存和多环境串连问题，而不是直接判断为后端配置丢失。

## 8. SSRF 校验白名单（`SSRF_WHITELIST`）

可选配置。在 `.env` 中设置 `SSRF_WHITELIST`，用于在 URL 校验等环节将指定目标加入白名单，从而绕过常规 SSRF 限制。值为逗号分隔的多条规则，每条可以是：

- **精确域名**：如 `api.internal`
- **通配域名**：如 `*.example.com`
- **IPv4**：如 `203.0.113.5`
- **IPv6**：如 `2001:db8::1`（不要带方括号）
- **CIDR**：如 `10.0.0.0/8`、`2001:db8::/32`

列入白名单的地址会在 URL 校验等处绕过常规 SSRF 规则，**生产环境请谨慎配置**，仅加入确实需要且可信的目标。

示例（与 `.env.example` 一致，可按需取消注释并修改）：

```bash
# SSRF_WHITELIST=internal.service,*.corp.example,172.16.0.0/12,2001:db8::1,fd00::/8
```


## 9. 如何开启和查看 Langfuse 可观测性追踪？

WeKnora 支持通过 Langfuse 对 Agent 的 ReAct 循环、大模型 Token 消耗、工具调用以及异步任务流水线进行全链路追踪。

**开启步骤**：
1. 准备一个可用的 Langfuse 实例（支持云端版或私有部署版）。
2. 在 `.env` 文件中配置以下环境变量：
```bash
LANGFUSE_PUBLIC_KEY=pk-lf-...
LANGFUSE_SECRET_KEY=sk-lf-...
LANGFUSE_HOST=https://cloud.langfuse.com # 或你的私有部署地址
```
3. 重启服务后，系统会自动对所有支持的模型调用和 Agent 运行轨迹进行追踪，你可以在 Langfuse 的 Traces 面板中直观地看到每次对话和后台任务的详细执行瀑布图与 Token 统计。

## 10. 什么是 Wiki 模式？如何使用？

Wiki 模式允许 Agent 根据原始文档自动生成并维护一套结构化、相互链接的 Markdown Wiki 知识库，从而实现复杂知识的体系化沉淀和图谱化。

**使用方法**：
1. 进入指定**知识库的设置** -> **索引策略 (Indexing Strategy)**。
2. 开启 **Wiki** 索引功能（可同时结合开启**知识图谱**）。
3. 当你向该知识库上传文档时，系统会自动触发异步任务，通过大模型提取文档中的实体与核心概念，并自动生成结构化的 Wiki 页面及页面间的知识图谱链接。
4. 你可以在该知识库的“Wiki”标签页中，使用专用的 Wiki 浏览器查阅、管理页面，并通过可视化的知识图谱查看不同内容之间的关联关系。

## 11. 升级到 0.6.0 后，原本能做的操作变成了「权限不足」？

0.6.0 引入了租户内 RBAC（角色矩阵 + 资源归属），所有写入接口都会按角色 + `creator_id` 鉴权。常见现象：

- **看得到但点不动**：你大概率是该资源的 `Viewer` 或非创建者的 `Contributor`，UI 已经把写操作隐藏/置灰。检查 **用户菜单 → 当前工作区** 角色徽章。
- **共享空间里的 KB / Agent**：他人共享给你的 KB 默认按 `Viewer` 看待；要写需要在源租户里被授予 `Admin+`。
- **API Key 调用**：`X-API-Key` 合成虚拟用户固定为所属租户的 `Admin`（仅删除租户需 `Owner`），脚本一般无需迁移。
- **跨租户超管**：要 `User.CanAccessAllTenants=true` 且 `enable_cross_tenant_access=true`，并通过 `X-Tenant-ID` 切租户。

如需临时回退到「仅审计、不拦截」灰度窗口，可在配置里设置 `tenant.enable_rbac=false`（或环境变量 `WEKNORA_TENANT_ENABLE_RBAC=false`）。完整的角色矩阵和归属链请见 [`docs/RBAC说明.md`](./RBAC说明.md)。

## 12. 为什么登录后没有自动回到上次的工作区？

升级到 0.6.0 后系统会记住「最后活跃工作区」并在登录后自动恢复。若仍未恢复，通常是：

1. 浏览器清理了 LocalStorage / 切换了浏览器；
2. 你最后访问的那个工作区已经把你移除（`/leave` 或被管理员剔除）— 系统会回退到默认租户；
3. JWT 中携带了 `tenant_id` 但已无效 — 退出重登录即可。

## 13. 如何让多人协作时正确分配权限？

按照 [`docs/RBAC说明.md`](./RBAC说明.md) 的角色矩阵：

- 只读用户 → `Viewer`
- 普通成员（上传文档、维护「自己」的 KB / Agent）→ `Contributor`
- 运维人员（管理共享模型、向量库、解析器等基础设施）→ `Admin`
- 租户所有者（拥有删除租户权限，每租户唯一）→ `Owner`

如果你希望开启「invite-only」（不允许自助注册到本租户），可在租户设置里打开邀请制，并通过「邀请」入口签发邀请码或链接。

## 14. 文档解析卡在「处理中」/ 解析追踪时间线打不开怎么办？

每个文档解析都会记录一棵 Langfuse 风格的 Span 树，并由 PostgreSQL 文档工作流
记录队列、实例、boot、epoch、租约和当前阶段。可在卡片状态悬停或时间线中查看
主解析、向量、多模态、摘要、问题/图谱和 Wiki。常见情况：

- **显示队列位置**：文档已经持久入队，正在等待某个 app 的完整文档槽位，不是
  任务丢失。管理员可查看实例 capacity/active 和系统等待总量。
- **文档长时间处理中**：先定位具体阶段。大文件、VLM、ASR、图谱和 Wiki 本来会
  更慢；检查对应模型准入、DocReader、Neo4j/OBS 和工作流 lease。
- **一个 app 异常后没有立刻转移**：心跳超时不能单独证明旧进程已停止。系统会
  等待租约、boot/epoch fencing 和 Kubernetes 终止/节点隔离证明，优先避免双执行。
- **需要中止**：使用取消解析接口/界面，等待 `cancelling` 收敛到 `cancelled`；
  不要直接改数据库状态。
- **时间线一直显示「更新中」但无数据**：通常是轮询请求静默失败（网络 / 反向代理截断 SSE）。0.6.1 会显式暴露轮询失败，刷新页面或检查 Nginx 是否缓冲了响应即可。
- **升级后没有时间线/队列数据**：生产 migration 只能由一个维护 app 执行；
  正常多副本必须 `AUTO_MIGRATE=false`。按生产执行手册核对迁移，不要让三个 app
  同时补 DDL。

## 15. 如何启用 OpenSearch 作为向量库？

0.6.1 新增了 OpenSearch 向量库驱动（k-NN）。在 **设置 → 向量库** 中新增 OpenSearch 引擎并填写连接地址、凭据即可；KB 可绑定该向量库。注意：

- 连接地址会经过 SSRF 策略校验，内网 / 回环地址需符合放行规则；可用「测试连接」先行校验。
- 集成测试与索引映射细节见 [`docs/dev/opensearch-integration-test.md`](./dev/opensearch-integration-test.md)。

## 16. 内置模型（builtin models）如何用 YAML 声明式管理？

0.6.1 起平台内置模型由 `config/builtin_models.yaml` 声明式驱动，支持 `${ENV}` 变量插值，并通过 `managed_by` 字段与漂移巡检保持数据库与 YAML 一致。常见问题：

- **改了 YAML 不生效**：内置模型在服务启动时做生命周期对账（drift sweep）；确认重启了服务，且条目通过了 schema 校验（ID 长度、必填字段）。
- **Docker 下环境变量未注入**：`builtin_models` 依赖 `env_file` 数组形式注入变量，确认 compose 中按数组形式挂载了 `.env`。
- 参考样例：`config/builtin_models.yaml.example`。

## 17. 系统管理员（System Admin）与平台设置怎么用？

0.6.1 引入了系统管理员与统一平台设置面板（含平台审计日志），与租户内 RBAC 区分：系统管理员管理的是「平台级」配置，而非单个租户内的资源。首次启用需通过系统管理员 bootstrap 流程晋升首个管理员；撤销管理员权限有安全防护（避免误撤导致无人可管）。相关迁移为 `000053_system_admin_and_settings`。

## 18. 上传时如何自定义解析配置（process_config）？

0.6.2 起，文件 / URL / 文件夹上传可携带 `process_config`（`KnowledgeProcessOverrides`），在**本次批次**内覆盖知识库默认的解析引擎、分块、多模态（VLM / ASR）、问题生成、图谱抽取等设置，而不会改动 KB 全局配置。Web UI 在上传前会弹出确认对话框供调整；API 与 `weknora doc upload` 传同名 JSON 即可。

- **与 KB 默认配置的关系**：未传的字段沿用 KB 默认值；`graph_enabled` 仅在 `extract_config.enabled` 为 true 时生效。
- **重新解析**：`POST /knowledge/:id/reparse` 可在 body 中传 `process_config` 以新配置重跑解析，覆盖项会写入 `knowledge.metadata.process_overrides`。
- **图片 / 音频校验**：批次含图片时需 KB 已配置 VLM；含音频时需已配置 ASR，否则上传会被拒绝。
- 详见 [`docs/api/knowledge.md`](./api/knowledge.md)。

## 19. 升级到 0.6.2 后 `weknora` CLI 登录或 MCP 工具报错？

0.6.2 随附 **CLI v0.9**（破坏性变更），常见迁移：

- **`auth login` 不再创建 profile**：先 `weknora profile add <name> --host <url> --use`，再 `weknora auth login`；切换 profile 用全局 `--profile <name>`。
- **`auth logout` / `auth refresh` 去掉 `--name`**：作用于当前 active profile。
- **MCP 工具 `agent_invoke` 已更名为 `session_ask`**：外部 MCP 客户端需刷新工具 schema。
- **`agent create --kb` 改为 `--attach-kb`**；`doc delete --all` 与 `search chunks` / `search docs` 的 `--kb` 必填且支持名称或 ID。
- 新增 `weknora session stop <session-id>` 可中止进行中的 Agent 运行；仓库内附带 `weknora-rag-search` / `weknora-shared` 内置 Skills。
- 详见 [`cli/CHANGELOG.md`](../cli/CHANGELOG.md)。

## 20. pgvector 检索变慢或刚升级后需要做什么？

0.6.2 新增迁移 `000059_embeddings_hnsw_1024`，为 **1024 维** embedding（如 bge-m3）在 PostgreSQL pgvector 上创建 HNSW 索引。服务启动会自动执行迁移；若你使用其他维度，该索引可能不适用，需按自身 embedding 维度另行调优。升级后首次大批量入库期间索引构建可能占用额外 I/O，属正常现象。

## 21. 如何在网站嵌入 WeKnora 智能体（Embed Widget）？

0.6.3 起支持**嵌入渠道**：在 **集成中心** 或 Agent 编辑器中创建 embed 渠道，绑定自定义 Agent，获取渠道 ID 与发布 Token（`em_…`），将 `weknora-widget.js` 嵌入外部网页即可提供访客问答。

- **域名白名单**：必须在渠道配置中填写允许加载 Widget 的 Origin，否则 exchange 会返回 403。
- **安全模式（推荐）**：生产环境不要把 `em_…` 写在页面 HTML 里；由业务后端提供 `token-endpoint`，用发布 Token 调 `POST /api/v1/embed/:id/exchange` 换取短时令牌 `ems_…`（约 30 分钟有效）。详见 [`docs/embed-secure-mode.md`](./embed-secure-mode.md) 与 [`docs/embed-subdomain.md`](./embed-subdomain.md)。
- **限流**：渠道可配置每分钟 / 每日请求上限；超限返回 429。
- **子域部署**：若 embed 页面与 API 不同子域，参考 `docs/embed-subdomain.md` 配置 CORS 与 Nginx。

## 22. 文档如何设置多个标签？

0.6.3 将文档标签从单选升级为**多标签**（迁移 `000063_knowledge_multi_tags`）。在知识库列表可为文档打多个标签，侧边栏支持按标签筛选；**标签管理**抽屉可批量维护标签。API 上传 / 更新知识时传 `tag_ids` 数组（取代旧的单 `tag_id`）。

## 23. 如何批量重新解析文档？

在知识库文档列表框选多篇文档后，使用批量操作栏的 **重新解析**；也可调用 `POST /knowledge/batch-reparse`，body 可含 `ids` 与可选 `process_config`。任务异步入队，UI 会在入队后刷新状态。单篇仍可用 `POST /knowledge/:id/reparse`。

## 24. RSS 数据源如何配置？

0.6.3 新增 **RSS / Atom** 连接器。在知识库 **设置 → 数据源** 中选择 RSS，填写 Feed URL 与同步策略即可全量 / 增量拉取正文入库。若部分条目失败，同步日志会展示 partial failure 详情；编辑数据源保存配置**不会**自动触发同步，需手动点同步。

## 25. MCP 远程服务如何配置 OAuth2？

0.6.3 支持 MCP 服务的 **OAuth2 授权**（迁移 `000062_mcp_oauth`）。在 **设置 → MCP** 添加 HTTP 类型服务并选择 OAuth2，按向导完成授权回调；另支持自定义 HTTP Header 与 JSON **代码导入**快速粘贴配置。授权 Token 加密存储，过期后需在 UI 重新授权。

## 26. Embedding 维度如何覆盖？

在 **设置 → 模型** 编辑 Embedding 模型时可填写 **dimensions** 覆盖值（如 1024、1536）。0.6.3 修复了部分提供商请求未携带 `dimensions` 的问题（#1654）。若向量库索引维度与模型不一致，检索可能异常，请保持 KB 绑定向量库与模型维度一致。

## 27. Agent 提示「模型未就绪」无法对话？

0.6.3 在 Agent 选择器引入**模型就绪校验**：绑定的 LLM / Embedding / Rerank / VLM 缺失或配置无效时会阻断对话并给出修复指引。可在模型卡片打开 **调试抽屉** 先测试连通性；确认 KB 与 Agent 引用的模型均存在且可用。

## 28. 为什么摘要、问题/图谱或 Wiki 显示“尚未开始”，不是“已跳过”？

衍生状态列的历史初始值 `none` 同时覆盖“尚未创建任务”和“不适用”，必须结合
功能开关和主解析状态解释：

- 文档等待、主解析处理中：显示“尚未开始/等待前置解析”。
- 功能关闭：显示“未启用”。
- 内容不适用或任务明确记录结构化跳过原因：显示“已跳过”。
- 功能启用但执行异常：显示失败/降级。

不要把 `none` 直接映射为“已跳过”。文档总状态只有启用项全部成功才是已完成。

当前实现中允许“跳过”的证据包括：阶段 span 明确为 `skipped`，或
`output.skipped` 给出原因，例如没有可供问题/图谱抽取的 source chunk、图片为空/
装饰性内容，或旧 generation 已因重建、取消、删除被 fencing。功能关闭显示
“未启用”，不冒充“已跳过”；不支持的上传类型在接收阶段直接拒绝，也不算跳过。
模型/解析器调用失败、超时、限额后重试耗尽、Neo4j/Wiki 写入失败或批次部分失败，
必须进入失败/降级并可由“失败”筛选查到。

## 29. 为什么“失败”或“全部状态”筛选结果不对？

使用 `workflow_status`，不要只按 `parse_status`。主解析完成但摘要、问题/图谱或
Wiki 失败的文档仍属于 `failed`。允许值和 API 见
[文档完整工作流 API](./api/document-workflow.md)。如果后端返回正确而界面错误，
核对前端完整状态投影和轮询后是否立即把卡片移出旧筛选桶。

## 30. 点击超大文档预览为什么改成分页内容或下载？

这是浏览器保护。前端先调用 `/knowledge/:id/preview-policy`；大于类型阈值、超过
240 个 chunk、结构复杂、经过物理拆分或解码器无界的文件不会被一次性加载为 Blob。
直接请求原始预览会返回 413 和 `PREVIEW_REQUIRES_PAGED_CHUNKS`。这不是上传上限，
也不表示解析失败。

## 31. 大文件上传返回 413/504 怎么办？

生产知识源原文件上限为 2048 MiB，但链路每一层都必须允许：

- frontend/mobile Nginx：知识路由 2304 MiB，普通附件 80 MiB。
- Ingress：2304 MiB，request buffering 关闭，read/send timeout 7200 秒。
- 云 ELB/WAF：body 和 idle/upstream timeout 至少覆盖同一范围。
- app→Agent 如果经过代理：至少 128 MiB，覆盖 50 MiB 附件 Base64/JSON 膨胀。

任一层仍为 Nginx 默认 1 MiB 都会在到达 app 前失败。入口放宽不改变后端 2048/50
MiB 的业务校验。

## 32. 多 app/DocReader 是否意味着系统已经没有单点？

不是。app、DocReader、两个 Agent、frontend 和 mobile-web 可以多副本，文档在
安全 fencing 后可跨 app 接管；但当前 PostgreSQL、Neo4j、LiteLLM 仍是单实例，
Redis 主从没有自动切换，且为单 AZ。执行层水平扩展解决吞吐和单 Pod/解析节点
故障，不自动解决状态层/模型网关/可用区单点。

## 33. 解析中或已完成文档能否安全删除？

可以。删除先隔离当前 generation，取消/清理在途任务，再删除 chunk、向量/关键词、
问题、图谱、Wiki 来源和对象。删除完成后要联合检查 PostgreSQL、Neo4j、Wiki 和
对象存储；只从列表消失不代表清理成功。重建同样生成新 generation，旧任务的
迟到结果会被 fencing。

## P.S.
如果以上方式未解决问题，请在issue中描述您的问题，并提供必要的日志信息辅助我们进行问题排查
