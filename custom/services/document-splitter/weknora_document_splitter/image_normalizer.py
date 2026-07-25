"""Bounded image normalization for DocReader -> App transport.

Office files may embed EMF/WMF vector pictures. Most OpenAI-compatible VLM
servers only decode browser raster formats, so forwarding those bytes unchanged
turns an otherwise valid document into a late VLM 500. Rasterize at the
DocReader boundary, where LibreOffice is already an explicit runtime
dependency, and fail the parse if a requested image cannot be made consumable.
"""

from __future__ import annotations

import os
import shutil
import subprocess
import tempfile
import threading
from pathlib import Path


class ImageNormalizationError(RuntimeError):
    """The image could not be converted into a bounded VLM-safe payload."""


_RASTER_MIME = {
    ".png": "image/png",
    ".jpg": "image/jpeg",
    ".jpeg": "image/jpeg",
    ".gif": "image/gif",
    ".webp": "image/webp",
    ".bmp": "image/bmp",
}
_VECTOR_SUFFIXES = {".emf", ".x-emf", ".wmf", ".x-wmf", ".svg", ".svgz"}
_MAX_INPUT_BYTES = 64 * 1024 * 1024
_MAX_OUTPUT_BYTES = 32 * 1024 * 1024
_MAX_EDGE = 4096
_RASTER_TIMEOUT_SECONDS = 120


def _positive_int_env(name: str, default: int) -> int:
    try:
        value = int((os.environ.get(name) or "").strip())
    except ValueError:
        return default
    return value if value > 0 else default


_RASTER_SLOTS = threading.BoundedSemaphore(
    _positive_int_env("CUSTOM_DOCREADER_IMAGE_RASTER_CONCURRENCY", 1)
)
_RASTER_BATCH_SIZE = _positive_int_env(
    "CUSTOM_DOCREADER_IMAGE_RASTER_BATCH_SIZE", 64
)


def _looks_like_emf(data: bytes) -> bool:
    # ENHMETAHEADER signature dSignature == 0x464D4520 (" EMF") at offset 40.
    return len(data) >= 44 and data[40:44] == b" EMF"


def _looks_like_wmf(data: bytes) -> bool:
    if len(data) < 4:
        return False
    # Aldus placeable WMF key, or the common METAHEADER type/header-size pair.
    return data[:4] == b"\xd7\xcd\xc6\x9a" or (
        data[:2] in {b"\x01\x00", b"\x02\x00"} and data[2:4] == b"\x09\x00"
    )


def _looks_like_svg(data: bytes) -> bool:
    head = data[:4096].lstrip(b"\xef\xbb\xbf\x00\t\r\n ").lower()
    return head.startswith(b"<svg") or b"<svg" in head


def _vector_suffix(filename: str, data: bytes) -> str:
    suffix = Path(filename).suffix.lower()
    if suffix in {".emf", ".x-emf"} or _looks_like_emf(data):
        return ".emf"
    if suffix in {".wmf", ".x-wmf"} or _looks_like_wmf(data):
        return ".wmf"
    if suffix in {".svg", ".svgz"} or _looks_like_svg(data):
        return ".svg"
    return ""


def _run(
    argv: list[str],
    *,
    timeout: int = _RASTER_TIMEOUT_SECONDS,
    env: dict[str, str] | None = None,
) -> subprocess.CompletedProcess[bytes]:
    try:
        return subprocess.run(
            argv,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            timeout=timeout,
            check=False,
            env=env,
            start_new_session=True,
        )
    except subprocess.TimeoutExpired as exc:
        raise ImageNormalizationError(
            f"image conversion timed out after {timeout}s"
        ) from exc
    except OSError as exc:
        raise ImageNormalizationError(f"cannot start image converter: {exc}") from exc


def _read_bounded(path: Path) -> bytes:
    try:
        size = path.stat().st_size
    except OSError as exc:
        raise ImageNormalizationError(f"converted image is missing: {exc}") from exc
    if size <= 0 or size > _MAX_OUTPUT_BYTES:
        raise ImageNormalizationError(
            f"converted image size {size} is outside 1..{_MAX_OUTPUT_BYTES} bytes"
        )
    return path.read_bytes()


