"""Tool catalogue, allowlist enforcement, and MCP result shaping."""

from __future__ import annotations

import asyncio
import json
import logging
import os
from dataclasses import dataclass
from typing import Any, Callable, Iterable

import mcp.types as types

from .backend import BackendClient, BackendError
from .context import require_api_key


logger = logging.getLogger(__name__)


def _object(properties: dict[str, Any] | None = None, required: Iterable[str] = ()) -> dict[str, Any]:
    schema: dict[str, Any] = {
        "type": "object",
        "properties": properties or {},
        "additionalProperties": False,
    }
    required_items = list(required)
    if required_items:
        schema["required"] = required_items
    return schema


def _string(description: str, *, default: str | None = None) -> dict[str, Any]:
    value: dict[str, Any] = {"type": "string", "description": description}
    if default is not None:
        value["default"] = default
    return value


def _integer(description: str, *, default: int | None = None, minimum: int | None = None) -> dict[str, Any]:
    value: dict[str, Any] = {"type": "integer", "description": description}
    if default is not None:
        value["default"] = default
    if minimum is not None:
        value["minimum"] = minimum
    return value


def _number(description: str, *, default: float | None = None) -> dict[str, Any]:
    value: dict[str, Any] = {"type": "number", "description": description}
    if default is not None:
        value["default"] = default
    return value


def _boolean(description: str, *, default: bool | None = None) -> dict[str, Any]:
    value: dict[str, Any] = {"type": "boolean", "description": description}
    if default is not None:
        value["default"] = default
    return value


def _array(description: str, items: dict[str, Any] | None = None) -> dict[str, Any]:
    return {"type": "array", "description": description, "items": items or {"type": "string"}}


def _any_object(description: str) -> dict[str, Any]:
    return {"type": "object", "description": description, "additionalProperties": True}


@dataclass(frozen=True)
class ToolSpec:
    name: str
    description: str
    input_schema: dict[str, Any]
    read_only: bool = False
    destructive: bool = False
    idempotent: bool = False

    def as_mcp_tool(self) -> types.Tool:
        return types.Tool(
            name=self.name,
            description=self.description,
            inputSchema=self.input_schema,
            annotations=types.ToolAnnotations(
                readOnlyHint=self.read_only,
                destructiveHint=self.destructive,
                idempotentHint=self.idempotent,
                openWorldHint=False,
            ),
        )


_kb_id = _string("知识库 ID 或名称")
_knowledge_id = _string("知识条目 ID")
_session_id = _string("会话 ID")


