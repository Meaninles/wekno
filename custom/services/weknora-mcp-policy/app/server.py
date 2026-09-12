"""Entrypoint for the configurable WeKnora MCP service."""

from __future__ import annotations

import argparse
import asyncio
import json
import logging
import os
from contextlib import asynccontextmanager
from pathlib import Path
from typing import Any

import mcp.server.stdio
from mcp.server import NotificationOptions, Server
from mcp.server.models import InitializationOptions
from starlette.applications import Starlette
from starlette.requests import Request
from starlette.responses import JSONResponse, Response
from starlette.routing import Mount, Route

from .config import ConfigError, ToolPolicy, default_policy_path
from .context import RequestContext, reset_request_context, set_request_context
from .tools import TOOL_NAMES, ToolRegistry


logging.basicConfig(
    level=os.getenv("MCP_LOG_LEVEL", "INFO").upper(),
    format="%(asctime)s %(levelname)s %(name)s %(message)s",
)
logger = logging.getLogger(__name__)

SERVER_NAME = "weknora-mcp-policy"
SERVER_VERSION = "0.1.0"

MCP_INSTRUCTIONS = """这是 WeKnora 的统一 MCP 服务，提供知识库、文档、检索、智能体、模型、会话和 Wiki 能力。

通用使用规则：
1. 用户意图涉及 WeKnora、知识库、文档、知识检索、智能体、模型、会话或 Wiki 时，优先使用本 MCP 服务的原生工具。
2. 使用标准的 tools/list 发现当前已开放的工具；如果客户端采用延迟加载，先使用客户端提供的 MCP 工具发现机制。
3. 根据工具名称、标题、描述和输入 Schema 选择最匹配的工具；不要猜测工具名。
4. 不要通过本地文件系统、源码、端口扫描、Bash、curl、Python 或猜测 REST API 替代 MCP 工具调用。
5. 空列表或空结果是有效的业务结果，不代表 MCP 连接失败；只有工具返回 isError=true 才应报告工具执行失败。
6. 只有用户明确要求对话、写入、更新或删除时，才调用有相应副作用的工具；调用前应核对必填参数。
7. 如果当前会话没有看到本服务或工具，应报告 MCP 尚未连接，不要自行推断本地部署状态。
"""


def _header(scope: dict[str, Any], name: str) -> str:
    wanted = name.casefold().encode("latin-1")
    for key, value in scope.get("headers", []):
        if key.lower() == wanted:
            return value.decode("latin-1").strip()
    return ""


def _api_key_from_scope(scope: dict[str, Any]) -> str:
    # X-API-Key is the canonical WorkBuddy setting.  Bearer is accepted as a
    # convenience because several MCP clients expose only an Authorization
    # header field; the value is still forwarded upstream as X-API-Key.
    key = _header(scope, "x-api-key")
    if key:
        return key
    authorization = _header(scope, "authorization")
    if authorization.casefold().startswith("bearer "):
        return authorization[7:].strip()
    return ""


async def _send_json(send, status: int, payload: dict[str, Any]) -> None:
    body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    headers = [
        (b"content-type", b"application/json; charset=utf-8"),
        (b"content-length", str(len(body)).encode("ascii")),
        (b"cache-control", b"no-store"),
    ]
    await send({"type": "http.response.start", "status": status, "headers": headers})
    await send({"type": "http.response.body", "body": body})


class RequestAuthContextMiddleware:
    """Attach the caller API key to each stateless HTTP MCP request."""

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") != "http":
            await self.app(scope, receive, send)
            return

        path = scope.get("path", "")
        if path == "/healthz" or not (path == "/mcp" or path.startswith("/mcp/")):
            await self.app(scope, receive, send)
            return

        api_key = _api_key_from_scope(scope)
        if not api_key:
            await _send_json(send, 401, {"error": "X-API-Key is required"})
            return

        token = set_request_context(RequestContext(api_key=api_key, source="http"))
        try:
            await self.app(scope, receive, send)
        finally:
            reset_request_context(token)


class CanonicalMcpPathMiddleware:
    """Accept both /mcp and /mcp/ without relying on a 307 redirect.

    Some MCP clients follow the redirect, while lightweight fallbacks and
    connector probes may treat a redirect as an empty response.  Rewriting
    the scope keeps both spellings on the same Streamable HTTP handler.
    """

    def __init__(self, app):
        self.app = app

    async def __call__(self, scope, receive, send):
        if scope.get("type") == "http" and scope.get("path") == "/mcp":
            scope = dict(scope)
            scope["path"] = "/mcp/"
            scope["raw_path"] = b"/mcp/"
        await self.app(scope, receive, send)


