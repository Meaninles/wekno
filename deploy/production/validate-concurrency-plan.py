#!/usr/bin/env python3
"""Validate the coupled production resource and measured-capacity plan."""

from __future__ import annotations

import argparse
import json
import math
from pathlib import Path


EXPECTED_MODELS = {
    "qwen27_chat",
    "deepseek_v4_flash_chat",
    "qwen35_chat_derivative",
    "qwen3_embedding",
    "bge_reranker",
    "qwen3_vl",
    "qwen25_omni_asr",
}


def require(condition: bool, message: str, errors: list[str]) -> None:
    if not condition:
        errors.append(message)


def lane_shares(total: int, derivative_weight: int, wiki_weight: int) -> tuple[int, int]:
    if total <= 0:
        return 0, 0
    if total == 1:
        return 1, 1
    weight_total = derivative_weight + wiki_weight
    derivative = (total * derivative_weight + weight_total - 1) // weight_total
    derivative = min(max(derivative, 1), total - 1)
    wiki = total - derivative
    return derivative, wiki


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument(
        "plan",
        nargs="?",
        type=Path,
        default=Path(__file__).with_name("concurrency-plan.json"),
    )
    parser.add_argument(
        "--require-all-model-evidence",
        action="store_true",
        help="retained for release-command compatibility; schema v2 always requires all evidence",
    )
    args = parser.parse_args()
    plan = json.loads(args.plan.read_text(encoding="utf-8"))
    errors: list[str] = []
    require(plan.get("schema_version") == 2, "schema_version must be 2", errors)

    pipeline = plan["document_pipeline"]
    scheduler = plan["model_scheduler"]
    parse_slots = pipeline["parse_replicas"] * pipeline["document_workflows_per_parse_replica"]
    docreader_slots = pipeline["docreader_replicas"] * pipeline["docreader_workers_per_replica"]
    split_slots = pipeline["parse_replicas"] * pipeline["split_workers_per_parse_replica"]
    multimodal_consumers = (
        pipeline["parse_replicas"] * pipeline["multimodal_consumers_per_parse_replica"]
    )
    embedding_consumers = (
        pipeline["parse_replicas"] * pipeline["embedding_pool_size_per_parse_replica"]
    )
    derivative_consumers = (
        pipeline["derivative_replicas"] * pipeline["derivative_consumers_per_replica"]
    )
    wiki_consumers = pipeline["wiki_replicas"] * pipeline["wiki_consumers_per_replica"]

    require(parse_slots == 12, f"document admission is {parse_slots}, expected 12", errors)
    require(
        docreader_slots == parse_slots,
        f"DocReader capacity {docreader_slots} must equal document admission {parse_slots}",
        errors,
    )
    require(
        split_slots == parse_slots,
        f"split capacity {split_slots} must equal document admission {parse_slots}",
        errors,
    )
    require(
        pipeline["split_per_document_window"] == pipeline["split_workers_per_parse_replica"],
        "per-document split window must match each parse replica's split workers",
        errors,
    )
    require(pipeline["embedding_batch_size"] == 5, "embedding batch size must be 5", errors)

    routes = {route.get("key"): route for route in plan["model_routes"]}
    require(set(routes) == EXPECTED_MODELS, "the seven measured route keys must be exact", errors)
    repo_root = args.plan.resolve().parents[2]
    for key, route in routes.items():
        require(route.get("evidence_status") == "verified", f"{key} evidence is not verified", errors)
        evidence = repo_root / str(route.get("evidence", ""))
        require(evidence.is_file(), f"{key} evidence file is missing: {evidence}", errors)
        target = route["target"]
        expected = math.floor(route["verified_concurrency"] * route["target_fraction"])
        require(
            target["max_inflight"] == expected,
            f"{key} target {target['max_inflight']} differs from evidence fraction {expected}",
            errors,
        )
        require(
            target["max_background_inflight"]
            == target["max_inflight"] - target["interactive_reserve"],
            f"{key} background ceiling is inconsistent",
            errors,
        )

    qwen = routes["qwen27_chat"]["target"]
    v4 = routes["deepseek_v4_flash_chat"]["target"]
    constraints = plan["constraints"]
    running = qwen["max_inflight"] + v4["max_inflight"]
    waiting = qwen["chat_max_waiting"] + v4["chat_max_waiting"]
    require(running == 48, f"primary chat running total is {running}, expected 48", errors)
    require(waiting == 52, f"primary chat waiting total is {waiting}, expected 52", errors)
    require(running + waiting == 100, "primary chat admitted total must be 100", errors)
    require(running == constraints["primary_chat_running_limit"], "running constraint differs", errors)
    require(waiting == constraints["primary_chat_waiting_limit"], "waiting constraint differs", errors)
    require(
        running + waiting == constraints["primary_chat_admitted_limit"],
        "admitted constraint differs",
        errors,
    )

    derivative_route = routes["qwen35_chat_derivative"]["target"]
    background = derivative_route["max_background_inflight"]
    work_window = background * scheduler["prefetch_factor"]
    derivative_share, wiki_share = lane_shares(
        work_window, scheduler["derivative_weight"], scheduler["wiki_weight"]
    )
    provider_derivative, provider_wiki = lane_shares(
        background, scheduler["derivative_weight"], scheduler["wiki_weight"]
    )
    require(work_window == 48, f"background task window is {work_window}, expected 48", errors)
    require(
        (provider_derivative, provider_wiki) == (18, 6),
        f"provider protected shares are {(provider_derivative, provider_wiki)}, expected (18, 6)",
        errors,
    )
    require(
        derivative_consumers == derivative_share,
        f"derivative consumers {derivative_consumers} must equal task share {derivative_share}",
        errors,
    )
    require(
        wiki_consumers == wiki_share,
        f"Wiki consumers {wiki_consumers} must equal task share {wiki_share}",
        errors,
    )
    require(
        multimodal_consumers <= routes["qwen3_vl"]["target"]["max_inflight"],
        "multimodal consumers exceed the VLM provider ceiling",
        errors,
    )
    require(
        embedding_consumers <= routes["qwen3_embedding"]["target"]["max_inflight"],
        "embedding worker pools exceed the embedding provider ceiling",
        errors,
    )

    db = plan["database_connections"]
    steady_connections = sum(
        item["replicas"] * item["max_open_per_replica"]
        for item in db["steady_components"]
    )
    overlap_connections = steady_connections + db["migration_job_max_open"]
    max_connections = constraints["postgres_max_connections"]
    connection_budget = math.floor(
        max_connections * constraints["postgres_connection_budget_percent"] / 100
    )
    require(steady_connections == 55, f"steady DB pool total is {steady_connections}, expected 55", errors)
    require(
        overlap_connections <= connection_budget,
        f"worst DB pool total {overlap_connections} exceeds budget {connection_budget}",
        errors,
    )

    expected_node_totals = {
        "10.14.201.1": (4130, 8400),
        "10.14.201.2": (4130, 8400),
        "10.14.201.7": (3580, 6800),
        "10.14.201.6": (5110, 11316),
        "10.14.201.54": (2610, 3636),
    }
    for node in plan["nodes"]:
        cpu_request = node["fixed_cluster_requests"]["cpu_m"] + sum(
            item["cpu_request_m"] for item in node["workloads"]
        )
        memory_request = node["fixed_cluster_requests"]["memory_mib"] + sum(
            item["memory_request_mib"] for item in node["workloads"]
        )
        cpu_limit = sum(item["cpu_limit_m"] for item in node["workloads"])
        memory_limit = sum(item["memory_limit_mib"] for item in node["workloads"])
        expected = expected_node_totals[node["name"]]
        require(
            (cpu_request, memory_request) == expected,
            f"{node['name']} requests {(cpu_request, memory_request)} differ from {expected}",
            errors,
        )
        require(cpu_request <= node["allocatable"]["cpu_m"], f"{node['name']} CPU requests exceed allocatable", errors)
        require(memory_request <= node["allocatable"]["memory_mib"], f"{node['name']} memory requests exceed allocatable", errors)
        cpu_pct = cpu_request / node["allocatable"]["cpu_m"] * 100
        memory_pct = memory_request / node["allocatable"]["memory_mib"] * 100
        print(
            f"{node['name']}: requests={cpu_request}m/{memory_request:.0f}Mi "
            f"({cpu_pct:.1f}% CPU, {memory_pct:.1f}% memory); "
            f"workload-limits={cpu_limit}m/{memory_limit:.0f}Mi"
        )

    if errors:
        for error in errors:
            print(f"ERROR: {error}")
        return 1
    print(
        f"OK: document={parse_slots}, derivative/wiki={derivative_consumers}/{wiki_consumers}, "
        f"multimodal/embedding={multimodal_consumers}/{embedding_consumers}, "
        f"chat={running}+{waiting}=100, DB={steady_connections}/{overlap_connections}/{max_connections}"
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