TOOL_SPECS: tuple[ToolSpec, ...] = (
    ToolSpec(
        "create_tenant",
        "创建租户。需要租户级管理权限。",
        _object(
            {
                "name": _string("租户名称"),
                "description": _string("租户描述"),
                "business": _string("业务类型"),
                "retriever_engines": _any_object("检索引擎配置；省略时使用 PostgreSQL 关键词+向量检索"),
            },
            ("name",),
        ),
    ),
    ToolSpec("list_tenants", "列出租户。", _object(), read_only=True, idempotent=True),
    ToolSpec(
        "create_knowledge_base",
        "创建知识库。config 可传入知识库类型、索引和模型配置。",
        _object(
            {
                "name": _string("知识库名称"),
                "description": _string("知识库描述"),
                "type": _string("知识库类型，如 document、faq、wiki"),
                "embedding_model_id": _string("Embedding 模型 ID"),
                "summary_model_id": _string("摘要模型 ID"),
                "derivative_model_id": _string("衍生任务模型 ID"),
                "chunking_config": _any_object("分块配置"),
                "indexing_strategy": _any_object("索引策略配置"),
                "config": _any_object("可选的知识库配置"),
            },
            ("name",),
        ),
    ),
    ToolSpec("list_knowledge_bases", "列出当前租户可访问的知识库。", _object(), read_only=True, idempotent=True),
    ToolSpec(
        "get_knowledge_base",
        "查看知识库详情和统计信息。",
        _object({"kb_id": _kb_id}, ("kb_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "update_knowledge_base",
        "更新知识库名称、描述或配置。",
        _object(
            {
                "kb_id": _kb_id,
                "name": _string("更新后的知识库名称"),
                "description": _string("更新后的描述"),
                "config": _any_object("可选的更新配置"),
            },
            ("kb_id", "name"),
        ),
    ),
    ToolSpec(
        "delete_knowledge_base",
        "删除知识库及其内容。此操作不可逆。",
        _object({"kb_id": _kb_id}, ("kb_id",)),
        destructive=True,
    ),
    ToolSpec(
        "hybrid_search",
        "在知识库中执行关键词+向量混合检索，返回匹配片段。",
        _object(
            {
                "kb_id": _kb_id,
                "query": _string("检索问题"),
                "vector_threshold": _number("向量相似度阈值", default=0.5),
                "keyword_threshold": _number("关键词匹配阈值", default=0.3),
                "match_count": _integer("返回数量", default=5, minimum=1),
            },
            ("kb_id", "query"),
        ),
        read_only=True,
    ),
    ToolSpec(
        "create_knowledge_from_content",
        "把 Markdown/纯文本内容写入知识库。成功后只返回知识条目 ID 和处理状态，不回显正文。",
        _object(
            {
                "kb_id": _kb_id,
                "title": _string("文档标题"),
                "content": _string("Markdown 或纯文本正文"),
                "status": _string("draft 或 publish", default="publish"),
                "tag_ids": _array("分类 ID 列表"),
                "channel": _string("写入来源标识", default="api"),
                "process_config": _any_object("可选的处理覆盖配置"),
            },
            ("kb_id", "title", "content"),
        ),
    ),
    ToolSpec(
        "create_knowledge_from_file",
        "上传文件并写入知识库。优先传 content_base64；成功后不回显文件内容。",
        _object(
            {
                "kb_id": _kb_id,
                "filename": _string("文件名"),
                "content_base64": _string("文件内容的 Base64；也接受 data:*/*;base64,... 数据 URI"),
                "content_type": _string("文件 MIME 类型", default="application/octet-stream"),
                "file_path": _string("仅在服务端显式开启 MCP_ALLOW_SERVER_FILE_PATH 时可用"),
                "metadata": _any_object("可选文件元数据"),
                "enable_multimodel": _boolean("是否启用多模态解析", default=True),
                "tag_ids": _array("分类 ID 列表"),
                "channel": _string("写入来源标识", default="api"),
                "process_config": _any_object("可选的处理覆盖配置"),
            },
            ("kb_id", "filename"),
        ),
    ),
    ToolSpec(
        "create_knowledge_from_url",
        "让 WeKnora 抓取 URL 并写入知识库。成功后只返回条目元信息。",
        _object(
            {
                "kb_id": _kb_id,
                "url": _string("待抓取的 URL"),
                "file_name": _string("可选文件名"),
                "file_type": _string("可选文件类型"),
                "enable_multimodel": _boolean("是否启用多模态解析"),
                "title": _string("可选标题"),
                "tag_ids": _array("分类 ID 列表"),
                "channel": _string("写入来源标识", default="api"),
                "process_config": _any_object("可选的处理覆盖配置"),
            },
            ("kb_id", "url"),
        ),
    ),
    ToolSpec(
        "ingest_status",
        "查看知识条目的解析、索引、摘要和 Wiki 处理状态，不返回正文。",
        _object({"knowledge_id": _knowledge_id}, ("knowledge_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "list_knowledge",
        "列出知识库中的文档条目及处理状态。",
        _object(
            {
                "kb_id": _kb_id,
                "page": _integer("页码", default=1, minimum=1),
                "page_size": _integer("每页数量", default=20, minimum=1),
                "keyword": _string("按标题或文件名过滤"),
                "file_type": _string("文件类型过滤"),
                "parse_status": _string("解析状态过滤"),
                "workflow_status": _string("工作流状态过滤"),
                "source": _string("来源过滤"),
            },
            ("kb_id",),
        ),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "get_knowledge",
        "查看知识条目详情和处理状态。",
        _object({"knowledge_id": _knowledge_id}, ("knowledge_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "download_knowledge",
        "下载知识条目关联的原始文件并以 Base64 返回；受 MCP_MAX_DOWNLOAD_BYTES 限制。",
        _object({"knowledge_id": _knowledge_id}, ("knowledge_id",)),
        read_only=True,
    ),
    ToolSpec(
        "delete_knowledge",
        "删除一个知识条目及其处理结果。此操作不可逆。",
        _object({"knowledge_id": _knowledge_id}, ("knowledge_id",)),
        destructive=True,
    ),
    ToolSpec(
        "create_model",
        "创建租户模型配置。模型参数中的密钥不会出现在返回值中。",
        _object(
            {
                "name": _string("模型名称"),
                "display_name": _string("显示名称"),
                "model_type": _string("模型类型，如 KnowledgeQA、Embedding、Rerank"),
                "type": _string("model_type 的兼容别名"),
                "source": _string("模型来源", default="local"),
                "description": _string("模型描述"),
                "parameters": _any_object("模型连接和提供商参数"),
                "base_url": _string("兼容旧客户端的模型 API 地址"),
                "api_key": _string("兼容旧客户端的模型 API Key"),
                "is_default": _boolean("兼容旧客户端的默认模型标记"),
                "workload_scope": _string("interactive 或 derivative_only"),
            },
            ("name",),
        ),
    ),
    ToolSpec("list_models", "列出租户模型配置；密钥字段会被脱敏。", _object(), read_only=True, idempotent=True),
    ToolSpec(
        "get_model",
        "查看模型配置；密钥字段会被脱敏。",
        _object({"model_id": _string("模型 ID")}, ("model_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "create_session",
        "创建聊天会话。当前后端会话与知识库无关，知识库在 chat/agent_chat 调用时指定。",
        _object(
            {
                "title": _string("会话标题"),
                "description": _string("会话描述"),
                "last_request_state": _any_object("可选的输入状态"),
                "kb_id": _string("兼容旧客户端的可选字段，当前后端不使用"),
            }
        ),
    ),
    ToolSpec(
        "get_session",
        "查看会话详情。",
        _object({"session_id": _session_id}, ("session_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "list_sessions",
        "列出聊天会话。",
        _object(
            {"page": _integer("页码", default=1, minimum=1), "page_size": _integer("每页数量", default=20, minimum=1)}
        ),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "delete_session",
        "删除聊天会话及其消息。",
        _object({"session_id": _session_id}, ("session_id",)),
        destructive=True,
    ),
    ToolSpec(
        "chat",
        "调用知识库问答并汇总 SSE 结果；建议显式传 knowledge_base_ids。",
        _object(
            {
                "session_id": _session_id,
                "query": _string("问题"),
                "knowledge_base_ids": _array("要检索的知识库 ID 或名称"),
                "web_search_enabled": _boolean("是否启用联网搜索", default=False),
                "enable_memory": _boolean("是否启用跨会话记忆", default=False),
            },
            ("session_id", "query"),
        ),
    ),
    ToolSpec(
        "agent_chat",
        "调用智能体问答并汇总 SSE 结果；agent_id 可传名称或 UUID。",
        _object(
            {
                "session_id": _session_id,
                "query": _string("问题"),
                "agent_id": _string("智能体名称或 UUID"),
                "knowledge_base_ids": _array("要检索的知识库 ID 或名称"),
                "web_search_enabled": _boolean("是否启用联网搜索", default=False),
                "enable_memory": _boolean("是否启用跨会话记忆", default=False),
            },
            ("session_id", "query", "agent_id"),
        ),
    ),
    ToolSpec(
        "list_agents",
        "列出当前租户可用智能体。",
        _object({"page": _integer("页码", default=1, minimum=1), "page_size": _integer("每页数量", default=50, minimum=1)}),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "get_agent",
        "查看智能体配置。",
        _object({"agent_id": _string("智能体 UUID")}, ("agent_id",)),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "list_chunks",
        "列出知识条目的文本分块。",
        _object(
            {
                "knowledge_id": _knowledge_id,
                "page": _integer("页码", default=1, minimum=1),
                "page_size": _integer("每页数量", default=20, minimum=1),
            },
            ("knowledge_id",),
        ),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "delete_chunk",
        "删除知识条目中的一个分块。此操作不可逆。",
        _object(
            {"knowledge_id": _knowledge_id, "chunk_id": _string("分块 ID")},
            ("knowledge_id", "chunk_id"),
        ),
        destructive=True,
    ),
    ToolSpec(
        "wiki_search",
        "在知识库 Wiki 页面中全文检索。",
        _object(
            {"kb_id": _kb_id, "query": _string("检索词"), "limit": _integer("返回数量", default=10, minimum=1)},
            ("kb_id", "query"),
        ),
        read_only=True,
    ),
    ToolSpec(
        "wiki_read_page",
        "读取知识库 Wiki 页面的 Markdown 和元数据。",
        _object({"kb_id": _kb_id, "slug": _string("Wiki 页面 slug")}, ("kb_id", "slug")),
        read_only=True,
        idempotent=True,
    ),
    ToolSpec(
        "wiki_index_view",
        "查看知识库 Wiki 索引目录。",
        _object(
            {"kb_id": _kb_id, "limit": _integer("每类最多返回数量", default=50, minimum=1)},
            ("kb_id",),
        ),
        read_only=True,
        idempotent=True,
    ),
)


TOOL_BY_NAME: dict[str, ToolSpec] = {spec.name: spec for spec in TOOL_SPECS}
TOOL_NAMES: tuple[str, ...] = tuple(spec.name for spec in TOOL_SPECS)


class ToolDisabledError(PermissionError):
    """Raised when a tool is not present in the JSONC allowlist."""


def _extract_data(value: Any) -> Any:
    if isinstance(value, dict) and "data" in value:
        return value["data"]
    return value


_SECRET_KEY_PARTS = (
    "api_key",
    "apikey",
    "access_token",
    "refresh_token",
    "authorization",
    "password",
    "secret",
    "private_key",
    "credential",
)


def redact_sensitive(value: Any) -> Any:
    """Recursively redact common credential fields in upstream JSON."""

    if isinstance(value, dict):
        clean: dict[str, Any] = {}
        for key, item in value.items():
            lower = str(key).casefold().replace("-", "_")
            if any(part in lower for part in _SECRET_KEY_PARTS):
                clean[key] = "[REDACTED]"
            else:
                clean[key] = redact_sensitive(item)
        return clean
    if isinstance(value, list):
        return [redact_sensitive(item) for item in value]
    return value


def _write_projection(name: str, value: Any) -> Any:
    """Keep write-only ingestion responses from echoing document contents."""

    data = _extract_data(value)
    if name in {"create_knowledge_from_content", "create_knowledge_from_file", "create_knowledge_from_url"}:
        if not isinstance(data, dict):
            return {"success": True}
        keys = (
            "id",
            "knowledge_id",
            "knowledge_base_id",
            "title",
            "file_name",
            "file_type",
            "type",
            "parse_status",
            "core_status",
            "summary_status",
            "enrichment_status",
            "wiki_status",
            "created_at",
            "updated_at",
        )
        projected = {key: data[key] for key in keys if key in data}
        if isinstance(value, dict):
            projected["success"] = bool(value.get("success", True))
        return projected

    if name == "create_knowledge_base":
        if isinstance(data, dict):
            return {key: data[key] for key in ("id", "name", "type", "description", "created_at") if key in data}
    if name == "update_knowledge_base":
        if isinstance(data, dict):
            return {key: data[key] for key in ("id", "name", "type", "description", "updated_at") if key in data}
    if name == "create_tenant":
        if isinstance(data, dict):
            return {key: data[key] for key in ("id", "name", "description", "business", "created_at") if key in data}
    if name == "create_model":
        if isinstance(data, dict):
            return {key: redact_sensitive(data[key]) for key in ("id", "name", "display_name", "type", "source", "description") if key in data}

    return redact_sensitive(value)


def _result_text(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, indent=2, default=str)


def _success_result(name: str, value: Any) -> types.CallToolResult:
    output = _write_projection(name, value) if not TOOL_BY_NAME[name].read_only else redact_sensitive(value)
    return types.CallToolResult(
        content=[types.TextContent(type="text", text=_result_text(output))],
        structuredContent={"result": output},
        isError=False,
    )


def _error_result(message: str) -> types.CallToolResult:
    safe = " ".join(str(message).split())[:512]
    payload = {"error": safe}
    return types.CallToolResult(
        content=[types.TextContent(type="text", text=_result_text(payload))],
        structuredContent=payload,
        isError=True,
    )


def _execute(client: BackendClient, name: str, args: dict[str, Any]) -> Any:
    if name == "create_tenant":
        return client.create_tenant(args)
    if name == "list_tenants":
        return client.list_tenants()
    if name == "create_knowledge_base":
        return client.create_knowledge_base(args)
    if name == "list_knowledge_bases":
        return client.list_knowledge_bases()
    if name == "get_knowledge_base":
        return client.get_knowledge_base(args["kb_id"])
    if name == "update_knowledge_base":
        return client.update_knowledge_base(args)
    if name == "delete_knowledge_base":
        return client.delete_knowledge_base(args["kb_id"])
    if name == "hybrid_search":
        return client.hybrid_search(args)
    if name == "create_knowledge_from_content":
        return client.create_knowledge_from_content(args)
    if name == "create_knowledge_from_file":
        return client.create_knowledge_from_file(args)
    if name == "create_knowledge_from_url":
        return client.create_knowledge_from_url(args)
    if name == "ingest_status":
        return client.ingest_status(args["knowledge_id"])
    if name == "list_knowledge":
        return client.list_knowledge(args)
    if name == "get_knowledge":
        return client.get_knowledge(args["knowledge_id"])
    if name == "download_knowledge":
        return client.download_knowledge(args["knowledge_id"])
    if name == "delete_knowledge":
        return client.delete_knowledge(args["knowledge_id"])
    if name == "create_model":
        return client.create_model(args)
    if name == "list_models":
        return client.list_models()
    if name == "get_model":
        return client.get_model(args["model_id"])
    if name == "create_session":
        return client.create_session(args)
    if name == "get_session":
        return client.get_session(args["session_id"])
    if name == "list_sessions":
        return client.list_sessions(args)
    if name == "delete_session":
        return client.delete_session(args["session_id"])
    if name == "chat":
        return client.chat(args)
    if name == "agent_chat":
        return client.agent_chat(args)
    if name == "list_agents":
        return client.list_agents(args)
    if name == "get_agent":
        return client.get_agent(args["agent_id"])
    if name == "list_chunks":
        return client.list_chunks(args)
    if name == "delete_chunk":
        return client.delete_chunk(args)
    if name == "wiki_search":
        return client.wiki_search(args)
    if name == "wiki_read_page":
        return client.wiki_read_page(args)
    if name == "wiki_index_view":
        return client.wiki_index_view(args)
    raise KeyError(name)


class ToolRegistry:
    """MCP-facing registry whose visible tools follow the JSONC policy."""

    def __init__(self, policy, base_url: str):
        self.policy = policy
        self.base_url = base_url

    def list_tools(self) -> list[types.Tool]:
        enabled = self.policy.snapshot().enabled_tools
        return [spec.as_mcp_tool() for spec in TOOL_SPECS if spec.name in enabled]

    async def call_tool(self, name: str, arguments: dict[str, Any] | None) -> types.CallToolResult:
        if name not in TOOL_BY_NAME:
            return _error_result(f"unknown tool: {name}")
        try:
            if not self.policy.is_enabled(name):
                raise ToolDisabledError(f"tool is disabled by policy: {name}")
            api_key = require_api_key()
            client = BackendClient(self.base_url, api_key)
            result = await asyncio.to_thread(_execute, client, name, arguments or {})
            return _success_result(name, result)
        except ToolDisabledError as exc:
            return _error_result(str(exc))
        except PermissionError as exc:
            return _error_result(str(exc))
        except BackendError as exc:
            logger.warning("MCP tool failed: name=%s status=%s message=%s", name, exc.status_code, str(exc))
            return _error_result(str(exc))
        except (KeyError, TypeError, ValueError) as exc:
            logger.warning("MCP tool input failed: name=%s type=%s", name, type(exc).__name__)
            return _error_result(f"invalid arguments for {name}")
        except Exception:
            logger.exception("MCP tool failed unexpectedly: name=%s", name)
            return _error_result(f"tool execution failed: {name}")