def _normalize_png(source: Path, destination: Path) -> bytes:
    convert = shutil.which("convert")
    if not convert:
        return _read_bounded(source)
    result = _run(
        [
            convert,
            "-limit",
            "memory",
            "256MiB",
            "-limit",
            "map",
            "512MiB",
            str(source),
            "-auto-orient",
            "-resize",
            f"{_MAX_EDGE}x{_MAX_EDGE}>",
            "-strip",
            str(destination),
        ]
    )
    if result.returncode != 0:
        detail = result.stderr.decode("utf-8", errors="replace")[-1000:]
        raise ImageNormalizationError(
            f"PNG bounding failed with exit {result.returncode}: {detail}"
        )
    return _read_bounded(destination)


def _rasterize_vector(data: bytes, suffix: str) -> bytes:
    soffice = shutil.which("soffice") or shutil.which("libreoffice")
    convert = shutil.which("convert")
    if suffix in {".emf", ".wmf"} and not soffice:
        raise ImageNormalizationError(
            f"LibreOffice is required to rasterize {suffix} images"
        )
    if suffix == ".svg" and not convert and not soffice:
        raise ImageNormalizationError("no SVG rasterizer is installed")

    acquired = _RASTER_SLOTS.acquire(timeout=_RASTER_TIMEOUT_SECONDS)
    if not acquired:
        raise ImageNormalizationError("timed out waiting for an image raster slot")
    try:
        with tempfile.TemporaryDirectory(prefix="weknora-image-normalize-") as raw:
            root = Path(raw)
            source = root / f"source{suffix}"
            source.write_bytes(data)
            first_png = root / "source.png"

            if suffix == ".svg" and convert:
                result = _run([convert, str(source), str(first_png)])
            else:
                output_dir = root / "output"
                profile_dir = root / "lo-profile"
                output_dir.mkdir()
                profile_dir.mkdir()
                env = os.environ.copy()
                env.setdefault("SAL_USE_VCLPLUGIN", "svp")
                result = _run(
                    [
                        str(soffice),
                        "--headless",
                        f"-env:UserInstallation={profile_dir.resolve().as_uri()}",
                        "--convert-to",
                        "png",
                        "--outdir",
                        str(output_dir),
                        str(source),
                    ],
                    env=env,
                )
                first_png = output_dir / "source.png"

            if result.returncode != 0 or not first_png.is_file():
                detail = result.stderr.decode("utf-8", errors="replace")[-1000:]
                raise ImageNormalizationError(
                    f"{suffix} rasterization failed with exit "
                    f"{result.returncode}: {detail}"
                )
            return _normalize_png(first_png, root / "bounded.png")
    finally:
        _RASTER_SLOTS.release()


def _rasterize_office_vector_batch(
    vectors: list[tuple[bytes, str]],
) -> list[bytes]:
    """Rasterize several EMF/WMF payloads in one LibreOffice process.

    Starting a fresh headless LibreOffice process/profile costs several
    seconds. Office documents commonly contain multiple vector diagrams, so a
    per-image process multiplied that startup cost and made DocReader appear
    hung. The bounded batch keeps request memory predictable while paying the
    startup cost once for up to ``_RASTER_BATCH_SIZE`` images.
    """

    if not vectors:
        return []
    if any(suffix not in {".emf", ".wmf"} for _, suffix in vectors):
        raise ImageNormalizationError("office vector batch contains a non-EMF/WMF item")
    soffice = shutil.which("soffice") or shutil.which("libreoffice")
    if not soffice:
        raise ImageNormalizationError(
            "LibreOffice is required to rasterize EMF/WMF images"
        )
    acquired = _RASTER_SLOTS.acquire(timeout=_RASTER_TIMEOUT_SECONDS)
    if not acquired:
        raise ImageNormalizationError("timed out waiting for an image raster slot")
    try:
        with tempfile.TemporaryDirectory(
            prefix="weknora-image-normalize-batch-"
        ) as raw:
            root = Path(raw)
            input_dir = root / "input"
            output_dir = root / "output"
            profile_dir = root / "lo-profile"
            bounded_dir = root / "bounded"
            for directory in (input_dir, output_dir, profile_dir, bounded_dir):
                directory.mkdir()

            sources: list[Path] = []
            for index, (data, suffix) in enumerate(vectors):
                source = input_dir / f"source-{index:04d}{suffix}"
                source.write_bytes(data)
                sources.append(source)

            env = os.environ.copy()
            env.setdefault("SAL_USE_VCLPLUGIN", "svp")
            result = _run(
                [
                    str(soffice),
                    "--headless",
                    f"-env:UserInstallation={profile_dir.resolve().as_uri()}",
                    "--convert-to",
                    "png",
                    "--outdir",
                    str(output_dir),
                    *(str(source) for source in sources),
                ],
                env=env,
            )
            if result.returncode != 0:
                detail = result.stderr.decode("utf-8", errors="replace")[-1000:]
                raise ImageNormalizationError(
                    "office vector batch rasterization failed with exit "
                    f"{result.returncode}: {detail}"
                )

            normalized: list[bytes] = []
            for index, source in enumerate(sources):
                first_png = output_dir / f"{source.stem}.png"
                if not first_png.is_file():
                    detail = result.stderr.decode("utf-8", errors="replace")[-1000:]
                    raise ImageNormalizationError(
                        f"office vector batch omitted {source.name}: {detail}"
                    )
                normalized.append(
                    _normalize_png(
                        first_png,
                        bounded_dir / f"source-{index:04d}.png",
                    )
                )
            return normalized
    finally:
        _RASTER_SLOTS.release()


