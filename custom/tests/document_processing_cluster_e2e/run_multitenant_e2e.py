from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
from dataclasses import asdict
from pathlib import Path
from typing import Any

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import (  # type: ignore
        APIClient,
        ClusterE2ERunner,
        E2EFailure,
        JsonlRecorder,
        load_json_object,
        utc_now,
        validate_instance_topology,
    )
    from multitenant_e2e import (  # type: ignore
        EXACT_DEEPSEEK_NAME,
        EXACT_DEEPSEEK_SOURCE_ID,
        MultiTenantClusterRunner,
        TenantProvisioner,
        VariantFactory,
        scrape_metrics,
    )
else:
    from .cluster_e2e import (
        APIClient,
        ClusterE2ERunner,
        E2EFailure,
        JsonlRecorder,
        load_json_object,
        utc_now,
        validate_instance_topology,
    )
    from .multitenant_e2e import (
        EXACT_DEEPSEEK_NAME,
        EXACT_DEEPSEEK_SOURCE_ID,
        MultiTenantClusterRunner,
        TenantProvisioner,
        VariantFactory,
        scrape_metrics,
    )


HERE = Path(__file__).resolve().parent


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=(
            "1000+ mixed-format, multi-user and multi-KB horizontal document "
            "processing acceptance test"
        )
    )
    p.add_argument(
        "--base-url",
        default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"),
    )
    p.add_argument(
        "--admin-token",
        default=os.getenv("WEKNORA_E2E_ADMIN_TOKEN", ""),
        help="system-admin token; never written to the report",
    )
    p.add_argument(
        "--admin-auth-mode",
        choices=("bearer", "api-key"),
        default=os.getenv("WEKNORA_E2E_ADMIN_AUTH_MODE", "bearer"),
    )
    p.add_argument("--documents", type=int, default=1000)
    p.add_argument("--allow-small", action="store_true")
    p.add_argument("--principals", type=int, default=8)
    p.add_argument("--knowledge-bases-per-principal", type=int, default=2)
    p.add_argument("--upload-concurrency", type=int, default=64)
    p.add_argument("--verify-concurrency", type=int, default=32)
    p.add_argument("--poll-interval", type=float, default=5.0)
    p.add_argument("--timeout", type=float, default=21600.0)
    p.add_argument("--http-timeout", type=float, default=120.0)
    p.add_argument("--wiki-timeout", type=float, default=7200.0)
    p.add_argument("--retrieval-sample", type=int, default=128)
    p.add_argument("--instance-count", type=int, default=3)
    p.add_argument("--expected-instance-concurrency", type=int, default=4)
    p.add_argument(
        "--worker-container",
        action="append",
        default=[],
        help="repeat once per app/document-worker replica for per-process stage metrics",
    )
    p.add_argument(
        "--fixture-dir",
        type=Path,
        default=HERE / "format_fixtures",
    )
    p.add_argument(
        "--process-config",
        type=Path,
        default=HERE / "process_config.full.example.json",
    )
    p.add_argument("--small-kib", type=int, default=4)
    p.add_argument("--medium-kib", type=int, default=32)
    p.add_argument("--large-kib", type=int, default=128)
    p.add_argument(
        "--exact-chat-source-id",
        default=EXACT_DEEPSEEK_SOURCE_ID,
    )
    p.add_argument("--exact-chat-name", default=EXACT_DEEPSEEK_NAME)
    p.add_argument("--expected-chat-base-url", default=":14000")
    p.add_argument("--source-tenant-id", type=int, default=10000)
    p.add_argument("--keep-data", action="store_true")
    p.add_argument(
        "--output-dir",
        type=Path,
        default=HERE / "outputs" / "multitenant-1000",
    )
    return p


def validate_args(args: argparse.Namespace) -> None:
    if not args.admin_token:
        raise E2EFailure(
            "--admin-token or WEKNORA_E2E_ADMIN_TOKEN is required"
        )
    if args.documents <= 0:
        raise E2EFailure("--documents must be positive")
    if args.documents < 1000 and not args.allow_small:
        raise E2EFailure(
            "capacity acceptance requires at least 1000 documents; "
            "use --allow-small only for harness smoke tests"
        )
    if args.principals < 2:
        raise E2EFailure("--principals must be at least 2")
    if args.knowledge_bases_per_principal < 2:
        raise E2EFailure("--knowledge-bases-per-principal must be at least 2")
    for name in (
        "upload_concurrency",
        "verify_concurrency",
        "instance_count",
        "expected_instance_concurrency",
        "small_kib",
        "medium_kib",
        "large_kib",
    ):
        if getattr(args, name) <= 0:
            raise E2EFailure(f"--{name.replace('_', '-')} must be positive")
    if args.retrieval_sample <= 0:
        raise E2EFailure("--retrieval-sample must be positive")
    if len(args.worker_container) != args.instance_count:
        raise E2EFailure(
            "--worker-container must be supplied exactly once for every expected "
            f"instance ({args.instance_count})"
        )
    if not args.fixture_dir.is_dir():
        raise E2EFailure(f"fixture directory not found: {args.fixture_dir}")
    if args.process_config and not args.process_config.is_file():
        raise E2EFailure(f"process config not found: {args.process_config}")


