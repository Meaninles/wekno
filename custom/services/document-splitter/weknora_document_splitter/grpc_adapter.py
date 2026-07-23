from __future__ import annotations

import hashlib
import logging
import os
import shutil
import tempfile
from collections.abc import Iterator
from pathlib import Path

from docreader.proto import docreader_pb2

from .service import (
    PLAN_VERSION,
    SplitFailure,
    SplitPolicy,
    create_split_archive,
)

LOGGER = logging.getLogger(__name__)
_FRAME_BYTES = 1024 * 1024


def _error(code: str, message: str, retryable: bool = False):
    return docreader_pb2.SplitResponse(
        error=docreader_pb2.SplitError(
            code=code,
            message=message,
            retryable=retryable,
        )
    )


def split_rpc(request_iterator, context) -> Iterator[docreader_pb2.SplitResponse]:
    """Receive a bounded stream and return a bounded archive stream."""

    work_dir = Path(tempfile.mkdtemp(prefix="weknora-document-split-"))
    archive_path = work_dir / "parts.zip"
    digest = hashlib.sha256()
    received = 0
    policy = SplitPolicy.from_env()

    try:
        frames = iter(request_iterator)
        try:
            first_frame = next(frames)
        except StopIteration:
            yield _error("invalid_stream", "split header is missing")
            return
        if first_frame.WhichOneof("payload") != "header":
            yield _error("invalid_stream", "split header must be the first frame")
            return
        header = first_frame.header
        source_path = work_dir / f"source{_source_suffix(header.file_type, header.file_name)}"
        with source_path.open("xb") as sink:
            for frame in frames:
                payload = frame.WhichOneof("payload")
                if payload == "header":
                    yield _error(
                        "invalid_stream",
                        "split header must be the first and only header frame",
                    )
                    return
                if payload != "data":
                    yield _error("invalid_stream", "split stream contains an empty frame")
                    return
                chunk = bytes(frame.data)
                received += len(chunk)
                if received > policy.max_source_bytes:
                    yield _error(
                        "source_too_large",
                        f"source exceeds configured maximum of {policy.max_source_bytes} bytes",
                    )
                    return
                digest.update(chunk)
                sink.write(chunk)
            sink.flush()
            os.fsync(sink.fileno())

        if header.source_size and received != header.source_size:
            yield _error(
                "source_size_mismatch",
                f"received {received} bytes, expected {header.source_size}",
                retryable=True,
            )
            return
        actual_sha = digest.hexdigest()
        if header.source_sha256 and actual_sha.lower() != header.source_sha256.lower():
            yield _error(
                "source_hash_mismatch",
                "received source hash does not match the durable source identity",
                retryable=True,
            )
            return

        manifest = create_split_archive(
            source_path=source_path,
            archive_path=archive_path,
            file_name=header.file_name,
            file_type=header.file_type,
            source_size=received,
            source_sha256=actual_sha,
            minimum_parts=max(1, int(header.minimum_parts or 1)),
            target_ratio=float(header.target_ratio or policy.target_ratio),
            policy=policy,
        )
        archive_size = archive_path.stat().st_size
        archive_sha = _sha256_file(archive_path)
        yield docreader_pb2.SplitResponse(
            header=docreader_pb2.SplitArchiveHeader(
                archive_size=archive_size,
                archive_sha256=archive_sha,
                part_count=len(manifest["parts"]),
                planner_version=PLAN_VERSION,
            )
        )
        with archive_path.open("rb") as source:
            while chunk := source.read(_FRAME_BYTES):
                if not context.is_active():
                    return
                yield docreader_pb2.SplitResponse(data=chunk)
    except SplitFailure as exc:
        LOGGER.info("document split rejected code=%s message=%s", exc.code, exc)
        yield _error(exc.code, str(exc), exc.retryable)
    except (OSError, TimeoutError) as exc:
        LOGGER.exception("document split infrastructure failure")
        yield _error("split_io_failed", str(exc), retryable=True)
    except Exception as exc:  # fail closed at the service boundary
        LOGGER.exception("document split failed")
        yield _error("split_failed", str(exc), retryable=False)
    finally:
        shutil.rmtree(work_dir, ignore_errors=True)


def _sha256_file(path: Path) -> str:
    digest = hashlib.sha256()
    with path.open("rb") as source:
        while chunk := source.read(_FRAME_BYTES):
            digest.update(chunk)
    return digest.hexdigest()


def _source_suffix(file_type: str, file_name: str) -> str:
    """Keep the logical format on the staged file without trusting its path."""

    normalized = str(file_type or "").strip().lower().lstrip(".")
    if normalized and len(normalized) <= 16 and normalized.isalnum():
        return f".{normalized}"
    candidate = Path(Path(str(file_name or "")).name).suffix.lower()
    if (
        candidate
        and len(candidate) <= 17
        and candidate[1:].isalnum()
    ):
        return candidate
    return ".bin"