def _validated_payload(data: bytes) -> bytes:
    if not isinstance(data, (bytes, bytearray)):
        raise ImageNormalizationError("image payload is not bytes")
    payload = bytes(data)
    if not payload or len(payload) > _MAX_INPUT_BYTES:
        raise ImageNormalizationError(
            f"image input size {len(payload)} is outside 1..{_MAX_INPUT_BYTES} bytes"
        )
    return payload


def normalize_image_for_transport(
    filename: str,
    data: bytes,
) -> tuple[str, str, bytes, bool]:
    """Return ``(filename, mime_type, bytes, converted)`` for one image.

    Raster inputs are passed through. Vector inputs are converted to PNG using
    a unique LibreOffice profile so concurrent document requests cannot contend
    on a shared user profile or silently reuse another request's output.
    """

    payload = _validated_payload(data)

    suffix = Path(filename).suffix.lower()
    vector = _vector_suffix(filename, payload)
    if not vector:
        return filename, _RASTER_MIME.get(suffix, "application/octet-stream"), payload, False

    png = _rasterize_vector(payload, vector)
    stem = Path(filename).stem or "image"
    return f"{stem}.png", "image/png", png, True


def normalize_images_for_transport(
    images: list[tuple[str, bytes]],
) -> list[tuple[str, str, bytes, bool]]:
    """Normalize a request's images while batching Office vector startup.

    Raster images remain zero-copy pass-through values. EMF/WMF inputs are
    grouped in bounded LibreOffice invocations; SVG and magic-only edge cases
    retain the proven single-image path.
    """

    normalized: list[tuple[str, str, bytes, bool] | None] = [None] * len(images)
    office_vectors: list[tuple[int, str, bytes, str]] = []
    for index, (filename, raw) in enumerate(images):
        payload = _validated_payload(raw)
        suffix = Path(filename).suffix.lower()
        vector = _vector_suffix(filename, payload)
        if vector in {".emf", ".wmf"}:
            office_vectors.append((index, filename, payload, vector))
        elif vector:
            normalized[index] = normalize_image_for_transport(filename, payload)
        else:
            normalized[index] = (
                filename,
                _RASTER_MIME.get(suffix, "application/octet-stream"),
                payload,
                False,
            )

    for start in range(0, len(office_vectors), _RASTER_BATCH_SIZE):
        group = office_vectors[start : start + _RASTER_BATCH_SIZE]
        pngs = _rasterize_office_vector_batch(
            [(payload, vector) for _, _, payload, vector in group]
        )
        if len(pngs) != len(group):
            raise ImageNormalizationError(
                "office vector batch returned an unexpected output count"
            )
        for (index, filename, _, _), png in zip(group, pngs):
            stem = Path(filename).stem or "image"
            normalized[index] = (f"{stem}.png", "image/png", png, True)

    if any(item is None for item in normalized):
        raise ImageNormalizationError("image normalization left an unresolved item")
    return [item for item in normalized if item is not None]
