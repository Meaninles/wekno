from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import re
import sys
import time
import uuid
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import (  # type: ignore
        APIClient,
        APIError,
        ClusterE2ERunner,
        E2EFailure,
        JsonlRecorder,
        SUPPORTED_FIXTURE_SUFFIXES,
        first_value,
        load_json_object,
        to_int,
        unwrap_data,
        utc_now,
        validate_instance_topology,
    )
    from multitenant_e2e import (  # type: ignore
        EXACT_DEEPSEEK_NAME,
        EXACT_DEEPSEEK_SOURCE_ID,
        EXACT_QWEN_ASR_NAME,
        EXACT_QWEN_VLM_NAME,
        MultiTenantClusterRunner,
        MultiTenantObservation,
        TenantProvisioner,
        TestPrincipal,
        Variant,
        metrics_delta,
        required_mapping,
        scrape_metrics,
    )
else:
    from .cluster_e2e import (
        APIClient,
        APIError,
        ClusterE2ERunner,
        E2EFailure,
        JsonlRecorder,
        SUPPORTED_FIXTURE_SUFFIXES,
        first_value,
        load_json_object,
        to_int,
        unwrap_data,
        utc_now,
        validate_instance_topology,
    )
    from .multitenant_e2e import (
        EXACT_DEEPSEEK_NAME,
        EXACT_DEEPSEEK_SOURCE_ID,
        EXACT_QWEN_ASR_NAME,
        EXACT_QWEN_VLM_NAME,
        MultiTenantClusterRunner,
        MultiTenantObservation,
        TenantProvisioner,
        TestPrincipal,
        Variant,
        metrics_delta,
        required_mapping,
        scrape_metrics,
    )


HERE = Path(__file__).resolve().parent
DEFAULT_POLICY_ROOT = Path(r"C:\weknora\.local-data\e2e-corpora\policy-20260724-2325")
DEFAULT_DATA_ROOTS = (
    Path(r"C:\Users\erjiguan\Documents\部门数据资产调研过程文档\01财务部-股份"),
    Path(
        r"C:\Users\erjiguan\Documents\大数据服务平台_数据资产目录_20230401"
        r"\大数据服务平台_数据资产目录_20230401"
    ),
)
EXPECTED_POLICY_DOCUMENTS = 635
EXPECTED_DATA_DOCUMENTS = 11


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=(
            "Upload and fully verify the real policy and data-asset corpora in "
            "two tenants while three document-processing instances are active."
        )
    )
    p.add_argument(
        "--base-url",
        default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"),
    )
    p.add_argument(
        "--admin-token",
        default=os.getenv("WEKNORA_E2E_ADMIN_TOKEN", ""),
        help="system-admin bearer token; never persisted in events or reports",
    )
    p.add_argument("--policy-root", type=Path, default=DEFAULT_POLICY_ROOT)
    p.add_argument(
        "--data-root",
        type=Path,
        action="append",
        default=None,
        help="repeatable; defaults to the two requested data-asset folders",
    )
    p.add_argument("--policy-kb-name", default="公司制度")
    p.add_argument("--data-kb-name", default="数据资产资料")
    p.add_argument(
        "--process-config",
        type=Path,
        default=HERE / "process_config.full.example.json",
    )
    p.add_argument("--upload-concurrency", type=int, default=24)
    p.add_argument("--verify-concurrency", type=int, default=12)
    p.add_argument("--poll-interval", type=float, default=5.0)
    p.add_argument("--timeout", type=float, default=86400.0)
    p.add_argument("--wiki-timeout", type=float, default=21600.0)
    p.add_argument("--http-timeout", type=float, default=300.0)
    p.add_argument("--instance-count", type=int, default=3)
    p.add_argument("--instance-concurrency", type=int, default=4)
    p.add_argument(
        "--worker-container",
        action="append",
        default=[],
        help="repeat for every app replica to preserve per-process metrics evidence",
    )
    p.add_argument("--allow-count-drift", action="store_true")
    p.add_argument("--resume-policy-kb-id", default="")
    p.add_argument("--resume-data-tenant-id", type=int, default=0)
    p.add_argument("--resume-data-kb-id", default="")
    p.add_argument("--resume-run-id", default="")
    p.add_argument(
        "--cleanup-on-exit",
        action="store_true",
        help="delete documents, both KBs, and the disposable second tenant",
    )
    p.add_argument(
        "--output-dir",
        type=Path,
        default=HERE / "outputs" / "dual-real-corpus",
    )
    return p


