from __future__ import annotations

import html
import io
import json
import math
import re
import struct
import subprocess
import threading
import time
import uuid
import zipfile
from collections import Counter, defaultdict
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

if __package__ in {None, ""}:
    from cluster_e2e import (  # type: ignore
        APIClient,
        APIError,
        E2EFailure,
        FAILED_STAGE_STATUSES,
        JsonlRecorder,
        QueueItem,
        TERMINAL_STATUSES,
        embedding_vector_evidence,
        first_value,
        flatten_span_nodes,
        graph_artifact_counts,
        normalize_question_stem,
        percentile,
        source_refs_include,
        to_int,
        unwrap_data,
        utc_now,
        validate_generated_question_quality,
        wiki_page_substantive_text,
    )
else:
    from .cluster_e2e import (
        APIClient,
        APIError,
        E2EFailure,
        FAILED_STAGE_STATUSES,
        JsonlRecorder,
        QueueItem,
        TERMINAL_STATUSES,
        embedding_vector_evidence,
        first_value,
        flatten_span_nodes,
        graph_artifact_counts,
        normalize_question_stem,
        percentile,
        source_refs_include,
        to_int,
        unwrap_data,
        utc_now,
        validate_generated_question_quality,
        wiki_page_substantive_text,
    )


EXACT_DEEPSEEK_SOURCE_ID = "10659f1e-87c5-4ceb-bdc2-49232217c091"
EXACT_DEEPSEEK_NAME = "deepseek-v4-flash-int8"
EXACT_QWEN_VLM_NAME = "/models/Qwen3-VL-32B-Instruct"
EXACT_QWEN_ASR_NAME = "/cusa/models/Qwen3-ASR-1.7B"
EXECUTOR_INSTANCE_KEY = "executor_instance_id"
EXECUTOR_BOOT_KEY = "executor_boot_id"
EXECUTOR_TASK_KEY = "executor_task_type"

IMAGE_EXTENSIONS = {"png", "jpg", "jpeg", "gif", "webp", "bmp", "tiff"}
TABLE_EXTENSIONS = {"csv", "xlsx", "xls"}
TEXT_EXTENSIONS = {"txt", "text", "md", "markdown"}


def required_mapping(value: Any, description: str) -> Mapping[str, Any]:
    data = unwrap_data(value)
    if not isinstance(data, Mapping):
        raise E2EFailure(f"{description} must be an object: {value!r}")
    return data


def response_items(value: Any, description: str) -> list[Mapping[str, Any]]:
    data = unwrap_data(value)
    if isinstance(data, Mapping):
        data = first_value(data, ("items", "list", "models", "data"), [])
    if not isinstance(data, list):
        raise E2EFailure(f"{description} must be an array: {value!r}")
    return [item for item in data if isinstance(item, Mapping)]


def model_type(model: Mapping[str, Any]) -> str:
    return str(model.get("type", "")).strip().lower()


def model_name(model: Mapping[str, Any]) -> str:
    return str(model.get("name", "")).strip()


@dataclass(frozen=True)
class ModelSelection:
    chat_id: str
    embedding_id: str
    vlm_id: str
    asr_id: str
    chat_name: str
    chat_base_url: str


@dataclass
class TestPrincipal:
    index: int
    username: str
    user_id: str
    tenant_id: int
    client: APIClient = field(repr=False)
    knowledge_base_ids: list[str] = field(default_factory=list)

    def public(self) -> dict[str, Any]:
        return {
            "index": self.index,
            "username": self.username,
            "user_id": self.user_id,
            "tenant_id": self.tenant_id,
            "knowledge_base_ids": list(self.knowledge_base_ids),
        }


