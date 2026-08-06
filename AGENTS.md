# AGENTS

本项目位于 `C:\weknora`。本地后端禁止使用单体 `app-dev`，固定使用 `custom/tests/runtime_profile_e2e/docker-compose.yml` 的分角色编排：API 3 个、解析 2 个、衍生任务 2 个、Wiki 2 个、维护 2 个、迁移 1 个（一次性）、DocReader 3 个（基础设施 1 个、运行时 2 个），另有 API 和 DocReader 入口各 1 个；API 入口对外为 `http://localhost:8080`。后端代码修改后，先停止该编排，再执行 `docker compose -p weknora-runtime-profile-e2e -f custom/tests/runtime_profile_e2e/docker-compose.yml build runtime-api-1` 构建共享镜像，并用同一 Compose 文件 `up -d --force-recreate` 拉起全部角色；拉起前必须确保 `docker-compose.dev.yml` 中的 PostgreSQL、Redis、MinIO、Neo4j 和基础 DocReader 已运行。

前端开发服务位于 `frontend/`，通过 `http://localhost:5177` 访问。

本地移动端固定通过 `http://localhost:5177/mobile/` 访问，不使用 `5178` 端口。

本项目做二开时，目录结构和原生代码修改边界必须先参考 `docs/custom/二开目录结构规范.md`；大段二开逻辑放到 `custom/` 或 `internal/custom/`，原生代码只保留必要注册点。

每次有修改后，必须在 Docker Desktop 中重新拉起受影响容器，确保用户可直接打开浏览器查看。

完成修改后重启应用时，必须先关闭此前仍在运行的本应用实例（如有），再启动更新后的实例。

当前为开发环境，二开部分无需兼容旧实现及数据库、存储、配置等兼容性，禁止降级实现。