def validate_args(args: argparse.Namespace) -> None:
    if not args.admin_token:
        raise E2EFailure("--admin-token or WEKNORA_E2E_ADMIN_TOKEN is required")
    if not args.policy_root.is_dir():
        raise E2EFailure(f"policy corpus directory not found: {args.policy_root}")
    data_roots = tuple(args.data_root or DEFAULT_DATA_ROOTS)
    missing = [str(path) for path in data_roots if not path.is_dir()]
    if missing:
        raise E2EFailure(f"data corpus directories not found: {missing}")
    if not args.process_config.is_file():
        raise E2EFailure(f"process config not found: {args.process_config}")
    for name in (
        "upload_concurrency",
        "verify_concurrency",
        "instance_count",
        "instance_concurrency",
    ):
        if getattr(args, name) <= 0:
            raise E2EFailure(f"--{name.replace('_', '-')} must be positive")
    if args.worker_container and len(args.worker_container) != args.instance_count:
        raise E2EFailure(
            "--worker-container must be omitted or supplied exactly once per instance"
        )
    if len(set(args.worker_container)) != len(args.worker_container):
        raise E2EFailure("--worker-container values must be unique")
    resume_values = (
        bool(args.resume_policy_kb_id),
        args.resume_data_tenant_id > 0,
        bool(args.resume_data_kb_id),
        bool(args.resume_run_id),
    )
    if any(resume_values) and not all(resume_values):
        raise E2EFailure(
            "resume requires --resume-policy-kb-id, --resume-data-tenant-id, "
            "--resume-data-kb-id, and --resume-run-id together"
        )


def _inventory(roots: Sequence[Path]) -> tuple[list[Path], list[Path]]:
    supported: list[Path] = []
    excluded: list[Path] = []
    seen: set[Path] = set()
    for root in roots:
        for path in sorted(root.rglob("*"), key=lambda value: str(value).casefold()):
            if not path.is_file():
                continue
            resolved = path.resolve()
            if resolved in seen:
                continue
            seen.add(resolved)
            # Microsoft Office lock/owner files are not documents. They may
            # be hidden and retain a supported suffix, but contain only a few
            # bytes of editor ownership metadata.
            if path.name.startswith("~$"):
                excluded.append(path)
            elif path.suffix.lower() in SUPPORTED_FIXTURE_SUFFIXES:
                supported.append(path)
            else:
                excluded.append(path)
    return supported, excluded


def _inventory_report(paths: Sequence[Path], excluded: Sequence[Path]) -> dict[str, Any]:
    sizes = [path.stat().st_size for path in paths]
    return {
        "documents": len(paths),
        "bytes": sum(sizes),
        "largest_bytes": max(sizes, default=0),
        "formats": dict(
            sorted(Counter(path.suffix.lower().lstrip(".") for path in paths).items())
        ),
        "excluded": [
            {"path": str(path), "suffix": path.suffix.lower(), "bytes": path.stat().st_size}
            for path in excluded
        ],
    }


@dataclass(frozen=True)
class CorpusEntry:
    source_index: int
    path: Path


def _canonical_entries(
    paths: Sequence[Path],
    *,
    index_offset: int,
) -> tuple[list[CorpusEntry], list[dict[str, Any]]]:
    canonical: dict[str, CorpusEntry] = {}
    entries: list[CorpusEntry] = []
    aliases: list[dict[str, Any]] = []
    for local_index, path in enumerate(paths):
        digest = RealCorpusFactory.digest(path)
        source_index = index_offset + local_index
        existing = canonical.get(digest)
        if existing is not None:
            aliases.append(
                {
                    "source_index": source_index,
                    "path": str(path),
                    "canonical_source_index": existing.source_index,
                    "canonical_path": str(existing.path),
                    "sha256": digest,
                }
            )
            continue
        entry = CorpusEntry(source_index=source_index, path=path)
        canonical[digest] = entry
        entries.append(entry)
    return entries, aliases


