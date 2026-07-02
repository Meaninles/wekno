#!/usr/bin/env python3
"""Register the optional MCP fixture service in WeKnora for general-agent tests.

Usage:
  WEKNORA_API_KEY=wk_xxx python custom/scripts/register_general_agent_mcp_fixtures.py

Optional env:
  WEKNORA_API_BASE=http://localhost:8080/api/v1
  MCP_FIXTURE_NAME="General Agent MCP Fixtures"
  MCP_FIXTURE_URL=http://weknora-custom-mcp-fixtures:8092/mcp
  MCP_FIXTURE_TEST=true
"""

from __future__ import annotations

import json
import os
import sys
import urllib.error
import urllib.request
from typing import Any


API_BASE = os.getenv("WEKNORA_API_BASE", "http://localhost:8080/api/v1").rstrip("/")
API_KEY = os.getenv("WEKNORA_API_KEY", "").strip()
NAME = os.getenv("MCP_FIXTURE_NAME", "General Agent MCP Fixtures").strip()
URL = os.getenv("MCP_FIXTURE_URL", "http://weknora-custom-mcp-fixtures:8092/mcp").strip()
SHOULD_TEST = os.getenv("MCP_FIXTURE_TEST", "true").strip().lower() not in {"0", "false", "no"}


def request_json(method: str, path: str, body: dict[str, Any] | None = None) -> dict[str, Any]:
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(f"{API_BASE}{path}", data=data, method=method)
    req.add_header("Content-Type", "application/json")
    if API_KEY:
        req.add_header("X-API-Key", API_KEY)
    try:
        with urllib.request.urlopen(req, timeout=60) as resp:
            raw = resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        detail = exc.read().decode("utf-8", "ignore")
        raise SystemExit(f"{method} {path} failed: HTTP {exc.code}: {detail}") from exc
    return json.loads(raw) if raw else {}


def main() -> int:
    if not API_KEY:
        print("WEKNORA_API_KEY is required", file=sys.stderr)
        return 2
    if not NAME or not URL:
        print("MCP_FIXTURE_NAME and MCP_FIXTURE_URL are required", file=sys.stderr)
        return 2

    payload = {
        "name": NAME,
        "description": "Optional MCP fixture service for validating general-agent MCP selection and tool bridge.",
        "enabled": True,
        "transport_type": "http-streamable",
        "url": URL,
        "headers": {},
        "auth_config": {"auth_type": ""},
        "advanced_config": {"timeout": 30, "retry_count": 1, "retry_delay": 1},
    }

    listed = request_json("GET", "/mcp-services")
    services = listed.get("data") or []
    existing = next((svc for svc in services if svc.get("name") == NAME), None)
    if existing:
        service_id = existing["id"]
        result = request_json("PUT", f"/mcp-services/{service_id}", payload)
        print(f"updated MCP fixture service: {service_id}")
    else:
        result = request_json("POST", "/mcp-services", payload)
        service_id = (result.get("data") or {}).get("id")
        print(f"created MCP fixture service: {service_id}")

    if not service_id:
        print(json.dumps(result, ensure_ascii=False, indent=2))
        raise SystemExit("MCP service ID missing from response")

    if SHOULD_TEST:
        test_result = request_json("POST", f"/mcp-services/{service_id}/test")
        print(json.dumps(test_result.get("data") or test_result, ensure_ascii=False, indent=2))

    print("Use this service in the agent editor via MCP 服务: all or selected.")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