class TenantProvisioner:
    """Creates disposable principals without ever persisting their credentials."""

    def __init__(
        self,
        admin: APIClient,
        recorder: JsonlRecorder,
        *,
        exact_chat_source_id: str = EXACT_DEEPSEEK_SOURCE_ID,
        exact_chat_name: str = EXACT_DEEPSEEK_NAME,
        expected_chat_base_url: str = ":14000",
        source_tenant_id: int = 10000,
        require_asr_credential: bool = True,
    ) -> None:
        self.admin = admin
        self.recorder = recorder
        self.exact_chat_source_id = exact_chat_source_id
        self.exact_chat_name = exact_chat_name
        self.expected_chat_base_url = expected_chat_base_url
        self.source_tenant_id = source_tenant_id
        self.require_asr_credential = require_asr_credential

    def _create_account(self, index: int, run_suffix: str) -> tuple[str, str, int]:
        # Local-account usernames intentionally reject punctuation. Run IDs
        # contain hyphens, so derive a stable alphanumeric suffix rather than
        # leaking the raw final "-abcdef" segment into the account name.
        safe_suffix = re.sub(r"[^A-Za-z0-9]", "", run_suffix)[-8:]
        username = f"mt{safe_suffix}{index:02d}"[:20]
        payload = self.admin.request(
            "POST",
            "/api/v1/custom/admin/users",
            json_body={
                "username": username,
                "display_name": f"Document cluster E2E {index}",
            },
        )
        data = required_mapping(payload, "local-account response")
        user = required_mapping(data.get("user"), "local-account user")
        user_id = str(user.get("id", "")).strip()
        tenant_id = to_int(user.get("tenant_id"), 0)
        if not user_id or tenant_id <= 0:
            raise E2EFailure(f"created account lacks user/tenant identity: {user!r}")
        # data["temporary_password"] is intentionally ignored.
        return username, user_id, tenant_id

    def _grant_exact_chat(self, user_id: str) -> None:
        self.admin.request(
            "PUT",
            f"/api/v1/custom/config-center/users/{user_id}/grants",
            json_body={
                "grants": [
                    {
                        "resource_type": "model",
                        "source_tenant_id": self.source_tenant_id,
                        "source_resource_id": self.exact_chat_source_id,
                    }
                ]
            },
        )
        result = required_mapping(
            self.admin.request(
                "POST",
                f"/api/v1/custom/config-center/users/{user_id}/apply",
                json_body={},
            ),
            "config-center apply response",
        )
        if isinstance(result.get("errors"), list) and result["errors"]:
            raise E2EFailure(
                f"exact model grant failed for user {user_id}: {result['errors']}"
            )

    def _reset_api_key(self, tenant_id: int) -> str:
        data = required_mapping(
            self.admin.request(
                "POST",
                f"/api/v1/tenants/{tenant_id}/api-key",
                json_body={},
            ),
            "tenant API-key response",
        )
        api_key = str(data.get("api_key", "")).strip()
        if not api_key:
            raise E2EFailure(f"tenant {tenant_id} API-key reset returned no key")
        return api_key

    @staticmethod
    def _pick_model(
        models: Sequence[Mapping[str, Any]],
        expected_type: str,
        preferred_names: Sequence[str],
    ) -> Mapping[str, Any]:
        candidates = [m for m in models if model_type(m) == expected_type.lower()]
        for preferred in preferred_names:
            for candidate in candidates:
                if model_name(candidate) == preferred:
                    return candidate
        if not candidates:
            raise E2EFailure(f"provisioned tenant has no {expected_type} model")
        return candidates[0]

    @staticmethod
    def _require_exact_model(
        models: Sequence[Mapping[str, Any]],
        expected_type: str,
        expected_name: str,
    ) -> Mapping[str, Any]:
        exact = [
            model
            for model in models
            if model_type(model) == expected_type.lower()
            and model_name(model) == expected_name
        ]
        if exact:
            return exact[0]
        available = sorted(
            model_name(model)
            for model in models
            if model_type(model) == expected_type.lower()
        )
        raise E2EFailure(
            f"required {expected_type} model {expected_name!r} is unavailable; "
            f"available={available}"
        )

    @staticmethod
    def _credential_configured(detail: Mapping[str, Any], field: str) -> bool:
        credentials = detail.get("credentials")
        status = credentials.get(field) if isinstance(credentials, Mapping) else None
        return isinstance(status, Mapping) and bool(status.get("configured"))

    def _wait_models(self, client: APIClient, timeout: float = 90.0) -> ModelSelection:
        deadline = time.monotonic() + timeout
        latest: list[Mapping[str, Any]] = []
        while time.monotonic() < deadline:
            latest = response_items(client.request("GET", "/api/v1/models"), "model list")
            exact = [
                m
                for m in latest
                if model_type(m) == "knowledgeqa"
                and model_name(m) == self.exact_chat_name
            ]
            if exact:
                chat = exact[0]
                detail = required_mapping(
                    client.request("GET", f"/api/v1/models/{chat['id']}"),
                    "exact chat model",
                )
                parameters = detail.get("parameters")
                base_url = (
                    str(parameters.get("base_url", "")).strip()
                    if isinstance(parameters, Mapping)
                    else ""
                )
                configured = self._credential_configured(detail, "api_key")
                if self.expected_chat_base_url and self.expected_chat_base_url not in base_url:
                    raise E2EFailure(
                        "provisioned DeepSeek model is not the required LiteLLM endpoint: "
                        f"name={self.exact_chat_name!r} base_url={base_url!r}"
                    )
                if not configured:
                    raise E2EFailure("provisioned DeepSeek model has no configured credential")
                embedding = self._pick_model(
                    latest,
                    "embedding",
                    ("BAAI/bge-m3", "/models/Qwen3-Embedding-8B"),
                )
                vlm = self._require_exact_model(
                    latest, "vllm", EXACT_QWEN_VLM_NAME
                )
                asr = self._require_exact_model(
                    latest, "asr", EXACT_QWEN_ASR_NAME
                )
                asr_detail = required_mapping(
                    client.request("GET", f"/api/v1/models/{asr['id']}"),
                    "exact Qwen ASR model",
                )
                if (
                    self.require_asr_credential
                    and not self._credential_configured(asr_detail, "api_key")
                ):
                    raise E2EFailure(
                        "required Qwen3-ASR-1.7B model has no configured API key; "
                        "refusing to fall back to another ASR model"
                    )
                return ModelSelection(
                    chat_id=str(chat["id"]),
                    embedding_id=str(embedding["id"]),
                    vlm_id=str(vlm["id"]),
                    asr_id=str(asr["id"]),
                    chat_name=model_name(chat),
                    chat_base_url=base_url,
                )
            time.sleep(2)
        raise E2EFailure(
            f"exact DeepSeek model {self.exact_chat_name!r} was not provisioned; "
            f"available={sorted(model_name(m) for m in latest)}"
        )

    @staticmethod
    def _kb_body(
        name: str,
        models: ModelSelection,
        *,
        description: str = "Disposable multi-tenant horizontal document-processing E2E",
        asr_enabled: bool = True,
        wiki_ingest_batch_size: int = 0,
        wiki_ingest_map_parallel: int = 0,
        wiki_ingest_reduce_parallel: int = 0,
    ) -> dict[str, Any]:
        wiki_config: dict[str, Any] = {
            "synthesis_model_id": models.chat_id,
            "max_pages_per_ingest": 16,
            "extraction_granularity": "focused",
        }
        if wiki_ingest_batch_size > 0:
            wiki_config["ingest_batch_size"] = wiki_ingest_batch_size
        if wiki_ingest_map_parallel > 0:
            wiki_config["ingest_map_parallel"] = wiki_ingest_map_parallel
        if wiki_ingest_reduce_parallel > 0:
            wiki_config["ingest_reduce_parallel"] = wiki_ingest_reduce_parallel
        return {
            "name": name,
            "description": description,
            "type": "document",
            "chunking_config": {
                "chunk_size": 2048,
                "chunk_overlap": 128,
                "strategy": "auto",
                "token_limit": 1024,
            },
            "embedding_model_id": models.embedding_id,
            "summary_model_id": models.chat_id,
            "image_processing_config": {"model_id": models.vlm_id},
            "vlm_config": {"enabled": True, "model_id": models.vlm_id},
            "asr_config": {
                "enabled": asr_enabled,
                "model_id": models.asr_id,
                "language": "zh",
            },
            "question_generation_config": {"enabled": True, "question_count": 1},
            "extract_config": {
                "enabled": True,
                "text": (
                    "The Example Data Policy assigns the Data Office responsibility "
                    "for executing Access Review, which applies to production data."
                ),
                "tags": ["ASSIGNS", "EXECUTES", "APPLIES_TO", "DEPENDS_ON", "REFERENCES"],
                "nodes": [
                    {"name": "Example Data Policy", "attributes": ["policy"]},
                    {"name": "Data Office", "attributes": ["responsible unit"]},
                    {"name": "Access Review", "attributes": ["control process"]},
                ],
                "relations": [
                    {
                        "node1": "Example Data Policy",
                        "node2": "Data Office",
                        "type": "ASSIGNS",
                    },
                    {
                        "node1": "Data Office",
                        "node2": "Access Review",
                        "type": "EXECUTES",
                    },
                ],
            },
            "wiki_config": wiki_config,
            "indexing_strategy": {
                "vector_enabled": True,
                "keyword_enabled": True,
                "wiki_enabled": True,
                "graph_enabled": True,
            },
        }

    def provision(
        self,
        *,
        principal_count: int,
        knowledge_bases_per_principal: int,
        run_suffix: str,
        knowledge_base_name_factory: Callable[[int, int], str] | None = None,
        knowledge_base_body_kwargs: Mapping[str, Any] | None = None,
    ) -> list[TestPrincipal]:
        principals: list[TestPrincipal] = []
        partial_identity: tuple[str, int] | None = None
        try:
            for index in range(principal_count):
                username, user_id, tenant_id = self._create_account(index, run_suffix)
                partial_identity = (user_id, tenant_id)
                self._grant_exact_chat(user_id)
                api_key = self._reset_api_key(tenant_id)
                client = APIClient(
                    self.admin.base_url,
                    api_key,
                    auth_mode="api-key",
                    timeout=max(120.0, self.admin.timeout),
                )
                principal = TestPrincipal(index, username, user_id, tenant_id, client)
                # Track the principal before any model/KB operation so a failure in
                # either phase cannot leak a disposable tenant or local account.
                principals.append(principal)
                partial_identity = None
                models = self._wait_models(client)
                for kb_index in range(knowledge_bases_per_principal):
                    knowledge_base_name = (
                        knowledge_base_name_factory(index, kb_index)
                        if knowledge_base_name_factory is not None
                        else f"MT-E2E-{run_suffix}-{index:02d}-{kb_index:02d}"
                    )
                    created = required_mapping(
                        client.request(
                            "POST",
                            "/api/v1/knowledge-bases",
                            json_body=self._kb_body(
                                knowledge_base_name,
                                models,
                                **dict(knowledge_base_body_kwargs or {}),
                            ),
                        ),
                        "knowledge-base create response",
                    )
                    kb_id = str(created.get("id", "")).strip()
                    if not kb_id:
                        raise E2EFailure("knowledge-base create response has no id")
                    if str(created.get("summary_model_id", "")) != models.chat_id:
                        raise E2EFailure(
                            "knowledge base did not bind the exact DeepSeek model"
                        )
                    principal.knowledge_base_ids.append(kb_id)
                self.recorder.emit(
                    "multitenant.principal_provisioned",
                    principal=principal.public(),
                    exact_chat_model=models.chat_name,
                    exact_chat_base_url=models.chat_base_url,
                )
            return principals
        except Exception as exc:
            cleanup_failures = self.cleanup_principals(principals)
            if partial_identity is not None:
                user_id, tenant_id = partial_identity
                try:
                    self.admin.request("DELETE", f"/api/v1/tenants/{tenant_id}")
                except Exception as cleanup_exc:
                    cleanup_failures.append(
                        f"delete partial tenant {tenant_id}: {cleanup_exc}"
                    )
                try:
                    self.admin.request(
                        "PUT",
                        f"/api/v1/custom/admin/users/{user_id}/active",
                        json_body={"active": False},
                    )
                except Exception as cleanup_exc:
                    cleanup_failures.append(
                        f"deactivate partial user {user_id}: {cleanup_exc}"
                    )
            self.recorder.emit(
                "multitenant.provision_failed",
                error=str(exc),
                cleanup_failures=cleanup_failures,
            )
            if cleanup_failures:
                raise E2EFailure(
                    f"tenant provisioning failed: {exc}; cleanup failed: "
                    + "; ".join(cleanup_failures)
                ) from exc
            raise

    def cleanup_principals(self, principals: Sequence[TestPrincipal]) -> list[str]:
        failures: list[str] = []
        for principal in principals:
            for kb_id in principal.knowledge_base_ids:
                try:
                    principal.client.request("DELETE", f"/api/v1/knowledge-bases/{kb_id}")
                except Exception as exc:
                    failures.append(f"delete KB {kb_id}: {exc}")
            try:
                self.admin.request("DELETE", f"/api/v1/tenants/{principal.tenant_id}")
            except Exception as exc:
                failures.append(f"delete tenant {principal.tenant_id}: {exc}")
            try:
                self.admin.request(
                    "PUT",
                    f"/api/v1/custom/admin/users/{principal.user_id}/active",
                    json_body={"active": False},
                )
            except Exception as exc:
                failures.append(f"deactivate user {principal.user_id}: {exc}")
        return failures


