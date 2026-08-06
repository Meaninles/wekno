#!/usr/bin/env python3
"""Plan or apply the measured production model-capacity policy via the API."""

from __future__ import annotations

import argparse
import json
import math
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path


class API:
    def __init__(self, base_url: str, token: str) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token

    def request(
        self,
        method: str,
        path: str,
        body: dict | None = None,
        if_match: int | None = None,
    ):
        payload = None if body is None else json.dumps(body).encode("utf-8")
        headers = {
            "Accept": "application/json",
            "Authorization": f"Bearer {self.token}",
        }
        if payload is not None:
            headers["Content-Type"] = "application/json"
        if if_match is not None:
            headers["If-Match"] = str(if_match)
        request = urllib.request.Request(
            self.base_url + path,
            data=payload,
            headers=headers,
            method=method,
        )
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                raw = response.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read().decode("utf-8", errors="replace")[:2000]
            raise RuntimeError(f"capacity API returned HTTP {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            raise RuntimeError(f"capacity API request failed: {exc.reason}") from exc
        parsed = json.loads(raw.decode("utf-8"))
        if not parsed.get("success"):
            raise RuntimeError(f"capacity API rejected request: {parsed}")
        return parsed.get("data")


def write_json(path: Path, value) -> None:
    path.write_text(
        json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    path.chmod(0o600)


def changed_fields(current: dict, target: dict) -> dict:
    return {
        key: {"before": current.get(key), "after": value}
        for key, value in target.items()
        if current.get(key) != value
    }


def load_and_validate_plan(plan_path: Path) -> dict:
    plan = json.loads(plan_path.read_text(encoding="utf-8"))
    if plan.get("schema_version") != 2:
        raise RuntimeError("capacity plan schema_version must be 2")
    routes = plan.get("model_routes")
    if not isinstance(routes, list) or len(routes) != 7:
        raise RuntimeError("capacity plan must contain the seven measured production routes")
    keys: set[str] = set()
    repo_root = plan_path.resolve().parents[2]
    for route in routes:
        key = str(route.get("key", "")).strip()
        if not key or key in keys:
            raise RuntimeError(f"route key is empty or duplicated: {key!r}")
        keys.add(key)
        if route.get("evidence_status") != "verified":
            raise RuntimeError(f"route {key} does not have verified capacity evidence")
        evidence = repo_root / str(route.get("evidence", ""))
        if not evidence.is_file():
            raise RuntimeError(f"capacity evidence is missing for {key}: {evidence}")
        kinds = route.get("resource_kinds")
        if not isinstance(kinds, list) or not kinds or not all(isinstance(v, str) for v in kinds):
            raise RuntimeError(f"route {key} has invalid resource_kinds")
        try:
            re.compile(str(route["name_regex"]))
        except (KeyError, re.error) as exc:
            raise RuntimeError(f"route {key} has invalid name_regex: {exc}") from exc
        target = route.get("target")
        if not isinstance(target, dict):
            raise RuntimeError(f"route {key} has no target policy")
        max_inflight = int(target.get("max_inflight", 0))
        reserve = int(target.get("interactive_reserve", -1))
        background = int(target.get("max_background_inflight", 0))
        if max_inflight < 1 or reserve < 0 or background != max_inflight - reserve:
            raise RuntimeError(f"route {key} has inconsistent provider concurrency")
        expected = math.floor(
            int(route["verified_concurrency"]) * float(route["target_fraction"])
        )
        if max_inflight != expected:
            raise RuntimeError(
                f"route {key} target {max_inflight} differs from evidence fraction {expected}"
            )
    constraints = plan["constraints"]
    primary = {route["key"]: route for route in routes}
    qwen = primary["qwen27_chat"]["target"]
    v4 = primary["deepseek_v4_flash_chat"]["target"]
    running = qwen["max_inflight"] + v4["max_inflight"]
    waiting = qwen["chat_max_waiting"] + v4["chat_max_waiting"]
    admitted = running + waiting
    if running != constraints["primary_chat_running_limit"]:
        raise RuntimeError("primary chat running total differs from the plan constraint")
    if waiting != constraints["primary_chat_waiting_limit"]:
        raise RuntimeError("primary chat waiting total differs from the plan constraint")
    if admitted != constraints["primary_chat_admitted_limit"]:
        raise RuntimeError("primary chat admitted total differs from the plan constraint")
    return plan


def ensure_safe_base_url(base_url: str, allow_non_loopback: bool) -> None:
    parsed = urllib.parse.urlparse(base_url)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        raise RuntimeError("--base-url must be an absolute HTTP(S) URL")
    if not allow_non_loopback and parsed.hostname not in {"127.0.0.1", "localhost", "::1"}:
        raise RuntimeError(
            "refusing to send an administrator token to a non-loopback URL; "
            "use kubectl port-forward or explicitly pass --allow-non-loopback"
        )


def parse_pool_overrides(values: list[str]) -> dict[str, str]:
    result: dict[str, str] = {}
    for value in values:
        key, separator, pool_id = value.partition("=")
        key = key.strip()
        pool_id = pool_id.strip()
        if separator != "=" or not key or not pool_id or key in result:
            raise RuntimeError(f"invalid or duplicate --pool override: {value!r}")
        result[key] = pool_id
    return result


def select_pools(
    pools: list[dict],
    routes: list[dict],
    overrides: dict[str, str],
) -> list[tuple[dict, dict]]:
    route_keys = {route["key"] for route in routes}
    unknown = sorted(set(overrides) - route_keys)
    if unknown:
        raise RuntimeError(f"--pool contains unknown route keys: {unknown}")
    selected: list[tuple[dict, dict]] = []
    used_ids: set[str] = set()
    for route in routes:
        pattern = re.compile(route["name_regex"])
        candidates = [
            pool
            for pool in pools
            if pool.get("resource_kind") in route["resource_kinds"]
            and pattern.search(str(pool.get("name", ""))) is not None
        ]
        explicit = overrides.get(route["key"])
        if explicit is not None:
            candidates = [pool for pool in candidates if pool.get("id") == explicit]
        if len(candidates) != 1:
            identities = [
                {
                    "id": pool.get("id"),
                    "name": pool.get("name"),
                    "resource_kind": pool.get("resource_kind"),
                }
                for pool in candidates
            ]
            raise RuntimeError(
                f"route {route['key']} expected one matching pool, found "
                f"{len(candidates)}: {identities}"
            )
        pool = candidates[0]
        if pool["id"] in used_ids:
            raise RuntimeError(f"pool {pool['id']} matched more than one route")
        used_ids.add(pool["id"])
        selected.append((route, pool))
    return selected


def proposed_pool(pool: dict, route: dict) -> dict:
    proposed = dict(pool)
    proposed.update(route["target"])
    return proposed


def pool_path(pool_id: str) -> str:
    return "/resource-pools/" + urllib.parse.quote(pool_id, safe="")


def best_effort_rollback(
    api: API,
    before_pools: dict[str, dict],
    before_scheduler: dict,
    updated_pool_ids: list[str],
    scheduler_updated: bool,
) -> dict:
    report: dict = {"pools": {}, "scheduler": "not-updated", "reconcile": "not-run"}
    try:
        current_pools = {pool["id"]: pool for pool in api.request("GET", "/resource-pools")}
    except Exception as exc:  # noqa: BLE001 - preserve the original apply error
        report["load_current_pools"] = f"failed: {exc}"
        current_pools = {}
    for pool_id in reversed(updated_pool_ids):
        try:
            current = current_pools[pool_id]
            api.request(
                "PUT",
                pool_path(pool_id),
                before_pools[pool_id],
                int(current["policy_version"]),
            )
            report["pools"][pool_id] = "restored"
        except Exception as exc:  # noqa: BLE001 - report every compensation failure
            report["pools"][pool_id] = f"failed: {exc}"
    if scheduler_updated:
        try:
            current = api.request("GET", "/scheduler-policy")
            api.request(
                "PUT",
                "/scheduler-policy",
                before_scheduler,
                int(current["policy_version"]),
            )
            report["scheduler"] = "restored"
        except Exception as exc:  # noqa: BLE001
            report["scheduler"] = f"failed: {exc}"
    try:
        api.request("POST", "/reconcile", {})
        report["reconcile"] = "completed"
    except Exception as exc:  # noqa: BLE001
        report["reconcile"] = f"failed: {exc}"
    return report


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("action", choices=("list", "plan", "apply"))
    parser.add_argument(
        "--base-url",
        default="http://127.0.0.1:18080/api/v1/custom/capacity-control",
    )
    parser.add_argument("--allow-non-loopback", action="store_true")
    parser.add_argument(
        "--pool",
        action="append",
        default=[],
        metavar="ROUTE_KEY=POOL_ID",
        help="optional exact pool override; may be repeated",
    )
    parser.add_argument(
        "--plan",
        type=Path,
        default=Path(__file__).with_name("concurrency-plan.json"),
    )
    parser.add_argument("--snapshot-dir", type=Path)
    args = parser.parse_args()

    ensure_safe_base_url(args.base_url, args.allow_non_loopback)
    plan = load_and_validate_plan(args.plan)
    overrides = parse_pool_overrides(args.pool)
    token = os.getenv("WEKNORA_ADMIN_TOKEN", "").strip()
    if not token:
        print("WEKNORA_ADMIN_TOKEN is required", file=sys.stderr)
        return 2
    api = API(args.base_url, token)
    pools = api.request("GET", "/resource-pools")
    if args.action == "list":
        for pool in pools:
            print(
                "\t".join(
                    str(pool.get(field, ""))
                    for field in (
                        "id", "name", "resource_kind", "state", "max_inflight",
                        "interactive_reserve", "tenant_burst", "document_burst",
                        "policy_version",
                    )
                )
            )
        return 0

    selected = select_pools(pools, plan["model_routes"], overrides)
    scheduler = api.request("GET", "/scheduler-policy")
    scheduler_target = plan["model_scheduler"]
    summary = {
        "pools": [
            {
                "route_key": route["key"],
                "model": route["model"],
                "pool_id": pool["id"],
                "pool_name": pool["name"],
                "pool_changes": changed_fields(pool, route["target"]),
            }
            for route, pool in selected
        ],
        "scheduler_changes": changed_fields(scheduler, scheduler_target),
    }
    print(json.dumps(summary, ensure_ascii=False, indent=2, sort_keys=True))
    if args.action == "plan":
        return 0
    if args.snapshot_dir is None or not args.snapshot_dir.is_absolute():
        print("apply requires an absolute --snapshot-dir", file=sys.stderr)
        return 2
    if args.snapshot_dir.exists() and any(args.snapshot_dir.iterdir()):
        raise RuntimeError(f"refusing to overwrite non-empty snapshot directory {args.snapshot_dir}")

    os.umask(0o077)
    args.snapshot_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
    before_effective = api.request("GET", "/effective")
    before_pools = {pool["id"]: pool for _, pool in selected}
    write_json(
        args.snapshot_dir / "before.json",
        {"pools": before_pools, "scheduler": scheduler, "effective": before_effective},
    )
    write_json(args.snapshot_dir / "requested-changes.json", summary)

    canonical: dict[str, dict] = {}
    validations: dict[str, dict] = {}
    for route, pool in selected:
        validation = api.request("POST", "/validate", proposed_pool(pool, route))
        validations[route["key"]] = validation
        if not validation.get("valid"):
            write_json(args.snapshot_dir / "pool-validations.json", validations)
            raise RuntimeError(
                f"capacity API rejected {route['key']}: {validation.get('issues')}"
            )
        canonical[pool["id"]] = validation.get("canonical") or proposed_pool(pool, route)
    write_json(args.snapshot_dir / "pool-validations.json", validations)

    updated_pool_ids: list[str] = []
    scheduler_updated = False
    try:
        if summary["scheduler_changes"]:
            proposed_scheduler = dict(scheduler)
            proposed_scheduler.update(scheduler_target)
            api.request(
                "PUT",
                "/scheduler-policy",
                proposed_scheduler,
                int(scheduler["policy_version"]),
            )
            scheduler_updated = True
        for route, pool in selected:
            if changed_fields(pool, route["target"]):
                api.request(
                    "PUT",
                    pool_path(pool["id"]),
                    canonical[pool["id"]],
                    int(pool["policy_version"]),
                )
                updated_pool_ids.append(pool["id"])
        api.request("POST", "/reconcile", {})
    except Exception as exc:
        rollback = best_effort_rollback(
            api, before_pools, scheduler, updated_pool_ids, scheduler_updated
        )
        write_json(args.snapshot_dir / "rollback-after-apply-error.json", rollback)
        raise RuntimeError(f"capacity apply failed: {exc}; compensation={rollback}") from exc

    after_pools_all = api.request("GET", "/resource-pools")
    after_pools = {pool["id"]: pool for pool in after_pools_all if pool["id"] in before_pools}
    after_scheduler = api.request("GET", "/scheduler-policy")
    after_effective = api.request("GET", "/effective")
    write_json(
        args.snapshot_dir / "after.json",
        {
            "pools": after_pools,
            "scheduler": after_scheduler,
            "effective": after_effective,
        },
    )
    remaining: dict[str, dict] = {
        route["key"]: changed_fields(after_pools.get(pool["id"], {}), route["target"])
        for route, pool in selected
    }
    remaining = {key: value for key, value in remaining.items() if value}
    scheduler_remaining = changed_fields(after_scheduler, scheduler_target)
    if remaining or scheduler_remaining:
        raise RuntimeError(
            f"capacity policy did not converge: pools={remaining}, "
            f"scheduler={scheduler_remaining}"
        )
    reports = {item.get("id"): item for item in after_effective.get("pools", [])}
    missing_reports = sorted(set(before_pools) - set(reports))
    if missing_reports:
        raise RuntimeError(f"effective capacity report is missing pools: {missing_reports}")
    errors = {
        pool_id: [
            issue
            for issue in reports[pool_id].get("issues", [])
            if issue.get("severity") == "error"
        ]
        for pool_id in before_pools
    }
    errors = {pool_id: issues for pool_id, issues in errors.items() if issues}
    if errors:
        raise RuntimeError(f"effective capacity errors: {errors}")
    print(
        f"OK: {len(selected)} capacity policies applied and verified; "
        f"snapshots={args.snapshot_dir}"
    )
    return 0


if __name__ == "__main__":
    try:
        raise SystemExit(main())
    except (OSError, ValueError, KeyError, RuntimeError, json.JSONDecodeError) as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        raise SystemExit(1)
