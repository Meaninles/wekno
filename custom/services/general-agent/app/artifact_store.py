"""Artifact byte storage and delivery admission shared by all runtime adapters."""
from __future__ import annotations

import hashlib
import json
import mimetypes
import re
import threading
import uuid
from pathlib import Path
from typing import Any, Callable

from .schemas import ChatPayload, SidecarArtifact

def safe_filename(name: str) -> str:
    name = (name or "").strip().replace("\\", "_").replace("/", "_")
    name = re.sub(r"[\x00-\x1f]+", "", name)
    name = Path(name).name
    return name[:180]


def normalized_ext(filename: str) -> str:
    ext = Path(filename).suffix.lower().lstrip(".")
    return ext


ARTIFACT_RETURN_LIMIT_BYTES = 128 * 1024 * 1024


class ArtifactStore:
    def __init__(self, run_dir: Path, payload: ChatPayload,
                 publisher: Callable[[ChatPayload, SidecarArtifact, Path], SidecarArtifact] | None = None) -> None:
        self.run_dir = run_dir
        self.generated_dir = run_dir / "generated"
        self.generated_dir.mkdir(parents=True, exist_ok=True)
        self.out_dir = run_dir / "artifacts"
        self.out_dir.mkdir(parents=True, exist_ok=True)
        self.payload = payload
        self.publisher = publisher
        self.items: list[SidecarArtifact] = []
        self.notice = ""
        self.original_count = 0
        self.returned_count = 0
        self.dropped_count = 0
        self.returned_size = 0
        self.reviewed_fingerprints: set[str] = set()
        self._lock = threading.RLock()

    def _check_capacity(self, filename: str, size: int) -> None:
        current = self._dedupe_by_filename_keep_last(self.items)
        retained = sum(item.file_size for item in current if item.filename != filename)
        if size < 0 or retained + size > ARTIFACT_RETURN_LIMIT_BYTES:
            raise ValueError(f"Artifact delivery exceeds the 128 MiB total limit: {retained} retained bytes + {size} new bytes. The file was not registered; already registered files are unchanged.")

    def store_file(self, filename: str, source: Path, *, require_review: bool = False) -> dict[str, Any]:
        """Copy bounded chunks and bind the actual delivered bytes to inspection.

        Capacity errors occur in the tool call, before the model claims delivery.
        The registry never silently discards a successfully registered file.
        """
        if not self.payload.enable_artifacts:
            raise ValueError("Artifacts are disabled for this agent")
        filename = safe_filename(filename)
        if not filename:
            raise ValueError("filename is required")
        with self._lock:
            self._check_capacity(filename, source.stat().st_size)
            token = str(uuid.uuid4())
            target = self.out_dir / token
            digest = hashlib.sha256()
            size = 0
            try:
                with source.open('rb') as reader, target.open('xb') as writer:
                    while chunk := reader.read(1024 * 1024):
                        size += len(chunk)
                        self._check_capacity(filename, size)
                        digest.update(chunk)
                        writer.write(chunk)
                sha = digest.hexdigest()
                if require_review and f"{source.resolve()}::{sha}" not in self.reviewed_fingerprints:
                    raise ValueError("The delivered file bytes have no complete page inspection; the file may have changed after inspection.")
                # A lost upload response must retry the same remote reservation,
                # including when the model retries this tool in a later call.
                token = str(uuid.uuid5(uuid.NAMESPACE_URL, json.dumps(
                    [self.payload.run_id, filename, sha], ensure_ascii=False)))
                existing = next((item for item in self.items if item.file_token == token), None)
                if existing is not None:
                    target.unlink()
                    return existing.model_dump()
                staged = self.out_dir / token
                target.replace(staged)
                target = staged
                return self._record(filename, token, sha, size)
            except BaseException:
                target.unlink(missing_ok=True)
                target.with_suffix('.json').unlink(missing_ok=True)
                raise

    def _record(self, filename: str, token: str, sha: str, size: int, content_type: str = "") -> dict[str, Any]:
        item = SidecarArtifact(file_token=token, filename=filename,
            file_type=normalized_ext(filename), file_size=size, sha256=sha,
            content_type=content_type or mimetypes.guess_type(filename)[0] or "application/octet-stream")
        if self.publisher is not None:
            item = self.publisher(self.payload, item, self.out_dir / token)
        (self.out_dir / token).with_suffix('.json').write_text(json.dumps(item.model_dump(), ensure_ascii=False), encoding='utf-8')
        self.items.append(item)
        return item.model_dump()

    @staticmethod
    def _dedupe_by_filename_keep_last(items: list[SidecarArtifact]) -> list[SidecarArtifact]:
        seen: set[str] = set()
        deduped_reversed: list[SidecarArtifact] = []
        for item in reversed(items):
            key = item.filename or item.file_token
            if key in seen:
                continue
            seen.add(key)
            deduped_reversed.append(item)
        return list(reversed(deduped_reversed))

    def finalize_for_result(self) -> list[SidecarArtifact]:
        """Return every registered filename's latest version, without truncation."""
        items = self._dedupe_by_filename_keep_last(self.items)
        self.original_count = len(items)
        self.returned_count = len(items)
        self.dropped_count = 0
        self.returned_size = sum(item.file_size for item in items)
        return items