class RealCorpusFactory:
    def __init__(self, entries: Sequence[CorpusEntry]) -> None:
        self.entries = list(entries)

    @staticmethod
    def digest(path: Path) -> str:
        digest = hashlib.sha256()
        with path.open("rb") as handle:
            while block := handle.read(1024 * 1024):
                digest.update(block)
        return digest.hexdigest()

    @staticmethod
    def _size_class(size: int) -> str:
        if size < 1024 * 1024:
            return "small"
        if size < 10 * 1024 * 1024:
            return "medium"
        return "large"

    def build(self, index: int, run_id: str) -> Variant:
        entry = self.entries[index]
        path = entry.path
        content = path.read_bytes()
        marker = f"REAL-{run_id}-{entry.source_index:05d}"
        return Variant(
            filename=f"{marker}-{path.name}",
            marker=marker,
            extension=path.suffix.lower().lstrip("."),
            size_class=self._size_class(len(content)),
            target_kib=max(1, math.ceil(len(content) / 1024)),
            content=content,
            content_unique=True,
        )


class DualCorpusRunner(MultiTenantClusterRunner):
    def __init__(self, *args: Any, policy_count: int, **kwargs: Any) -> None:
        super().__init__(*args, **kwargs)
        self.policy_count = policy_count

    def _assignment(self, index: int) -> tuple[TestPrincipal, str]:
        principal = self.principals[0 if index < self.policy_count else 1]
        return principal, principal.knowledge_base_ids[0]

    def verify_cross_tenant_isolation(self) -> None:
        # Principal 0 is intentionally the system administrator and is
        # allowed to inspect other tenants. The meaningful isolation
        # direction is the disposable ordinary tenant trying to read the
        # administrator's policy document.
        protected = next(
            observation
            for observation in self.observations.values()
            if observation.principal_index == 0
        )
        try:
            self.principals[1].client.get_knowledge(protected.knowledge_id)
        except APIError as exc:
            if exc.status not in {403, 404}:
                raise
        else:
            raise E2EFailure(
                "ordinary secondary tenant could read an administrator-tenant document"
            )
        self.recorder.emit(
            "dual_real_corpus.cross_tenant_isolation_passed",
            protected_knowledge_id=protected.knowledge_id,
        )

    @staticmethod
    def _metadata(row: Mapping[str, Any]) -> Mapping[str, Any]:
        metadata = row.get("metadata")
        if isinstance(metadata, str):
            try:
                metadata = json.loads(metadata)
            except json.JSONDecodeError:
                metadata = {}
        return metadata if isinstance(metadata, Mapping) else {}

    def restore_existing(
        self,
        *,
        expected_run_id: str,
        entry_sequence_by_source_index: Mapping[int, int],
    ) -> list[int]:
        restored_sources: set[int] = set()
        marker_pattern = re.compile(r"-(\d{5})$")
        for principal in self.principals:
            kb_id = principal.knowledge_base_ids[0]
            for row in principal.client.list_all_knowledge(kb_id):
                metadata = self._metadata(row)
                if str(metadata.get("e2e_run_id", "")) != expected_run_id:
                    continue
                marker = str(metadata.get("e2e_marker", "")).strip()
                match = marker_pattern.search(marker)
                if not match:
                    raise E2EFailure(
                        f"resumed document has no source index in marker: {row.get('id')}"
                    )
                source_index = int(match.group(1))
                sequence_index = entry_sequence_by_source_index.get(source_index)
                if sequence_index is None:
                    raise E2EFailure(
                        f"resumed document maps to a duplicate/unknown source index "
                        f"{source_index}: {row.get('id')}"
                    )
                knowledge_id = str(row.get("id", "")).strip()
                if not knowledge_id:
                    raise E2EFailure("resumed knowledge row has no id")
                if source_index in restored_sources or knowledge_id in self.observations:
                    raise E2EFailure(
                        f"duplicate resumed mapping for source {source_index}: {knowledge_id}"
                    )
                restored_sources.add(source_index)
                filename = str(
                    first_value(row, ("file_name", "title", "name"), "")
                ).strip()
                extension = str(row.get("file_type", "")).strip().lower()
                source_bytes = to_int(row.get("file_size"), 0)
                self.observations[knowledge_id] = MultiTenantObservation(
                    index=sequence_index,
                    principal_index=principal.index,
                    tenant_id=principal.tenant_id,
                    user_id=principal.user_id,
                    kb_id=kb_id,
                    knowledge_id=knowledge_id,
                    filename=filename,
                    marker=marker,
                    extension=extension,
                    size_class=RealCorpusFactory._size_class(source_bytes),
                    target_kib=max(1, math.ceil(source_bytes / 1024)),
                    source_bytes=source_bytes,
                    content_unique=True,
                    uploaded_at=time.monotonic(),
                )
        missing = sorted(set(entry_sequence_by_source_index) - restored_sources)
        self.recorder.emit(
            "dual_real_corpus.resume_restored",
            restored=len(restored_sources),
            missing_source_indices=missing,
        )
        return missing

    def upload_missing(
        self,
        source_indices: Sequence[int],
        *,
        entry_sequence_by_source_index: Mapping[int, int],
        process_config: Mapping[str, Any] | None,
        concurrency: int,
    ) -> None:
        def one(source_index: int) -> MultiTenantObservation:
            sequence_index = entry_sequence_by_source_index[source_index]
            principal, kb_id = self._assignment(sequence_index)
            variant = self.factory.build(sequence_index, self.run_id)
            uploaded_at = time.monotonic()
            data = self._upload_with_retry(
                principal, kb_id, variant, process_config, self.run_id
            )
            return MultiTenantObservation(
                index=sequence_index,
                principal_index=principal.index,
                tenant_id=principal.tenant_id,
                user_id=principal.user_id,
                kb_id=kb_id,
                knowledge_id=str(data["id"]),
                filename=variant.filename,
                marker=variant.marker,
                extension=variant.extension,
                size_class=variant.size_class,
                target_kib=variant.target_kib,
                source_bytes=len(variant.content),
                content_unique=True,
                uploaded_at=uploaded_at,
            )

        failures: list[str] = []
        with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
            futures = {
                pool.submit(one, source_index): source_index
                for source_index in source_indices
            }
            for future in as_completed(futures):
                source_index = futures[future]
                try:
                    observation = future.result()
                    if observation.knowledge_id in self.observations:
                        raise E2EFailure(
                            f"missing source {source_index} resolved to an existing "
                            f"knowledge id {observation.knowledge_id}"
                        )
                    self.observations[observation.knowledge_id] = observation
                except Exception as exc:
                    failures.append(f"source {source_index}: {exc}")
        if failures:
            raise E2EFailure(f"failed to upload missing corpus documents: {failures}")
        self.recorder.emit(
            "dual_real_corpus.resume_missing_uploaded",
            source_indices=list(source_indices),
        )


