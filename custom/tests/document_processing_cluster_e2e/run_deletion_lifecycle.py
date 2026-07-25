from __future__ import annotations

import argparse
import json
import os
import sys
import time
import uuid
from pathlib import Path
from typing import Any, Mapping

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import (  # type: ignore
        APIClient,
        APIError,
        E2EFailure,
        source_refs_include,
        utc_now,
    )
else:
    from .cluster_e2e import (
        APIClient,
        APIError,
        E2EFailure,
        source_refs_include,
        utc_now,
    )


LIVE_STATUSES = {"pending", "processing", "finalizing"}
TERMINAL_FAILURES = {"failed", "cancelled", "canceled", "deleting"}
COMPLETED_DERIVATIVE_FIELDS = (
    "summary_status",
    "enrichment_status",
    "wiki_status",
)


def parser() -> argparse.ArgumentParser:
    result = argparse.ArgumentParser(
        description=(
            "Delete documents through the public API at pending, processing, "
            "finalizing, and fully-completed lifecycle boundaries."
        )
    )
    result.add_argument(
        "--base-url",
        default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"),
    )
    result.add_argument("--token", default=os.getenv("WEKNORA_E2E_TOKEN", ""))
    result.add_argument(
        "--auth-mode",
        choices=("api-key", "bearer"),
        default=os.getenv("WEKNORA_E2E_AUTH_MODE", "api-key"),
    )
    result.add_argument("--kb-id", default=os.getenv("WEKNORA_E2E_KB_ID", ""))
    result.add_argument(
        "--scenarios",
        default="pending,processing,finalizing,completed",
        help="comma-separated lifecycle boundaries",
    )
    result.add_argument("--poll-interval", type=float, default=0.2)
    result.add_argument("--stage-timeout", type=float, default=900.0)
    result.add_argument("--delete-timeout", type=float, default=300.0)
    result.add_argument("--http-timeout", type=float, default=120.0)
    result.add_argument("--size-kib", type=int, default=384)
    result.add_argument("--pending-attempts", type=int, default=5)
    result.add_argument(
        "--output-dir",
        type=Path,
        default=Path(
            "custom/tests/document_processing_cluster_e2e/deletion_lifecycle_outputs"
        ),
    )
    return result


def normalized_status(knowledge: Mapping[str, Any]) -> str:
    return str(knowledge.get("parse_status", "")).strip().lower()


def generated_document(marker: str, size_kib: int) -> bytes:
    heading = (
        f"# 文档删除生命周期测试 {marker}\n\n"
        "本文件仅用于验证排队、解析、衍生任务以及删除之间的并发状态机。\n\n"
    )
    paragraph = (
        f"{marker}：数据治理委员会负责审批生产数据访问；信息安全部负责复核，"
        "审计中心按季度检查执行记录。未经审批不得导出敏感数据。\n\n"
    )
    target = max(8, size_kib) * 1024
    body = heading
    section = 1
    while len(body.encode("utf-8")) < target:
        body += f"## 第 {section} 节\n\n{paragraph}"
        section += 1
    return body.encode("utf-8")


def process_config(marker: str) -> dict[str, Any]:
    return {
        "question_generation_config": {
            "enabled": True,
            "question_count": 1,
        },
        "graph_enabled": True,
        "extract_config": {
            "enabled": True,
            "text": (
                f"{marker} 规定数据治理委员会负责审批生产数据访问，"
                "信息安全部负责复核。"
            ),
            "tags": ["规定职责", "审批", "复核"],
            "nodes": [
                {"name": "数据治理委员会", "attributes": ["责任部门"]},
                {"name": "生产数据访问", "attributes": ["管理事项"]},
                {"name": "信息安全部", "attributes": ["复核部门"]},
            ],
            "relations": [
                {
                    "node1": "数据治理委员会",
                    "node2": "生产数据访问",
                    "type": "审批",
                },
                {
                    "node1": "信息安全部",
                    "node2": "生产数据访问",
                    "type": "复核",
                },
            ],
        },
    }


def get_optional(client: APIClient, knowledge_id: str) -> Mapping[str, Any] | None:
    try:
        return client.get_knowledge(knowledge_id)
    except APIError as exc:
        if exc.status == 404:
            return None
        raise


def completed_with_derivatives(knowledge: Mapping[str, Any]) -> bool:
    if normalized_status(knowledge) != "completed":
        return False
    return all(
        str(knowledge.get(field, "")).strip().lower() == "completed"
        for field in COMPLETED_DERIVATIVE_FIELDS
    )