@dataclass(frozen=True)
class FixtureSeed:
    extension: str
    path: Path


@dataclass(frozen=True)
class Variant:
    filename: str
    marker: str
    extension: str
    size_class: str
    target_kib: int
    content: bytes
    content_unique: bool


def _repeat_to_target(text: str, target_kib: int) -> str:
    target = max(1, target_kib) * 1024
    result = text
    sequence = 0
    while len(result.encode("utf-8")) < target:
        sequence += 1
        result += (
            f"\nSection {sequence}: the responsible team must review the request within "
            "three working days; unauthorized bypass is prohibited and exceptions require "
            "written approval plus an auditable record.\n"
        )
    return result


def _zip_rewrite(source: bytes, transforms: Mapping[str, Callable[[bytes], bytes]]) -> bytes:
    output = io.BytesIO()
    with zipfile.ZipFile(io.BytesIO(source), "r") as src, zipfile.ZipFile(
        output, "w", compression=zipfile.ZIP_DEFLATED
    ) as dst:
        for info in src.infolist():
            value = src.read(info.filename)
            transform = transforms.get(info.filename)
            if transform is not None:
                value = transform(value)
            dst.writestr(info, value)
    return output.getvalue()


def _inject_before(value: bytes, closing: bytes, payload: bytes) -> bytes:
    offset = value.rfind(closing)
    if offset < 0:
        raise E2EFailure(f"fixture XML lacks closing tag {closing!r}")
    return value[:offset] + payload + value[offset:]


def _minimal_pdf(marker: str, target_kib: int) -> bytes:
    lines = _repeat_to_target(
        (
            f"{marker} Horizontal document processing acceptance. "
            "Digital management owns approval. Review is due in three working days. "
            "Unauthorized security-check bypass is prohibited."
        ),
        target_kib,
    ).splitlines()
    stream_parts = ["BT /F1 9 Tf 36 806 Td 11 TL"]
    for line in lines:
        safe = line.replace("\\", "\\\\").replace("(", "\\(").replace(")", "\\)")
        stream_parts.append(f"({safe[:180]}) Tj T*")
    stream_parts.append("ET")
    stream = "\n".join(stream_parts).encode("latin-1", "replace")
    objects = [
        b"<< /Type /Catalog /Pages 2 0 R >>",
        b"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
        b"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 595 842] "
        b"/Resources << /Font << /F1 4 0 R >> >> /Contents 5 0 R >>",
        b"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
        b"<< /Length " + str(len(stream)).encode() + b" >>\nstream\n" + stream + b"\nendstream",
    ]
    output = bytearray(b"%PDF-1.4\n")
    offsets = [0]
    for index, obj in enumerate(objects, start=1):
        offsets.append(len(output))
        output.extend(f"{index} 0 obj\n".encode())
        output.extend(obj)
        output.extend(b"\nendobj\n")
    xref = len(output)
    output.extend(f"xref\n0 {len(objects) + 1}\n".encode())
    output.extend(b"0000000000 65535 f \n")
    for offset in offsets[1:]:
        output.extend(f"{offset:010d} 00000 n \n".encode())
    output.extend(
        (
            f"trailer << /Size {len(objects) + 1} /Root 1 0 R >>\n"
            f"startxref\n{xref}\n%%EOF\n"
        ).encode()
    )
    return bytes(output)