def _current_identity(client: APIClient) -> tuple[str, int, str]:
    payload = unwrap_data(client.request("GET", "/api/v1/auth/me"))
    data = required_mapping(payload, "current-user response")
    user = required_mapping(data.get("user"), "current-user user")
    tenant = required_mapping(data.get("tenant"), "current-user tenant")
    user_id = str(user.get("id", "")).strip()
    tenant_id = to_int(tenant.get("id"), to_int(user.get("tenant_id"), 0))
    username = str(first_value(user, ("username", "email", "name"), "")).strip()
    if not user_id or tenant_id <= 0:
        raise E2EFailure(
            f"current user lacks user/tenant identity: user={user!r} tenant={tenant!r}"
        )
    return user_id, tenant_id, username


def _knowledge_bases(client: APIClient) -> list[Mapping[str, Any]]:
    data = unwrap_data(client.request("GET", "/api/v1/knowledge-bases"))
    if isinstance(data, Mapping):
        data = first_value(data, ("items", "list", "data"), [])
    if not isinstance(data, list):
        raise E2EFailure(f"knowledge-base list has unexpected shape: {data!r}")
    return [item for item in data if isinstance(item, Mapping)]


def _create_policy_kb(
    client: APIClient,
    provisioner: TenantProvisioner,
    name: str,
) -> tuple[str, Mapping[str, Any], Mapping[str, Any]]:
    conflicts = [
        item for item in _knowledge_bases(client) if str(item.get("name", "")).strip() == name
    ]
    if conflicts:
        raise E2EFailure(
            f"knowledge base {name!r} already exists; refusing to mix a production "
            "acceptance run with pre-existing documents"
        )
    models = provisioner._wait_models(client)
    body = provisioner._kb_body(
        name,
        models,
        description=(
            "真实生产验收：股份公司制度文件；向量、问题、知识图谱、Wiki、摘要和多模态全开"
        ),
        # The exact Qwen model remains bound, but the corpus has no audio and
        # the current upstream endpoint rejects unauthenticated calls.
        asr_enabled=False,
        wiki_ingest_batch_size=20,
        wiki_ingest_map_parallel=4,
        wiki_ingest_reduce_parallel=4,
    )
    created = required_mapping(
        client.request("POST", "/api/v1/knowledge-bases", json_body=body),
        "policy knowledge-base create response",
    )
    kb_id = str(created.get("id", "")).strip()
    if not kb_id:
        raise E2EFailure("policy knowledge-base create response has no id")
    detail = client.get_knowledge_base(kb_id)
    return kb_id, detail, {
        "chat_id": models.chat_id,
        "embedding_id": models.embedding_id,
        "vlm_id": models.vlm_id,
        "asr_id": models.asr_id,
        "chat_name": models.chat_name,
        "chat_base_url": models.chat_base_url,
    }


