"""Verify read-only and write-only MCP allowlists through the local HTTP entry."""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import os
import time
from datetime import timedelta
from typing import Any

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


ALL_TOOLS = {
    "create_tenant",
    "list_tenants",
    "create_knowledge_base",
    "list_knowledge_bases",
    "get_knowledge_base",
    "update_knowledge_base",
    "delete_knowledge_base",
    "hybrid_search",
    "create_knowledge_from_content",
    "create_knowledge_from_file",
    "create_knowledge_from_url",
    "ingest_status",
    "list_knowledge",
    "get_knowledge",
    "download_knowledge",
    "delete_knowledge",
    "create_model",
    "list_models",
    "get_model",
    "create_session",
    "get_session",
    "list_sessions",
    "delete_session",
    "chat",
    "agent_chat",
    "list_agents",
    "get_agent",
    "list_chunks",
    "delete_chunk",
    "wiki_search",
    "wiki_read_page",
    "wiki_index_view",
}

# For knowledge-base read-only mode, chat/agent_chat are intentionally omitted:
# they can persist conversation messages even though they do not mutate KB data.
READ_ONLY_TOOLS = {
    "list_tenants",
    "list_knowledge_bases",
    "get_knowledge_base",
    "hybrid_search",
    "ingest_status",
    "list_knowledge",
    "get_knowledge",
    "download_knowledge",
    "list_models",
    "get_model",
    "get_session",
    "list_sessions",
    "list_agents",
    "get_agent",
    "list_chunks",
    "wiki_search",
    "wiki_read_page",
    "wiki_index_view",
}

# Strict write-only ingestion mode: create a KB and push content into it, but
# expose no listing, detail, search, download, chat, or destructive operation.
WRITE_ONLY_TOOLS = {
    "create_knowledge_base",
    "create_knowledge_from_content",
    "create_knowledge_from_file",
    "create_knowledge_from_url",
}


def _payload(result: Any) -> Any:
    structured = getattr(result, "structuredContent", None)
    if isinstance(structured, dict) and "result" in structured:
        return structured["result"]
    for block in getattr(result, "content", []) or []:
        if getattr(block, "type", "") != "text":
            continue
        try:
            return json.loads(getattr(block, "text", ""))
        except (TypeError, json.JSONDecodeError):
            return {"text": getattr(block, "text", "")}
    return {}


def _id(value: Any) -> str:
    if isinstance(value, dict):
        for key in ("id", "knowledge_base_id", "kb_id"):
            if value.get(key):
                return str(value[key])
        for key in ("data", "result"):
            if key in value:
                found = _id(value[key])
                if found:
                    return found
    return ""


async def _rest_delete(client: httpx.AsyncClient, backend_url: str, path: str) -> None:
    try:
        await client.delete(f"{backend_url.rstrip('/')}{path}")
    except Exception:
        pass