class VariantFactory:
    """Builds unique valid variants in memory; no 1000-file fixture tree."""

    def __init__(
        self,
        fixture_dir: Path,
        *,
        small_kib: int = 4,
        medium_kib: int = 32,
        large_kib: int = 128,
    ) -> None:
        manifest = json.loads((fixture_dir / "manifest.json").read_text(encoding="utf-8"))
        raw_files = manifest.get("files", [])
        if not isinstance(raw_files, list):
            raise E2EFailure("supported-format manifest files must be an array")
        seeds: list[FixtureSeed] = []
        for item in raw_files:
            if not isinstance(item, Mapping):
                continue
            extension = str(item.get("extension", "")).lower().strip()
            path = fixture_dir / str(item.get("filename", ""))
            if not extension or not path.is_file():
                raise E2EFailure(f"missing fixture for extension {extension!r}: {path}")
            seeds.append(FixtureSeed(extension, path))
        if len(seeds) < 20:
            raise E2EFailure("multi-tenant workload must cover the complete format set")
        self.seeds = tuple(seeds)
        self.small_kib = small_kib
        self.medium_kib = medium_kib
        self.large_kib = large_kib

    def _size(self, index: int) -> tuple[str, int]:
        bucket = index % 100
        if bucket < 2:
            return "large", self.large_kib
        if bucket < 12:
            return "medium", self.medium_kib
        return "small", self.small_kib

    @staticmethod
    def _text(marker: str, target_kib: int, markdown: bool = False) -> bytes:
        prefix = "# Horizontal processing acceptance\n\n" if markdown else ""
        return _repeat_to_target(
            prefix
            + (
                f"{marker}. This document belongs to a distinct user and knowledge base. "
                "The digital management team is responsible for approval within three working days. "
                "Unauthorized security-check bypass is prohibited."
            ),
            target_kib,
        ).encode("utf-8")

    def _variant_content(
        self, seed: FixtureSeed, marker: str, target_kib: int
    ) -> tuple[bytes, bool]:
        extension = seed.extension
        if extension in TEXT_EXTENSIONS:
            return self._text(marker, target_kib, extension in {"md", "markdown"}), True
        if extension == "csv":
            rows = ["marker,responsibility,deadline,control"]
            sequence = 0
            while len("\n".join(rows).encode("utf-8")) < target_kib * 1024:
                rows.append(
                    f"{marker}-{sequence},digital management,three working days,"
                    "unauthorized bypass is prohibited"
                )
                sequence += 1
            return ("\ufeff" + "\n".join(rows) + "\n").encode("utf-8"), True
        if extension == "json":
            return json.dumps(
                {
                    "marker": marker,
                    "responsibility": "digital management",
                    "deadline": "three working days",
                    "control": "unauthorized bypass is prohibited",
                    "body": _repeat_to_target(marker + " ", target_kib),
                },
                ensure_ascii=False,
            ).encode("utf-8"), True
        if extension == "mhtml":
            body = html.escape(_repeat_to_target(marker + " ", target_kib))
            message = (
                "MIME-Version: 1.0\r\n"
                "Content-Type: text/html; charset=utf-8\r\n"
                "Subject: Horizontal processing acceptance\r\n\r\n"
                f"<html><body><h1>{html.escape(marker)}</h1><p>{body}</p></body></html>"
            )
            return message.encode("utf-8"), True
        if extension == "pdf":
            return _minimal_pdf(marker, target_kib), True

        source = seed.path.read_bytes()
        escaped = html.escape(_repeat_to_target(marker + " ", target_kib))
        if extension == "docx":
            paragraph = (
                "<w:p><w:r><w:t xml:space=\"preserve\">"
                + escaped
                + "</w:t></w:r></w:p>"
            ).encode()
            return _zip_rewrite(
                source,
                {
                    "word/document.xml": lambda value: _inject_before(
                        value, b"</w:body>", paragraph
                    )
                },
            ), True
        if extension == "xlsx":
            row = (
                '<row r="100000"><c r="A100000" t="inlineStr"><is><t>'
                + escaped
                + "</t></is></c></row>"
            ).encode()
            return _zip_rewrite(
                source,
                {
                    "xl/worksheets/sheet1.xml": lambda value: _inject_before(
                        value, b"</sheetData>", row
                    )
                },
            ), True
        if extension == "pptx":
            run = ("<a:r><a:t>" + escaped + "</a:t></a:r>").encode()

            def inject_slide(value: bytes) -> bytes:
                offset = value.find(b"</a:p>")
                if offset < 0:
                    raise E2EFailure("PPTX seed has no paragraph")
                return value[:offset] + run + value[offset:]

            return _zip_rewrite(source, {"ppt/slides/slide1.xml": inject_slide}), True
        if extension == "epub":
            with zipfile.ZipFile(io.BytesIO(source), "r") as archive:
                entries = [
                    name
                    for name in archive.namelist()
                    if name.lower().endswith((".xhtml", ".html", ".htm"))
                ]
            if entries:
                payload = f"<p>{escaped}</p>".encode()
                return _zip_rewrite(
                    source,
                    {
                        entries[0]: lambda value: _inject_before(
                            value, b"</body>", payload
                        )
                    },
                ), True
        if extension == "wav" and len(source) >= 12 and source[:4] == b"RIFF":
            marker_bytes = marker.encode("ascii", "replace")
            padding = b"\x00" if len(marker_bytes) % 2 else b""
            chunk = b"JUNK" + struct.pack("<I", len(marker_bytes)) + marker_bytes + padding
            result = bytearray(source + chunk)
            struct.pack_into("<I", result, 4, len(result) - 8)
            return bytes(result), True
        if extension == "mp3":
            marker_bytes = marker.encode("ascii", "replace")[:120]
            frame_payload = b"\x03e2e\x00" + marker_bytes
            frame = (
                b"TXXX"
                + struct.pack(">I", len(frame_payload))
                + b"\x00\x00"
                + frame_payload
            )
            size = len(frame)
            syncsafe = bytes(
                (
                    (size >> 21) & 0x7F,
                    (size >> 14) & 0x7F,
                    (size >> 7) & 0x7F,
                    size & 0x7F,
                )
            )
            return b"ID3\x04\x00\x00" + syncsafe + frame + source, True
        # Legacy Office, compressed audio and raster seeds stay byte-valid.
        # Filename/metadata remain unique; the full-format suite validates them
        # separately while the 1000-document run stresses queueing and fan-out.
        return source, False

    def build(self, index: int, run_id: str) -> Variant:
        seed = self.seeds[index % len(self.seeds)]
        size_class, target_kib = self._size(index)
        marker = f"MT-{run_id}-{index:05d}-{uuid.uuid4().hex[:8]}"
        content, unique = self._variant_content(seed, marker, target_kib)
        return Variant(
            filename=f"{marker}-{size_class}.{seed.extension}",
            marker=marker,
            extension=seed.extension,
            size_class=size_class,
            target_kib=target_kib,
            content=content,
            content_unique=unique,
        )