def wait_for_boundary(
    client: APIClient,
    knowledge_id: str,
    boundary: str,
    timeout: float,
    poll_interval: float,
) -> Mapping[str, Any] | None:
    deadline = time.monotonic() + timeout
    seen: list[str] = []
    while time.monotonic() < deadline:
        knowledge = get_optional(client, knowledge_id)
        if knowledge is None:
            raise E2EFailure(
                f"{knowledge_id} disappeared before reaching {boundary}"
            )
        status = normalized_status(knowledge)
        if not seen or seen[-1] != status:
            seen.append(status)
        if boundary == "completed":
            if completed_with_derivatives(knowledge):
                return knowledge
        elif status == boundary:
            return knowledge
        if status in TERMINAL_FAILURES:
            raise E2EFailure(
                f"{knowledge_id} reached {status} before {boundary}; seen={seen}"
            )
        if boundary == "pending" and status != "pending":
            return None
        if boundary == "processing" and status in {"finalizing", "completed"}:
            return None
        if boundary == "finalizing" and status == "completed":
            return None
        time.sleep(poll_interval)
    raise E2EFailure(
        f"{knowledge_id} did not reach {boundary} within {timeout}s; seen={seen}"
    )


def wait_deleted(
    client: APIClient,
    kb_id: str,
    knowledge_id: str,
    marker: str,
    timeout: float,
    poll_interval: float,
) -> dict[str, Any]:
    started = time.monotonic()
    deadline = started + timeout
    last_status = ""
    while time.monotonic() < deadline:
        knowledge = get_optional(client, knowledge_id)
        if knowledge is None:
            break
        last_status = normalized_status(knowledge)
        time.sleep(max(0.2, poll_interval))
    else:
        raise E2EFailure(
            f"{knowledge_id} remained visible after delete; status={last_status}"
        )

    queue = client.get_queue([knowledge_id])
    queue_item = queue.items.get(knowledge_id)
    if queue_item is not None and queue_item.state in {
        "waiting",
        "pending",
        "queued",
        "active",
        "processing",
        "running",
        "finalizing",
        "retry",
        "scheduled",
    }:
        raise E2EFailure(
            f"{knowledge_id} retained live queue state {queue_item.state}"
        )

    try:
        chunks = client.list_chunks(
            knowledge_id,
            (
                "text",
                "image",
                "image_ocr",
                "image_caption",
                "table",
                "table_summary",
                "table_column",
                "question",
                "summary",
            ),
        )
    except APIError as exc:
        if exc.status != 404:
            raise
        chunks = []
    if chunks:
        raise E2EFailure(
            f"{knowledge_id} still exposes {len(chunks)} chunks after delete"
        )

    try:
        search = client.hybrid_search(kb_id, marker, [knowledge_id])
    except APIError as exc:
        if exc.status not in {400, 404}:
            raise
        search = []
    if search:
        raise E2EFailure(
            f"{knowledge_id} still returns {len(search)} retrieval result(s)"
        )

    pages = client.list_wiki_pages(kb_id)
    leaked_pages = [
        str(page.get("id", ""))
        for page in pages
        if source_refs_include(page, knowledge_id)
    ]
    if leaked_pages:
        raise E2EFailure(
            f"{knowledge_id} remains in Wiki source refs: {leaked_pages[:10]}"
        )
    return {
        "delete_seconds": time.monotonic() - started,
        "queue_state_after_delete": (
            queue_item.state if queue_item is not None else "absent"
        ),
        "chunks_after_delete": 0,
        "retrieval_results_after_delete": 0,
        "wiki_source_pages_after_delete": 0,
    }


