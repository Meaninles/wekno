"""Local-file adapter for the platform's configured vision model."""
from __future__ import annotations

import base64
import io
import math
import threading
from pathlib import Path

from PIL import Image, ImageOps

MAX_SOURCE_BYTES = 128 * 1024 * 1024
MAX_SOURCE_PIXELS = 50_000_000
MAX_SOURCE_AXIS = 32_768
# RGBA PNG stays within the platform's 20 MiB transport even for noisy pixels.
MAX_VIEW_PIXELS = 4_000_000
_PREPARE_SLOTS = threading.BoundedSemaphore(2)

LOCAL_IMAGE_SCHEMA = {
    "type": "object",
    "properties": {
        "file_path": {"type": "string", "minLength": 1},
        "prompt": {"type": "string", "minLength": 1},
        "region": {"type": "array", "items": {"type": "integer", "minimum": 0}, "minItems": 4, "maxItems": 4,
                   "description": "Optional [left, top, right, bottom] crop in the original image's orientation-corrected pixel coordinates. Omit for the complete image overview; use a region to read fine details."},
        "frame": {"type": "integer", "minimum": 0, "description": "Frame index for a multi-frame image; required when there is more than one frame."},
    },
    "required": ["file_path", "prompt"], "additionalProperties": False,
}


def prepare_image(args: dict, run_dir: Path) -> tuple[dict, dict]:
    with _PREPARE_SLOTS:
        return _prepare_image(args, run_dir)


def _prepare_image(args: dict, run_dir: Path) -> tuple[dict, dict]:
    path = Path(args["file_path"])
    if not path.is_absolute():
        path = run_dir / path
    if path.stat().st_size > MAX_SOURCE_BYTES:
        raise ValueError("Image exceeds the 128 MiB source limit.")
    with Image.open(path) as source:
        frames = getattr(source, "n_frames", 1)
        if frames != 1 and "frame" not in args:
            raise ValueError(f"Image has {frames} frames; specify the frame index to inspect.")
        frame = args.get("frame", 0)
        if not 0 <= frame < frames:
            raise ValueError("Image frame index is outside the source.")
        source.seek(frame)
        if source.width * source.height > MAX_SOURCE_PIXELS or max(source.size) > MAX_SOURCE_AXIS:
            raise ValueError("Image exceeds the decoded safety limit of 50 million pixels or 32768 pixels per axis.")
        source = ImageOps.exif_transpose(source)
        width, height = source.size
        region = args.get("region", [0, 0, width, height])
        left, top, right, bottom = region
        if not (0 <= left < right <= width and 0 <= top < bottom <= height):
            raise ValueError(f"Image region must be inside the {width} x {height} source and have positive area.")
        view = source.crop(tuple(region)) if "region" in args else source
        region_size = view.size
        if view.width * view.height > MAX_VIEW_PIXELS:
            scale = math.sqrt(MAX_VIEW_PIXELS / (view.width * view.height))
            view.thumbnail((max(1, int(view.width * scale)), max(1, int(view.height * scale))), Image.Resampling.LANCZOS)
        view = view.convert("RGBA" if "A" in view.getbands() or "transparency" in view.info else "RGB")
        out = io.BytesIO()
        view.save(out, format="PNG")
        metadata = {"source_width": width, "source_height": height, "region": region,
                    "view_width": view.width, "view_height": view.height,
                    "resized": view.size != region_size, "frame": frame, "frame_count": frames}
    data = out.getvalue()
    if len(data) > 20 * 1024 * 1024:
        raise ValueError("Prepared image exceeds the 20 MiB platform transport limit.")
    return {"image_base64": base64.b64encode(data).decode("ascii"), "file_name": path.name, "prompt": args["prompt"]}, metadata


def native_read_modality_error(tool_input: dict, supports_vision: bool) -> str:
    if supports_vision:
        return ""
    path = Path(str(tool_input.get("file_path", "")))
    if path.suffix.lower() in {".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".tif", ".tiff", ".pdf"}:
        return "The active chat model accepts text tool results only. Use inspect_image for visual observations through the configured vision model; for PDF text use pdftotext, or render individual pages and inspect their images."
    return ""
