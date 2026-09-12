"""Run one Codex-side MCP ClientSession call for every exposed tool.

The script intentionally keeps output to tool names, IDs/counts, statuses and
short error summaries.  It never prints API keys, document bodies or download
Base64 content.
"""

from __future__ import annotations

import argparse
import asyncio
import base64
import json
import os
import sys
import time
from datetime import timedelta
from typing import Any

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


EXPECTED_TOOL_COUNT = 32
UUID_FALLBACK = "00000000-0000-0000-0000-000000000000"


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


def _short_error(result: Any) -> str:
    value = _payload(result)
    if isinstance(value, dict):
        value = value.get("error") or value.get("message") or value
    return " ".join(str(value).split())[:180]


def _records(value: Any) -> list[dict[str, Any]]:
    if isinstance(value, list):
        return [item for item in value if isinstance(item, dict)]
    if isinstance(value, dict):
        for key in ("data", "list", "items", "results"):
            nested = value.get(key)
            if isinstance(nested, list):
                return [item for item in nested if isinstance(item, dict)]
            if isinstance(nested, dict):
                found = _records(nested)
                if found:
                    return found
    return []


def _id(value: Any) -> str:
    if isinstance(value, dict):
        for key in ("id", "knowledge_id", "knowledge_base_id", "session_id", "model_id", "agent_id", "chunk_id"):
            if value.get(key):
                return str(value[key])
        for key in ("data", "result"):
            if key in value:
                found = _id(value[key])
                if found:
                    return found
    return ""


def _summarize(value: Any) -> str:
    if isinstance(value, dict):
        if "content_base64" in value:
            return "download metadata: " + ", ".join(
                f"{key}={value[key]}" for key in ("knowledge_id", "filename", "content_type", "size_bytes") if key in value
            )
        if "answer" in value:
            return f"answer_chars={len(str(value.get('answer') or ''))}, references={len(value.get('references') or [])}"
        important = {key: value[key] for key in ("id", "name", "title", "parse_status", "core_status", "status") if key in value}
        if important:
            return json.dumps(important, ensure_ascii=False, default=str)[:220]
        if "data" in value and isinstance(value["data"], list):
            return f"records={len(value['data'])}"
        return f"keys={','.join(str(key) for key in list(value)[:8])}"
    if isinstance(value, list):
        return f"records={len(value)}"
    return str(value)[:220]


class Probe:
    def __init__(self, session: ClientSession, timeout_seconds: int):
        self.session = session
        self.timeout = timedelta(seconds=timeout_seconds)
        self.outcomes: list[dict[str, Any]] = []

    async def call(
        self,
        name: str,
        arguments: dict[str, Any],
        *,
        expected_error: bool = False,
        dependency_error_allowed: bool = False,
    ) -> Any:
        started = time.monotonic()
        try:
            result = await self.session.call_tool(name, arguments, read_timeout_seconds=self.timeout)
        except Exception as exc:
            status = "WARN_DEPENDENCY" if dependency_error_allowed else "FAIL"
            detail = f"client exception: {type(exc).__name__}: {exc}"[:220]
            self.outcomes.append({"tool": name, "status": status, "detail": detail, "elapsed_ms": int((time.monotonic() - started) * 1000)})
            return None

        value = _payload(result)
        if getattr(result, "isError", False):
            if expected_error:
                status = "PASS_EXPECTED_ERROR"
            elif dependency_error_allowed:
                status = "WARN_DEPENDENCY"
            else:
                status = "FAIL"
            detail = _short_error(result)
        else:
            status = "PASS"
            detail = _summarize(value)
        self.outcomes.append({"tool": name, "status": status, "detail": detail, "elapsed_ms": int((time.monotonic() - started) * 1000)})
        return value


async def _rest_delete(client: httpx.AsyncClient, backend_url: str, path: str) -> None:
    try:
        await client.delete(f"{backend_url.rstrip('/')}{path}")
    except Exception:
        pass