def upload_for_boundary(
    client: APIClient,
    kb_id: str,
    boundary: str,
    *,
    size_kib: int,
    pending_attempts: int,
    stage_timeout: float,
    poll_interval: float,
) -> tuple[str, str, Mapping[str, Any], int]:
    attempts = pending_attempts if boundary == "pending" else 3
    for attempt in range(1, attempts + 1):
        marker = (
            f"WKN-DELETE-{boundary.upper()}-"
            f"{time.strftime('%Y%m%d%H%M%S')}-{uuid.uuid4().hex[:10]}"
        )
        content_size = 8 if boundary == "pending" else size_kib * attempt
        uploaded = client.upload_document(
            kb_id,
            f"{marker}.md",
            generated_document(marker, content_size),
            process_config=process_config(marker),
            metadata={
                "e2e_case": "document-deletion-lifecycle",
                "e2e_boundary": boundary,
                "e2e_marker": marker,
            },
        )
        knowledge_id = str(uploaded["id"])
        reached = wait_for_boundary(
            client,
            knowledge_id,
            boundary,
            stage_timeout,
            poll_interval,
        )
        if reached is not None:
            return knowledge_id, marker, reached, attempt
        # The stage advanced between upload and observation. Delete this
        # disposable attempt through the same public path before retrying with
        # a larger fixture; never leave untracked work behind.
        client.delete_knowledge(knowledge_id)
        wait_deleted(
            client,
            kb_id,
            knowledge_id,
            marker,
            300,
            poll_interval,
        )
    raise E2EFailure(
        f"could not observe exact {boundary} state after {attempts} attempt(s)"
    )


def main() -> int:
    args = parser().parse_args()
    if not args.token or not args.kb_id:
        print(
            "ERROR: --token/WEKNORA_E2E_TOKEN and --kb-id/WEKNORA_E2E_KB_ID "
            "are required",
            file=sys.stderr,
        )
        return 2
    if (
        args.poll_interval <= 0
        or args.stage_timeout <= 0
        or args.delete_timeout <= 0
        or args.size_kib <= 0
        or args.pending_attempts <= 0
    ):
        print("ERROR: timeout, interval, size, and attempts must be positive", file=sys.stderr)
        return 2
    scenarios = [
        value.strip().lower()
        for value in args.scenarios.split(",")
        if value.strip()
    ]
    unknown = set(scenarios) - (LIVE_STATUSES | {"completed"})
    if not scenarios or unknown or len(set(scenarios)) != len(scenarios):
        print(
            f"ERROR: invalid or duplicate lifecycle scenarios: {scenarios}",
            file=sys.stderr,
        )
        return 2

    run_stamp = time.strftime("%Y%m%d-%H%M%S")
    run_dir = args.output_dir / run_stamp
    run_dir.mkdir(parents=True, exist_ok=False)
    report_path = run_dir / "deletion_lifecycle_report.json"
    report: dict[str, Any] = {
        "status": "running",
        "started_at": utc_now(),
        "knowledge_base_id": args.kb_id,
        "scenarios": scenarios,
        "results": [],
    }
    client = APIClient(
        args.base_url,
        args.token,
        auth_mode=args.auth_mode,
        timeout=args.http_timeout,
    )
    try:
        client.get_knowledge_base(args.kb_id)
        for boundary in scenarios:
            knowledge_id, marker, before, attempt = upload_for_boundary(
                client,
                args.kb_id,
                boundary,
                size_kib=args.size_kib,
                pending_attempts=args.pending_attempts,
                stage_timeout=args.stage_timeout,
                poll_interval=args.poll_interval,
            )
            queue_before = client.get_queue([knowledge_id]).items.get(knowledge_id)
            observed = {
                "parse_status": normalized_status(before),
                "summary_status": str(before.get("summary_status", "")),
                "enrichment_status": str(before.get("enrichment_status", "")),
                "wiki_status": str(before.get("wiki_status", "")),
                "queue_state": (
                    queue_before.state if queue_before is not None else "absent"
                ),
                "queue_owner": (
                    queue_before.owner_instance_id if queue_before is not None else ""
                ),
                "queue_epoch": (
                    queue_before.execution_epoch if queue_before is not None else None
                ),
            }
            client.delete_knowledge(knowledge_id)
            after = wait_deleted(
                client,
                args.kb_id,
                knowledge_id,
                marker,
                args.delete_timeout,
                args.poll_interval,
            )
            result = {
                "boundary": boundary,
                "knowledge_id": knowledge_id,
                "attempt": attempt,
                "observed_before_delete": observed,
                **after,
            }
            report["results"].append(result)
            report_path.write_text(
                json.dumps(report, ensure_ascii=False, indent=2),
                encoding="utf-8",
            )
            print(json.dumps(result, ensure_ascii=False), flush=True)
        report["status"] = "passed"
        report["finished_at"] = utc_now()
        report_path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"PASS: {report_path}", flush=True)
        return 0
    except Exception as exc:
        report["status"] = "failed"
        report["finished_at"] = utc_now()
        report["error"] = str(exc)
        report_path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2),
            encoding="utf-8",
        )
        print(f"ERROR: {exc}", file=sys.stderr)
        print(f"REPORT: {report_path}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
