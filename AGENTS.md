# AGENTS

本项目位于 `C:\weknora`，Docker Desktop 中后端 `app-dev` 对外端口为 `http://localhost:8080`，前端开发服务位于 `frontend/` 并通过 `http://localhost:5177` 访问。

本项目做二开时，目录结构和原生代码修改边界必须先参考 `docs/custom/二开目录结构规范.md`；大段二开逻辑放到 `custom/` 或 `internal/custom/`，原生代码只保留必要注册点。

每次有修改后，必须在 Docker Desktop 中重新拉起受影响容器，确保用户可直接打开浏览器查看。

完成修改后重启应用时，必须先关闭此前仍在运行的本应用实例（如有），再启动更新后的实例。

当前为开发环境，二开部分无需兼容旧实现及数据库、存储、配置等兼容性，禁止降级实现。
