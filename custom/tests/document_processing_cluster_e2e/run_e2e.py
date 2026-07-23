from __future__ import annotations

import argparse
import json
import os
import sys
import time
from dataclasses import asdict
from pathlib import Path

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import (  # type: ignore
        APIClient,
        build_workload_profile,
        ClusterE2ERunner,
        DockerController,
        E2EFailure,
        JsonlRecorder,
        load_json_object,
        utc_now,
        validate_baseline_workload,
        validate_instance_topology,
        validate_performance,
        workload_profile_fingerprint,
    )
else:
    from .cluster_e2e import (
        APIClient,
        build_workload_profile,
        ClusterE2ERunner,
        DockerController,
        E2EFailure,
        JsonlRecorder,
        load_json_object,
        utc_now,
        validate_baseline_workload,
        validate_instance_topology,
        validate_performance,
        workload_profile_fingerprint,
    )


def csv_values(values: list[str]) -> list[str]:
    result: list[str] = []
    for value in values:
        result.extend(part.strip() for part in value.split(",") if part.strip())
    return result


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description="API-first load, failover, race and derivation E2E for WeKnora document workers"
    )
    p.add_argument("--base-url", default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"))
    p.add_argument("--token", default=os.getenv("WEKNORA_E2E_TOKEN", ""))
    p.add_argument("--auth-mode", choices=("api-key", "bearer"), default=os.getenv("WEKNORA_E2E_AUTH_MODE", "api-key"))
    p.add_argument("--kb-id", default=os.getenv("WEKNORA_E2E_KB_ID", ""))
    p.add_argument("--documents", type=int, default=20)
    p.add_argument("--upload-concurrency", type=int, default=16)
    p.add_argument(
        "--generated-size-kib",
        type=int,
        default=8,
        help="target size for generated Markdown documents; ignored when --fixture is used",
    )
    p.add_argument("--poll-interval", type=float, default=2.0)
    p.add_argument("--timeout", type=float, default=1800.0)
    p.add_argument("--http-timeout", type=float, default=60.0)
    p.add_argument(
        "--queue-status-path",
        default=os.getenv("WEKNORA_E2E_QUEUE_STATUS_PATH", "/api/v1/custom/document-queue/status"),
    )
    p.add_argument(
        "--instances-path",
        default=os.getenv("WEKNORA_E2E_INSTANCES_PATH", "/api/v1/custom/document-queue/instances"),
    )
    p.add_argument("--process-config", type=Path, help="JSON per-upload process overrides")
    p.add_argument("--fixture", action="append", default=[], type=Path)
    p.add_argument(
        "--expect-derived",
        action="append",
        default=[],
        help="comma-separated: summary,questions,graph,wiki,multimodal,table",
    )
    p.add_argument(
        "--expect-chunk-text",
        action="append",
        default=[],
        help="case-insensitive literal that must occur in persisted chunks; repeatable",
    )
    p.add_argument("--verify-sample", type=int, default=3)
    p.add_argument("--wiki-timeout", type=float, default=1800.0)
    p.add_argument("--min-throughput", type=float, default=0.0)
    p.add_argument("--max-p95-processing-seconds", type=float, default=0.0)
    p.add_argument("--baseline-report", type=Path)
    p.add_argument(
        "--instance-count",
        type=int,
        default=0,
        help=(
            "assert this many healthy-ready instances are visible at both measurement boundaries; "
            "0 auto-discovers from the instances API"
        ),
    )
    p.add_argument(
        "--expected-instance-concurrency",
        type=int,
        default=0,
        help="when positive, assert admin setting and every healthy instance capacity match this value",
    )
    p.add_argument("--min-scaling-efficiency", type=float, default=0.0)
    p.add_argument("--worker-container", action="append", default=[])
    p.add_argument("--fault-target", default="")
    p.add_argument("--allow-chaos", action="store_true")
    p.add_argument("--hard-kill", action="store_true")
    p.add_argument("--down-seconds", type=float, default=15.0)
    p.add_argument("--takeover-timeout", type=float, default=120.0)
    p.add_argument("--no-restart", action="store_true")
    p.add_argument("--restart-race", action="store_true")
    p.add_argument(
        "--pause-race",
        action="store_true",
        help="docker-pause a live worker through lease expiry, then revive its stale handler",
    )
    p.add_argument("--race-pause-seconds", type=float, default=25.0)
    p.add_argument("--skip-card-contract", action="store_true")
    p.add_argument("--keep-data", action="store_true")
    p.add_argument("--output-dir", type=Path, default=Path("custom/tests/document_processing_cluster_e2e/outputs"))
    return p


def main() -> int:
    args = parser().parse_args()
    if not args.token:
        print("ERROR: --token or WEKNORA_E2E_TOKEN is required", file=sys.stderr)
        return 2
    if not args.kb_id:
        print("ERROR: --kb-id or WEKNORA_E2E_KB_ID is required", file=sys.stderr)
        return 2
    if args.worker_container and not args.allow_chaos:
        print("ERROR: --worker-container requires --allow-chaos", file=sys.stderr)
        return 2
    if args.restart_race and args.pause_race:
        print("ERROR: choose only one of --restart-race and --pause-race", file=sys.stderr)
        return 2
    if args.instance_count < 0:
        print("ERROR: --instance-count cannot be negative", file=sys.stderr)
        return 2
    if args.min_scaling_efficiency > 0 and not args.baseline_report:
        print("ERROR: --min-scaling-efficiency requires --baseline-report", file=sys.stderr)
        return 2

    expected = set(csv_values(args.expect_derived))
    unknown = expected - {"summary", "questions", "graph", "wiki", "multimodal", "table"}
    if unknown:
        print(f"ERROR: unknown --expect-derived values: {sorted(unknown)}", file=sys.stderr)
        return 2
    topology_required = bool(
        args.baseline_report
        or args.instance_count > 0
        or args.expected_instance_concurrency > 0
        or args.min_scaling_efficiency > 0
    )

    run_stamp = time.strftime("%Y%m%d-%H%M%S")
    run_dir = args.output_dir / run_stamp
    recorder = JsonlRecorder(run_dir / "events.jsonl")
    client = APIClient(
        args.base_url,
        args.token,
        auth_mode=args.auth_mode,
        timeout=args.http_timeout,
        queue_status_path=args.queue_status_path,
        instances_path=args.instances_path,
    )
    runner = ClusterE2ERunner(client, args.kb_id, recorder, poll_interval=args.poll_interval)
    started_at = utc_now()
    report: dict[str, object] = {
        "run_id": runner.run_id,
        "started_at": started_at,
        "config": {
            "base_url": args.base_url,
            "kb_id": args.kb_id,
            "documents": args.documents,
            "upload_concurrency": args.upload_concurrency,
            "generated_size_kib": args.generated_size_kib,
            "expected_instance_count": args.instance_count or None,
            "expect_derived": csv_values(args.expect_derived),
            "expect_chunk_text": args.expect_chunk_text,
            "chaos": bool(args.worker_container),
        },
    }

    try:
        # Required ordering: prove the API/queue contract before creating workload.
        start_instances = runner.api_smoke(
            args.expected_instance_concurrency,
            require_instance_topology=topology_required,
        )
        # Fail before uploading hundreds of documents when the explicitly
        # requested fleet size is not actually live and healthy.
        report["instance_topology"] = validate_instance_topology(
            start_instances,
            start_instances,
            expected_count=args.instance_count,
            required=topology_required,
        )
        process_config = load_json_object(args.process_config)
        kb_snapshot = client.get_knowledge_base(args.kb_id)
        if args.pause_race:
            chaos_mode = "paused-owner-race"
        elif args.restart_race:
            chaos_mode = "restart-race"
        elif args.worker_container:
            chaos_mode = "worker-failover"
        else:
            chaos_mode = "none"
        chaos_config: dict[str, object] = {"enabled": bool(args.worker_container), "mode": chaos_mode}
        if args.worker_container:
            chaos_config.update(
                {
                    "hard_kill": bool(args.hard_kill),
                    "down_seconds": float(args.down_seconds),
                    "takeover_timeout_seconds": float(args.takeover_timeout),
                    "restart_after_fault": not args.no_restart,
                    "race_pause_seconds": float(args.race_pause_seconds),
                }
            )
        workload_profile = build_workload_profile(
            kb_id=args.kb_id,
            kb_snapshot=kb_snapshot,
            documents=args.documents,
            upload_concurrency=args.upload_concurrency,
            generated_size_kib=args.generated_size_kib,
            fixture_paths=args.fixture,
            process_config=process_config,
            expected_derivatives=expected,
            expected_chunk_text=args.expect_chunk_text,
            verify_sample=args.verify_sample,
            wiki_timeout=args.wiki_timeout,
            poll_interval=args.poll_interval,
            skip_card_contract=args.skip_card_contract,
            chaos_config=chaos_config,
        )
        report["workload_profile"] = workload_profile
        report["workload_fingerprint"] = workload_profile_fingerprint(workload_profile)
        if args.baseline_report:
            validate_baseline_workload(workload_profile, args.baseline_report)
        # Measure only the comparable workload.  API/KB preflight and baseline
        # file validation differ between the two commands and must not dilute
        # or penalize the scaled throughput measurement.
        workload_started_at = utc_now()
        started = time.monotonic()
        ids = runner.upload_batch(
            args.documents,
            upload_concurrency=args.upload_concurrency,
            process_config=process_config,
            fixture_paths=args.fixture,
            generated_size_kib=args.generated_size_kib,
        )
        initial = runner.sample_queue()
        runner.assert_queue_positions(initial)
        if not args.skip_card_contract:
            runner.assert_card_queue_fields(initial)

        if args.worker_container:
            target = args.fault_target or args.worker_container[0]
            if target not in args.worker_container:
                raise E2EFailure("--fault-target must also be listed as --worker-container")
            controller = DockerController(args.worker_container, recorder)
            if args.pause_race:
                runner.run_paused_owner_race(
                    controller,
                    target=target,
                    paused_seconds=args.down_seconds,
                )
            elif args.restart_race:
                runner.run_restart_race(
                    controller,
                    target=target,
                    pause_seconds=args.race_pause_seconds,
                )
            else:
                runner.run_worker_failover(
                    controller,
                    target=target,
                    hard_kill=args.hard_kill,
                    down_seconds=args.down_seconds,
                    takeover_timeout=args.takeover_timeout,
                    restart=not args.no_restart,
                )

        runner.wait_for_completion(args.timeout)
        runner.verify_document_outputs(
            ids,
            expected=expected,
            sample_limit=args.verify_sample,
            wiki_timeout=args.wiki_timeout,
            expected_chunk_text=args.expect_chunk_text,
        )
        result = runner.result(started, workload_started_at)
        # Persist raw measurements before applying acceptance thresholds. A
        # threshold failure is still valuable performance evidence and must not
        # collapse a completed 500-document run into an error-only report.
        report["result"] = asdict(result)
        recorder.emit("run.metrics", result=asdict(result))
        end_instances = client.get_instances(optional=not topology_required)
        instance_topology = validate_instance_topology(
            start_instances,
            end_instances,
            expected_count=args.instance_count,
            required=topology_required,
        )
        report["instance_topology"] = instance_topology
        scaling = validate_performance(
            result,
            min_throughput=args.min_throughput,
            max_p95_processing_seconds=args.max_p95_processing_seconds,
            baseline_report=args.baseline_report,
            min_scaling_efficiency=args.min_scaling_efficiency,
            workload_profile=workload_profile,
            instance_topology=instance_topology,
        )
        report.update({"status": "passed", "scaling": scaling})
        recorder.emit("run.passed", result=asdict(result), scaling=scaling)
        return_code = 0
    except Exception as exc:
        report.update({"status": "failed", "error": str(exc), "finished_at": utc_now()})
        recorder.emit("run.failed", error=str(exc), error_type=type(exc).__name__)
        return_code = 1
    finally:
        if runner.observations and not args.keep_data:
            try:
                runner.cleanup()
            except Exception as cleanup_exc:
                recorder.emit("cleanup.failed", error=str(cleanup_exc))
                report["cleanup_error"] = str(cleanup_exc)
        run_dir.mkdir(parents=True, exist_ok=True)
        (run_dir / "report.json").write_text(
            json.dumps(report, ensure_ascii=False, indent=2, default=str) + "\n",
            encoding="utf-8",
        )
        print(f"report: {(run_dir / 'report.json').resolve()}")
    return return_code


if __name__ == "__main__":
    raise SystemExit(main())
