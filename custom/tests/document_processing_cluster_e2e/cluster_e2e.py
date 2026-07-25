from __future__ import annotations

import hashlib
import json
import math
import mimetypes
import os
import re
import statistics
import subprocess
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass, field
from datetime import datetime, timezone
from difflib import SequenceMatcher
from pathlib import Path
from typing import Any, Iterable, Mapping, Sequence


TERMINAL_STATUSES = {"completed", "failed", "cancelled", "canceled"}
NON_TERMINAL_STATUSES = {"pending", "processing", "finalizing", "waiting", "active"}
FAILED_STAGE_STATUSES = {"failed", "error", "cancelled", "canceled", "degraded"}
SUPPORTED_FIXTURE_SUFFIXES = {
    ".txt",
    ".text",
    ".md",
    ".markdown",
    ".csv",
    ".json",
    ".pdf",
    ".doc",
    ".docx",
    ".xls",
    ".xlsx",
    ".ppt",
    ".pptx",
    ".epub",
    ".mhtml",
    ".png",
    ".jpg",
    ".jpeg",
    ".gif",
    ".bmp",
    ".tiff",
    ".webp",
    ".mp3",
    ".wav",
    ".m4a",
    ".flac",
    ".ogg",
}

QUESTION_SOURCE_METADATA_RE = re.compile(
    r"(?:"
    r"^\s*(?:根据|依据|参照|按照)\s*《[^》]+》|"
    r"^\s*(?:根据|依据|参照)\s*[^，。！？?]{0,40}(?:制度|规定|办法|细则)(?:中|的|，|,)|"
    r"原文件\s*第|原文\s*第|"
    r"第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*(?:页|组|段|chunk|分片)|"
    r"第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*条\s*(?:规定的?)?\s*(?:内容|要求|是什么|有哪些)|"
    r"(?:根据|参照)\s*(?:第\s*(?:\d+|[一二三四五六七八九十百千]+)\s*(?:页|组|段)|"
    r"上述(?:文档|片段|内容)|本(?:文|段|片段))|"
    r"制度原文(?:中|里)?|"
    r"(?:该|此|上述)\s*(?:文档|片段|chunk)\s*(?:中|里)?"
    r")",
    re.IGNORECASE,
)

WORKLOAD_PROFILE_SCHEMA_VERSION = 1
GENERATED_WORKLOAD_TEMPLATE = "horizontal-processing-markdown-v1"
KB_WORKLOAD_FIELDS = (
    "type",
    "chunking_config",
    "image_processing_config",
    "embedding_model_id",
    "summary_model_id",
    "vlm_config",
    "asr_config",
    "storage_provider_config",
    "storage_config",
    "vector_store_id",
    "extract_config",
    "question_generation_config",
    "wiki_config",
    "indexing_strategy",
)
DERIVATIVE_NAMES = {"summary", "questions", "graph", "wiki", "multimodal", "table"}


class E2EFailure(RuntimeError):
    """Raised when an acceptance invariant is violated."""


class APIError(E2EFailure):
    def __init__(self, method: str, url: str, status: int, body: str):
        super().__init__(f"{method} {url} returned HTTP {status}: {body[:1000]}")
        self.method = method
        self.url = url
        self.status = status
        self.body = body


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def unwrap_data(payload: Any) -> Any:
    if isinstance(payload, Mapping) and "data" in payload:
        return payload["data"]
    return payload


def first_value(source: Mapping[str, Any], names: Sequence[str], default: Any = None) -> Any:
    for name in names:
        if name in source and source[name] is not None:
            return source[name]
    return default


def to_int(value: Any, default: int = 0) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def percentile(values: Sequence[float], percent: float) -> float | None:
    if not values:
        return None
    ordered = sorted(float(v) for v in values)
    if len(ordered) == 1:
        return ordered[0]
    rank = (len(ordered) - 1) * percent / 100.0
    lower = math.floor(rank)
    upper = math.ceil(rank)
    if lower == upper:
        return ordered[lower]
    fraction = rank - lower
    return ordered[lower] * (1.0 - fraction) + ordered[upper] * fraction


def canonical_json_sha256(value: Any) -> str:
    encoded = json.dumps(
        value,
        ensure_ascii=False,
        sort_keys=True,
        separators=(",", ":"),
    ).encode("utf-8")
    return hashlib.sha256(encoded).hexdigest()


def file_sha256(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1024 * 1024), b""):
            digest.update(block)
    return digest.hexdigest()


def load_fixture_expectations(
    path: Path,
    fixture_paths: Sequence[Path],
) -> dict[str, dict[str, Any]]:
    try:
        payload = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise E2EFailure(f"cannot load fixture manifest {path}: {exc}") from exc
    raw_files = payload.get("files", []) if isinstance(payload, Mapping) else []
    if not isinstance(raw_files, list):
        raise E2EFailure("fixture manifest files must be an array")
    expectations: dict[str, dict[str, Any]] = {}
    for raw in raw_files:
        if not isinstance(raw, Mapping):
            raise E2EFailure("fixture manifest entries must be objects")
        filename = str(raw.get("filename", "")).strip()
        if not filename or filename in expectations or Path(filename).name != filename:
            raise E2EFailure(f"fixture manifest has invalid/duplicate filename: {filename!r}")
        expected = {
            str(value).strip().lower()
            for value in raw.get("expected_derivatives", [])
            if str(value).strip()
        } if isinstance(raw.get("expected_derivatives", []), list) else set()
        unknown = expected - DERIVATIVE_NAMES
        if unknown:
            raise E2EFailure(f"fixture {filename!r} has unknown derivatives: {sorted(unknown)}")
        expected_text = str(
            first_value(raw, ("expected_chunk_text", "marker"), "")
        ).strip()
        expectations[filename] = {
            "expected_derivatives": expected,
            "expected_chunk_text": (expected_text,) if expected_text else (),
        }
    fixture_names = {fixture.name for fixture in fixture_paths}
    if set(expectations) != fixture_names:
        missing = sorted(fixture_names - set(expectations))
        extra = sorted(set(expectations) - fixture_names)
        raise E2EFailure(f"fixture manifest mismatch: missing={missing}, extra={extra}")
    return expectations


def fixture_expectation_for_filename(
    uploaded_filename: str,
    expectations: Mapping[str, Mapping[str, Any]],
) -> Mapping[str, Any]:
    if not expectations:
        return {}
    matches = [
        expectation
        for filename, expectation in expectations.items()
        if uploaded_filename == filename or uploaded_filename.endswith("-" + filename)
    ]
    if len(matches) != 1:
        raise E2EFailure(
            f"uploaded fixture {uploaded_filename!r} maps to {len(matches)} manifest entries"
        )
    return matches[0]


