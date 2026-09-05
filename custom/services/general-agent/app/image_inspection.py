"""Local-file adapter for the platform's configured vision model."""
from __future__ import annotations

import base64
import io
from pathlib import Path

from PIL import Image

LOCAL_IMAGE_SCHEMA = {
    "type": "object",
    "properties": {"file_path": {"type": "string", "minLength": 1}, "prompt": {"type": "string", "minLength": 1}},
    "required": ["file_path", "prompt"], "additionalProperties": False,
}


def image_transport_args(args: dict, run_dir: Path) -> dict:
    path = Path(args["file_path"])
    if not path.is_absolute():
        path = run_dir / path
    if path.stat().st_size > 20 * 1024 * 1024:
        raise ValueError("Image exceeds 20 MiB; inspect a smaller rendered page or crop.")
    with Image.open(path) as source:
        if getattr(source, "n_frames", 1) != 1:
            raise ValueError("Select and render one frame before inspection.")
        out = io.BytesIO()
        source.convert("RGB").save(out, format="PNG")
    data = out.getvalue()
    if len(data) > 20 * 1024 * 1024:
        raise ValueError("Decoded image exceeds 20 MiB; inspect a smaller rendered page or crop.")
    return {"image_base64": base64.b64encode(data).decode("ascii"), "file_name": path.name, "prompt": args["prompt"]}


def native_read_modality_error(tool_input: dict, supports_vision: bool) -> str:
    if supports_vision:
        return ""
    path = Path(str(tool_input.get("file_path", "")))
    if path.suffix.lower() in {".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp", ".tif", ".tiff", ".pdf"}:
        return "The active chat model accepts text tool results only. Use inspect_image for visual observations through the configured vision model; for PDF text use pdftotext, or render individual pages and inspect their images."
    return ""