def build_registry() -> ToolRegistry:
    base_url = os.getenv("WEKNORA_BASE_URL", "http://localhost:8080/api/v1").rstrip("/")
    policy_path = Path(default_policy_path())
    policy = ToolPolicy(policy_path, TOOL_NAMES)
    return ToolRegistry(policy, base_url)


def build_mcp_server(registry: ToolRegistry) -> Server:
    # Pass instructions to the Server itself as well as stdio's explicit
    # InitializationOptions.  Streamable HTTP uses Server.initialize directly
    # and does not call _init_options().
    server = Server(SERVER_NAME, version=SERVER_VERSION, instructions=MCP_INSTRUCTIONS)

    @server.list_tools()
    async def handle_list_tools():
        return registry.list_tools()

    @server.call_tool()
    async def handle_call_tool(name: str, arguments: dict[str, Any] | None):
        return await registry.call_tool(name, arguments)

    return server


def _init_options(server: Server) -> InitializationOptions:
    return InitializationOptions(
        server_name=SERVER_NAME,
        server_version=SERVER_VERSION,
        instructions=MCP_INSTRUCTIONS,
        capabilities=server.get_capabilities(
            notification_options=NotificationOptions(),
            experimental_capabilities={},
        ),
    )


async def healthz(request: Request) -> JSONResponse:
    registry: ToolRegistry = request.app.state.registry
    try:
        snapshot = registry.policy.snapshot()
        return JSONResponse(
            {
                "status": "ok",
                "service": SERVER_NAME,
                "policy_file": str(snapshot.path),
                "enabled_tool_count": len(snapshot.enabled_tools),
            },
            headers={"Cache-Control": "no-store"},
        )
    except ConfigError as exc:
        return JSONResponse(
            {"status": "error", "service": SERVER_NAME, "error": str(exc)[:512]},
            status_code=503,
            headers={"Cache-Control": "no-store"},
        )


async def run_http(host: str, port: int) -> None:
    registry = build_registry()
    server = build_mcp_server(registry)
    max_body = int(os.getenv("MCP_MAX_REQUEST_BODY_BYTES", str(64 * 1024 * 1024)))

    from mcp.server.streamable_http_manager import StreamableHTTPSessionManager
    import uvicorn

    session_manager = StreamableHTTPSessionManager(
        app=server,
        event_store=None,
        json_response=False,
        stateless=True,
        max_request_body_size=max_body,
    )

    @asynccontextmanager
    async def lifespan(starlette_app: Starlette):
        starlette_app.state.registry = registry
        async with session_manager.run():
            yield

    starlette_app = Starlette(
        routes=[
            Route("/healthz", healthz, methods=["GET"]),
            Mount("/mcp", app=session_manager.handle_request),
        ],
        lifespan=lifespan,
    )
    starlette_app = CanonicalMcpPathMiddleware(starlette_app)
    starlette_app = RequestAuthContextMiddleware(starlette_app)

    logger.info("Starting %s HTTP server on %s:%d", SERVER_NAME, host, port)
    logger.info("MCP endpoint: http://%s:%d/mcp", host, port)
    config = uvicorn.Config(starlette_app, host=host, port=port, log_level="info")
    await uvicorn.Server(config).serve()


async def run_stdio() -> None:
    registry = build_registry()
    server = build_mcp_server(registry)
    api_key = os.getenv("WEKNORA_API_KEY", "").strip()
    token = set_request_context(RequestContext(api_key=api_key, source="stdio"))
    try:
        async with mcp.server.stdio.stdio_server() as (read_stream, write_stream):
            await server.run(read_stream, write_stream, _init_options(server))
    finally:
        reset_request_context(token)


def main() -> None:
    parser = argparse.ArgumentParser(description="Configurable WeKnora MCP server")
    parser.add_argument(
        "--transport",
        choices=("stdio", "http"),
        default=os.getenv("MCP_TRANSPORT", "stdio"),
        help="MCP transport (default: stdio)",
    )
    parser.add_argument("--host", default=os.getenv("MCP_HOST", "0.0.0.0"))
    parser.add_argument("--port", type=int, default=int(os.getenv("MCP_PORT", "8000")))
    args = parser.parse_args()

    if args.transport == "stdio":
        asyncio.run(run_stdio())
    else:
        asyncio.run(run_http(args.host, args.port))


if __name__ == "__main__":
    main()
