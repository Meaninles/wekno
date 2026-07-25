from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Any, Mapping

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import APIClient, E2EFailure, JsonlRecorder, unwrap_data  # type: ignore
    from multitenant_e2e import TenantProvisioner  # type: ignore
else:
    from .cluster_e2e import APIClient, E2EFailure, JsonlRecorder, unwrap_data
    from .multitenant_e2e import TenantProvisioner


HERE = Path(__file__).resolve().parent
AUDIO_EXTENSIONS = {"mp3", "wav", "m4a", "flac", "ogg"}
EXPECTED_EXTENSIONS = {
    "pdf",
    "txt",
    "text",
    "docx",
    "doc",
    "epub",
    "mhtml",
    "md",
    "markdown",
    "png",
    "jpg",
    "jpeg",
    "gif",
    "webp",
    "bmp",
    "tiff",
    "csv",
    "xlsx",
    "xls",
    "pptx",
    "ppt",
    "json",
    *AUDIO_EXTENSIONS,
}


class StateStore:
    def __init__(self, path: Path) -> None:
        self.path = path.resolve()
        self._lock = threading.Lock()

    def exists(self) -> bool:
        return self.path.is_file()

    def load(self) -> dict[str, Any]:
        value = json.loads(self.path.read_text(encoding="utf-8"))
        if not isinstance(value, dict):
            raise E2EFailure(f"state must be a JSON object: {self.path}")
        return value

    def save(self, state: Mapping[str, Any]) -> None:
        # The state intentionally contains only disposable object IDs and test
        # evidence. API keys and bearer/refresh tokens must never be persisted.
        forbidden = {"api_key", "token", "refresh_token", "password"}
        if forbidden.intersection(state):
            raise E2EFailure("refusing to persist credentials in E2E state")
        encoded = json.dumps(state, ensure_ascii=False, indent=2) + "\n"
        self.path.parent.mkdir(parents=True, exist_ok=True)
        temporary = self.path.with_suffix(self.path.suffix + ".tmp")
        with self._lock:
            temporary.write_text(encoded, encoding="utf-8")
            temporary.replace(self.path)


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=(
            "Resumable real-account acceptance test for every supported document "
            "extension. Non-audio and audio phases are intentionally separable so "
            "an unavailable external ASR service cannot duplicate healthy documents."
        )
    )
    p.add_argument(
        "phase",
        choices=("nonaudio", "audio", "status"),
        help="stage non-audio files, stage audio files, or inspect the durable state",
    )
    p.add_argument(
        "--base-url",
        default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"),
    )
    p.add_argument(
        "--admin-token",
        default=os.getenv("WEKNORA_E2E_ADMIN_TOKEN", ""),
        help="system-admin bearer token; never persisted or printed",
    )
    p.add_argument(
        "--tenant-api-key",
        default=os.getenv("WEKNORA_E2E_TENANT_API_KEY", ""),
        help=(
            "existing test tenant API key; permits resumable phases without "
            "rotating the key or requiring a system-admin token"
        ),
    )
    p.add_argument(
        "--fixture-dir",
        type=Path,
        required=True,
    )
    p.add_argument(
        "--state",
        type=Path,
        required=True,
    )
    p.add_argument(
        "--process-config",
        type=Path,
        default=HERE / "process_config.full.example.json",
    )
    p.add_argument(
        "--knowledge-base-name",
        default="全格式解析验证",
    )
    p.add_argument("--upload-concurrency", type=int, default=2)
    return p


def required_object(value: Any, description: str) -> Mapping[str, Any]:
    data = unwrap_data(value)
    if not isinstance(data, Mapping):
        raise E2EFailure(f"{description} must be an object")
    return data


def load_manifest(fixture_dir: Path) -> tuple[dict[str, Mapping[str, Any]], Path]:
    root = fixture_dir.resolve()
    manifest_path = root / "manifest.json"
    if not manifest_path.is_file():
        raise E2EFailure(f"fixture manifest does not exist: {manifest_path}")
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    raw_files = manifest.get("files") if isinstance(manifest, Mapping) else None
    if not isinstance(raw_files, list):
        raise E2EFailure("fixture manifest files must be an array")
    by_extension: dict[str, Mapping[str, Any]] = {}
    for raw in raw_files:
        if not isinstance(raw, Mapping):
            raise E2EFailure("fixture manifest entry must be an object")
        extension = str(raw.get("extension", "")).strip().lower()
        filename = str(raw.get("filename", "")).strip()
        path = root / filename
        if not extension or not filename or not path.is_file():
            raise E2EFailure(f"invalid fixture manifest entry: {raw!r}")
        expected_size = int(raw.get("size_bytes", -1))
        if expected_size < 0 or path.stat().st_size != expected_size:
            raise E2EFailure(f"fixture size mismatch: {filename}")
        if extension in by_extension:
            raise E2EFailure(f"duplicate fixture extension: {extension}")
        by_extension[extension] = raw
    if set(by_extension) != EXPECTED_EXTENSIONS:
        raise E2EFailure(
            "fixture extension mismatch: "
            f"missing={sorted(EXPECTED_EXTENSIONS - set(by_extension))}, "
            f"extra={sorted(set(by_extension) - EXPECTED_EXTENSIONS)}"
        )
    return by_extension, root