async def run(args: argparse.Namespace) -> int:
    api_key = args.api_key or os.getenv(args.api_key_env, "")
    if not api_key:
        raise RuntimeError("API key is required through --api-key-env")

    timeout = httpx.Timeout(args.timeout)
    async with httpx.AsyncClient(headers={"X-API-Key": api_key}, timeout=timeout) as http_client:
        async with streamable_http_client(args.url, http_client=http_client) as (read_stream, write_stream, _):
            async with ClientSession(read_stream, write_stream) as session:
                await session.initialize()
                listed = await session.list_tools()
                names = [tool.name for tool in listed.tools]
                protocol = {
                    "initialized": True,
                    "tool_count": len(names),
                    "expected_tool_count": EXPECTED_TOOL_COUNT,
                    "all_expected_tools_visible": len(names) == EXPECTED_TOOL_COUNT,
                }
                if len(names) != EXPECTED_TOOL_COUNT:
                    protocol["missing_tools"] = sorted(set(args.expected_tools) - set(names))
                print(json.dumps({"protocol": protocol}, ensure_ascii=False))
                if len(names) != EXPECTED_TOOL_COUNT:
                    return 1

                probe = Probe(session, args.timeout)
                created_kb = ""
                created_model = ""
                created_session = ""
                knowledge_content = ""
                knowledge_file = ""
                knowledge_url = ""
                selected_agent = ""
                selected_chunk = ""
                existing_kb = ""

                try:
                    await probe.call("create_tenant", {}, expected_error=True)
                    await probe.call("list_tenants", {})

                    unique = f"codex-mcp-probe-{time.strftime('%Y%m%d%H%M%S')}-{os.getpid()}"
                    kb_value = await probe.call(
                        "create_knowledge_base",
                        {
                            "name": unique,
                            "description": "temporary Codex MCP protocol probe",
                            "type": "document",
                            "chunking_config": {"chunk_size": 1000, "chunk_overlap": 100, "separators": ["."]},
                        },
                        dependency_error_allowed=True,
                    )
                    created_kb = _id(kb_value)

                    kb_list_value = await probe.call("list_knowledge_bases", {})
                    kb_records = _records(kb_list_value)
                    existing_kb = next((str(item.get("id")) for item in kb_records if item.get("id") and str(item.get("id")) != created_kb), "")
                    test_kb = created_kb or existing_kb or UUID_FALLBACK

                    await probe.call("get_knowledge_base", {"kb_id": test_kb}, expected_error=not bool(created_kb or existing_kb), dependency_error_allowed=True)
                    await probe.call(
                        "update_knowledge_base",
                        {"kb_id": created_kb or UUID_FALLBACK, "name": unique + "-updated", "description": "updated by Codex MCP probe"},
                        expected_error=not bool(created_kb),
                        dependency_error_allowed=True,
                    )

                    content_value = await probe.call(
                        "create_knowledge_from_content",
                        {"kb_id": test_kb, "title": unique + "-content", "content": "Codex MCP probe marker: local-only temporary content.", "status": "publish", "channel": "api"},
                        expected_error=not bool(created_kb),
                        dependency_error_allowed=True,
                    )
                    knowledge_content = _id(content_value)

                    file_value = await probe.call(
                        "create_knowledge_from_file",
                        {"kb_id": test_kb, "filename": unique + ".txt", "content_base64": base64.b64encode(b"Codex MCP probe file marker").decode("ascii"), "content_type": "text/plain", "enable_multimodel": False, "channel": "api"},
                        expected_error=not bool(created_kb),
                        dependency_error_allowed=True,
                    )
                    knowledge_file = _id(file_value)

                    url_value = await probe.call(
                        "create_knowledge_from_url",
                        {"kb_id": test_kb, "url": "https://example.com", "title": unique + "-url", "enable_multimodel": False, "channel": "api"},
                        expected_error=not bool(created_kb),
                        dependency_error_allowed=True,
                    )
                    knowledge_url = _id(url_value)

                    status_target = knowledge_content or knowledge_file or UUID_FALLBACK
                    await probe.call("ingest_status", {"knowledge_id": status_target}, expected_error=not bool(knowledge_content or knowledge_file), dependency_error_allowed=True)
                    await probe.call("list_knowledge", {"kb_id": test_kb, "page": 1, "page_size": 20}, expected_error=not bool(created_kb or existing_kb), dependency_error_allowed=True)
                    detail_target = knowledge_content or knowledge_file or UUID_FALLBACK
                    await probe.call("get_knowledge", {"knowledge_id": detail_target}, expected_error=not bool(knowledge_content or knowledge_file), dependency_error_allowed=True)
                    await probe.call("download_knowledge", {"knowledge_id": knowledge_file or UUID_FALLBACK}, expected_error=not bool(knowledge_file), dependency_error_allowed=True)
                    delete_target = knowledge_url or knowledge_file or knowledge_content or UUID_FALLBACK
                    await probe.call("delete_knowledge", {"knowledge_id": delete_target}, expected_error=not bool(knowledge_url or knowledge_file or knowledge_content), dependency_error_allowed=True)

                    await probe.call("hybrid_search", {"kb_id": test_kb, "query": "Codex MCP probe marker", "match_count": 3}, expected_error=not bool(created_kb or existing_kb), dependency_error_allowed=True)

                    model_name = unique + "-model"
                    model_value = await probe.call(
                        "create_model",
                        {"name": model_name, "model_type": "KnowledgeQA", "source": "local", "description": "temporary Codex MCP probe model", "parameters": {}},
                        dependency_error_allowed=True,
                    )
                    created_model = _id(model_value)
                    model_list_value = await probe.call("list_models", {})
                    model_records = _records(model_list_value)
                    model_target = created_model or (str(model_records[0].get("id")) if model_records and model_records[0].get("id") else UUID_FALLBACK)
                    await probe.call("get_model", {"model_id": model_target}, expected_error=not bool(created_model or model_records), dependency_error_allowed=True)

                    session_value = await probe.call("create_session", {"title": unique + "-session", "description": "temporary Codex MCP probe session"}, dependency_error_allowed=True)
                    created_session = _id(session_value)
                    session_target = created_session or UUID_FALLBACK
                    await probe.call("get_session", {"session_id": session_target}, expected_error=not bool(created_session), dependency_error_allowed=True)
                    await probe.call("list_sessions", {"page": 1, "page_size": 20}, dependency_error_allowed=True)

                    agent_list_value = await probe.call("list_agents", {"page": 1, "page_size": 10}, dependency_error_allowed=True)
                    agent_records = _records(agent_list_value)
                    selected_agent = str(agent_records[0].get("id")) if agent_records and agent_records[0].get("id") else ""
                    await probe.call("get_agent", {"agent_id": selected_agent or UUID_FALLBACK}, expected_error=not bool(selected_agent), dependency_error_allowed=True)

                    chunk_target = knowledge_content or knowledge_file or UUID_FALLBACK
                    chunks_value = await probe.call("list_chunks", {"knowledge_id": chunk_target, "page": 1, "page_size": 20}, expected_error=not bool(knowledge_content or knowledge_file), dependency_error_allowed=True)
                    chunk_records = _records(chunks_value)
                    selected_chunk = str(chunk_records[0].get("id")) if chunk_records and chunk_records[0].get("id") else ""
                    await probe.call("delete_chunk", {"knowledge_id": chunk_target, "chunk_id": selected_chunk or UUID_FALLBACK}, expected_error=not bool(selected_chunk), dependency_error_allowed=True)

                    await probe.call("wiki_search", {"kb_id": test_kb, "query": "Codex MCP probe", "limit": 3}, expected_error=not bool(created_kb or existing_kb), dependency_error_allowed=True)
                    await probe.call("wiki_read_page", {"kb_id": test_kb, "slug": "codex-mcp-probe-not-found"}, expected_error=True, dependency_error_allowed=True)
                    await probe.call("wiki_index_view", {"kb_id": test_kb, "limit": 10}, expected_error=not bool(created_kb or existing_kb), dependency_error_allowed=True)

                    await probe.call("chat", {"session_id": session_target, "query": "请简短确认这是一次本地 MCP 协议测试。", "knowledge_base_ids": [test_kb] if test_kb != UUID_FALLBACK else []}, expected_error=not bool(created_session), dependency_error_allowed=True)
                    await probe.call("agent_chat", {"session_id": session_target, "query": "请简短确认这是一次本地 MCP 协议测试。", "agent_id": selected_agent or UUID_FALLBACK, "knowledge_base_ids": [test_kb] if test_kb != UUID_FALLBACK else []}, expected_error=not bool(created_session and selected_agent), dependency_error_allowed=True)
                    if created_session:
                        delete_session_value = await probe.call("delete_session", {"session_id": created_session}, dependency_error_allowed=True)
                        if delete_session_value is not None:
                            created_session = ""
                    else:
                        await probe.call("delete_session", {"session_id": UUID_FALLBACK}, expected_error=True, dependency_error_allowed=True)

                    await probe.call("delete_knowledge_base", {"kb_id": created_kb or UUID_FALLBACK}, expected_error=not bool(created_kb), dependency_error_allowed=True)
                    if created_kb:
                        created_kb = ""
                finally:
                    # Cleanup fallbacks are intentionally REST-only and are not
                    # counted as tool tests. They run only when an earlier MCP
                    # call interrupted the normal cleanup sequence.
                    backend_url = args.backend_url
                    if created_session:
                        await _rest_delete(http_client, backend_url, f"/sessions/{created_session}")
                    if created_kb:
                        await _rest_delete(http_client, backend_url, f"/knowledge-bases/{created_kb}")
                    if created_model:
                        await _rest_delete(http_client, backend_url, f"/models/{created_model}")

                print(json.dumps({"tools": probe.outcomes}, ensure_ascii=False, indent=2))
                failed = [item for item in probe.outcomes if item["status"] == "FAIL"]
                return 1 if failed else 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--backend-url", default="http://host.docker.internal:8080/api/v1")
    parser.add_argument("--api-key-env", default="WEKNORA_E2E_TENANT_API_KEY")
    parser.add_argument("--api-key", default="", help=argparse.SUPPRESS)
    parser.add_argument("--timeout", type=int, default=180)
    parser.add_argument("--expected-tools", nargs="*", default=[])
    args = parser.parse_args()
    if not args.expected_tools:
        args.expected_tools = []
    try:
        return asyncio.run(run(args))
    except Exception as exc:
        print(f"probe failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())