def _assert_kb_models(
    detail: Mapping[str, Any],
    expected: Mapping[str, Any],
    *,
    label: str,
) -> dict[str, Any]:
    if str(detail.get("summary_model_id", "")) != expected["chat_id"]:
        raise E2EFailure(f"{label} does not bind exact DeepSeek summary model")
    if str(detail.get("embedding_model_id", "")) != expected["embedding_id"]:
        raise E2EFailure(f"{label} does not bind expected embedding model")
    vlm = detail.get("vlm_config")
    if not isinstance(vlm, Mapping):
        vlm = detail.get("image_processing_config")
    if not isinstance(vlm, Mapping) or str(vlm.get("model_id", "")) != expected["vlm_id"]:
        raise E2EFailure(f"{label} does not bind exact Qwen VLM model")
    asr = detail.get("asr_config")
    if not isinstance(asr, Mapping) or str(asr.get("model_id", "")) != expected["asr_id"]:
        raise E2EFailure(f"{label} does not retain exact Qwen ASR model binding")
    indexing = detail.get("indexing_strategy")
    if not isinstance(indexing, Mapping) or not all(
        bool(indexing.get(key))
        for key in ("vector_enabled", "keyword_enabled", "wiki_enabled", "graph_enabled")
    ):
        raise E2EFailure(f"{label} does not enable every indexing/derivative capability")
    return {
        "summary_model_id": expected["chat_id"],
        "embedding_model_id": expected["embedding_id"],
        "vlm_model_id": expected["vlm_id"],
        "vlm_model_name": EXACT_QWEN_VLM_NAME,
        "asr_model_id": expected["asr_id"],
        "asr_model_name": EXACT_QWEN_ASR_NAME,
        "asr_enabled": bool(asr.get("enabled")),
        "deepseek_model_name": expected["chat_name"],
        "deepseek_base_url": expected["chat_base_url"],
    }


def _model_mapping(models: Any) -> dict[str, Any]:
    return {
        "chat_id": models.chat_id,
        "embedding_id": models.embedding_id,
        "vlm_id": models.vlm_id,
        "asr_id": models.asr_id,
        "chat_name": models.chat_name,
        "chat_base_url": models.chat_base_url,
    }