def tenant_client(admin: APIClient, tenant_id: int) -> APIClient:
    payload = required_object(
        admin.request("POST", f"/api/v1/tenants/{tenant_id}/api-key", json_body={}),
        "tenant API-key reset response",
    )
    api_key = str(payload.get("api_key", "")).strip()
    if not api_key:
        raise E2EFailure(f"tenant {tenant_id} API-key reset returned no key")
    return APIClient(admin.base_url, api_key, auth_mode="api-key", timeout=120)


def provision(
    admin: APIClient,
    recorder: JsonlRecorder,
    store: StateStore,
    knowledge_base_name: str,
) -> tuple[dict[str, Any], APIClient]:
    run_id = f"supported-formats-{time.strftime('%Y%m%d-%H%M%S')}"
    provisioner = TenantProvisioner(
        admin,
        recorder,
        require_asr_credential=False,
    )
    principals = provisioner.provision(
        principal_count=1,
        knowledge_bases_per_principal=1,
        run_suffix=run_id,
        knowledge_base_name_factory=lambda _principal, _kb: knowledge_base_name,
        knowledge_base_body_kwargs={"asr_enabled": False},
    )
    principal = principals[0]
    state: dict[str, Any] = {
        "schema_version": 1,
        "run_id": run_id,
        "tenant_id": principal.tenant_id,
        "user_id": principal.user_id,
        "username": principal.username,
        "knowledge_base_id": principal.knowledge_base_ids[0],
        "knowledge_base_name": knowledge_base_name,
        "asr_deferred": True,
        "documents": [],
        "created_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    }
    store.save(state)
    return state, principal.client


def existing_or_provisioned(
    admin: APIClient | None,
    recorder: JsonlRecorder,
    store: StateStore,
    knowledge_base_name: str,
    tenant_api_key: str,
    base_url: str,
) -> tuple[dict[str, Any], APIClient]:
    if not store.exists():
        if admin is None:
            raise E2EFailure(
                "initial provisioning requires --admin-token; direct tenant "
                "authentication is accepted only with an existing state file"
            )
        return provision(admin, recorder, store, knowledge_base_name)
    state = store.load()
    tenant_id = int(state.get("tenant_id", 0))
    kb_id = str(state.get("knowledge_base_id", "")).strip()
    if tenant_id <= 0 or not kb_id:
        raise E2EFailure("existing state lacks tenant or knowledge-base identity")
    if tenant_api_key:
        client = APIClient(
            admin.base_url if admin is not None else base_url,
            tenant_api_key,
            auth_mode="api-key",
            timeout=120,
        )
    else:
        if admin is None:
            raise E2EFailure(
                "--admin-token or --tenant-api-key is required for an existing run"
            )
        client = tenant_client(admin, tenant_id)
    client.get_knowledge_base(kb_id)
    return state, client


def asr_enabled(knowledge_base: Mapping[str, Any]) -> bool:
    candidates = [knowledge_base]
    config = knowledge_base.get("config")
    if isinstance(config, Mapping):
        candidates.append(config)
    for candidate in candidates:
        value = candidate.get("asr_config")
        if isinstance(value, Mapping):
            return bool(value.get("enabled"))
    return False


def stage(
    *,
    state: dict[str, Any],
    store: StateStore,
    client: APIClient,
    fixture_dir: Path,
    entries: Mapping[str, Mapping[str, Any]],
    process_config: Mapping[str, Any],
    extensions: set[str],
    upload_concurrency: int,
) -> dict[str, Any]:
    kb_id = str(state["knowledge_base_id"])
    existing = {
        str(item.get("extension", "")).lower(): item
        for item in state.get("documents", [])
        if isinstance(item, Mapping)
    }
    missing = sorted(extensions - set(existing))
    if not missing:
        return {
            "requested": len(extensions),
            "uploaded": 0,
            "already_present": len(extensions),
            "errors": [],
        }

    run_id = str(state["run_id"])

    def upload(extension: str) -> dict[str, Any]:
        entry = entries[extension]
        source_name = str(entry["filename"])
        source = fixture_dir / source_name
        data = client.upload_document(
            kb_id,
            f"{run_id}-{source_name}",
            source.read_bytes(),
            process_config=process_config,
            metadata={
                "e2e_run_id": run_id,
                "e2e_marker": str(entry.get("marker", "")),
                "e2e_extension": extension,
            },
        )
        return {
            "knowledge_id": str(data["id"]),
            "extension": extension,
            "filename": source_name,
            "marker": str(entry.get("marker", "")),
            "size_bytes": source.stat().st_size,
            "uploaded_at": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
        }

    uploaded: list[dict[str, Any]] = []
    errors: list[dict[str, str]] = []
    with ThreadPoolExecutor(max_workers=max(1, upload_concurrency)) as pool:
        futures = {pool.submit(upload, extension): extension for extension in missing}
        for future in as_completed(futures):
            extension = futures[future]
            try:
                document = future.result()
                existing[extension] = document
                uploaded.append(document)
                state["documents"] = [
                    existing[key] for key in sorted(existing)
                ]
                store.save(state)
            except Exception as exc:
                errors.append({"extension": extension, "error": str(exc)})

    state["documents"] = [existing[key] for key in sorted(existing)]
    state["last_upload_errors"] = errors
    state["updated_at"] = time.strftime("%Y-%m-%dT%H:%M:%S%z")
    store.save(state)
    return {
        "requested": len(extensions),
        "uploaded": len(uploaded),
        "already_present": len(extensions) - len(missing),
        "errors": errors,
    }


def status(client: APIClient, state: Mapping[str, Any]) -> dict[str, Any]:
    counts: dict[str, int] = {}
    derivative_counts: dict[str, dict[str, int]] = {
        "summary_status": {},
        "enrichment_status": {},
        "wiki_status": {},
    }
    for item in state.get("documents", []):
        if not isinstance(item, Mapping):
            continue
        knowledge_id = str(item.get("knowledge_id", "")).strip()
        if not knowledge_id:
            continue
        knowledge = client.get_knowledge(knowledge_id)
        parse_status = str(knowledge.get("parse_status", "unknown"))
        counts[parse_status] = counts.get(parse_status, 0) + 1
        for field, values in derivative_counts.items():
            value = str(knowledge.get(field, "unknown"))
            values[value] = values.get(value, 0) + 1
    return {
        "documents": sum(counts.values()),
        "parse_status": dict(sorted(counts.items())),
        "derivatives": {
            key: dict(sorted(value.items()))
            for key, value in derivative_counts.items()
        },
    }


def main() -> int:
    args = parser().parse_args()
    if not args.admin_token and not args.tenant_api_key:
        print(
            "ERROR: --admin-token or --tenant-api-key is required",
            file=sys.stderr,
        )
        return 2
    if args.upload_concurrency <= 0:
        print("ERROR: --upload-concurrency must be positive", file=sys.stderr)
        return 2
    store = StateStore(args.state)
    recorder = JsonlRecorder(
        store.path.with_name(store.path.stem + ".events.jsonl")
    )
    admin = (
        APIClient(
            args.base_url,
            args.admin_token,
            auth_mode="bearer",
            timeout=120,
        )
        if args.admin_token
        else None
    )
    try:
        entries, fixture_dir = load_manifest(args.fixture_dir)
        process_config = json.loads(
            args.process_config.resolve().read_text(encoding="utf-8")
        )
        state, client = existing_or_provisioned(
            admin,
            recorder,
            store,
            args.knowledge_base_name,
            args.tenant_api_key,
            args.base_url,
        )
        if args.phase == "status":
            result = status(client, state)
        else:
            selected = (
                AUDIO_EXTENSIONS
                if args.phase == "audio"
                else EXPECTED_EXTENSIONS - AUDIO_EXTENSIONS
            )
            if args.phase == "audio":
                kb = client.get_knowledge_base(str(state["knowledge_base_id"]))
                if not asr_enabled(kb):
                    raise E2EFailure(
                        "audio phase refused: exact ASR must be healthy and enabled "
                        "on the test knowledge base before uploading audio"
                    )
            result = stage(
                state=state,
                store=store,
                client=client,
                fixture_dir=fixture_dir,
                entries=entries,
                process_config=process_config,
                extensions=set(selected),
                upload_concurrency=args.upload_concurrency,
            )
        public = {
            "phase": args.phase,
            "run_id": state["run_id"],
            "tenant_id": state["tenant_id"],
            "knowledge_base_id": state["knowledge_base_id"],
            "result": result,
        }
        recorder.emit("supported_formats.phase_finished", **public)
        print(json.dumps(public, ensure_ascii=False))
        if isinstance(result, Mapping) and result.get("errors"):
            return 1
        return 0
    except Exception as exc:
        recorder.emit(
            "supported_formats.phase_failed",
            phase=args.phase,
            error=str(exc),
            error_type=type(exc).__name__,
        )
        print(f"ERROR: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