async def run(args: argparse.Namespace) -> int:
    api_key = args.api_key or os.getenv(args.api_key_env, "")
    if not api_key:
        raise RuntimeError("API key is required through --api-key-env")

    expected = READ_ONLY_TOOLS if args.mode == "read_only" else WRITE_ONLY_TOOLS
    blocked = ALL_TOOLS - expected
    timeout = timedelta(seconds=args.timeout)
    result: dict[str, Any] = {"mode": args.mode}
    created_kb = ""

    async with httpx.AsyncClient(headers={"X-API-Key": api_key}, timeout=args.timeout) as http_client:
        async with streamable_http_client(args.url, http_client=http_client) as (read_stream, write_stream, _):
            async with ClientSession(read_stream, write_stream) as session:
                await session.initialize()
                listed = await session.list_tools()
                names = {tool.name for tool in listed.tools}
                result["listed_count"] = len(names)
                result["expected_count"] = len(expected)
                result["visible_exact"] = names == expected
                result["unexpected_visible"] = sorted(names - expected)
                result["missing_visible"] = sorted(expected - names)

                disabled_errors: dict[str, bool] = {}
                for name in sorted(blocked):
                    call = await session.call_tool(name, {}, read_timeout_seconds=timeout)
                    disabled_errors[name] = bool(call.isError)
                result["blocked_count"] = len(blocked)
                result["blocked_calls_all_error"] = all(disabled_errors.values())
                result["blocked_calls_failed"] = sorted(name for name, is_error in disabled_errors.items() if not is_error)

                if args.mode == "read_only":
                    call = await session.call_tool("list_knowledge_bases", {}, read_timeout_seconds=timeout)
                    result["allowed_probe"] = {
                        "tool": "list_knowledge_bases",
                        "is_error": bool(call.isError),
                    }
                else:
                    unique = f"codex-mcp-permission-{int(time.time())}"
                    kb_call = await session.call_tool(
                        "create_knowledge_base",
                        {
                            "name": unique,
                            "description": "temporary local permission probe",
                            "type": "document",
                            "chunking_config": {"chunk_size": 1000, "chunk_overlap": 100, "separators": ["."]},
                        },
                        read_timeout_seconds=timeout,
                    )
                    kb_value = _payload(kb_call)
                    created_kb = _id(kb_value)
                    allowed_results = {
                        "create_knowledge_base": not bool(kb_call.isError),
                    }

                    if created_kb:
                        content_call = await session.call_tool(
                            "create_knowledge_from_content",
                            {
                                "kb_id": created_kb,
                                "title": unique + "-content",
                                "content": "Codex local permission probe marker.",
                                "status": "publish",
                                "channel": "api",
                            },
                            read_timeout_seconds=timeout,
                        )
                        file_call = await session.call_tool(
                            "create_knowledge_from_file",
                            {
                                "kb_id": created_kb,
                                "filename": unique + ".txt",
                                "content_base64": base64.b64encode(b"Codex local permission probe file").decode("ascii"),
                                "content_type": "text/plain",
                                "enable_multimodel": False,
                                "channel": "api",
                            },
                            read_timeout_seconds=timeout,
                        )
                        url_call = await session.call_tool(
                            "create_knowledge_from_url",
                            {
                                "kb_id": created_kb,
                                "url": "https://example.com",
                                "title": unique + "-url",
                                "enable_multimodel": False,
                                "channel": "api",
                            },
                            read_timeout_seconds=timeout,
                        )
                        allowed_results.update(
                            {
                                "create_knowledge_from_content": not bool(content_call.isError),
                                "create_knowledge_from_file": not bool(file_call.isError),
                                "create_knowledge_from_url": not bool(url_call.isError),
                            }
                        )
                    else:
                        allowed_results.update(
                            {
                                "create_knowledge_from_content": False,
                                "create_knowledge_from_file": False,
                                "create_knowledge_from_url": False,
                            }
                        )
                    result["allowed_probe"] = allowed_results

        if created_kb:
            await _rest_delete(http_client, args.backend_url, f"/knowledge-bases/{created_kb}")
            result["cleanup"] = "temporary knowledge base deleted through local REST cleanup"
        else:
            result["cleanup"] = "no temporary knowledge base was created"

    result["passed"] = bool(
        result["visible_exact"]
        and result["blocked_calls_all_error"]
        and not result["allowed_probe"].get("is_error", False)
        and all(result["allowed_probe"].get(name, False) for name in expected if name.startswith("create_knowledge"))
    )
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result["passed"] else 1


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--mode", choices=("read_only", "write_only"), required=True)
    parser.add_argument("--url", required=True)
    parser.add_argument("--backend-url", default="http://host.docker.internal:8080/api/v1")
    parser.add_argument("--api-key-env", default="WEKNORA_E2E_TENANT_API_KEY")
    parser.add_argument("--api-key", default="", help=argparse.SUPPRESS)
    parser.add_argument("--timeout", type=int, default=180)
    args = parser.parse_args()
    return asyncio.run(run(args))


if __name__ == "__main__":
    raise SystemExit(main())