def _assert_metric_distribution(
    before: Mapping[str, Mapping[str, float]],
    containers: Sequence[str],
) -> dict[str, dict[str, float]]:
    if not containers:
        return {}
    delta = metrics_delta(before, scrape_metrics(containers))
    required_each = ("document", "postprocess", "summary", "questions", "graph", "wiki")
    for container in containers:
        values = delta.get(container, {})
        missing = [
            stage for stage in required_each if values.get(f"{stage}:success", 0) <= 0
        ]
        if missing:
            raise E2EFailure(
                f"worker {container} did not execute every core/derivative stage: {missing}"
            )
    for specialized in ("multimodal", "table"):
        if sum(values.get(f"{specialized}:success", 0) for values in delta.values()) <= 0:
            raise E2EFailure(
                f"real corpus produced no successful {specialized} stage on any instance"
            )
    return delta


def main() -> int:
    args = parser().parse_args()
    try:
        validate_args(args)
    except E2EFailure as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2

    policy_paths, policy_excluded = _inventory((args.policy_root,))
    data_roots = tuple(args.data_root or DEFAULT_DATA_ROOTS)
    data_paths, data_excluded = _inventory(data_roots)
    policy_entries, policy_aliases = _canonical_entries(
        policy_paths,
        index_offset=0,
    )
    data_entries, data_aliases = _canonical_entries(
        data_paths,
        index_offset=len(policy_paths),
    )
    corpus_entries = [*policy_entries, *data_entries]
    entry_sequence_by_source_index = {
        entry.source_index: sequence_index
        for sequence_index, entry in enumerate(corpus_entries)
    }
    if not args.allow_count_drift:
        if len(policy_paths) != EXPECTED_POLICY_DOCUMENTS:
            print(
                f"ERROR: expected {EXPECTED_POLICY_DOCUMENTS} policy documents, "
                f"found {len(policy_paths)}",
                file=sys.stderr,
            )
            return 2
        if len(data_paths) != EXPECTED_DATA_DOCUMENTS:
            print(
                f"ERROR: expected {EXPECTED_DATA_DOCUMENTS} data documents, "
                f"found {len(data_paths)}",
                file=sys.stderr,
            )
            return 2

    run_stamp = time.strftime("%Y%m%d-%H%M%S")
    run_id = args.resume_run_id or f"dual-real-{run_stamp}-{uuid.uuid4().hex[:6]}"
    run_dir = args.output_dir / run_stamp
    recorder = JsonlRecorder(run_dir / "events.jsonl")
    admin = APIClient(
        args.base_url,
        args.admin_token,
        auth_mode="bearer",
        timeout=args.http_timeout,
    )
    provisioner = TenantProvisioner(
        admin,
        recorder,
        exact_chat_source_id=EXACT_DEEPSEEK_SOURCE_ID,
        exact_chat_name=EXACT_DEEPSEEK_NAME,
        expected_chat_base_url=":14000",
        source_tenant_id=10000,
        require_asr_credential=False,
    )
    report: dict[str, Any] = {
        "run_id": run_id,
        "started_at": utc_now(),
        "corpora": {
            "policy": {
                **_inventory_report(policy_paths, policy_excluded),
                "unique_content_documents": len(policy_entries),
                "duplicate_aliases": policy_aliases,
            },
            "data": {
                **_inventory_report(data_paths, data_excluded),
                "unique_content_documents": len(data_entries),
                "duplicate_aliases": data_aliases,
            },
        },
        "config": {
            "base_url": args.base_url,
            "policy_kb_name": args.policy_kb_name,
            "data_kb_name": args.data_kb_name,
            "upload_concurrency": args.upload_concurrency,
            "verify_concurrency": args.verify_concurrency,
            "instance_count": args.instance_count,
            "instance_concurrency": args.instance_concurrency,
            "worker_containers": list(args.worker_container),
            "resumed": bool(args.resume_run_id),
            "asr_runtime_tested": False,
            "asr_runtime_blocker": (
                "Exact Qwen3-ASR-1.7B endpoint currently returns HTTP 401; "
                "this non-audio corpus binds the exact model but disables ASR calls."
            ),
        },
    }
    started = time.monotonic()
    policy_kb_id = ""
    secondary_principals: list[TestPrincipal] = []
    runner: DualCorpusRunner | None = None
    return_code = 1

    try:
        smoke = ClusterE2ERunner(
            admin,
            "00000000-0000-0000-0000-000000000000",
            recorder,
            run_id=run_id,
            poll_interval=args.poll_interval,
        )
        start_instances = smoke.api_smoke(
            args.instance_concurrency,
            require_instance_topology=True,
        )
        topology_start = validate_instance_topology(
            start_instances,
            start_instances,
            expected_count=args.instance_count,
            required=True,
        )
        expected_instances = [
            item["instance_id"]
            for item in topology_start["start"]["healthy_ready_instances"]
        ]
        metrics_before = (
            scrape_metrics(args.worker_container) if args.worker_container else {}
        )

        admin_user_id, admin_tenant_id, admin_username = _current_identity(admin)
        if args.resume_run_id:
            policy_kb_id = args.resume_policy_kb_id
            policy_detail = admin.get_knowledge_base(policy_kb_id)
            if str(policy_detail.get("name", "")) != args.policy_kb_name:
                raise E2EFailure(
                    f"resume policy KB name mismatch: {policy_detail.get('name')!r}"
                )
            policy_models = _model_mapping(provisioner._wait_models(admin))
            policy_principal = TestPrincipal(
                index=0,
                username=admin_username,
                user_id=admin_user_id,
                tenant_id=admin_tenant_id,
                client=admin,
                knowledge_base_ids=[policy_kb_id],
            )

            resumed_api_key = provisioner._reset_api_key(args.resume_data_tenant_id)
            resumed_client = APIClient(
                args.base_url,
                resumed_api_key,
                auth_mode="api-key",
                timeout=args.http_timeout,
            )
            data_detail = resumed_client.get_knowledge_base(args.resume_data_kb_id)
            if str(data_detail.get("name", "")) != args.data_kb_name:
                raise E2EFailure(
                    f"resume data KB name mismatch: {data_detail.get('name')!r}"
                )
            resumed_user_id = str(data_detail.get("creator_id", "")).strip()
            if not resumed_user_id:
                resumed_rows = resumed_client.list_all_knowledge(args.resume_data_kb_id)
                resumed_user_id = next(
                    (
                        str(row.get("creator_id", "")).strip()
                        for row in resumed_rows
                        if str(row.get("creator_id", "")).strip()
                    ),
                    "",
                )
            if not resumed_user_id:
                raise E2EFailure("resume data KB has no creator identity")
            secondary_principal = TestPrincipal(
                index=1,
                username=f"tenant-{args.resume_data_tenant_id}",
                user_id=resumed_user_id,
                tenant_id=args.resume_data_tenant_id,
                client=resumed_client,
                knowledge_base_ids=[args.resume_data_kb_id],
            )
            secondary_principals = [secondary_principal]
        else:
            policy_kb_id, policy_detail, policy_models = _create_policy_kb(
                admin, provisioner, args.policy_kb_name
            )
            policy_principal = TestPrincipal(
                index=0,
                username=admin_username,
                user_id=admin_user_id,
                tenant_id=admin_tenant_id,
                client=admin,
                knowledge_base_ids=[policy_kb_id],
            )
            secondary_principals = provisioner.provision(
                principal_count=1,
                knowledge_bases_per_principal=1,
                run_suffix=run_id,
                knowledge_base_name_factory=lambda _principal, _kb: args.data_kb_name,
                knowledge_base_body_kwargs={
                    "description": (
                        "真实生产验收：财务部和大数据平台数据资产资料；"
                        "向量、问题、知识图谱、Wiki、摘要和多模态全开"
                    ),
                    "asr_enabled": False,
                    "wiki_ingest_batch_size": 20,
                    "wiki_ingest_map_parallel": 4,
                    "wiki_ingest_reduce_parallel": 4,
                },
            )
            secondary_principal = secondary_principals[0]
            secondary_principal.index = 1
            data_detail = secondary_principal.client.get_knowledge_base(
                secondary_principal.knowledge_base_ids[0]
            )
        data_models_selection = provisioner._wait_models(secondary_principal.client)
        data_models = _model_mapping(data_models_selection)
        report["model_bindings"] = {
            "policy": _assert_kb_models(
                policy_detail, policy_models, label=args.policy_kb_name
            ),
            "data": _assert_kb_models(
                data_detail, data_models, label=args.data_kb_name
            ),
        }

        factory = RealCorpusFactory(corpus_entries)
        runner = DualCorpusRunner(
            admin,
            [policy_principal, secondary_principal],
            factory,  # type: ignore[arg-type]
            recorder,
            run_id=run_id,
            poll_interval=args.poll_interval,
            worker_containers=(),
            policy_count=len(policy_entries),
        )
        process_config = load_json_object(args.process_config)
        if args.resume_run_id:
            missing_source_indices = runner.restore_existing(
                expected_run_id=run_id,
                entry_sequence_by_source_index=entry_sequence_by_source_index,
            )
            if missing_source_indices:
                runner.upload_missing(
                    missing_source_indices,
                    entry_sequence_by_source_index=entry_sequence_by_source_index,
                    process_config=process_config,
                    concurrency=args.upload_concurrency,
                )
        else:
            runner.upload(
                len(corpus_entries),
                concurrency=args.upload_concurrency,
                process_config=process_config,
            )
        if len(runner.observations) != len(corpus_entries):
            raise E2EFailure(
                "actual knowledge coverage does not match unique source content: "
                f"actual={len(runner.observations)} expected={len(corpus_entries)}"
            )
        runner.verify_cross_tenant_isolation()
        runner.wait_for_completion(args.timeout)
        verification = runner.verify_outputs(
            concurrency=args.verify_concurrency,
            wiki_timeout=args.wiki_timeout,
            # Every real document is queried, not sampled.
            retrieval_sample=len(corpus_entries),
            expected_instances=expected_instances,
            metrics_before={},
        )
        metric_distribution = (
            metrics_delta(metrics_before, scrape_metrics(args.worker_container))
            if args.resume_run_id and args.worker_container
            else _assert_metric_distribution(metrics_before, args.worker_container)
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
                "metric_distribution": metric_distribution,
                "instance_topology": topology,
                "policy_knowledge_base": {
                    "id": policy_kb_id,
                    "name": args.policy_kb_name,
                    "tenant_id": admin_tenant_id,
                    "source_files": len(policy_paths),
                    "unique_documents": len(policy_entries),
                    "duplicate_aliases": len(policy_aliases),
                },
                "data_knowledge_base": {
                    "id": secondary_principal.knowledge_base_ids[0],
                    "name": args.data_kb_name,
                    "tenant_id": secondary_principal.tenant_id,
                    "username": secondary_principal.username,
                    "source_files": len(data_paths),
                    "unique_documents": len(data_entries),
                    "duplicate_aliases": len(data_aliases),
                },
            }
        )
        recorder.emit(
            "dual_real_corpus.run_passed",
            result=asdict(result),
            policy_kb_id=policy_kb_id,
            data_kb_id=secondary_principal.knowledge_base_ids[0],
            instance_topology=topology,
        )
        return_code = 0
    except Exception as exc:
        report.update(
            {
                "status": "failed",
                "error": str(exc),
                "error_type": type(exc).__name__,
            }
        )
        recorder.emit(
            "dual_real_corpus.run_failed",
            error=str(exc),
            error_type=type(exc).__name__,
        )
    finally:
        cleanup_failures: list[str] = []
        if args.cleanup_on_exit:
            if runner is not None and runner.observations:
                cleanup_failures.extend(runner.cleanup_documents(args.verify_concurrency))
            if policy_kb_id:
                try:
                    admin.request("DELETE", f"/api/v1/knowledge-bases/{policy_kb_id}")
                except Exception as exc:
                    cleanup_failures.append(f"delete policy KB {policy_kb_id}: {exc}")
            if secondary_principals:
                cleanup_failures.extend(
                    provisioner.cleanup_principals(secondary_principals)
                )
        if cleanup_failures:
            report["cleanup_failures"] = cleanup_failures
        report["finished_at"] = utc_now()
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