def public_config(args: argparse.Namespace) -> dict[str, Any]:
    return {
        "base_url": args.base_url,
        "documents": args.documents,
        "principals": args.principals,
        "knowledge_bases_per_principal": args.knowledge_bases_per_principal,
        "upload_concurrency": args.upload_concurrency,
        "verify_concurrency": args.verify_concurrency,
        "timeout_seconds": args.timeout,
        "wiki_timeout_seconds": args.wiki_timeout,
        "retrieval_sample": args.retrieval_sample,
        "instance_count": args.instance_count,
        "expected_instance_concurrency": args.expected_instance_concurrency,
        "worker_containers": list(args.worker_container),
        "size_kib": {
            "small": args.small_kib,
            "medium": args.medium_kib,
            "large": args.large_kib,
        },
        "exact_chat_source_id": args.exact_chat_source_id,
        "exact_chat_name": args.exact_chat_name,
        "expected_chat_base_url_fragment": args.expected_chat_base_url,
        "source_tenant_id": args.source_tenant_id,
        "process_config_enabled": args.process_config is not None,
    }


def main() -> int:
    args = parser().parse_args()
    try:
        validate_args(args)
    except E2EFailure as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    run_stamp = time.strftime("%Y%m%d-%H%M%S")
    run_id = f"mt1000-{run_stamp}-{uuid.uuid4().hex[:6]}"
    run_dir = args.output_dir / run_stamp
    recorder = JsonlRecorder(run_dir / "events.jsonl")
    admin = APIClient(
        args.base_url,
        args.admin_token,
        auth_mode=args.admin_auth_mode,
        timeout=args.http_timeout,
    )
    provisioner = TenantProvisioner(
        admin,
        recorder,
        exact_chat_source_id=args.exact_chat_source_id,
        exact_chat_name=args.exact_chat_name,
        expected_chat_base_url=args.expected_chat_base_url,
        source_tenant_id=args.source_tenant_id,
    )
    principals = []
    factory: VariantFactory | None = None
    runner: MultiTenantClusterRunner | None = None
    report: dict[str, Any] = {
        "run_id": run_id,
        "started_at": utc_now(),
        "config": public_config(args),
    }
    started = time.monotonic()
    return_code = 1

    try:
        factory = VariantFactory(
            args.fixture_dir,
            small_kib=args.small_kib,
            medium_kib=args.medium_kib,
            large_kib=args.large_kib,
        )
        # Reuse the same API contract check as the single-tenant suite before
        # creating any disposable users or workload.
        smoke = ClusterE2ERunner(
            admin,
            "00000000-0000-0000-0000-000000000000",
            recorder,
            run_id=run_id,
            poll_interval=args.poll_interval,
        )
        start_instances = smoke.api_smoke(
            args.expected_instance_concurrency,
            require_instance_topology=True,
        )
        start_topology = validate_instance_topology(
            start_instances,
            start_instances,
            expected_count=args.instance_count,
            required=True,
        )
        expected_instances = [
            item["instance_id"]
            for item in start_topology["start"]["healthy_ready_instances"]
        ]
        if len(set(args.worker_container)) != len(args.worker_container):
            raise E2EFailure("--worker-container values must be unique")
        metrics_before = scrape_metrics(args.worker_container)
        principals = provisioner.provision(
            principal_count=args.principals,
            knowledge_bases_per_principal=args.knowledge_bases_per_principal,
            run_suffix=run_id,
        )
        runner = MultiTenantClusterRunner(
            admin,
            principals,
            factory,
            recorder,
            run_id=run_id,
            poll_interval=args.poll_interval,
            worker_containers=args.worker_container,
        )
        process_config = load_json_object(args.process_config)
        runner.upload(
            args.documents,
            concurrency=args.upload_concurrency,
            process_config=process_config,
        )
        runner.verify_cross_tenant_isolation()
        runner.wait_for_completion(args.timeout)
        verification = runner.verify_outputs(
            concurrency=args.verify_concurrency,
            wiki_timeout=args.wiki_timeout,
            retrieval_sample=args.retrieval_sample,
            expected_instances=expected_instances,
            metrics_before=metrics_before,
        )
        result = runner.result(started, report["started_at"])
        end_instances = admin.get_instances()
        topology = validate_instance_topology(
            start_instances,
            end_instances,
            expected_count=args.instance_count,
            required=True,
        )
        report.update(
            {
                "status": "passed",
                "result": asdict(result),
                "verification": verification,
                "instance_topology": topology,
                "principals": [principal.public() for principal in principals],
            }
        )
        recorder.emit(
            "multitenant.run_passed",
            result=asdict(result),
            instance_topology=topology,
        )
        return_code = 0
    except Exception as exc:
        report.update(
            {
                "status": "failed",
                "error": str(exc),
                "error_type": type(exc).__name__,
                "finished_at": utc_now(),
            }
        )
        recorder.emit(
            "multitenant.run_failed",
            error=str(exc),
            error_type=type(exc).__name__,
        )
    finally:
        cleanup_failures: list[str] = []
        if not args.keep_data:
            if runner is not None and runner.observations:
                cleanup_failures.extend(
                    f"document {failure}"
                    for failure in runner.cleanup_documents(args.verify_concurrency)
                )
            if principals:
                cleanup_failures.extend(provisioner.cleanup_principals(principals))
        if cleanup_failures:
            report["cleanup_failures"] = cleanup_failures
            recorder.emit(
                "multitenant.cleanup_failed",
                failures=cleanup_failures,
            )
        report.setdefault("finished_at", utc_now())
        run_dir.mkdir(parents=True, exist_ok=True)
        report_path = run_dir / "report.json"
        report_path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2, default=str) + "\n",
            encoding="utf-8",
        )
        print(f"report: {report_path.resolve()}")
    return return_code


if __name__ == "__main__":
    raise SystemExit(main())