@dataclass
class MultiTenantObservation:
    index: int
    principal_index: int
    tenant_id: int
    user_id: str
    kb_id: str
    knowledge_id: str
    filename: str
    marker: str
    extension: str
    size_class: str
    target_kib: int
    source_bytes: int
    content_unique: bool
    uploaded_at: float
    first_waiting_at: float | None = None
    first_active_at: float | None = None
    terminal_at: float | None = None
    final_status: str = ""
    owners: set[str] = field(default_factory=set)
    positions: list[int] = field(default_factory=list)
    epochs: set[int] = field(default_factory=set)

    def observe_queue(self, item: QueueItem, now: float) -> None:
        if item.state in {"waiting", "pending", "queued"} and self.first_waiting_at is None:
            self.first_waiting_at = now
        if item.state in {"active", "processing", "running", "finalizing"}:
            self.first_active_at = self.first_active_at or now
        if item.owner_instance_id:
            self.owners.add(item.owner_instance_id)
        if item.position:
            self.positions.append(item.position)
        if item.execution_epoch is not None:
            self.epochs.add(item.execution_epoch)


def parse_prometheus_stage_metrics(text: str) -> dict[str, float]:
    result: dict[str, float] = {}
    for line in text.splitlines():
        if not line.startswith("weknora_document_stage_executions_total{"):
            continue
        labels, _, raw_value = line.partition("} ")
        stage = re.search(r'stage="([^"]+)"', labels)
        outcome = re.search(r'outcome="([^"]+)"', labels)
        if not raw_value or not stage or not outcome:
            continue
        try:
            result[f"{stage.group(1)}:{outcome.group(1)}"] = float(raw_value)
        except ValueError:
            pass
    return result


def scrape_container_metrics(container: str) -> dict[str, float]:
    completed = subprocess.run(
        [
            "docker",
            "exec",
            container,
            "sh",
            "-lc",
            "wget -qO- http://127.0.0.1:8080/metrics",
        ],
        check=False,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        timeout=30,
    )
    if completed.returncode != 0:
        raise E2EFailure(f"failed to scrape metrics from {container}: {completed.stdout}")
    return parse_prometheus_stage_metrics(completed.stdout)


def scrape_metrics(containers: Sequence[str]) -> dict[str, dict[str, float]]:
    return {container: scrape_container_metrics(container) for container in containers}


def metrics_delta(
    before: Mapping[str, Mapping[str, float]],
    after: Mapping[str, Mapping[str, float]],
) -> dict[str, dict[str, float]]:
    result: dict[str, dict[str, float]] = {}
    for instance, values in after.items():
        old = before.get(instance, {})
        result[instance] = {
            key: max(0.0, value - float(old.get(key, 0.0)))
            for key, value in values.items()
            if value - float(old.get(key, 0.0)) != 0
        }
    return result


def classify_span(node: Mapping[str, Any]) -> str:
    name = str(first_value(node, ("name", "span_name"), "")).lower()
    metadata = node.get("metadata")
    task_type = (
        str(metadata.get(EXECUTOR_TASK_KEY, "")).lower()
        if isinstance(metadata, Mapping)
        else ""
    )
    if name in {"docreader", "chunking", "embedding"}:
        return name
    if task_type == "knowledge:post_process" and name == "postprocess":
        return "postprocess"
    return {
        "summary:generation": "summary",
        "question:generation": "questions",
        "chunk:extract": "graph",
        "wiki:ingest": "wiki",
        "image:multimodal": "multimodal",
        "datatable:summary": "table",
        "document:split_part": "split_part",
        "document:split_finalize": "split_finalize",
    }.get(task_type, "")


@dataclass
class MultiTenantResult:
    run_id: str
    started_at: str
    finished_at: str
    documents: int
    principals: int
    knowledge_bases: int
    completed: int
    failed: int
    wall_seconds: float
    throughput_docs_per_second: float
    queue_wait_p50_seconds: float | None
    queue_wait_p95_seconds: float | None
    processing_p50_seconds: float | None
    processing_p95_seconds: float | None
    max_waiting_total: int
    max_active_total: int
    max_active_by_instance: dict[str, int]
    root_owner_distribution: dict[str, int]
    documents_by_tenant: dict[str, int]
    documents_by_knowledge_base: dict[str, int]
    documents_by_format: dict[str, int]
    documents_by_size: dict[str, int]
    unique_content_documents: int


