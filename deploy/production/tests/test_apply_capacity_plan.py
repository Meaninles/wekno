from __future__ import annotations

import json
import os
import subprocess
import sys
import tempfile
import threading
import unittest
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path


ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "deploy" / "production" / "apply-capacity-plan.py"
PLAN = ROOT / "deploy" / "production" / "concurrency-plan.json"


class CapacityState:
    def __init__(self) -> None:
        plan = json.loads(PLAN.read_text(encoding="utf-8"))
        self.targets = {route["key"]: route["target"] for route in plan["model_routes"]}
        self.pools: dict[str, dict] = {}
        for index, route in enumerate(plan["model_routes"], start=1):
            pool_id = f"pool-{route['key']}"
            kind = route["resource_kinds"][0]
            if route["key"] == "qwen35_chat_derivative":
                kind = "derivative"
            pool = {
                "id": pool_id,
                "name": route["model"],
                "resource_kind": kind,
                "chat_max_concurrent": None,
                "chat_max_waiting": 9,
                "im_max_concurrent": 1 if kind == "chat" else 0,
                "im_max_waiting": 10 if kind == "chat" else 0,
                "max_inflight": 4,
                "max_background_inflight": 3,
                "interactive_reserve": 1,
                "tenant_guaranteed": 1,
                "tenant_burst": 4,
                "document_guaranteed": 1,
                "document_burst": 2,
                "rpm": 20,
                "tpm": 2000,
                "token_burst": 0,
                "request_timeout_seconds": 60,
                "circuit_threshold": 5,
                "circuit_window_seconds": 60,
                "circuit_open_seconds": 30,
                "state": "enabled",
                "policy_version": index + 2,
                "created_at": "2026-08-04T00:00:00Z",
                "updated_at": "2026-08-04T00:00:00Z",
            }
            self.pools[pool_id] = pool
        self.scheduler = {
            "id": 1,
            "prefetch_factor": 1,
            "derivative_weight": 1,
            "wiki_weight": 1,
            "background_max_wait_seconds": 10,
            "dispatch_lease_seconds": 60,
            "policy_version": 2,
            "updated_by": "",
            "created_at": "2026-08-04T00:00:00Z",
            "updated_at": "2026-08-04T00:00:00Z",
        }
        self.reconciled = False

    def effective(self) -> dict:
        return {
            "healthy": True,
            "pools": [{"id": pool_id, "issues": []} for pool_id in self.pools],
        }


class Handler(BaseHTTPRequestHandler):
    state: CapacityState

    def log_message(self, *_args) -> None:
        return

    def read_json(self) -> dict:
        size = int(self.headers.get("Content-Length", "0"))
        return json.loads(self.rfile.read(size).decode("utf-8"))

    def send_data(self, data=None) -> None:
        payload = json.dumps({"success": True, "data": data}).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)

    def authorized(self) -> bool:
        if self.headers.get("Authorization") == "Bearer unit-test-token":
            return True
        self.send_response(401)
        self.end_headers()
        return False

    def route(self) -> str:
        return self.path.removeprefix("/api/v1/custom/capacity-control")

    def do_GET(self) -> None:
        if not self.authorized():
            return
        if self.route() == "/resource-pools":
            self.send_data(list(self.state.pools.values()))
        elif self.route() == "/scheduler-policy":
            self.send_data(self.state.scheduler)
        elif self.route() == "/effective":
            self.send_data(self.state.effective())
        else:
            self.send_error(404)

    def do_POST(self) -> None:
        if not self.authorized():
            return
        if self.route() == "/validate":
            body = self.read_json()
            body["chat_max_concurrent"] = None
            if body["resource_kind"] != "chat":
                body["im_max_concurrent"] = 0
                body["im_max_waiting"] = 0
            self.send_data({"valid": True, "canonical": body, "issues": []})
        elif self.route() == "/reconcile":
            self.read_json()
            self.state.reconciled = True
            self.send_data(None)
        else:
            self.send_error(404)

    def do_PUT(self) -> None:
        if not self.authorized():
            return
        expected = int(self.headers["If-Match"])
        body = self.read_json()
        if self.route() == "/scheduler-policy":
            if expected != self.state.scheduler["policy_version"]:
                self.send_error(409)
                return
            body["policy_version"] = expected + 1
            self.state.scheduler = body
            self.send_data(body)
            return
        prefix = "/resource-pools/"
        if self.route().startswith(prefix):
            pool_id = urllib.parse.unquote(self.route()[len(prefix):])
            pool = self.state.pools.get(pool_id)
            if pool is None:
                self.send_error(404)
                return
            if expected != pool["policy_version"]:
                self.send_error(409)
                return
            body["policy_version"] = expected + 1
            self.state.pools[pool_id] = body
            self.send_data(body)
            return
        self.send_error(404)


class ApplyCapacityPlanTest(unittest.TestCase):
    def setUp(self) -> None:
        self.state = CapacityState()
        handler = type("StatefulHandler", (Handler,), {"state": self.state})
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), handler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.base_url = (
            f"http://127.0.0.1:{self.server.server_port}"
            "/api/v1/custom/capacity-control"
        )

    def tearDown(self) -> None:
        self.server.shutdown()
        self.server.server_close()
        self.thread.join(timeout=5)

    def run_tool(self, *arguments: str) -> subprocess.CompletedProcess[str]:
        environment = dict(os.environ)
        environment["WEKNORA_ADMIN_TOKEN"] = "unit-test-token"
        return subprocess.run(
            [sys.executable, str(SCRIPT), *arguments, "--base-url", self.base_url],
            cwd=ROOT,
            env=environment,
            text=True,
            encoding="utf-8",
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            check=False,
        )

    def test_list_plan_and_apply_all_measured_pools(self) -> None:
        listed = self.run_tool("list")
        self.assertEqual(listed.returncode, 0, listed.stderr)
        self.assertIn("pool-qwen27_chat", listed.stdout)

        planned = self.run_tool("plan")
        self.assertEqual(planned.returncode, 0, planned.stderr)
        self.assertIn('"route_key": "qwen27_chat"', planned.stdout)
        self.assertIn('"after": 32', planned.stdout)
        self.assertIn('"after": 8', planned.stdout)

        with tempfile.TemporaryDirectory() as temporary:
            snapshot = Path(temporary) / "capacity"
            applied = self.run_tool(
                "apply",
                "--snapshot-dir",
                str(snapshot),
            )
            self.assertEqual(applied.returncode, 0, applied.stderr)
            self.assertTrue((snapshot / "before.json").is_file())
            self.assertTrue((snapshot / "after.json").is_file())
            self.assertTrue((snapshot / "pool-validations.json").is_file())
        self.assertTrue(self.state.reconciled)
        for key, target in self.state.targets.items():
            pool = self.state.pools[f"pool-{key}"]
            for field, value in target.items():
                self.assertEqual(pool.get(field), value, f"{key}.{field}")
        self.assertEqual(self.state.scheduler["prefetch_factor"], 2)
        self.assertEqual(self.state.scheduler["derivative_weight"], 3)

    def test_plan_rejects_ambiguous_pool_match(self) -> None:
        duplicate = dict(self.state.pools["pool-qwen27_chat"])
        duplicate["id"] = "pool-qwen27-chat-duplicate"
        self.state.pools[duplicate["id"]] = duplicate
        planned = self.run_tool("plan")
        self.assertEqual(planned.returncode, 1)
        self.assertIn("expected one matching pool", planned.stderr)


if __name__ == "__main__":
    unittest.main()