def build_workload_profile(
    *,
    kb_id: str,
    kb_snapshot: Mapping[str, Any],
    documents: int,
    upload_concurrency: int,
    generated_size_kib: int,
    fixture_paths: Sequence[Path],
    process_config: Mapping[str, Any] | None,
    expected_derivatives: Iterable[str],
    expected_chunk_text: Sequence[str],
    verify_sample: int,
    wiki_timeout: float,
    poll_interval: float,
    skip_card_contract: bool,
    question_retrieval_sample: int = 3,
    chaos_config: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    """Build the immutable workload identity used for scaling comparison.

    Paths and raw process/KB configuration are deliberately not persisted.
    Fixture content and configuration are represented by SHA-256 fingerprints,
    which makes copied fixtures comparable without leaking parser tokens or
    legacy inline model credentials into reports.
    """

    if documents <= 0:
        raise E2EFailure("document count must be positive")
    if upload_concurrency <= 0:
        raise E2EFailure("upload concurrency must be positive")
    fixtures = list(fixture_paths)
    rejected = [str(path) for path in fixtures if path.suffix.lower() not in SUPPORTED_FIXTURE_SUFFIXES]
    if rejected:
        raise E2EFailure(f"unsupported fixture suffixes: {rejected}")
    if fixtures and documents > len(fixtures):
        raise E2EFailure("binary/format fixtures are uploaded once each; count must not exceed fixture count")

    if fixtures:
        fixture_descriptors: list[dict[str, Any]] = []
        for path in fixtures[:documents]:
            stat = path.stat()
            fixture_descriptors.append(
                {
                    "name": path.name,
                    "suffix": path.suffix.lower(),
                    "size_bytes": stat.st_size,
                    "sha256": file_sha256(path),
                }
            )
        input_profile: dict[str, Any] = {
            "kind": "fixtures",
            "fixtures": fixture_descriptors,
            "total_size_bytes": sum(item["size_bytes"] for item in fixture_descriptors),
        }
    else:
        if generated_size_kib <= 0:
            raise E2EFailure("generated document size must be positive")
        input_profile = {
            "kind": "generated_markdown",
            "template": GENERATED_WORKLOAD_TEMPLATE,
            "target_size_kib_per_document": generated_size_kib,
        }

    normalized_derivatives = sorted({str(value).strip().lower() for value in expected_derivatives if str(value).strip()})
    normalized_chunk_text = sorted({str(value).strip().casefold() for value in expected_chunk_text if str(value).strip()})
    effective_process_config: Mapping[str, Any] | None = process_config if process_config else None
    kb_config = {field: kb_snapshot.get(field) for field in KB_WORKLOAD_FIELDS}
    chaos = dict(chaos_config or {"enabled": False})

    return {
        "schema_version": WORKLOAD_PROFILE_SCHEMA_VERSION,
        "documents": documents,
        "input": input_profile,
        "upload_concurrency": upload_concurrency,
        "process_config_present": effective_process_config is not None,
        "process_config_sha256": canonical_json_sha256(effective_process_config),
        "expected_derivatives": normalized_derivatives,
        "expected_chunk_text": normalized_chunk_text,
        "verification": {
            "sample_documents": min(documents, max(1, verify_sample)),
            "wiki_timeout_seconds": float(wiki_timeout) if "wiki" in normalized_derivatives else None,
            "question_retrieval_sample": (
                max(0, int(question_retrieval_sample))
                if "questions" in normalized_derivatives
                else None
            ),
            "poll_interval_seconds": float(poll_interval),
            "card_contract_enabled": not skip_card_contract,
        },
        "knowledge_base": {
            "id": kb_id,
            "config_sha256": canonical_json_sha256(kb_config),
        },
        "chaos": chaos,
    }


def workload_profile_fingerprint(profile: Mapping[str, Any]) -> str:
    return canonical_json_sha256(profile)


def workload_profile_differences(
    baseline: Any,
    current: Any,
    *,
    path: str = "workload_profile",
) -> list[str]:
    """Return precise, stable paths for fields that make two runs incomparable."""

    if isinstance(baseline, Mapping) and isinstance(current, Mapping):
        differences: list[str] = []
        for key in sorted(set(baseline) | set(current)):
            child_path = f"{path}.{key}"
            if key not in baseline:
                differences.append(f"{child_path}: missing from baseline")
            elif key not in current:
                differences.append(f"{child_path}: missing from current run")
            else:
                differences.extend(workload_profile_differences(baseline[key], current[key], path=child_path))
        return differences
    if isinstance(baseline, list) and isinstance(current, list):
        if baseline == current:
            return []
        return [f"{path}: baseline={baseline!r} current={current!r}"]
    if baseline != current:
        return [f"{path}: baseline={baseline!r} current={current!r}"]
    return []


def missing_chunk_texts(
    chunks: Sequence[Mapping[str, Any]],
    required: Sequence[str],
) -> list[str]:
    """Return required literals absent from the combined persisted chunk text."""
    combined = "\n".join(
        str(first_value(chunk, ("content", "text", "page_content"), ""))
        for chunk in chunks
    ).casefold()
    return [literal for literal in required if literal.casefold() not in combined]


@dataclass(frozen=True)
class QueueItem:
    knowledge_id: str
    state: str
    position: int | None = None
    ahead_count: int | None = None
    owner_instance_id: str = ""
    owner_boot_id: str = ""
    stage: str = ""
    execution_epoch: int | None = None
    lease_until: str = ""
    last_progress_at: str = ""

    @classmethod
    def from_mapping(cls, knowledge_id: str, raw: Mapping[str, Any]) -> "QueueItem":
        raw_position = first_value(raw, ("position", "queue_position", "rank"))
        raw_ahead = first_value(raw, ("ahead_count", "documents_ahead", "ahead"))
        raw_epoch = first_value(raw, ("execution_epoch", "epoch", "lease_epoch"))
        return cls(
            knowledge_id=str(first_value(raw, ("knowledge_id", "document_id", "id"), knowledge_id)),
            state=str(first_value(raw, ("state", "queue_state", "status"), "none")).lower(),
            position=None if raw_position is None else to_int(raw_position),
            ahead_count=None if raw_ahead is None else to_int(raw_ahead),
            owner_instance_id=str(first_value(raw, ("owner_instance_id", "instance_id", "owner"), "")),
            owner_boot_id=str(first_value(raw, ("owner_boot_id", "boot_id"), "")),
            stage=str(first_value(raw, ("stage", "current_stage"), "")),
            execution_epoch=None if raw_epoch is None else to_int(raw_epoch),
            lease_until=str(first_value(raw, ("lease_until", "lease_expires_at"), "")),
            last_progress_at=str(first_value(raw, ("last_progress_at", "progress_at", "updated_at"), "")),
        )


@dataclass(frozen=True)
class WorkerInstance:
    instance_id: str
    boot_id: str
    state: str
    capacity: int
    active_count: int
    active_documents: tuple[str, ...] = ()
    last_heartbeat_at: str = ""
    # Newer coordinators expose freshness separately from the persisted state.
    # ``None`` keeps compatibility with third-party/older instance endpoints;
    # an explicit false must never be treated as a ready worker merely because
    # its last persisted state is still ``ready``.
    healthy: bool | None = None

    @property
    def is_ready(self) -> bool:
        return (
            self.state in {"ready", "healthy", "active", "running"}
            and self.healthy is not False
        )

    @property
    def is_healthy_ready(self) -> bool:
        """Return true only for an explicitly fresh, runnable instance.

        ``is_ready`` intentionally accepts legacy APIs which do not expose a
        health bit. Performance/scaling evidence must be stricter: a stale
        persisted ``state=ready`` row or an old endpoint without freshness
        evidence cannot be counted as live capacity.
        """

        return self.state in {"ready", "healthy", "active", "running"} and self.healthy is True

    @classmethod
    def from_mapping(cls, raw: Mapping[str, Any]) -> "WorkerInstance":
        active_raw = first_value(raw, ("active_documents", "documents", "active_knowledge_ids"), [])
        if isinstance(active_raw, list):
            active_documents = tuple(
                str(first_value(item, ("knowledge_id", "document_id", "id"), ""))
                if isinstance(item, Mapping)
                else str(item)
                for item in active_raw
            )
            active_count = len([value for value in active_documents if value])
        else:
            active_documents = ()
            active_count = to_int(active_raw)
        active_count = to_int(first_value(raw, ("active_count", "in_flight", "running"), active_count))
        raw_healthy = raw.get("healthy")
        if isinstance(raw_healthy, bool):
            healthy: bool | None = raw_healthy
        elif isinstance(raw_healthy, str):
            normalized = raw_healthy.strip().lower()
            healthy = True if normalized in {"1", "true", "yes"} else False if normalized in {"0", "false", "no"} else None
        else:
            healthy = None
        return cls(
            instance_id=str(first_value(raw, ("instance_id", "worker_id", "id", "name"), "")),
            boot_id=str(first_value(raw, ("boot_id", "incarnation_id", "process_id"), "")),
            state=str(first_value(raw, ("state", "status"), "unknown")).lower(),
            capacity=to_int(first_value(raw, ("capacity", "concurrency", "max_active"), 0)),
            active_count=active_count,
            active_documents=active_documents,
            last_heartbeat_at=str(first_value(raw, ("last_heartbeat_at", "heartbeat_at", "updated_at"), "")),
            healthy=healthy,
        )


def summarize_instance_topology(instances: Sequence[WorkerInstance]) -> dict[str, Any]:
    """Return the live capacity evidence exposed by the instances API.

    Only instances with both an explicitly fresh ``healthy=true`` signal and a
    runnable state are eligible.  A persisted ready row without a health bit is
    intentionally retained in ``api_instances_total`` but cannot inflate the
    scaling denominator.
    """

    healthy_ready = sorted(
        (instance for instance in instances if instance.is_healthy_ready),
        key=lambda instance: (instance.instance_id, instance.boot_id),
    )
    return {
        "api_instances_total": len(instances),
        "healthy_ready_count": len(healthy_ready),
        "healthy_ready_instances": [
            {
                "instance_id": instance.instance_id,
                "boot_id": instance.boot_id,
                "capacity": instance.capacity,
            }
            for instance in healthy_ready
        ],
    }


def validate_instance_topology(
    start_instances: Sequence[WorkerInstance],
    end_instances: Sequence[WorkerInstance],
    *,
    expected_count: int = 0,
    required: bool,
) -> dict[str, Any]:
    """Validate stable, API-observed healthy capacity for a measured run."""

    if expected_count < 0:
        raise E2EFailure("--instance-count cannot be negative")
    start = summarize_instance_topology(start_instances)
    end = summarize_instance_topology(end_instances)
    topology: dict[str, Any] = {
        "required": required,
        "expected_healthy_ready_count": expected_count or None,
        "start": start,
        "end": end,
        "effective_healthy_ready_count": start["healthy_ready_count"] or None,
    }
    if not required:
        return topology

    start_count = int(start["healthy_ready_count"])
    end_count = int(end["healthy_ready_count"])
    if start_count <= 0 or end_count <= 0:
        raise E2EFailure(
            "instances API must expose at least one runnable instance with healthy=true "
            f"at both measurement boundaries (start={start_count}, end={end_count})"
        )
    if expected_count > 0 and (start_count != expected_count or end_count != expected_count):
        raise E2EFailure(
            "instances API healthy-ready count does not match --instance-count: "
            f"start={start_count} end={end_count} expected={expected_count}"
        )
    if start_count != end_count:
        raise E2EFailure(
            "healthy-ready instance count changed during the measured run: "
            f"start={start_count} end={end_count}"
        )

    def indexed(boundary: Mapping[str, Any]) -> dict[str, tuple[str, int]]:
        result: dict[str, tuple[str, int]] = {}
        for raw in boundary.get("healthy_ready_instances", []):
            instance_id = str(raw.get("instance_id", ""))
            boot_id = str(raw.get("boot_id", ""))
            capacity = to_int(raw.get("capacity"), 0)
            if not instance_id or not boot_id:
                raise E2EFailure(
                    "healthy-ready instances must expose non-empty instance_id and boot_id "
                    "for stable topology evidence"
                )
            if capacity <= 0:
                raise E2EFailure(
                    f"healthy-ready instance {instance_id} has no positive per-instance capacity"
                )
            if instance_id in result:
                raise E2EFailure(f"instances API returned duplicate instance_id {instance_id!r}")
            result[instance_id] = (boot_id, capacity)
        return result

    start_identity = indexed(start)
    end_identity = indexed(end)
    if start_identity != end_identity:
        raise E2EFailure(
            "healthy-ready instance identity, boot, or capacity changed during the measured run: "
            f"start={start_identity!r} end={end_identity!r}"
        )
    topology["effective_healthy_ready_count"] = start_count
    topology["per_instance_capacity"] = sorted({capacity for _, capacity in start_identity.values()})
    return topology


@dataclass(frozen=True)
class QueueSnapshot:
    waiting_total: int
    active_total: int
    items: dict[str, QueueItem]
    instances: tuple[WorkerInstance, ...] = ()

    @classmethod
    def from_payload(cls, payload: Any) -> "QueueSnapshot":
        data = unwrap_data(payload)
        if not isinstance(data, Mapping):
            raise E2EFailure(f"queue response data must be an object, got {type(data).__name__}")

        raw_items = first_value(data, ("items", "documents", "queue"), {})
        items: dict[str, QueueItem] = {}
        if isinstance(raw_items, Mapping):
            for knowledge_id, raw in raw_items.items():
                if not isinstance(raw, Mapping):
                    raw = {"state": raw}
                item = QueueItem.from_mapping(str(knowledge_id), raw)
                items[item.knowledge_id] = item
        elif isinstance(raw_items, list):
            for raw in raw_items:
                if not isinstance(raw, Mapping):
                    continue
                item = QueueItem.from_mapping("", raw)
                if item.knowledge_id:
                    items[item.knowledge_id] = item
        else:
            raise E2EFailure("queue response items/documents must be an object or array")

        raw_instances = first_value(data, ("instances", "workers"), [])
        instances = tuple(
            WorkerInstance.from_mapping(raw)
            for raw in raw_instances
            if isinstance(raw, Mapping)
        ) if isinstance(raw_instances, list) else ()

        waiting_default = sum(item.state in {"waiting", "pending", "queued"} for item in items.values())
        active_default = sum(item.state in {"active", "processing", "running", "finalizing"} for item in items.values())
        return cls(
            waiting_total=to_int(first_value(data, ("waiting_total", "total_waiting", "pending_total"), waiting_default)),
            active_total=to_int(first_value(data, ("active_total", "total_active", "running_total"), active_default)),
            items=items,
            instances=instances,
        )


class APIClient:
    """Small stdlib-only API client so the suite has no pip dependency."""

    def __init__(
        self,
        base_url: str,
        token: str,
        *,
        auth_mode: str = "api-key",
        timeout: float = 60.0,
        queue_status_path: str = "/api/v1/custom/document-queue/status",
        instances_path: str = "/api/v1/custom/document-queue/instances",
    ) -> None:
        self.base_url = base_url.rstrip("/")
        self.token = token
        self.auth_mode = auth_mode
        self.timeout = timeout
        self.queue_status_path = queue_status_path
        self.instances_path = instances_path

    def _headers(self) -> dict[str, str]:
        headers = {"Accept": "application/json", "User-Agent": "weknora-document-cluster-e2e/1"}
        if self.auth_mode == "bearer":
            headers["Authorization"] = f"Bearer {self.token}"
        else:
            headers["X-API-Key"] = self.token
        return headers

    def request(
        self,
        method: str,
        path: str,
        *,
        json_body: Any | None = None,
        raw_body: bytes | None = None,
        headers: Mapping[str, str] | None = None,
        timeout: float | None = None,
    ) -> Any:
        url = path if path.startswith("http://") or path.startswith("https://") else self.base_url + path
        body = raw_body
        request_headers = self._headers()
        if json_body is not None:
            body = json.dumps(json_body, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
            request_headers["Content-Type"] = "application/json"
        if headers:
            request_headers.update(headers)
        req = urllib.request.Request(url, data=body, method=method.upper(), headers=request_headers)
        try:
            with urllib.request.urlopen(req, timeout=timeout or self.timeout) as response:
                content = response.read()
                if not content:
                    return None
                content_type = response.headers.get("Content-Type", "")
                if "json" in content_type or content[:1] in {b"{", b"["}:
                    return json.loads(content.decode("utf-8"))
                return content
        except urllib.error.HTTPError as exc:
            body_text = exc.read().decode("utf-8", "replace")
            raise APIError(method.upper(), url, exc.code, body_text) from exc
        except urllib.error.URLError as exc:
            raise E2EFailure(f"{method.upper()} {url} failed: {exc}") from exc

    def system_info(self) -> Any:
        return self.request("GET", "/api/v1/system/info")

    def get_knowledge_base(self, kb_id: str) -> Mapping[str, Any]:
        payload = self.request("GET", f"/api/v1/knowledge-bases/{urllib.parse.quote(kb_id)}")
        data = unwrap_data(payload)
        if not isinstance(data, Mapping):
            raise E2EFailure(f"knowledge-base response data is not an object: {payload!r}")
        return data

    def system_setting(self, key: str) -> Mapping[str, Any]:
        encoded = urllib.parse.quote(key, safe="")
        try:
            payload = self.request("GET", f"/api/v1/system/admin/settings/{encoded}")
            if isinstance(payload, Mapping):
                return payload
        except APIError as exc:
            if exc.status != 404:
                raise
        payload = self.request("GET", "/api/v1/system/admin/settings")
        if isinstance(payload, list):
            for item in payload:
                if isinstance(item, Mapping) and item.get("key") == key:
                    return item
        raise E2EFailure(f"system setting {key!r} is not available")

    def upload_document(
        self,
        kb_id: str,
        filename: str,
        content: bytes,
        *,
        process_config: Mapping[str, Any] | None = None,
        metadata: Mapping[str, str] | None = None,
    ) -> Mapping[str, Any]:
        fields: dict[str, str] = {"fileName": filename}
        if process_config:
            fields["process_config"] = json.dumps(process_config, ensure_ascii=False, separators=(",", ":"))
        if metadata:
            fields["metadata"] = json.dumps(metadata, ensure_ascii=False, separators=(",", ":"))
        boundary = "----weknora-e2e-" + uuid.uuid4().hex
        chunks: list[bytes] = []
        for name, value in fields.items():
            chunks.extend(
                [
                    f"--{boundary}\r\n".encode(),
                    f'Content-Disposition: form-data; name="{name}"\r\n\r\n'.encode(),
                    value.encode("utf-8"),
                    b"\r\n",
                ]
            )
        mime = mimetypes.guess_type(filename)[0] or "application/octet-stream"
        safe_filename = filename.replace('"', "_").replace("\r", "_").replace("\n", "_")
        chunks.extend(
            [
                f"--{boundary}\r\n".encode(),
                f'Content-Disposition: form-data; name="file"; filename="{safe_filename}"\r\n'.encode(),
                f"Content-Type: {mime}\r\n\r\n".encode(),
                content,
                b"\r\n",
                f"--{boundary}--\r\n".encode(),
            ]
        )
        payload = self.request(
            "POST",
            f"/api/v1/knowledge-bases/{urllib.parse.quote(kb_id)}/knowledge/file",
            raw_body=b"".join(chunks),
            headers={"Content-Type": f"multipart/form-data; boundary={boundary}"},
            timeout=max(self.timeout, 120.0),
        )
        data = unwrap_data(payload)
        if not isinstance(data, Mapping) or not data.get("id"):
            raise E2EFailure(f"upload response has no knowledge id: {payload!r}")
        return data

    def get_knowledge(self, knowledge_id: str) -> Mapping[str, Any]:
        payload = self.request("GET", f"/api/v1/knowledge/{urllib.parse.quote(knowledge_id)}")
        data = unwrap_data(payload)
        if not isinstance(data, Mapping):
            raise E2EFailure(f"knowledge response data is not an object: {payload!r}")
        return data

    def list_knowledge(self, kb_id: str, *, page: int = 1, page_size: int = 100) -> tuple[list[Mapping[str, Any]], int]:
        query = urllib.parse.urlencode({"page": page, "page_size": page_size})
        payload = self.request(
            "GET",
            f"/api/v1/knowledge-bases/{urllib.parse.quote(kb_id)}/knowledge?{query}",
        )
        if not isinstance(payload, Mapping):
            raise E2EFailure("knowledge list response must be an object")
        raw = payload.get("data", [])
        if isinstance(raw, Mapping):
            items = first_value(raw, ("data", "items", "list"), [])
            total = to_int(first_value(raw, ("total",), payload.get("total", len(items))))
        else:
            items = raw
            total = to_int(payload.get("total", len(items) if isinstance(items, list) else 0))
        return [item for item in items if isinstance(item, Mapping)], total

    def list_all_knowledge(self, kb_id: str) -> list[Mapping[str, Any]]:
        first, total = self.list_knowledge(kb_id, page=1, page_size=100)
        result = list(first)
        pages = max(1, math.ceil(total / 100))
        for page in range(2, pages + 1):
            items, _ = self.list_knowledge(kb_id, page=page, page_size=100)
            result.extend(items)
        return result

    def get_queue(self, knowledge_ids: Sequence[str]) -> QueueSnapshot:
        # Positions must come from one database snapshot. Merging sequential
        # batches is racy because claims between requests shift every later
        # rank and can create duplicate positions in the merged result.
        payload = self.request(
            "POST",
            self.queue_status_path,
            json_body={"knowledge_ids": list(knowledge_ids)},
        )
        return QueueSnapshot.from_payload(payload)

    def get_instances(self, *, optional: bool = False) -> tuple[WorkerInstance, ...]:
        try:
            payload = self.request("GET", self.instances_path)
        except APIError as exc:
            # The endpoint is optional and, when present, may be system-admin
            # only while the workload token is a normal KB editor token.
            if optional and exc.status in {401, 403, 404, 405, 501}:
                return ()
            raise
        data = unwrap_data(payload)
        if isinstance(data, Mapping):
            raw = first_value(data, ("instances", "workers"), [])
        else:
            raw = data
        if not isinstance(raw, list):
            raise E2EFailure("instances response must contain an instances/workers array")
        return tuple(WorkerInstance.from_mapping(item) for item in raw if isinstance(item, Mapping))

    def attest_instance_termination(self, instance_id: str, boot_id: str, proof: str) -> None:
        self.request(
            "POST",
            "/api/v1/custom/document-queue/instances/termination-attestation",
            json_body={"instance_id": instance_id, "boot_id": boot_id, "proof": proof},
        )

    def get_spans(self, knowledge_id: str) -> Mapping[str, Any]:
        payload = self.request("GET", f"/api/v1/knowledge/{urllib.parse.quote(knowledge_id)}/spans")
        data = unwrap_data(payload)
        return data if isinstance(data, Mapping) else {}

    def list_chunks(self, knowledge_id: str, chunk_types: Sequence[str]) -> list[Mapping[str, Any]]:
        result: list[Mapping[str, Any]] = []
        page = 1
        while True:
            query = urllib.parse.urlencode(
                [("page", str(page)), ("page_size", "100")] + [("chunk_type", value) for value in chunk_types]
            )
            payload = self.request(
                "GET",
                f"/api/v1/chunks/{urllib.parse.quote(knowledge_id)}?{query}",
            )
            if not isinstance(payload, Mapping):
                raise E2EFailure("chunk list response must be an object")
            items = payload.get("data", [])
            if not isinstance(items, list):
                items = []
            result.extend(item for item in items if isinstance(item, Mapping))
            total = to_int(payload.get("total", len(result)))
            if len(result) >= total or not items:
                break
            page += 1
        return result

    def hybrid_search(self, kb_id: str, query_text: str, knowledge_ids: Sequence[str]) -> list[Mapping[str, Any]]:
        payload = self.request(
            "POST",
            f"/api/v1/knowledge-bases/{urllib.parse.quote(kb_id)}/hybrid-search",
            json_body={
                "query_text": query_text,
                "match_count": 20,
                "knowledge_ids": list(knowledge_ids),
                "vector_threshold": 0,
            },
            timeout=max(self.timeout, 120.0),
        )
        data = unwrap_data(payload)
        return [item for item in data if isinstance(item, Mapping)] if isinstance(data, list) else []

    def list_wiki_pages(self, kb_id: str) -> list[Mapping[str, Any]]:
        result: list[Mapping[str, Any]] = []
        page = 1
        while True:
            query = urllib.parse.urlencode({"page": page, "page_size": 100})
            payload = self.request(
                "GET",
                f"/api/v1/knowledgebase/{urllib.parse.quote(kb_id)}/wiki/pages?{query}",
            )
            data = unwrap_data(payload)
            if not isinstance(data, Mapping):
                break
            pages = first_value(data, ("pages", "items", "data"), [])
            if not isinstance(pages, list):
                break
            result.extend(item for item in pages if isinstance(item, Mapping))
            total = to_int(data.get("total", len(result)))
            if len(result) >= total or not pages:
                break
            page += 1
        return result

    def delete_knowledge(self, knowledge_id: str) -> None:
        self.request("DELETE", f"/api/v1/knowledge/{urllib.parse.quote(knowledge_id)}")


class JsonlRecorder:
    def __init__(self, path: Path | None):
        self.path = path
        self._lock = threading.Lock()
        if path:
            path.parent.mkdir(parents=True, exist_ok=True)

    def emit(self, event: str, **fields: Any) -> None:
        row = {"timestamp": utc_now(), "event": event, **fields}
        line = json.dumps(row, ensure_ascii=False, default=str)
        print(line, flush=True)
        if self.path:
            with self._lock, self.path.open("a", encoding="utf-8") as handle:
                handle.write(line + "\n")


@dataclass
class DocumentObservation:
    knowledge_id: str
    filename: str
    marker: str
    uploaded_monotonic: float
    first_waiting_monotonic: float | None = None
    first_active_monotonic: float | None = None
    terminal_monotonic: float | None = None
    final_status: str = ""
    queue_positions: list[int] = field(default_factory=list)
    owners: set[str] = field(default_factory=set)
    epochs: list[int] = field(default_factory=list)

    def observe_queue(self, item: QueueItem, now: float) -> None:
        if item.state in {"waiting", "pending", "queued"} and self.first_waiting_monotonic is None:
            self.first_waiting_monotonic = now
        if item.state in {"active", "processing", "running", "finalizing"} and self.first_active_monotonic is None:
            self.first_active_monotonic = now
        if item.position is not None and item.position > 0:
            self.queue_positions.append(item.position)
        if item.owner_instance_id:
            self.owners.add(item.owner_instance_id)
        if item.execution_epoch is not None:
            self.epochs.append(item.execution_epoch)


@dataclass
class RunResult:
    run_id: str
    started_at: str
    finished_at: str
    documents: int
    completed: int
    failed: int
    cancelled: int
    wall_seconds: float
    throughput_docs_per_second: float
    queue_wait_p50_seconds: float | None
    queue_wait_p95_seconds: float | None
    processing_p50_seconds: float | None
    processing_p95_seconds: float | None
    max_waiting_total: int
    max_active_total: int
    max_active_by_instance: dict[str, int]
    owner_distribution: dict[str, int]
    errors: list[str]


class DockerController:
    """Opt-in controller used only by chaos scenarios against disposable workers."""

    def __init__(self, containers: Sequence[str], recorder: JsonlRecorder):
        self.containers = [value for value in containers if value]
        self.recorder = recorder

    @staticmethod
    def _run(args: Sequence[str], timeout: float = 60.0) -> str:
        proc = subprocess.run(
            list(args),
            check=False,
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            timeout=timeout,
        )
        if proc.returncode != 0:
            raise E2EFailure(f"command failed ({proc.returncode}): {' '.join(args)}\n{proc.stdout}")
        return proc.stdout.strip()

    def assert_present(self) -> None:
        if len(self.containers) < 2:
            raise E2EFailure("chaos/failover test requires at least two --worker-container values")
        for container in self.containers:
            self._run(["docker", "inspect", container])

    def inspect_state(self, container: str) -> Mapping[str, Any]:
        raw = self._run(["docker", "inspect", "--format", "{{json .State}}", container])
        try:
            state = json.loads(raw)
        except json.JSONDecodeError as exc:
            raise E2EFailure(f"docker returned invalid state JSON for {container}: {raw!r}") from exc
        if not isinstance(state, Mapping):
            raise E2EFailure(f"docker state for {container} is not an object")
        return state

    def wait_running(self, container: str, expected: bool, *, timeout: float = 60.0) -> Mapping[str, Any]:
        deadline = time.monotonic() + timeout
        last_state: Mapping[str, Any] = {}
        while time.monotonic() < deadline:
            last_state = self.inspect_state(container)
            if bool(last_state.get("Running")) is expected:
                self.recorder.emit(
                    "chaos.container_state_verified",
                    container=container,
                    expected_running=expected,
                    status=str(last_state.get("Status", "")),
                    exit_code=last_state.get("ExitCode"),
                )
                return last_state
            time.sleep(0.25)
        raise E2EFailure(
            f"container {container} did not become running={expected} within {timeout}s; "
            f"last_state={dict(last_state)!r}"
        )

    def configured_instance_identity(self, container: str) -> tuple[str, str]:
        raw = self._run(
            [
                "docker",
                "inspect",
                "--format",
                "{{json .Config.Env}}|{{json .Config.Hostname}}",
                container,
            ]
        )
        environment_raw, separator, hostname_raw = raw.partition("|")
        if not separator:
            raise E2EFailure(f"cannot inspect configured identity for container {container}")
        try:
            environment_values = json.loads(environment_raw)
            hostname = json.loads(hostname_raw)
        except json.JSONDecodeError as exc:
            raise E2EFailure(f"invalid Docker identity metadata for {container}") from exc
        environment: dict[str, str] = {}
        if isinstance(environment_values, list):
            for value in environment_values:
                key, equals, setting = str(value).partition("=")
                if equals:
                    environment[key] = setting
        explicit = (
            environment.get("CUSTOM_DOCUMENT_QUEUE_INSTANCE_ID")
            or environment.get("WEKNORA_DOCUMENT_INSTANCE_ID")
            or ""
        ).strip()
        return explicit, str(hostname or "").strip()

    def stop(self, container: str, *, hard_kill: bool) -> None:
        action = "kill" if hard_kill else "stop"
        args = ["docker", action]
        if not hard_kill:
            args.extend(["--time", "1"])
        args.append(container)
        self.recorder.emit("chaos.worker_stop", container=container, hard_kill=hard_kill)
        self._run(args)
        self.wait_running(container, False)

    def start(self, container: str) -> None:
        self.recorder.emit("chaos.worker_start", container=container)
        self._run(["docker", "start", container])
        self.wait_running(container, True)

    def pause(self, container: str) -> None:
        self.recorder.emit("chaos.worker_pause", container=container)
        self._run(["docker", "pause", container])
        state = self.inspect_state(container)
        if not state.get("Running") or not state.get("Paused"):
            raise E2EFailure(f"container {container} was not confirmed paused: {dict(state)!r}")

    def unpause(self, container: str) -> None:
        self.recorder.emit("chaos.worker_unpause", container=container)
        self._run(["docker", "unpause", container])
        state = self.inspect_state(container)
        if not state.get("Running") or state.get("Paused"):
            raise E2EFailure(f"container {container} was not confirmed unpaused: {dict(state)!r}")

    def is_running(self, container: str) -> bool:
        return bool(self.inspect_state(container).get("Running"))

    def wait_redis_ping(self, container: str, *, timeout: float = 60.0) -> None:
        """Wait for an authenticated PONG without logging the discovered password."""
        command_raw = self._run(["docker", "inspect", "--format", "{{json .Config.Cmd}}", container])
        try:
            configured_command = json.loads(command_raw)
        except json.JSONDecodeError:
            configured_command = []
        password = ""
        if isinstance(configured_command, list):
            for index, value in enumerate(configured_command):
                value = str(value)
                if value == "--requirepass" and index + 1 < len(configured_command):
                    password = str(configured_command[index + 1])
                    break
                if value.startswith("--requirepass="):
                    password = value.split("=", 1)[1]
                    break

        deadline = time.monotonic() + timeout
        last_output = ""
        while time.monotonic() < deadline:
            args = ["docker", "exec"]
            if password:
                args.extend(["-e", f"REDISCLI_AUTH={password}"])
            args.extend([container, "redis-cli", "--no-auth-warning", "ping"])
            process = subprocess.run(
                args,
                check=False,
                stdout=subprocess.PIPE,
                stderr=subprocess.STDOUT,
                text=True,
                timeout=min(10.0, max(1.0, deadline - time.monotonic())),
            )
            last_output = process.stdout.strip()
            if process.returncode == 0 and last_output.upper() == "PONG":
                self.recorder.emit("chaos.redis_ping_verified", container=container)
                return
            time.sleep(0.5)
        raise E2EFailure(
            f"Redis container {container} did not return PONG within {timeout}s; "
            f"last_output={last_output!r}"
        )


def flatten_span_nodes(value: Any) -> list[Mapping[str, Any]]:
    result: list[Mapping[str, Any]] = []
    if isinstance(value, Mapping):
        if any(key in value for key in ("name", "span_name", "kind")):
            result.append(value)
        for child in value.values():
            result.extend(flatten_span_nodes(child))
    elif isinstance(value, list):
        for child in value:
            result.extend(flatten_span_nodes(child))
    return result


def generated_questions(chunk: Mapping[str, Any]) -> list[Any]:
    metadata = chunk.get("metadata")
    if isinstance(metadata, str):
        try:
            metadata = json.loads(metadata)
        except json.JSONDecodeError:
            metadata = {}
    if not isinstance(metadata, Mapping):
        return []
    value = metadata.get("generated_questions", [])
    return value if isinstance(value, list) else []


def generated_question_text(value: Any) -> str:
    if isinstance(value, Mapping):
        return str(first_value(value, ("question", "text", "content"), "")).strip()
    return str(value).strip() if value is not None else ""


def normalize_question_stem(question: str) -> str:
    return "".join(character.casefold() for character in question if character.isalnum())


def validate_generated_question_quality(
    chunks: Sequence[Mapping[str, Any]],
    *,
    near_duplicate_ratio: float = 0.96,
) -> dict[str, Any]:
    """Validate persisted questions, not merely the LLM task status.

    Numbering, whitespace and punctuation differences cannot make two stems
    unique. Very high textual similarity is also rejected because concurrent
    chunk batches commonly produce superficial paraphrases. The threshold is
    intentionally conservative to avoid conflating distinct policy duties
    which share a long organization or policy name.
    """

    questions: list[str] = []
    for chunk in chunks:
        questions.extend(
            text
            for text in (generated_question_text(value) for value in generated_questions(chunk))
            if text
        )
    if not questions:
        raise E2EFailure("no generated questions were persisted")

    invalid: list[str] = []
    exact_duplicates: list[tuple[str, str]] = []
    near_duplicates: list[tuple[str, str, float]] = []
    seen: dict[str, str] = {}
    normalized: list[tuple[str, str]] = []
    for question in questions:
        stem = normalize_question_stem(question)
        if (
            len(question) < 6
            or len(question) > 240
            or len(stem) < 6
            or QUESTION_SOURCE_METADATA_RE.search(question)
            or not question.endswith(("?", "？"))
        ):
            invalid.append(question)
            continue
        previous = seen.get(stem)
        if previous is not None:
            exact_duplicates.append((previous, question))
            continue
        seen[stem] = question
        normalized.append((stem, question))

    # A quadratic pass is acceptable here because the production budget caps
    # generated questions per document. Length bucketing avoids comparing
    # clearly unrelated stems in large policy documents.
    for index, (left_stem, left_question) in enumerate(normalized):
        for right_stem, right_question in normalized[index + 1 :]:
            length_ratio = min(len(left_stem), len(right_stem)) / max(len(left_stem), len(right_stem))
            if length_ratio < near_duplicate_ratio:
                continue
            ratio = SequenceMatcher(None, left_stem, right_stem, autojunk=False).ratio()
            if ratio >= near_duplicate_ratio:
                near_duplicates.append((left_question, right_question, ratio))

    if invalid:
        raise E2EFailure(
            "generated questions contain unnatural/source-generation wording: "
            + json.dumps(invalid[:10], ensure_ascii=False)
        )
    if exact_duplicates:
        raise E2EFailure(
            "generated questions contain duplicate stems: "
            + json.dumps(exact_duplicates[:10], ensure_ascii=False)
        )
    if near_duplicates:
        raise E2EFailure(
            "generated questions contain superficial paraphrases: "
            + json.dumps(near_duplicates[:10], ensure_ascii=False)
        )
    return {
        "questions": len(questions),
        "unique_stems": len(seen),
        "near_duplicate_ratio": near_duplicate_ratio,
    }


def wiki_page_substantive_text(page: Mapping[str, Any]) -> str:
    values = [
        first_value(page, ("title", "name"), ""),
        first_value(page, ("summary", "description"), ""),
        first_value(page, ("content", "markdown", "body"), ""),
    ]
    return "\n".join(str(value).strip() for value in values if str(value).strip())


def graph_artifact_counts(span_nodes: Sequence[Mapping[str, Any]]) -> tuple[int, int]:
    """Return persisted graph node/relation counts from successful graph spans.

    A completed extraction span alone is not useful evidence: the model may
    have returned an empty graph while the handler still completed normally.
    The graph worker records the actual write counts in the span output, so
    E2E assertions should consume that durable evidence instead.
    """

    nodes_added = 0
    relations_added = 0
    for node in span_nodes:
        name = str(first_value(node, ("name", "span_name"), "")).lower()
        status = str(first_value(node, ("status",), "")).lower()
        if "graph" not in name or status not in {"done", "completed", "success"}:
            continue
        output = node.get("output")
        if not isinstance(output, Mapping):
            continue
        for key, target in (("nodes_added", "nodes"), ("relations_added", "relations")):
            value = output.get(key, 0)
            try:
                count = max(0, int(value))
            except (TypeError, ValueError):
                count = 0
            if target == "nodes":
                nodes_added += count
            else:
                relations_added += count
    return nodes_added, relations_added


def embedding_vector_evidence(
    span_nodes: Sequence[Mapping[str, Any]],
) -> dict[str, int] | None:
    """Return durable vector-write evidence from the successful embedding stage.

    A successful hybrid-search probe is not sufficient evidence that every
    persisted text chunk was embedded: keyword retrieval can hide a partially
    written vector index. The core embedding stage records both its planned
    and committed counts, so E2E validation compares those counts with the
    authoritative text chunks returned by the API.
    """

    matching: list[Mapping[str, Any]] = []
    for node in span_nodes:
        name = str(first_value(node, ("name", "span_name"), "")).strip().lower()
        status = str(first_value(node, ("status",), "")).strip().lower()
        if name == "embedding" and status in {"done", "completed", "success"}:
            matching.append(node)
    if not matching:
        return None
    if len(matching) != 1:
        raise E2EFailure(
            f"latest processing attempt has {len(matching)} successful embedding stages; expected exactly one"
        )

    node = matching[0]
    raw_input = node.get("input")
    raw_output = node.get("output")
    if isinstance(raw_input, str):
        try:
            raw_input = json.loads(raw_input)
        except json.JSONDecodeError:
            raw_input = {}
    if isinstance(raw_output, str):
        try:
            raw_output = json.loads(raw_output)
        except json.JSONDecodeError:
            raw_output = {}
    if not isinstance(raw_input, Mapping) or not isinstance(raw_output, Mapping):
        raise E2EFailure("successful embedding stage has no structured input/output evidence")

    try:
        planned = int(raw_input["chunks_to_embed"])
        written = int(raw_output["vectors_written"])
    except (KeyError, TypeError, ValueError) as exc:
        raise E2EFailure(
            "successful embedding stage is missing integer chunks_to_embed/vectors_written evidence"
        ) from exc
    if planned < 0 or written < 0:
        raise E2EFailure(
            f"embedding stage reported negative counts: planned={planned}, written={written}"
        )
    return {"chunks_to_embed": planned, "vectors_written": written}


def source_refs_include(page: Mapping[str, Any], knowledge_id: str) -> bool:
    refs = page.get("source_refs", [])
    if isinstance(refs, str):
        try:
            refs = json.loads(refs)
        except json.JSONDecodeError:
            refs = [refs]
    return isinstance(refs, list) and any(str(ref).split("|", 1)[0] == knowledge_id for ref in refs)


class ClusterE2ERunner:
    def __init__(
        self,
        client: APIClient,
        kb_id: str,
        recorder: JsonlRecorder,
        *,
        run_id: str | None = None,
        poll_interval: float = 2.0,
        expected_derivatives: Iterable[str] = (),
    ) -> None:
        self.client = client
        self.kb_id = kb_id
        self.recorder = recorder
        self.run_id = run_id or f"doc-cluster-{int(time.time())}-{uuid.uuid4().hex[:6]}"
        self.poll_interval = poll_interval
        self.expected_derivatives = {
            str(value).strip().lower()
            for value in expected_derivatives
            if str(value).strip()
        }
        self.observations: dict[str, DocumentObservation] = {}
        self.max_waiting_total = 0
        self.max_active_total = 0
        self.max_active_by_instance: dict[str, int] = {}
        self.errors: list[str] = []

    def api_smoke(
        self,
        expected_instance_concurrency: int = 0,
        *,
        require_instance_topology: bool = False,
    ) -> tuple[WorkerInstance, ...]:
        started = time.monotonic()
        info = self.client.system_info()
        # The browser never calls the endpoint with an empty visible-ID set;
        # use a syntactically valid absent ID so request validation follows the
        # same path as production without observing another tenant's document.
        snapshot = self.client.get_queue(["00000000-0000-0000-0000-000000000000"])
        if snapshot.waiting_total < 0 or snapshot.active_total < 0:
            raise E2EFailure("queue totals must be non-negative")
        topology_required = require_instance_topology or expected_instance_concurrency > 0
        instances = self.client.get_instances(optional=not topology_required)
        if topology_required and not any(instance.is_healthy_ready for instance in instances):
            raise E2EFailure(
                "instances API returned no runnable instance with an explicit healthy=true signal"
            )
        configured_concurrency: int | None = None
        if expected_instance_concurrency > 0:
            setting = self.client.system_setting("asynq.concurrency")
            configured_concurrency = to_int(setting.get("value"), -1)
            if configured_concurrency != expected_instance_concurrency:
                raise E2EFailure(
                    "asynq.concurrency setting mismatch: "
                    f"actual={configured_concurrency} expected={expected_instance_concurrency}"
                )
            wrong_capacity = [
                instance.instance_id for instance in instances
                if instance.is_healthy_ready
                and instance.capacity != expected_instance_concurrency
            ]
            if wrong_capacity:
                raise E2EFailure(
                    "healthy instances do not apply the per-instance concurrency setting: "
                    f"{wrong_capacity}"
                )
        self.recorder.emit(
            "api.smoke_passed",
            elapsed_seconds=time.monotonic() - started,
            queue_waiting_total=snapshot.waiting_total,
            queue_active_total=snapshot.active_total,
            instances=len(instances),
            healthy_ready_instances=sum(instance.is_healthy_ready for instance in instances),
            configured_instance_concurrency=configured_concurrency,
            system_info_present=info is not None,
        )
        return instances

    def _generated_content(self, index: int, target_size_kib: int) -> tuple[str, bytes]:
        marker = f"WKN-{self.run_id}-{index:05d}-{uuid.uuid4().hex[:8]}"
        paragraphs = [
            f"# Horizontal processing acceptance document {index}",
            f"Unique retrieval marker: {marker}.",
        ]
        for section in range(1, 9):
            paragraphs.append(
                f"## Section {section}\n"
                f"Document {index} section {section} verifies parsing, chunk persistence, embedding, "
                "vector retrieval, summary generation, question generation, graph extraction, and Wiki ingestion. "
                f"The immutable marker for this section remains {marker}."
            )
        base = "\n\n".join(paragraphs) + "\n"
        target_bytes = max(1, target_size_kib) * 1024
        content = base
        repeat = 1
        while len(content.encode("utf-8")) < target_bytes:
            content += (
                f"\n## Load section {repeat}\n"
                f"Repeated deterministic workload text for {marker}. It increases chunk and embedding work "
                "without relying on large binary fixtures or changing the semantic acceptance assertions.\n"
            )
            repeat += 1
        return marker, content.encode("utf-8")

    def upload_batch(
        self,
        count: int,
        *,
        upload_concurrency: int,
        process_config: Mapping[str, Any] | None,
        fixture_paths: Sequence[Path] = (),
        generated_size_kib: int = 8,
    ) -> list[str]:
        if count <= 0:
            raise E2EFailure("document count must be positive")
        fixtures = [path for path in fixture_paths if path.suffix.lower() in SUPPORTED_FIXTURE_SUFFIXES]
        if fixture_paths and len(fixtures) != len(fixture_paths):
            rejected = [str(path) for path in fixture_paths if path not in fixtures]
            raise E2EFailure(f"unsupported fixture suffixes: {rejected}")
        if fixtures and count > len(fixtures):
            raise E2EFailure("binary/format fixtures are uploaded once each; count must not exceed fixture count")

        started = time.monotonic()

        def upload(index: int) -> tuple[Mapping[str, Any], str, str, float]:
            marker, generated = self._generated_content(index, generated_size_kib)
            if fixtures:
                fixture = fixtures[index]
                content = fixture.read_bytes()
                marker = f"{self.run_id}-{index:05d}"
                # The core index prepends the knowledge title, so the unique
                # marker is still searchable for binary fixtures without
                # corrupting them by appending test bytes.
                filename = f"{marker}-{fixture.name}"
            else:
                content = generated
                filename = f"{self.run_id}-{index:05d}.md"
            uploaded_at = time.monotonic()
            data = self.client.upload_document(
                self.kb_id,
                filename,
                content,
                process_config=process_config,
                metadata={"e2e_run_id": self.run_id, "e2e_marker": marker},
            )
            return data, filename, marker, uploaded_at

        with ThreadPoolExecutor(max_workers=max(1, upload_concurrency)) as pool:
            futures = {pool.submit(upload, index): index for index in range(count)}
            upload_failures: list[str] = []
            for future in as_completed(futures):
                index = futures[future]
                try:
                    data, filename, marker, uploaded_at = future.result()
                except Exception as exc:
                    upload_failures.append(f"upload {index} failed: {exc}")
                    continue
                knowledge_id = str(data["id"])
                self.observations[knowledge_id] = DocumentObservation(
                    knowledge_id=knowledge_id,
                    filename=filename,
                    marker=marker,
                    uploaded_monotonic=uploaded_at,
                )
        ids = list(self.observations)
        if upload_failures:
            self.recorder.emit(
                "workload.upload_failed",
                requested=count,
                uploaded=len(ids),
                failures=upload_failures,
                knowledge_ids=ids,
            )
            raise E2EFailure(
                f"{len(upload_failures)} of {count} uploads failed; "
                f"{len(ids)} successful uploads remain registered for cleanup: "
                + "; ".join(upload_failures)
            )
        self.recorder.emit(
            "workload.uploaded",
            documents=len(ids),
            upload_concurrency=upload_concurrency,
            elapsed_seconds=time.monotonic() - started,
            knowledge_ids=ids,
        )
        return ids

    def sample_queue(self) -> QueueSnapshot:
        snapshot = self.client.get_queue(list(self.observations))
        now = time.monotonic()
        self.max_waiting_total = max(self.max_waiting_total, snapshot.waiting_total)
        self.max_active_total = max(self.max_active_total, snapshot.active_total)
        for knowledge_id, item in snapshot.items.items():
            observation = self.observations.get(knowledge_id)
            if observation:
                observation.observe_queue(item, now)
        instances = snapshot.instances or self.client.get_instances(optional=True)
        for instance in instances:
            self.max_active_by_instance[instance.instance_id] = max(
                self.max_active_by_instance.get(instance.instance_id, 0), instance.active_count
            )
            if instance.capacity > 0 and instance.active_count > instance.capacity:
                raise E2EFailure(
                    f"instance {instance.instance_id} exceeded capacity: "
                    f"active={instance.active_count} capacity={instance.capacity}"
                )
            for knowledge_id in instance.active_documents:
                observation = self.observations.get(knowledge_id)
                if observation and instance.instance_id:
                    observation.owners.add(instance.instance_id)
        self.recorder.emit(
            "queue.sample",
            waiting_total=snapshot.waiting_total,
            active_total=snapshot.active_total,
            tracked_states={key: item.state for key, item in snapshot.items.items()},
            instances=[asdict(instance) for instance in instances],
        )
        return snapshot

    def assert_queue_positions(self, snapshot: QueueSnapshot) -> None:
        waiting = [
            item for item in snapshot.items.values()
            if item.state in {"waiting", "pending", "queued"}
        ]
        positions = [item.position for item in waiting if item.position is not None]
        if waiting and len(positions) != len(waiting):
            missing = [item.knowledge_id for item in waiting if item.position is None]
            raise E2EFailure(f"waiting queue items have no position: {missing[:20]}")
        if any(position is not None and position < 1 for position in positions):
            raise E2EFailure(f"queue positions must be one-based positive integers: {positions}")
        if len(positions) != len(set(positions)):
            raise E2EFailure(f"queue positions are not unique in one snapshot: {positions}")
        for item in waiting:
            if item.position is not None and item.ahead_count is not None and item.ahead_count != item.position - 1:
                raise E2EFailure(
                    f"queue ahead_count mismatch for {item.knowledge_id}: "
                    f"position={item.position} ahead_count={item.ahead_count}"
                )

    def assert_card_queue_fields(self, snapshot: QueueSnapshot) -> None:
        waiting_ids = {
            item.knowledge_id
            for item in snapshot.items.values()
            if item.state in {"waiting", "pending", "queued"}
        }
        if not waiting_ids:
            self.recorder.emit("queue.card_fields_skipped", reason="no tracked document remained waiting")
            return
        listed = {str(item.get("id")): item for item in self.client.list_all_knowledge(self.kb_id)}
        missing_cards: list[str] = []
        api_backed: list[str] = []
        inconsistent: list[str] = []
        for knowledge_id in waiting_ids:
            card = listed.get(knowledge_id)
            if not card:
                missing_cards.append(knowledge_id)
                continue
            if "queue_position" not in card:
                # Current UI intentionally joins queue position from the
                # dedicated status endpoint; it need not mutate Knowledge DTOs.
                api_backed.append(knowledge_id)
                continue
            expected = snapshot.items[knowledge_id].position
            if expected is not None and to_int(card.get("queue_position")) != expected:
                inconsistent.append(knowledge_id)
        if missing_cards:
            raise E2EFailure(f"waiting knowledge cards missing from knowledge list: {missing_cards[:20]}")
        if inconsistent:
            raise E2EFailure(f"knowledge card and queue API positions differ: {inconsistent[:20]}")
        self.recorder.emit(
            "queue.card_fields_passed",
            checked=len(waiting_ids),
            queue_position_from_status_api=len(api_backed),
        )

    def _pipeline_terminal_status(self, item: Mapping[str, Any]) -> str:
        parse_status = str(item.get("parse_status", "")).strip().lower()
        if parse_status in TERMINAL_STATUSES - {"completed"}:
            return parse_status
        if parse_status != "completed":
            return ""

        enrichment_expected = bool(
            self.expected_derivatives
            & {"summary", "questions", "graph", "multimodal", "table"}
        )
        if enrichment_expected:
            enrichment_status = str(item.get("enrichment_status", "")).strip().lower()
            summary_status = str(item.get("summary_status", "")).strip().lower()
            if (
                enrichment_status in FAILED_STAGE_STATUSES
                or (
                    "summary" in self.expected_derivatives
                    and summary_status in FAILED_STAGE_STATUSES
                )
            ):
                return "failed"
            if enrichment_status not in {"completed", "done"}:
                return ""
            try:
                pending = int(item.get("pending_subtasks_count", 0))
            except (TypeError, ValueError):
                return ""
            if pending != 0:
                return ""
        if "wiki" in self.expected_derivatives:
            wiki_status = str(item.get("wiki_status", "")).strip().lower()
            if wiki_status in FAILED_STAGE_STATUSES:
                return "failed"
            if wiki_status not in {"completed", "done"}:
                return ""
        return "completed"

    def _refresh_terminal_states(self) -> None:
        tracked = set(self.observations)
        listed = self.client.list_all_knowledge(self.kb_id)
        now = time.monotonic()
        found = 0
        for item in listed:
            knowledge_id = str(item.get("id", ""))
            if knowledge_id not in tracked:
                continue
            found += 1
            status = self._pipeline_terminal_status(item)
            if status:
                observation = self.observations[knowledge_id]
                if observation.terminal_monotonic is None:
                    observation.terminal_monotonic = now
                observation.final_status = status
        if found < len(tracked):
            missing = [key for key, obs in self.observations.items() if not obs.final_status]
            for knowledge_id in missing[:20]:
                item = self.client.get_knowledge(knowledge_id)
                status = self._pipeline_terminal_status(item)
                if status:
                    observation = self.observations[knowledge_id]
                    observation.terminal_monotonic = observation.terminal_monotonic or now
                    observation.final_status = status

    def wait_for_completion(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        last_terminal = -1
        while time.monotonic() < deadline:
            snapshot = self.sample_queue()
            self.assert_queue_positions(snapshot)
            self._refresh_terminal_states()
            failed = [
                observation
                for observation in self.observations.values()
                if observation.final_status
                and observation.final_status != "completed"
            ]
            if failed:
                raise E2EFailure(
                    "document pipeline failed before the rest of the batch completed: "
                    + ", ".join(
                        f"{observation.knowledge_id}={observation.final_status}"
                        for observation in failed[:20]
                    )
                )
            terminal = sum(bool(obs.final_status) for obs in self.observations.values())
            if terminal != last_terminal:
                self.recorder.emit("workload.progress", terminal=terminal, total=len(self.observations))
                last_terminal = terminal
            if terminal == len(self.observations):
                return
            time.sleep(self.poll_interval)
        pending = [
            {"knowledge_id": obs.knowledge_id, "filename": obs.filename, "status": obs.final_status}
            for obs in self.observations.values()
            if not obs.final_status
        ]
        raise E2EFailure(f"documents did not reach terminal state within {timeout}s: {pending[:20]}")

    def wait_for_some_activity(self, timeout: float = 60.0) -> QueueSnapshot:
        deadline = time.monotonic() + timeout
        latest = QueueSnapshot(0, 0, {})
        while time.monotonic() < deadline:
            latest = self.sample_queue()
            tracked = latest.items.values()
            if latest.active_total > 0 or any(item.state in {"active", "processing", "running"} for item in tracked):
                return latest
            time.sleep(self.poll_interval)
        raise E2EFailure("no document became active before fault injection timeout")

    def run_worker_failover(
        self,
        controller: DockerController,
        *,
        target: str,
        hard_kill: bool,
        down_seconds: float,
        takeover_timeout: float,
        restart: bool,
    ) -> None:
        controller.assert_present()
        self.wait_for_some_activity()
        before_instances = self.client.get_instances(optional=True)
        before_terminal = sum(bool(obs.final_status) for obs in self.observations.values())
        before_snapshot = self.sample_queue()
        controller.stop(target, hard_kill=hard_kill)
        stopped_at = time.monotonic()
        api_failures: list[str] = []
        progress_seen = False
        deadline = stopped_at + takeover_timeout
        while time.monotonic() < deadline:
            try:
                self.client.system_info()
                current_snapshot = self.sample_queue()
                self._refresh_terminal_states()
                now_terminal = sum(bool(obs.final_status) for obs in self.observations.values())
                state_changed = any(
                    current_snapshot.items.get(knowledge_id) != item
                    for knowledge_id, item in before_snapshot.items.items()
                )
                if (
                    now_terminal > before_terminal
                    or current_snapshot.waiting_total < before_snapshot.waiting_total
                    or state_changed
                ):
                    progress_seen = True
                    break
            except Exception as exc:  # public endpoint must remain up through one worker loss
                api_failures.append(str(exc))
            time.sleep(self.poll_interval)
        if api_failures:
            raise E2EFailure(f"public API became unavailable after one worker stopped: {api_failures[:3]}")
        if not progress_seen:
            # It is possible every remaining document is one long active parse; accept a visible
            # healthy survivor as proof of takeover capacity, but never accept a single instance.
            survivors = [
                instance for instance in self.client.get_instances(optional=True)
                if instance.is_ready
            ]
            if before_instances and not survivors:
                raise E2EFailure("no healthy worker survived the injected instance failure")
            if not before_instances:
                raise E2EFailure(
                    "no document progress was observed after one worker stopped and the optional "
                    "instances API is unavailable; failover cannot be proven"
                )
            self.recorder.emit("chaos.takeover_progress_deferred", survivors=len(survivors))
        if down_seconds > 0:
            remaining = down_seconds - (time.monotonic() - stopped_at)
            if remaining > 0:
                time.sleep(remaining)
        if restart:
            controller.start(target)
            if not controller.is_running(target):
                raise E2EFailure(f"worker {target} did not restart")
        self.recorder.emit(
            "chaos.failover_passed",
            target=target,
            hard_kill=hard_kill,
            restarted=restart,
            progress_seen=progress_seen,
        )

    def run_restart_race(
        self,
        controller: DockerController,
        *,
        target: str,
        pause_seconds: float,
    ) -> None:
        controller.assert_present()
        self.wait_for_some_activity()
        epochs_before = {
            key: max(obs.epochs) if obs.epochs else None for key, obs in self.observations.items()
        }
        controller.stop(target, hard_kill=True)
        if pause_seconds:
            time.sleep(pause_seconds)
        controller.start(target)
        deadline = time.monotonic() + 60.0
        while time.monotonic() < deadline:
            self.sample_queue()
            if controller.is_running(target):
                break
            time.sleep(self.poll_interval)
        for knowledge_id, observation in self.observations.items():
            if any(later < earlier for earlier, later in zip(observation.epochs, observation.epochs[1:])):
                raise E2EFailure(f"execution epoch regressed for {knowledge_id}: {observation.epochs}")
            before = epochs_before.get(knowledge_id)
            if before is not None and observation.epochs and max(observation.epochs) < before:
                raise E2EFailure(f"execution epoch regressed after restart for {knowledge_id}")
        self.recorder.emit("chaos.restart_race_passed", target=target, pause_seconds=pause_seconds)

    def run_paused_owner_race(
        self,
        controller: DockerController,
        *,
        target: str,
        paused_seconds: float,
    ) -> None:
        """Freeze and revive an old process without permitting unsafe reclaim.

        Unlike SIGKILL, docker pause leaves the old handler and all of its
        memory intact. Heartbeat/lease expiry is therefore deliberately not a
        termination proof; the workflow must remain owned until the process is
        resumed or an external orchestrator attests a hard execution boundary.
        """
        controller.assert_present()
        self.wait_for_some_activity()
        controller.pause(target)
        paused_at = time.monotonic()
        try:
            while time.monotonic() - paused_at < paused_seconds:
                self.client.system_info()
                self.sample_queue()
                self._refresh_terminal_states()
                time.sleep(self.poll_interval)
        finally:
            controller.unpause(target)
        self.recorder.emit(
            "chaos.paused_owner_revived",
            target=target,
            paused_seconds=time.monotonic() - paused_at,
        )

    def verify_document_outputs(
        self,
        knowledge_ids: Sequence[str],
        *,
        expected: set[str],
        sample_limit: int,
        wiki_timeout: float,
        expected_chunk_text: Sequence[str] = (),
        question_retrieval_sample: int = 3,
        fixture_expectations: Mapping[str, Mapping[str, Any]] | None = None,
    ) -> None:
        all_chunk_types = [
            "text",
            "parent_text",
            "image_ocr",
            "image_caption",
            "summary",
            "entity",
            "relationship",
            "table_summary",
            "table_column",
        ]
        selected = list(knowledge_ids)[: max(1, sample_limit)]
        wiki_selected: list[str] = []
        for knowledge_id in selected:
            observation = self.observations[knowledge_id]
            fixture_expectation = fixture_expectation_for_filename(
                observation.filename,
                fixture_expectations or {},
            )
            document_expected = expected | set(
                fixture_expectation.get("expected_derivatives", set())
            )
            document_expected_text = [
                *expected_chunk_text,
                *fixture_expectation.get("expected_chunk_text", ()),
            ]
            if "wiki" in document_expected:
                wiki_selected.append(knowledge_id)
            knowledge = self.client.get_knowledge(knowledge_id)
            status = str(knowledge.get("parse_status", "")).lower()
            if status != "completed":
                raise E2EFailure(f"cannot verify outputs for {knowledge_id}: parse_status={status!r}")
            chunks = self.client.list_chunks(knowledge_id, all_chunk_types)
            text_chunks = [chunk for chunk in chunks if chunk.get("chunk_type") == "text"]
            if not text_chunks:
                raise E2EFailure(f"completed document {knowledge_id} has no text chunk")
            chunk_ids = [str(chunk.get("id", "")) for chunk in chunks]
            if len(chunk_ids) != len(set(chunk_ids)):
                raise E2EFailure(f"document {knowledge_id} has duplicate chunk ids")
            search_results = self.client.hybrid_search(self.kb_id, observation.marker, [knowledge_id])
            if not any(str(item.get("knowledge_id", "")) == knowledge_id for item in search_results):
                raise E2EFailure(f"vector/keyword search did not retrieve completed document {knowledge_id}")

            spans = self.client.get_spans(knowledge_id)
            span_nodes = flatten_span_nodes(spans)
            span_names = [str(first_value(node, ("name", "span_name"), "")).lower() for node in span_nodes]
            chunk_types = {str(chunk.get("chunk_type", "")) for chunk in chunks}
            vector_evidence = embedding_vector_evidence(span_nodes)
            if vector_evidence is None:
                # Physically split documents currently publish vectors from
                # durable part rows rather than one whole-document embedding
                # span. Their complete coverage is verified by the database
                # audit in the large-document suite. Every ordinary document
                # must expose the stronger per-attempt count evidence here.
                is_physical_split = any(bool(chunk.get("source_locator")) for chunk in text_chunks)
                if not is_physical_split:
                    raise E2EFailure(
                        f"completed document {knowledge_id} has no successful embedding-stage evidence"
                    )
            else:
                persisted_text_count = len(text_chunks)
                if (
                    vector_evidence["chunks_to_embed"] != persisted_text_count
                    or vector_evidence["vectors_written"] != persisted_text_count
                ):
                    raise E2EFailure(
                        f"incomplete vector coverage for {knowledge_id}: "
                        f"persisted_text_chunks={persisted_text_count}, "
                        f"chunks_to_embed={vector_evidence['chunks_to_embed']}, "
                        f"vectors_written={vector_evidence['vectors_written']}"
                    )

            missing_text = missing_chunk_texts(chunks, document_expected_text)
            if missing_text:
                raise E2EFailure(
                    f"persisted chunks for {knowledge_id} are missing expected text: {missing_text}"
                )

            if "summary" in document_expected:
                summary_status = str(knowledge.get("summary_status", "")).lower()
                if summary_status not in {"completed", "done"} and "summary" not in chunk_types:
                    raise E2EFailure(f"summary did not complete for {knowledge_id}: {summary_status!r}")
            question_evidence: dict[str, Any] | None = None
            if "questions" in document_expected:
                question_evidence = validate_generated_question_quality(text_chunks)
                question_texts = [
                    generated_question_text(value)
                    for chunk in text_chunks
                    for value in generated_questions(chunk)
                ]
                verified_retrieval = 0
                for question in [value for value in question_texts if value][: max(0, question_retrieval_sample)]:
                    question_results = self.client.hybrid_search(
                        self.kb_id,
                        question,
                        [knowledge_id],
                    )
                    if not any(
                        str(item.get("knowledge_id", "")) == knowledge_id
                        for item in question_results
                    ):
                        raise E2EFailure(
                            f"generated question is not traceable through retrieval for "
                            f"{knowledge_id}: {question!r}"
                        )
                    verified_retrieval += 1
                question_evidence["retrieval_queries_verified"] = verified_retrieval
            if "graph" in document_expected:
                graph_nodes, graph_relations = graph_artifact_counts(span_nodes)
                if graph_nodes + graph_relations <= 0 and not ({"entity", "relationship"} & chunk_types):
                    raise E2EFailure(
                        f"graph completed without persisted nodes/relations for {knowledge_id}"
                    )
            if "multimodal" in document_expected and not ({"image_ocr", "image_caption"} & chunk_types):
                raise E2EFailure(f"no multimodal child chunks for {knowledge_id}")
            if "table" in document_expected and not ({"table_summary", "table_column"} & chunk_types):
                raise E2EFailure(f"no table-derived chunks for {knowledge_id}")

            # Stable count catches the most common crash/retry double-commit symptom.
            time.sleep(min(1.0, self.poll_interval))
            stable = self.client.list_chunks(knowledge_id, all_chunk_types)
            if len(stable) != len(chunks):
                raise E2EFailure(
                    f"chunk count changed after terminal state for {knowledge_id}: {len(chunks)} -> {len(stable)}"
                )
            self.recorder.emit(
                "document.outputs_passed",
                knowledge_id=knowledge_id,
                chunks=len(chunks),
                chunk_types=sorted(chunk_types),
                search_results=len(search_results),
                span_names=span_names,
                graph_nodes=graph_nodes if "graph" in document_expected else None,
                graph_relations=graph_relations if "graph" in document_expected else None,
                vector_evidence=vector_evidence,
                question_evidence=question_evidence,
                expected_derivatives=sorted(document_expected),
                expected_chunk_text=list(document_expected_text),
            )

        if wiki_selected:
            deadline = time.monotonic() + wiki_timeout
            missing = set(wiki_selected)
            pages: list[Mapping[str, Any]] = []
            while time.monotonic() < deadline and missing:
                pages = self.client.list_wiki_pages(self.kb_id)
                missing = {
                    knowledge_id
                    for knowledge_id in missing
                    if not any(
                        source_refs_include(page, knowledge_id)
                        and len(normalize_question_stem(wiki_page_substantive_text(page))) >= 40
                        for page in pages
                    )
                }
                if missing:
                    time.sleep(max(2.0, self.poll_interval))
            if missing:
                raise E2EFailure(
                    "Wiki did not produce substantive source-linked pages within "
                    f"{wiki_timeout}s: {sorted(missing)}"
                )
            linked_pages = [
                page
                for page in pages
                if any(source_refs_include(page, knowledge_id) for knowledge_id in wiki_selected)
            ]
            self.recorder.emit(
                "wiki.outputs_passed",
                documents=len(wiki_selected),
                source_linked_pages=len(linked_pages),
                substantive_pages=sum(
                    len(normalize_question_stem(wiki_page_substantive_text(page))) >= 40
                    for page in linked_pages
                ),
            )

    def result(self, started_monotonic: float, started_at: str) -> RunResult:
        finished = time.monotonic()
        queue_waits = [
            obs.first_active_monotonic - obs.uploaded_monotonic
            for obs in self.observations.values()
            if obs.first_active_monotonic is not None
        ]
        process_times = [
            obs.terminal_monotonic - (obs.first_active_monotonic or obs.uploaded_monotonic)
            for obs in self.observations.values()
            if obs.terminal_monotonic is not None
        ]
        owner_distribution: dict[str, int] = {}
        for obs in self.observations.values():
            for owner in obs.owners:
                owner_distribution[owner] = owner_distribution.get(owner, 0) + 1
        wall = max(0.001, finished - started_monotonic)
        completed = sum(obs.final_status == "completed" for obs in self.observations.values())
        failed = sum(obs.final_status == "failed" for obs in self.observations.values())
        cancelled = sum(obs.final_status in {"cancelled", "canceled"} for obs in self.observations.values())
        return RunResult(
            run_id=self.run_id,
            started_at=started_at,
            finished_at=utc_now(),
            documents=len(self.observations),
            completed=completed,
            failed=failed,
            cancelled=cancelled,
            wall_seconds=wall,
            throughput_docs_per_second=completed / wall,
            queue_wait_p50_seconds=percentile(queue_waits, 50),
            queue_wait_p95_seconds=percentile(queue_waits, 95),
            processing_p50_seconds=percentile(process_times, 50),
            processing_p95_seconds=percentile(process_times, 95),
            max_waiting_total=self.max_waiting_total,
            max_active_total=self.max_active_total,
            max_active_by_instance=dict(sorted(self.max_active_by_instance.items())),
            owner_distribution=dict(sorted(owner_distribution.items())),
            errors=list(self.errors),
        )

    def cleanup(self) -> None:
        failures: list[str] = []
        with ThreadPoolExecutor(max_workers=8) as pool:
            futures = {
                pool.submit(self.client.delete_knowledge, knowledge_id): knowledge_id
                for knowledge_id in self.observations
            }
            for future in as_completed(futures):
                knowledge_id = futures[future]
                try:
                    future.result()
                except Exception as exc:
                    failures.append(f"{knowledge_id}: {exc}")
        self.recorder.emit("cleanup.finished", failures=failures, deleted=len(self.observations) - len(failures))


def load_json_object(path: Path | None) -> Mapping[str, Any] | None:
    if path is None:
        return None
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, Mapping):
        raise E2EFailure(f"{path} must contain a JSON object")
    return value


def validate_baseline_workload(
    workload_profile: Mapping[str, Any],
    baseline_report: Path,
) -> Mapping[str, Any]:
    """Load a successful baseline and fail fast when workload identity differs."""

    try:
        baseline_report_data = json.loads(baseline_report.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise E2EFailure(f"cannot read baseline report {baseline_report}: {exc}") from exc
    if not isinstance(baseline_report_data, Mapping):
        raise E2EFailure("baseline report root must be an object")
    if baseline_report_data.get("status") != "passed":
        raise E2EFailure("baseline report must be a completed report with status=passed")

    baseline_profile = baseline_report_data.get("workload_profile")
    if not isinstance(baseline_profile, Mapping):
        raise E2EFailure(
            "baseline report has no workload_profile; rerun the baseline with the current harness"
        )
    profile_differences = workload_profile_differences(baseline_profile, workload_profile)
    if profile_differences:
        visible = "; ".join(profile_differences[:20])
        suffix = "" if len(profile_differences) <= 20 else f"; ... {len(profile_differences) - 20} more"
        raise E2EFailure("baseline and scaled workloads are not comparable: " + visible + suffix)
    return baseline_report_data


def validate_performance(
    result: RunResult,
    *,
    min_throughput: float,
    max_p95_processing_seconds: float,
    baseline_report: Path | None,
    min_scaling_efficiency: float,
    workload_profile: Mapping[str, Any] | None = None,
    instance_topology: Mapping[str, Any] | None = None,
) -> dict[str, Any]:
    if result.failed or result.cancelled or result.completed != result.documents:
        raise E2EFailure(
            f"workload did not fully complete: completed={result.completed} "
            f"failed={result.failed} cancelled={result.cancelled} total={result.documents}"
        )
    if min_throughput > 0 and result.throughput_docs_per_second < min_throughput:
        raise E2EFailure(
            f"throughput {result.throughput_docs_per_second:.4f} docs/s is below {min_throughput:.4f}"
        )
    if (
        max_p95_processing_seconds > 0
        and result.processing_p95_seconds is not None
        and result.processing_p95_seconds > max_p95_processing_seconds
    ):
        raise E2EFailure(
            f"processing p95 {result.processing_p95_seconds:.2f}s exceeds "
            f"{max_p95_processing_seconds:.2f}s"
        )
    scaling: dict[str, Any] = {}
    if baseline_report:
        if not isinstance(workload_profile, Mapping):
            raise E2EFailure("current run has no workload_profile and cannot be compared")
        baseline_report_data = validate_baseline_workload(workload_profile, baseline_report)

        baseline = baseline_report_data.get("result")
        if not isinstance(baseline, Mapping):
            raise E2EFailure("baseline report has no result object")
        baseline_documents = to_int(baseline.get("documents"), -1)
        baseline_completed = to_int(baseline.get("completed"), -1)
        baseline_failed = to_int(baseline.get("failed"), -1)
        baseline_cancelled = to_int(baseline.get("cancelled"), -1)
        if (
            baseline_documents <= 0
            or baseline_completed != baseline_documents
            or baseline_failed != 0
            or baseline_cancelled != 0
        ):
            raise E2EFailure(
                "baseline workload did not fully complete successfully: "
                f"completed={baseline_completed} failed={baseline_failed} "
                f"cancelled={baseline_cancelled} total={baseline_documents}"
            )
        try:
            baseline_throughput = float(baseline.get("throughput_docs_per_second", 0))
        except (TypeError, ValueError) as exc:
            raise E2EFailure("baseline throughput_docs_per_second is not numeric") from exc
        if baseline_throughput <= 0:
            raise E2EFailure("baseline report has no positive throughput_docs_per_second")

        def topology_evidence(raw: Any, label: str) -> tuple[int, int]:
            if not isinstance(raw, Mapping):
                raise E2EFailure(
                    f"{label} report has no instance_topology; rerun it with the current harness"
                )
            count = to_int(raw.get("effective_healthy_ready_count"), 0)
            start = raw.get("start")
            end = raw.get("end")
            if count <= 0 or not isinstance(start, Mapping) or not isinstance(end, Mapping):
                raise E2EFailure(f"{label} instance_topology has no positive start/end live count")

            boundary_identities: list[dict[str, tuple[str, int]]] = []
            for boundary_name, boundary in (("start", start), ("end", end)):
                boundary_count = to_int(boundary.get("healthy_ready_count"), -1)
                items = boundary.get("healthy_ready_instances")
                if boundary_count != count or not isinstance(items, list) or len(items) != count:
                    raise E2EFailure(
                        f"{label} {boundary_name} topology count/list does not match "
                        f"effective count {count}"
                    )
                identities: dict[str, tuple[str, int]] = {}
                for item in items:
                    if not isinstance(item, Mapping):
                        raise E2EFailure(f"{label} {boundary_name} topology item is not an object")
                    instance_id = str(item.get("instance_id", ""))
                    boot_id = str(item.get("boot_id", ""))
                    capacity = to_int(item.get("capacity"), 0)
                    if not instance_id or not boot_id or capacity <= 0:
                        raise E2EFailure(
                            f"{label} {boundary_name} topology lacks instance_id, boot_id, or capacity"
                        )
                    if instance_id in identities:
                        raise E2EFailure(
                            f"{label} {boundary_name} topology duplicates instance_id {instance_id!r}"
                        )
                    identities[instance_id] = (boot_id, capacity)
                boundary_identities.append(identities)
            if boundary_identities[0] != boundary_identities[1]:
                raise E2EFailure(f"{label} topology changed during its measured run")
            capacities = {capacity for _, capacity in boundary_identities[0].values()}
            if len(capacities) != 1:
                raise E2EFailure(
                    f"{label} healthy-ready instances do not share one per-instance capacity: "
                    f"{sorted(capacities)}"
                )
            return count, capacities.pop()

        baseline_instances, baseline_capacity = topology_evidence(
            baseline_report_data.get("instance_topology"),
            "baseline",
        )
        scaled_instances, scaled_capacity = topology_evidence(instance_topology, "scaled")
        if baseline_capacity != scaled_capacity:
            raise E2EFailure(
                "per-instance concurrency differs between baseline and scaled runs: "
                f"baseline={baseline_capacity} scaled={scaled_capacity}"
            )
        expansion_factor = scaled_instances / baseline_instances
        if expansion_factor <= 1:
            raise E2EFailure(
                "scaled run must have more API-observed healthy-ready instances than baseline: "
                f"baseline={baseline_instances} scaled={scaled_instances}"
            )
        speedup = result.throughput_docs_per_second / baseline_throughput
        efficiency = speedup / expansion_factor
        scaling = {
            "workload_fingerprint": workload_profile_fingerprint(workload_profile),
            "baseline_throughput_docs_per_second": baseline_throughput,
            "speedup": speedup,
            "baseline_healthy_ready_instances": baseline_instances,
            "scaled_healthy_ready_instances": scaled_instances,
            "instance_expansion_factor": expansion_factor,
            "per_instance_capacity": scaled_capacity,
            "scaling_efficiency": efficiency,
        }
        if min_scaling_efficiency > 0 and efficiency < min_scaling_efficiency:
            raise E2EFailure(
                f"scaling efficiency {efficiency:.3f} is below {min_scaling_efficiency:.3f}"
            )
    return scaling