class MultiTenantClusterRunner:
    def __init__(
        self,
        admin: APIClient,
        principals: Sequence[TestPrincipal],
        factory: VariantFactory,
        recorder: JsonlRecorder,
        *,
        run_id: str,
        poll_interval: float = 5.0,
        worker_containers: Sequence[str] = (),
    ) -> None:
        self.admin = admin
        self.principals = list(principals)
        self.factory = factory
        self.recorder = recorder
        self.run_id = run_id
        self.poll_interval = poll_interval
        self.worker_containers = list(worker_containers)
        self.observations: dict[str, MultiTenantObservation] = {}
        self.max_waiting_total = 0
        self.max_active_total = 0
        self.max_active_by_instance: dict[str, int] = {}
        self._lock = threading.Lock()

    def _assignment(self, index: int) -> tuple[TestPrincipal, str]:
        principal = self.principals[index % len(self.principals)]
        kb_index = (index // len(self.principals)) % len(principal.knowledge_base_ids)
        return principal, principal.knowledge_base_ids[kb_index]

    @staticmethod
    def _upload_with_retry(
        principal: TestPrincipal,
        kb_id: str,
        variant: Variant,
        process_config: Mapping[str, Any] | None,
        run_id: str,
        retries: int = 5,
    ) -> Mapping[str, Any]:
        last_error: Exception | None = None
        for attempt in range(retries):
            try:
                return principal.client.upload_document(
                    kb_id,
                    variant.filename,
                    variant.content,
                    process_config=process_config,
                    metadata={
                        "e2e_run_id": run_id,
                        "e2e_marker": variant.marker,
                        "e2e_principal": str(principal.index),
                        "e2e_size_class": variant.size_class,
                    },
                )
            except APIError as exc:
                last_error = exc
                if exc.status not in {408, 409, 425, 429, 500, 502, 503, 504}:
                    raise
            except E2EFailure as exc:
                last_error = exc
            time.sleep(min(8.0, 0.5 * (2**attempt)))
        raise E2EFailure(f"upload retries exhausted: {last_error}")

    def upload(
        self,
        count: int,
        *,
        concurrency: int,
        process_config: Mapping[str, Any] | None,
    ) -> list[str]:
        started = time.monotonic()

        def one(index: int) -> MultiTenantObservation:
            principal, kb_id = self._assignment(index)
            variant = self.factory.build(index, self.run_id)
            uploaded_at = time.monotonic()
            data = self._upload_with_retry(
                principal, kb_id, variant, process_config, self.run_id
            )
            return MultiTenantObservation(
                index=index,
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
                content_unique=variant.content_unique,
                uploaded_at=uploaded_at,
            )

        failures: list[str] = []
        with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
            futures = {pool.submit(one, index): index for index in range(count)}
            for future in as_completed(futures):
                index = futures[future]
                try:
                    observation = future.result()
                    self.observations[observation.knowledge_id] = observation
                except Exception as exc:
                    failures.append(f"upload {index}: {exc}")
        if failures:
            raise E2EFailure(
                f"{len(failures)} of {count} uploads failed; "
                f"successful={len(self.observations)}; failures={failures[:20]}"
            )
        self.recorder.emit(
            "multitenant.uploaded",
            documents=len(self.observations),
            principals=len(self.principals),
            knowledge_bases=sum(len(p.knowledge_base_ids) for p in self.principals),
            upload_concurrency=concurrency,
            elapsed_seconds=time.monotonic() - started,
            formats=dict(Counter(o.extension for o in self.observations.values())),
            sizes=dict(Counter(o.size_class for o in self.observations.values())),
            unique_content_documents=sum(o.content_unique for o in self.observations.values()),
        )
        return list(self.observations)

    def _by_principal(self) -> dict[int, list[str]]:
        result: dict[int, list[str]] = defaultdict(list)
        for knowledge_id, observation in self.observations.items():
            result[observation.principal_index].append(knowledge_id)
        return result

    def sample_queue(self) -> None:
        grouped = self._by_principal()
        snapshots: list[Any] = []
        with ThreadPoolExecutor(max_workers=len(self.principals)) as pool:
            futures = {
                pool.submit(
                    principal.client.get_queue,
                    grouped.get(principal.index, []),
                ): principal.index
                for principal in self.principals
            }
            for future in as_completed(futures):
                principal_index = futures[future]
                snapshot = future.result()
                snapshots.append(snapshot)
                now = time.monotonic()
                positions: list[int] = []
                for knowledge_id, item in snapshot.items.items():
                    observation = self.observations.get(knowledge_id)
                    if observation is None or observation.principal_index != principal_index:
                        raise E2EFailure(f"cross-tenant queue leak for {knowledge_id}")
                    observation.observe_queue(item, now)
                    if item.state in {"waiting", "pending", "queued"}:
                        if item.position is None or item.position < 1:
                            raise E2EFailure(
                                f"waiting document {knowledge_id} has invalid position"
                            )
                        positions.append(item.position)
                        if (
                            item.ahead_count is not None
                            and item.ahead_count != item.position - 1
                        ):
                            raise E2EFailure(
                                f"queue ahead_count mismatch for {knowledge_id}"
                            )
                if len(positions) != len(set(positions)):
                    raise E2EFailure(
                        f"principal {principal_index} saw duplicate queue positions"
                    )
        if snapshots:
            self.max_waiting_total = max(
                self.max_waiting_total,
                max(snapshot.waiting_total for snapshot in snapshots),
            )
            self.max_active_total = max(
                self.max_active_total,
                max(snapshot.active_total for snapshot in snapshots),
            )
        instances = self.admin.get_instances()
        for instance in instances:
            if not instance.is_healthy_ready:
                continue
            if instance.active_count > instance.capacity:
                raise E2EFailure(
                    f"instance {instance.instance_id} exceeded capacity "
                    f"{instance.active_count}/{instance.capacity}"
                )
            self.max_active_by_instance[instance.instance_id] = max(
                self.max_active_by_instance.get(instance.instance_id, 0),
                instance.active_count,
            )
            for knowledge_id in instance.active_documents:
                observation = self.observations.get(knowledge_id)
                if observation:
                    observation.owners.add(instance.instance_id)
        self.recorder.emit(
            "multitenant.queue_sample",
            waiting_total=max((s.waiting_total for s in snapshots), default=0),
            active_total=max((s.active_total for s in snapshots), default=0),
            terminal=sum(bool(o.final_status) for o in self.observations.values()),
            active_by_instance={
                i.instance_id: i.active_count for i in instances if i.is_healthy_ready
            },
        )

    def refresh_terminal(self) -> None:
        now = time.monotonic()
        with ThreadPoolExecutor(
            max_workers=sum(len(p.knowledge_base_ids) for p in self.principals)
        ) as pool:
            futures = {
                pool.submit(principal.client.list_all_knowledge, kb_id): (
                    principal.index,
                    kb_id,
                )
                for principal in self.principals
                for kb_id in principal.knowledge_base_ids
            }
            for future in as_completed(futures):
                principal_index, kb_id = futures[future]
                for row in future.result():
                    knowledge_id = str(row.get("id", ""))
                    observation = self.observations.get(knowledge_id)
                    if observation is None:
                        continue
                    if observation.principal_index != principal_index or observation.kb_id != kb_id:
                        raise E2EFailure(f"knowledge ownership changed for {knowledge_id}")
                    parse_status = str(row.get("parse_status", "")).lower()
                    status = ""
                    if parse_status in TERMINAL_STATUSES - {"completed"}:
                        status = parse_status
                    elif parse_status == "completed":
                        derivative_statuses = {
                            "summary": str(row.get("summary_status", "")).lower(),
                            "enrichment": str(row.get("enrichment_status", "")).lower(),
                            "wiki": str(row.get("wiki_status", "")).lower(),
                        }
                        if any(
                            value in FAILED_STAGE_STATUSES
                            for value in derivative_statuses.values()
                        ):
                            status = "failed"
                        elif (
                            derivative_statuses["enrichment"] in {"completed", "done"}
                            and derivative_statuses["wiki"] in {"completed", "done"}
                            and to_int(row.get("pending_subtasks_count"), -1) == 0
                        ):
                            status = "completed"
                    if status:
                        observation.terminal_at = observation.terminal_at or now
                        observation.final_status = status

    def wait_for_completion(self, timeout: float) -> None:
        deadline = time.monotonic() + timeout
        last_terminal = -1
        while time.monotonic() < deadline:
            self.sample_queue()
            self.refresh_terminal()
            failed = [
                observation
                for observation in self.observations.values()
                if observation.final_status
                and observation.final_status != "completed"
            ]
            if failed:
                raise E2EFailure(
                    "multi-tenant document pipeline failed before the remaining "
                    "documents completed: "
                    + ", ".join(
                        f"{observation.knowledge_id}={observation.final_status}"
                        for observation in failed[:20]
                    )
                )
            terminal = sum(bool(o.final_status) for o in self.observations.values())
            if terminal != last_terminal:
                self.recorder.emit(
                    "multitenant.progress",
                    terminal=terminal,
                    total=len(self.observations),
                )
                last_terminal = terminal
            if terminal == len(self.observations):
                failed = [
                    o.knowledge_id
                    for o in self.observations.values()
                    if o.final_status != "completed"
                ]
                if failed:
                    raise E2EFailure(
                        f"{len(failed)} documents ended non-completed: {failed[:20]}"
                    )
                return
            time.sleep(self.poll_interval)
        pending = [
            o.knowledge_id for o in self.observations.values() if not o.final_status
        ]
        raise E2EFailure(
            f"{len(pending)} documents did not finish within {timeout}s: {pending[:20]}"
        )

    def verify_cross_tenant_isolation(self) -> None:
        if len(self.principals) < 2:
            raise E2EFailure("cross-tenant verification needs at least two principals")
        foreign = next(o for o in self.observations.values() if o.principal_index == 1)
        try:
            self.principals[0].client.get_knowledge(foreign.knowledge_id)
        except APIError as exc:
            if exc.status not in {403, 404}:
                raise
        else:
            raise E2EFailure("one test tenant could read another tenant's document")
        self.recorder.emit("multitenant.cross_tenant_isolation_passed")

    def _deep_verify_one(
        self, observation: MultiTenantObservation
    ) -> tuple[str, dict[str, Any]]:
        principal = self.principals[observation.principal_index]
        knowledge = principal.client.get_knowledge(observation.knowledge_id)
        if str(knowledge.get("parse_status", "")).lower() != "completed":
            raise E2EFailure(f"{observation.knowledge_id} is not completed")
        if to_int(knowledge.get("tenant_id"), observation.tenant_id) != observation.tenant_id:
            raise E2EFailure(f"tenant mismatch for {observation.knowledge_id}")
        if str(knowledge.get("knowledge_base_id", "")) != observation.kb_id:
            raise E2EFailure(f"knowledge-base mismatch for {observation.knowledge_id}")
        creator_id = str(knowledge.get("creator_id", ""))
        if creator_id and creator_id != observation.user_id:
            raise E2EFailure(f"creator mismatch for {observation.knowledge_id}")
        chunks = principal.client.list_chunks(
            observation.knowledge_id,
            [
                "text",
                "parent_text",
                "image_ocr",
                "image_caption",
                "summary",
                "entity",
                "relationship",
                "table_summary",
                "table_column",
            ],
        )
        text_chunks = [c for c in chunks if c.get("chunk_type") == "text"]
        if not text_chunks:
            raise E2EFailure(f"completed document {observation.knowledge_id} has no text")
        chunk_ids = [str(c.get("id", "")) for c in chunks]
        if len(chunk_ids) != len(set(chunk_ids)):
            raise E2EFailure(f"duplicate chunk ids for {observation.knowledge_id}")
        chunk_types = {str(c.get("chunk_type", "")) for c in chunks}
        if (
            str(knowledge.get("summary_status", "")).lower()
            not in {"completed", "done"}
            and "summary" not in chunk_types
        ):
            raise E2EFailure(f"summary missing for {observation.knowledge_id}")
        question_evidence = validate_generated_question_quality(text_chunks)
        nodes = flatten_span_nodes(principal.client.get_spans(observation.knowledge_id))
        vector_evidence = embedding_vector_evidence(nodes)
        if vector_evidence is None:
            is_physical_split = any(bool(chunk.get("source_locator")) for chunk in text_chunks)
            if not is_physical_split:
                raise E2EFailure(
                    f"{observation.knowledge_id} has no successful embedding-stage evidence"
                )
        elif (
            vector_evidence["chunks_to_embed"] != len(text_chunks)
            or vector_evidence["vectors_written"] != len(text_chunks)
        ):
            raise E2EFailure(
                f"incomplete vector coverage for {observation.knowledge_id}: "
                f"text={len(text_chunks)} evidence={vector_evidence}"
            )
        stage_instances: dict[str, set[str]] = defaultdict(set)
        missing_identity: list[str] = []
        for node in nodes:
            stage = classify_span(node)
            if not stage:
                continue
            metadata = node.get("metadata")
            instance = (
                str(metadata.get(EXECUTOR_INSTANCE_KEY, ""))
                if isinstance(metadata, Mapping)
                else ""
            )
            boot = (
                str(metadata.get(EXECUTOR_BOOT_KEY, ""))
                if isinstance(metadata, Mapping)
                else ""
            )
            if not instance or not boot:
                missing_identity.append(str(node.get("name", "")))
            else:
                stage_instances[stage].add(instance)
        required = {
            "docreader",
            "chunking",
            "embedding",
            "postprocess",
            "summary",
            "questions",
            "graph",
            "wiki",
        }
        missing_stages = sorted(stage for stage in required if not stage_instances.get(stage))
        if missing_identity:
            raise E2EFailure(
                f"{observation.knowledge_id} has stages without executor identity: "
                f"{missing_identity[:20]}"
            )
        if missing_stages:
            raise E2EFailure(
                f"{observation.knowledge_id} has no executor evidence for {missing_stages}"
            )
        graph_nodes, graph_relations = graph_artifact_counts(nodes)
        if graph_nodes + graph_relations <= 0 and not (
            {"entity", "relationship"} & chunk_types
        ):
            raise E2EFailure(f"graph artifact missing for {observation.knowledge_id}")
        if observation.extension in IMAGE_EXTENSIONS:
            if not ({"image_ocr", "image_caption"} & chunk_types):
                raise E2EFailure(f"multimodal artifact missing for {observation.knowledge_id}")
            if not stage_instances.get("multimodal"):
                raise E2EFailure(
                    f"multimodal executor missing for {observation.knowledge_id}"
                )
        if observation.extension in TABLE_EXTENSIONS and not (
            {"table_summary", "table_column"} & chunk_types
        ):
            raise E2EFailure(f"table artifact missing for {observation.knowledge_id}")
        return observation.knowledge_id, {
            "chunks": len(chunks),
            "chunk_types": sorted(chunk_types),
            "questions": question_evidence,
            "vector_evidence": vector_evidence,
            "graph_nodes": graph_nodes,
            "graph_relations": graph_relations,
            "stage_instances": {
                stage: sorted(instances) for stage, instances in stage_instances.items()
            },
        }

    def _verify_wiki_coverage(self, timeout: float) -> dict[str, int]:
        deadline = time.monotonic() + timeout
        expected: dict[str, set[str]] = defaultdict(set)
        for observation in self.observations.values():
            expected[observation.kb_id].add(observation.knowledge_id)
        client_by_kb = {
            kb_id: principal.client
            for principal in self.principals
            for kb_id in principal.knowledge_base_ids
        }
        missing = {kb_id: set(ids) for kb_id, ids in expected.items()}
        coverage: dict[str, int] = {}
        while time.monotonic() < deadline and any(missing.values()):
            for kb_id, ids in missing.items():
                if not ids:
                    continue
                pages = client_by_kb[kb_id].list_wiki_pages(kb_id)
                covered = {
                    knowledge_id
                    for knowledge_id in ids
                    if any(
                        source_refs_include(page, knowledge_id)
                        and len(normalize_question_stem(wiki_page_substantive_text(page))) >= 40
                        for page in pages
                    )
                }
                ids.difference_update(covered)
                coverage[kb_id] = len(expected[kb_id]) - len(ids)
            if any(missing.values()):
                time.sleep(max(5.0, self.poll_interval))
        unresolved = {kb: sorted(ids)[:20] for kb, ids in missing.items() if ids}
        if unresolved:
            raise E2EFailure(f"Wiki source coverage incomplete: {unresolved}")
        return coverage

    def verify_outputs(
        self,
        *,
        concurrency: int,
        wiki_timeout: float,
        retrieval_sample: int,
        expected_instances: Sequence[str],
        metrics_before: Mapping[str, Mapping[str, float]],
    ) -> dict[str, Any]:
        evidence: dict[str, dict[str, Any]] = {}
        with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
            futures = {
                pool.submit(self._deep_verify_one, observation): observation.knowledge_id
                for observation in self.observations.values()
            }
            for future in as_completed(futures):
                knowledge_id, result = future.result()
                evidence[knowledge_id] = result
        wiki_coverage = self._verify_wiki_coverage(wiki_timeout)

        selected = sorted(
            self.observations.values(),
            key=lambda o: (o.principal_index, o.kb_id, o.extension, o.index),
        )
        if 0 < retrieval_sample < len(selected):
            stride = len(selected) / retrieval_sample
            selected = [
                selected[min(len(selected) - 1, math.floor(i * stride))]
                for i in range(retrieval_sample)
            ]
        for observation in selected:
            results = self.principals[observation.principal_index].client.hybrid_search(
                observation.kb_id,
                observation.marker,
                [observation.knowledge_id],
            )
            if not any(
                str(item.get("knowledge_id", "")) == observation.knowledge_id
                for item in results
            ):
                raise E2EFailure(
                    f"retrieval did not find {observation.knowledge_id}"
                )

        stage_matrix: dict[str, Counter[str]] = defaultdict(Counter)
        for result in evidence.values():
            for stage, instances in result["stage_instances"].items():
                for instance in instances:
                    stage_matrix[stage][instance] += 1
        expected = set(expected_instances)
        for stage in {
            "docreader",
            "chunking",
            "embedding",
            "postprocess",
            "summary",
            "questions",
            "graph",
            "wiki",
        }:
            actual = set(stage_matrix.get(stage, {}))
            if expected and not expected.issubset(actual):
                raise E2EFailure(
                    f"stage {stage} did not execute on every instance: "
                    f"expected={sorted(expected)} actual={sorted(actual)}"
                )

        metrics_after = (
            scrape_metrics(self.worker_containers) if self.worker_containers else {}
        )
        metric_matrix = metrics_delta(metrics_before, metrics_after)
        for container, values in metric_matrix.items():
            missing = [
                stage
                for stage in {
                    "document",
                    "postprocess",
                    "summary",
                    "questions",
                    "graph",
                    "wiki",
                    "multimodal",
                    "table",
                }
                if values.get(f"{stage}:success", 0) <= 0
            ]
            if missing:
                raise E2EFailure(
                    f"worker {container} did not successfully execute {missing}"
                )
        public_stage_matrix = {
            stage: dict(counts) for stage, counts in sorted(stage_matrix.items())
        }
        self.recorder.emit(
            "multitenant.outputs_passed",
            documents=len(evidence),
            retrieval_queries=len(selected),
            wiki_coverage=wiki_coverage,
            stage_instance_matrix=public_stage_matrix,
            metric_instance_matrix=metric_matrix,
        )
        return {
            "documents_verified": len(evidence),
            "retrieval_queries_verified": len(selected),
            "wiki_source_coverage": wiki_coverage,
            "stage_instance_matrix": public_stage_matrix,
            "metric_instance_matrix": metric_matrix,
        }

    def result(self, started: float, started_at: str) -> MultiTenantResult:
        wall = max(0.001, time.monotonic() - started)
        queue_waits = [
            o.first_active_at - o.uploaded_at
            for o in self.observations.values()
            if o.first_active_at is not None
        ]
        processing = [
            o.terminal_at - (o.first_active_at or o.uploaded_at)
            for o in self.observations.values()
            if o.terminal_at is not None
        ]
        owners: Counter[str] = Counter()
        for observation in self.observations.values():
            for owner in observation.owners:
                owners[owner] += 1
        completed = sum(o.final_status == "completed" for o in self.observations.values())
        return MultiTenantResult(
            run_id=self.run_id,
            started_at=started_at,
            finished_at=utc_now(),
            documents=len(self.observations),
            principals=len(self.principals),
            knowledge_bases=sum(len(p.knowledge_base_ids) for p in self.principals),
            completed=completed,
            failed=sum(o.final_status == "failed" for o in self.observations.values()),
            wall_seconds=wall,
            throughput_docs_per_second=completed / wall,
            queue_wait_p50_seconds=percentile(queue_waits, 50),
            queue_wait_p95_seconds=percentile(queue_waits, 95),
            processing_p50_seconds=percentile(processing, 50),
            processing_p95_seconds=percentile(processing, 95),
            max_waiting_total=self.max_waiting_total,
            max_active_total=self.max_active_total,
            max_active_by_instance=dict(sorted(self.max_active_by_instance.items())),
            root_owner_distribution=dict(sorted(owners.items())),
            documents_by_tenant=dict(
                sorted(Counter(str(o.tenant_id) for o in self.observations.values()).items())
            ),
            documents_by_knowledge_base=dict(
                sorted(Counter(o.kb_id for o in self.observations.values()).items())
            ),
            documents_by_format=dict(
                sorted(Counter(o.extension for o in self.observations.values()).items())
            ),
            documents_by_size=dict(
                sorted(Counter(o.size_class for o in self.observations.values()).items())
            ),
            unique_content_documents=sum(o.content_unique for o in self.observations.values()),
        )

    def cleanup_documents(self, concurrency: int = 32) -> list[str]:
        failures: list[str] = []

        def delete(observation: MultiTenantObservation) -> None:
            self.principals[observation.principal_index].client.delete_knowledge(
                observation.knowledge_id
            )

        with ThreadPoolExecutor(max_workers=max(1, concurrency)) as pool:
            futures = {
                pool.submit(delete, observation): observation.knowledge_id
                for observation in self.observations.values()
            }
            for future in as_completed(futures):
                try:
                    future.result()
                except Exception as exc:
                    failures.append(f"{futures[future]}: {exc}")
        return failures
