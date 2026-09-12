"""Simulate a WorkBuddy MCP client against the local Streamable HTTP endpoint."""

from __future__ import annotations

import argparse
import asyncio
import json
import os
import sys

import httpx
from mcp import ClientSession
from mcp.client.streamable_http import streamable_http_client


async def probe(args: argparse.Namespace) -> int:
    api_key = args.api_key or os.getenv(args.api_key_env, "")
    if not api_key:
        raise SystemExit("an API key is required through --api-key-env or --api-key")

    async with httpx.AsyncClient(headers={"X-API-Key": api_key}) as http_client:
        async with streamable_http_client(args.url, http_client=http_client) as (read_stream, write_stream, _):
            async with ClientSession(read_stream, write_stream) as session:
                await session.initialize()
                tools = await session.list_tools()
                names = [tool.name for tool in tools.tools]
                result: dict[str, object] = {
                    "server": "initialized",
                    "tool_count": len(names),
                    "tools": names,
                }
                if args.expect_tool and args.expect_tool not in names:
                    raise AssertionError(f"expected enabled tool is missing: {args.expect_tool}")
                if args.expect_absent and args.expect_absent in names:
                    raise AssertionError(f"expected disabled tool is visible: {args.expect_absent}")

                if args.call_disabled:
                    call = await session.call_tool(args.call_disabled, {})
                    result["disabled_call_is_error"] = bool(call.isError)
                    if not call.isError:
                        raise AssertionError(f"disabled tool unexpectedly succeeded: {args.call_disabled}")

                if args.call_list_knowledge_bases:
                    if "list_knowledge_bases" not in names:
                        raise AssertionError("list_knowledge_bases is not enabled")
                    call = await session.call_tool("list_knowledge_bases", {})
                    result["list_knowledge_bases_is_error"] = bool(call.isError)
                    if call.isError:
                        raise AssertionError("list_knowledge_bases returned an MCP error")

                print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--url", required=True)
    parser.add_argument("--api-key-env", default="WEKNORA_E2E_TENANT_API_KEY")
    parser.add_argument("--api-key", default="", help=argparse.SUPPRESS)
    parser.add_argument("--expect-tool", default="")
    parser.add_argument("--expect-absent", default="")
    parser.add_argument("--call-disabled", default="")
    parser.add_argument("--call-list-knowledge-bases", action="store_true")
    args = parser.parse_args()
    try:
        return asyncio.run(probe(args))
    except Exception as exc:
        print(f"probe failed: {type(exc).__name__}: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
