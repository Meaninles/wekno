from __future__ import annotations

import asyncio
import base64
import copy
import hashlib
import importlib.util
import subprocess
import sys
from functools import lru_cache
import json
import os
import posixpath
import re
import shutil
import time
import unicodedata
import uuid
import zipfile
from dataclasses import dataclass, replace
from pathlib import Path
from typing import Any, AsyncIterator, Callable, Iterable
from urllib import request as urlrequest
from urllib.error import HTTPError, URLError
import xml.etree.ElementTree as ET

from .artifact_store import ArtifactStore, ARTIFACT_RETURN_LIMIT_BYTES, normalized_ext, safe_filename
from .final_delivery import ClaudeSDKTerminalCollector
from .schemas import ChatPayload, ChatResult, RunEvent, SidecarArtifact

SAFE_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$")
MAX_PROFESSIONAL_SKILL_PATH_CHARS = 240
MAX_PROFESSIONAL_SKILL_FILE_BYTES = 2 * 1024 * 1024
MAX_PROFESSIONAL_SKILL_TOTAL_BYTES = 8 * 1024 * 1024


def env_int(name: str, default: int) -> int:
    try:
        value = int(str(os.getenv(name, "")).strip())
    except ValueError:
        return default
    return value if value > 0 else default


def effective_max_turns(payload: ChatPayload) -> int:
    configured = payload.runtime_config.max_iterations
    return configured if configured > 0 else env_int("CUSTOM_GENERAL_AGENT_MAX_TURNS", 30)


def effective_llm_api_timeout_seconds(payload: ChatPayload) -> int:
    if payload.runtime_config.llm_call_timeout > 0:
        return payload.runtime_config.llm_call_timeout
    timeout_ms = env_int("CUSTOM_GENERAL_AGENT_CLAUDE_API_TIMEOUT_MS", 600000)
    return max(1, (timeout_ms + 999) // 1000)


def claude_auth_env(payload: ChatPayload, config_dir: Path) -> tuple[dict[str, str], str, str | None]:
    llm = payload.llm
    api_key = (llm.api_key or "").strip()
    api_key_helper = (llm.api_key_helper or "").strip()
    auth_type = (llm.auth_type or "").strip().lower()
    model = (llm.model_name or "").strip()
    if not model:
        raise RuntimeError("通用智能体需要可用 LLM：当前模型缺少模型名称")
    base_url = (llm.base_url or "").strip()
    api_timeout_ms = str(effective_llm_api_timeout_seconds(payload) * 1000)
    env = {
        "CLAUDE_CONFIG_DIR": str(config_dir),
        "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
        "API_TIMEOUT_MS": api_timeout_ms,
        "CLAUDE_CODE_MAX_RETRIES": os.getenv("CUSTOM_GENERAL_AGENT_CLAUDE_MAX_RETRIES", "2"),
        "CLAUDE_ENABLE_STREAM_WATCHDOG": "1",
        "CLAUDE_STREAM_IDLE_TIMEOUT_MS": os.getenv("CUSTOM_GENERAL_AGENT_CLAUDE_IDLE_TIMEOUT_MS", "900000"),
        "CLAUDE_AGENT_SDK_CLIENT_APP": "weknora-general-agent/1.0",
    }
    settings: str | None = None
    if api_key:
        env["ANTHROPIC_API_KEY"] = api_key
        env["ANTHROPIC_AUTH_TOKEN"] = api_key
    elif auth_type == "api_key_helper" and api_key_helper:
        settings = json.dumps({"apiKeyHelper": api_key_helper}, ensure_ascii=False)
    else:
        raise RuntimeError("通用智能体需要可用 LLM：当前模型缺少 API key")
    if base_url:
        env["ANTHROPIC_BASE_URL"] = base_url
    return env, model, settings


DATA_IMAGE_RE = re.compile(r"^data:(image/[A-Za-z0-9.+-]+);base64,(.+)$", re.DOTALL)
DATA_BASE64_URL_RE = re.compile(r"^data:([A-Za-z0-9.+/-]+);base64,(.+)$", re.DOTALL)


def mcp_text(data: Any, is_error: bool = False) -> dict[str, Any]:
    text = data if isinstance(data, str) else json.dumps(data, ensure_ascii=False)
    out: dict[str, Any] = {"content": [{"type": "text", "text": text}]}
    if is_error:
        out["is_error"] = True
    return out


def prompt_media_reference(value: str) -> str:
    text = (value or "").strip()
    match = DATA_BASE64_URL_RE.match(text)
    if not match:
        return text
    mime_type = match.group(1)
    encoded = match.group(2)
    return f"[inline {mime_type} data omitted from text prompt; base64_length={len(encoded)}]"


def normalize_professional_skill_path(value: str) -> str:
    rel = unicodedata.normalize("NFC", (value or "").replace("\\", "/").strip())
    if rel.startswith("./"):
        rel = rel[2:]
    if not rel or rel.startswith("/"):
        raise RuntimeError(f"invalid professional skill file path: {value}")
    if len(rel) > MAX_PROFESSIONAL_SKILL_PATH_CHARS:
        raise RuntimeError(f"professional skill file path is too long: {rel}")
    for ch in rel:
        if ch == "\x00" or unicodedata.category(ch).startswith("C"):
            raise RuntimeError(f"invalid professional skill file path: {rel}")
    parts = rel.split("/")
    if any(part in {"", ".", ".."} or ":" in part for part in parts):
        raise RuntimeError(f"invalid professional skill file path: {rel}")
    clean = posixpath.normpath(rel)
    if not clean or clean == "." or clean == ".." or clean.startswith("../"):
        raise RuntimeError(f"invalid professional skill file path: {rel}")
    if len(clean) > MAX_PROFESSIONAL_SKILL_PATH_CHARS:
        raise RuntimeError(f"professional skill file path is too long: {clean}")
    return clean


def materialize_professional_skills(payload: ChatPayload, run_dir: Path) -> list[str]:
    skills = payload.professional_skills or []
    if not skills:
        return []

    skills_root = run_dir / ".claude" / "skills"
    skills_root.mkdir(parents=True, exist_ok=True)
    loaded: list[str] = []
    total = 0
    for skill in skills:
        name = (skill.name or "").strip()
        if not SAFE_ID_RE.match(name):
            raise RuntimeError(f"invalid professional skill name: {name}")
        skill_dir = skills_root / name
        skill_dir.mkdir(parents=True, exist_ok=True)
        has_skill_md = False
        for file in skill.files or []:
            try:
                rel = normalize_professional_skill_path(file.path or "")
            except RuntimeError as exc:
                raise RuntimeError(f"invalid file path in professional skill {name}: {file.path}") from exc
            try:
                content = base64.b64decode(file.content_base64 or "", validate=True)
            except Exception as exc:
                raise RuntimeError(f"invalid base64 file payload in professional skill {name}/{rel}") from exc
            if len(content) > MAX_PROFESSIONAL_SKILL_FILE_BYTES:
                raise RuntimeError(f"professional skill file too large: {name}/{rel}")
            total += len(content)
            if total > MAX_PROFESSIONAL_SKILL_TOTAL_BYTES:
                raise RuntimeError("professional skills payload is too large")
            target = skill_dir / rel
            resolved = target.resolve()
            if not str(resolved).startswith(str(skill_dir.resolve())):
                raise RuntimeError(f"professional skill path escapes skill directory: {name}/{rel}")
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(content)
            if rel == "SKILL.md":
                has_skill_md = True
        if not has_skill_md:
            raise RuntimeError(f"professional skill {name} is missing SKILL.md")
        loaded.append(name)
    return unique_tool_names(loaded)


def _image_content_block(value: str) -> tuple[dict[str, Any] | None, dict[str, Any]]:
    image = (value or "").strip()
    meta: dict[str, Any] = {
        "kind": "unknown",
        "length": len(image),
    }
    if not image:
        return None, meta
    m = DATA_IMAGE_RE.match(image)
    if m:
        mime_type, encoded = m.group(1), m.group(2)
        meta.update({"kind": "data_uri", "mime_type": mime_type, "base64_length": len(encoded)})
        try:
            # Validate that the payload is actually base64 before passing it to
            # the MCP layer. Do not decode into the text fallback because tool
            # results can legitimately contain large images.
            base64.b64decode(encoded, validate=True)
        except Exception:
            meta["invalid"] = True
            return None, meta
        return {"type": "image", "data": encoded, "mimeType": mime_type}, meta
    if image.startswith(("http://", "https://")):
        meta.update({"kind": "url", "url": image})
    else:
        meta.update({"kind": "opaque"})
    return None, meta


def mcp_tool_result(result: dict[str, Any], current_user_request: str = "") -> dict[str, Any]:
    """Use the same model output as the native engine, without re-projecting
    UI data or reassigning source handles in the transport adapter. Go owns
    the canonical evidence, handles, pagination and error text. Images remain
    typed blocks. The full execution result stays in the platform trace.
    """
    output = str(result.get("output") or "")
    if result.get("error"):
        output = output + ("\n" if output else "") + "Tool error: " + str(result["error"])
    if not output:
        output = json.dumps({"success": bool(result.get("success")), "data": result.get("data") or {}}, ensure_ascii=False)
    content = [{"type": "text", "text": output}]
    for value in result.get("images") or []:
        block, _ = _image_content_block(str(value))
        if block is not None:
            content.append(block)
    return {"content": content, "is_error": result.get("success") is False}


def call_tool_callback(payload: ChatPayload, tool_name: str, args: dict[str, Any], call_id: str = "") -> dict[str, Any]:
    body = json.dumps(
        {
            "run_id": payload.run_id,
            "tool_name": tool_name,
            "arguments": args,
            "tool_call_id": call_id or str(uuid.uuid4()),
        },
        ensure_ascii=False,
    ).encode("utf-8")
    req = urlrequest.Request(
        payload.tool_callback_url,
        data=body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )
    if payload.tool_callback_api_key:
        req.add_header("Authorization", f"Bearer {payload.tool_callback_api_key}")
    try:
        with urlrequest.urlopen(req, timeout=env_int("CUSTOM_GENERAL_AGENT_TOOL_TIMEOUT_SEC", 900)) as resp:
            raw = resp.read()
    except HTTPError as exc:
        raw = exc.read()[:4096]
        raise RuntimeError(f"WeKnora tool callback HTTP {exc.code}: {raw.decode('utf-8', 'ignore')}") from exc
    except URLError as exc:
        raise RuntimeError(f"WeKnora tool callback failed: {exc}") from exc
    return json.loads(raw.decode("utf-8"))


def upload_artifact_before_completion(
    payload: ChatPayload,
    artifact: SidecarArtifact,
    artifact_path: Path,
) -> SidecarArtifact:
    """Durably hand one artifact to WeKnora before a terminal result is sent.

    The raw body avoids Base64 amplification. The compact, URL-safe metadata
    header is authenticated by the same internal key as tool callbacks.
    """
    upload_url = (payload.artifact_upload_url or "").strip()
    if not upload_url:
        raise RuntimeError("WeKnora private artifact upload URL is not configured")
    if not artifact_path.is_file():
        raise RuntimeError(f"artifact staging file is missing: {artifact.file_token}")
    metadata = {
        "tenant_id": payload.tenant_id,
        "user_id": payload.user_id,
        "session_id": payload.session_id,
        "run_id": payload.run_id,
        "assistant_message_id": payload.assistant_message_id,
        "file_token": artifact.file_token,
        "filename": artifact.filename,
        "file_type": artifact.file_type,
        "file_size": artifact.file_size,
        "sha256": artifact.sha256,
        "content_type": artifact.content_type,
    }
    encoded_metadata = base64.urlsafe_b64encode(
        json.dumps(metadata, ensure_ascii=False, separators=(",", ":")).encode("utf-8")
    ).decode("ascii").rstrip("=")
    max_attempts = env_int("CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_RETRIES", 3)
    timeout = env_int("CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_TIMEOUT_SEC", 900)
    last_error: Exception | None = None
    for attempt in range(1, max_attempts + 1):
        try:
            # Reopen on each retry so urllib streams from byte zero without
            # retaining a second complete 128 MiB file in the worker heap.
            with artifact_path.open("rb") as body:
                req = urlrequest.Request(upload_url, data=body, method="POST", headers={
                    "Content-Type": artifact.content_type or "application/octet-stream",
                    "Content-Length": str(artifact.file_size),
                    "X-WeKnora-Artifact-Metadata": encoded_metadata,
                })
                if payload.tool_callback_api_key:
                    req.add_header("Authorization", f"Bearer {payload.tool_callback_api_key}")
                with urlrequest.urlopen(req, timeout=timeout) as resp:
                    raw = resp.read()
            persisted = SidecarArtifact.model_validate_json(raw)
            if (
                not persisted.persisted
                or not persisted.artifact_id
                or persisted.file_token != artifact.file_token
                or persisted.filename != artifact.filename
                or persisted.file_size != artifact.file_size
                or persisted.sha256.lower() != artifact.sha256.lower()
            ):
                raise RuntimeError("WeKnora returned inconsistent artifact persistence metadata")
            return persisted
        except (HTTPError, URLError, TimeoutError, ValueError, RuntimeError) as exc:
            last_error = exc
            if attempt < max_attempts:
                time.sleep(min(2 ** (attempt - 1), 4))
    raise RuntimeError(f"WeKnora private artifact upload failed after {max_attempts} attempts: {last_error}")


def effective_weknora_tool_specs(payload: ChatPayload) -> list[Any]:
    """Return the permission-checked runtime catalog without query filtering."""

    return list(payload.tools)


def sdk_thinking_config(payload: ChatPayload) -> dict[str, Any] | None:
    thinking = payload.runtime_config.thinking
    if thinking is True:
        return {"type": "adaptive", "display": "omitted"}
    if thinking is False:
        return {"type": "disabled"}
    return None


@dataclass(frozen=True)
class PreparedOriginalInputFile:
    id: str
    source: str
    role: str
    file_name: str
    file_type: str
    file_size: int
    sha256: str
    path: str
    knowledge_id: str = ""
    knowledge_base_id: str = ""


def original_input_download_timeout_seconds() -> int:
    return env_int("CUSTOM_GENERAL_AGENT_ORIGINAL_INPUT_DOWNLOAD_TIMEOUT_SEC", 120)


def original_input_source_dir(source: str, file_type: str) -> str:
    normalized = (source or "").strip().lower()
    ext = (file_type or "").strip().lower().lstrip(".")
    if normalized == "weknora_chat_image_original":
        return "images"
    if normalized == "weknora_selected_knowledge_original":
        return "knowledge_files"
    if ext in {"mp3", "wav", "m4a", "flac", "ogg"}:
        return "audio"
    return "uploads"


def original_input_target_path(run_dir: Path, index: int, item: Any) -> Path:
    file_name = safe_filename(getattr(item, "file_name", "") or f"original_{index}.bin")
    if not file_name:
        file_name = f"original_{index}.bin"
    source_dir = original_input_source_dir(getattr(item, "source", ""), getattr(item, "file_type", ""))
    target = run_dir / "input_files" / source_dir / f"{index:02d}_{file_name}"
    resolved = target.resolve()
    root = run_dir.resolve()
    if root not in resolved.parents:
        raise RuntimeError("original input path escapes run directory")
    target.parent.mkdir(parents=True, exist_ok=True)
    return target


def download_and_verify_original_input_file(item: Any, run_dir: Path, index: int, stopped=None) -> PreparedOriginalInputFile:
    url = (getattr(item, "download_url", "") or "").strip()
    if not url.lower().startswith(("http://", "https://")):
        raise RuntimeError(f"original input file {getattr(item, 'file_name', '') or index} is missing an HTTP(S) download URL")
    expected_sha = (getattr(item, "sha256", "") or "").strip().lower()
    if not re.fullmatch(r"[0-9a-f]{64}", expected_sha):
        raise RuntimeError(f"original input file {getattr(item, 'file_name', '') or index} is missing a valid sha256")
    expected_size = int(getattr(item, "file_size", 0) or 0)
    if expected_size < 0:
        raise RuntimeError(f"original input file {getattr(item, 'file_name', '') or index} has invalid size")

    target = original_input_target_path(run_dir, index, item)
    tmp = target.with_suffix(target.suffix + ".download")
    hasher = hashlib.sha256()
    total = 0
    try:
        req = urlrequest.Request(url, method="GET")
        with urlrequest.urlopen(req, timeout=original_input_download_timeout_seconds()) as resp:
            with tmp.open("wb") as fh:
                while True:
                    if stopped is not None and stopped.is_set():
                        raise RuntimeError("original file download cancelled")
                    chunk = resp.read(1024 * 1024)
                    if not chunk:
                        break
                    total += len(chunk)
                    if total > expected_size:
                        raise RuntimeError(f"original exceeds its declared size: {expected_size}")
                    hasher.update(chunk)
                    fh.write(chunk)
    except HTTPError as exc:
        tmp.unlink(missing_ok=True)
        raw = exc.read()[:512]
        raise RuntimeError(f"download HTTP {exc.code}: {raw.decode('utf-8', 'ignore')}") from exc
    except URLError as exc:
        tmp.unlink(missing_ok=True)
        raise RuntimeError(f"download failed: {exc}") from exc
    except Exception:
        tmp.unlink(missing_ok=True)
        raise

    actual_sha = hasher.hexdigest()
    if expected_size > 0 and total != expected_size:
        tmp.unlink(missing_ok=True)
        raise RuntimeError(f"size mismatch: expected {expected_size}, got {total}")
    if actual_sha != expected_sha:
        tmp.unlink(missing_ok=True)
        raise RuntimeError(f"sha256 mismatch: expected {expected_sha}, got {actual_sha}")
    tmp.replace(target)
    return PreparedOriginalInputFile(
        id=getattr(item, "id", "") or str(uuid.uuid4()),
        source=getattr(item, "source", "") or "weknora_original_file",
        role=getattr(item, "role", "") or "original_file",
        file_name=getattr(item, "file_name", "") or target.name,
        file_type=(getattr(item, "file_type", "") or normalized_ext(target.name)).lstrip("."),
        file_size=total,
        sha256=actual_sha,
        path=str(target.resolve()),
        knowledge_id=getattr(item, "knowledge_id", "") or "",
        knowledge_base_id=getattr(item, "knowledge_base_id", "") or "",
    )


def unique_tool_names(items: list[str]) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for item in items:
        if not item or item in seen:
            continue
        seen.add(item)
        out.append(item)
    return out


DOCUMENT_TEMPLATE_VARIABLES = {
    "document_template_context",
    "document_template_usage_rules",
    "word_template_requirement",
    "word_template_files",
    "excel_template_requirement",
    "excel_template_files",
    "pdf_template_requirement",
    "pdf_template_files",
    "ppt_template_requirement",
    "ppt_template_files",
}

DOCUMENT_TEMPLATE_DEFAULT_REFERENCE_LIMIT = 3
DOCUMENT_TEMPLATE_PPT_REFERENCE_LIMIT = 3
DATA_ANALYSIS_REFERENCE_VARIABLES = {
    "data_analysis_runtime_reference_path",
    "data_analysis_runtime_reference_absolute_path",
}
DATA_ANALYSIS_REFERENCE_SOURCE = Path(__file__).with_name("references") / "data_analysis_runtime_reference.md"


DOCUMENT_TEMPLATE_USAGE_RULES = """\
Template requirements describe configured formatting constraints; reference files are examples of structure and visual style. Use files relevant to the requested output format. For edits, preserve the original document except for requested changes. The user's explicit instructions take precedence. Missing template files are normal. Available libraries and file tools support generation without a prescribed renderer or intermediate specification. Use the current bytes and real page observations to assess the deliverable before registration.
""".strip()


DOCUMENT_TEMPLATE_CONTEXT_SYSTEM_POINTER = (
    "Document template context is provided once in the run prompt's "
    "<document_template_context> block near the <user_request>. Read that block and the files it lists."
)
DOCUMENT_TEMPLATE_USAGE_RULES_SYSTEM_POINTER = (
    "Document-template usage rules are provided in the run prompt's "
    "<document_template_context><usage_rules> block near the <user_request>."
)


def _strip_data_uri(value: str) -> str:
    raw = (value or "").strip()
    if "," in raw and "base64" in raw.split(",", 1)[0]:
        return raw.split(",", 1)[1].strip()
    return raw


def _decode_b64(value: str) -> bytes:
    raw = _strip_data_uri(value)
    return base64.b64decode(raw, validate=True)


def _relative_path(path: Path, root: Path) -> str:
    try:
        return path.relative_to(root).as_posix()
    except ValueError:
        return path.as_posix()


def _format_display_name(format_name: str) -> str:
    return {"word": "Word", "excel": "Excel", "pdf": "PDF", "ppt": "PPT"}.get(format_name, format_name or "unknown")


def _format_variable(format_name: str, role: str) -> str:
    if role == "requirement":
        return {
            "word": "word_template_requirement",
            "excel": "excel_template_requirement",
            "pdf": "pdf_template_requirement",
            "ppt": "ppt_template_requirement",
        }.get(format_name, "")
    return {
        "word": "word_template_files",
        "excel": "excel_template_files",
        "pdf": "pdf_template_files",
        "ppt": "ppt_template_files",
    }.get(format_name, "")


def _reference_file_limit(format_name: str) -> int:
    return DOCUMENT_TEMPLATE_PPT_REFERENCE_LIMIT if format_name == "ppt" else DOCUMENT_TEMPLATE_DEFAULT_REFERENCE_LIMIT


def prepare_document_template_context(payload: ChatPayload, run_dir: Path) -> PreparedDocumentTemplateContext:
    files = list(payload.document_template_context.files or [])
    cfg = payload.runtime_config
    if cfg.agent_type != "document-processing-agent":
        replacements = {name: "Document template context is not enabled for this agent type." for name in DOCUMENT_TEMPLATE_VARIABLES}
        return PreparedDocumentTemplateContext(xml="", replacements=replacements)

    template_root = run_dir / "document_templates"
    template_root.mkdir(parents=True, exist_ok=True)
    by_format: dict[str, dict[str, list[dict[str, str]]]] = {
        "word": {"requirement": [], "reference": []},
        "excel": {"requirement": [], "reference": []},
        "pdf": {"requirement": [], "reference": []},
        "ppt": {"requirement": [], "reference": []},
    }

    for index, item in enumerate(files, start=1):
        format_name = (item.format or "").strip().lower()
        role = (item.role or "").strip().lower()
        if format_name not in by_format or role not in {"requirement", "reference"}:
            continue
        filename = safe_filename(item.file_name) or f"{format_name}_{role}_{index}.{item.file_type or 'bin'}"
        target_dir = template_root / format_name / ("requirement" if role == "requirement" else "references")
        target_dir.mkdir(parents=True, exist_ok=True)
        if role == "reference":
            filename = f"{len(by_format[format_name][role]) + 1:02d}_{filename}"
        path = target_dir / filename
        try:
            data = _decode_b64(item.content_base64)
        except Exception:
            continue
        path.write_bytes(data)
        rel = _relative_path(path, run_dir)
        by_format[format_name][role].append(
            {
                "path": rel,
                "name": item.file_name or filename,
                "type": item.file_type or normalized_ext(filename),
                "source": item.source or "",
                "builtin_id": item.builtin_id or "",
                "size": str(len(data)),
                "variable": item.variable or _format_variable(format_name, role),
            }
        )

    replacements: dict[str, str] = {
        "document_template_usage_rules": DOCUMENT_TEMPLATE_USAGE_RULES,
    }
    lines: list[str] = []
    lines.append('<document_template_context source="WeKnora agent document template settings" role="format_requirements_and_soft_templates">')
    lines.append("<usage_rules>")
    lines.append(DOCUMENT_TEMPLATE_USAGE_RULES)
    lines.append("</usage_rules>")
    for format_name in ("word", "excel", "pdf", "ppt"):
        display = _format_display_name(format_name)
        requirement = by_format[format_name]["requirement"][:1]
        references = by_format[format_name]["reference"][:_reference_file_limit(format_name)]
        req_var = _format_variable(format_name, "requirement")
        ref_var = _format_variable(format_name, "reference")
        lines.append(f'<format name="{format_name}" display_name="{display}">')
        if requirement:
            req = requirement[0]
            req_summary = f"{display} template requirement file: {req['path']} (name={req['name']}, type={req['type']}, source={req['source'] or 'upload'})"
            replacements[req_var] = req_summary
            lines.append(
                f'<requirement_file variable="{{{{{req_var}}}}}" path="{req["path"]}" name="{req["name"]}" '
                f'type="{req["type"]}" source="{req["source"]}" builtin_id="{req["builtin_id"]}" size_bytes="{req["size"]}" />'
            )
        else:
            missing = f"No {display} template requirement file is configured. Use the prompt's document-quality fallback requirements for {display}."
            replacements[req_var] = missing
            lines.append(f'<requirement_file variable="{{{{{req_var}}}}}" present="false">{missing}</requirement_file>')
        if references:
            ref_lines = [f"{idx}. {ref['path']} (name={ref['name']}, type={ref['type']}, source={ref['source'] or 'upload'})" for idx, ref in enumerate(references, start=1)]
            replacements[ref_var] = "\n".join(ref_lines)
            lines.append(f'<reference_files variable="{{{{{ref_var}}}}}" count="{len(references)}">')
            for idx, ref in enumerate(references, start=1):
                lines.append(
                    f'<reference_file index="{idx}" path="{ref["path"]}" name="{ref["name"]}" type="{ref["type"]}" '
                    f'source="{ref["source"]}" size_bytes="{ref["size"]}" />'
                )
            lines.append("</reference_files>")
        else:
            missing = f"No {display} reference template files are configured. Treat this as normal and rely on the requirement file plus fallback rules."
            replacements[ref_var] = missing
            lines.append(f'<reference_files variable="{{{{{ref_var}}}}}" count="0">{missing}</reference_files>')
        lines.append("</format>")
    lines.append("</document_template_context>")
    xml = "\n".join(lines)
    replacements["document_template_context"] = xml
    return PreparedDocumentTemplateContext(xml=xml, replacements=replacements)


def replace_document_template_placeholders(
    text: str,
    prepared: PreparedDocumentTemplateContext | None,
    *,
    inline_context: bool = True,
) -> str:
    if not text:
        return text
    replacements = prepared.replacements if prepared else {}
    for key in DOCUMENT_TEMPLATE_VARIABLES:
        if not inline_context and key == "document_template_context":
            value = DOCUMENT_TEMPLATE_CONTEXT_SYSTEM_POINTER
        elif not inline_context and key == "document_template_usage_rules":
            value = DOCUMENT_TEMPLATE_USAGE_RULES_SYSTEM_POINTER
        else:
            value = replacements.get(key, f"{{{{{key}}}}}")
        text = text.replace("{{" + key + "}}", value)
    return text


def prompt_visible_context(raw: dict[str, Any]) -> dict[str, Any]:
    if not raw:
        return {}
    try:
        ctx = copy.deepcopy(raw)
    except Exception:
        ctx = dict(raw)
    agent = ctx.get("agent")
    if isinstance(agent, dict):
        agent.pop("system_prompt", None)
    current_turn = ctx.get("current_turn")
    if isinstance(current_turn, dict):
        current_turn.pop("user_request_verbatim", None)
        current_turn.pop("image_urls", None)
    effective = ctx.get("effective_configuration")
    if isinstance(effective, dict):
        effective.pop("allowed_tools", None)
        effective.pop("artifact_return_policy", None)
    return ctx


KNOWLEDGE_MANAGER_WORKSPACE_README = """\
# WeKnora knowledge-management workspace

This directory contains optional, read-only preparation helpers for the `knowledge-base-manager` agent type.

- `inspect_candidate.py FILE` prints a JSON profile (name, extension, MIME guess, byte size and SHA-256) for any candidate format. For plain-text formats it also reports text statistics. It never uploads or mutates a knowledge base.
- `compare_text.py OLD NEW [--diff-output FILE]` compares two UTF-8 text exports and optionally writes a unified diff. Use document libraries/converters already available in the runtime when comparing Word, PDF, Excel, PowerPoint or other binary formats.
- `operation_plan.template.json` is an optional scratch template for recording intended targets before tool calls. It is not an authorization artifact and passing it to a tool grants nothing.

Only WeKnora management tools perform mutations. Native ingestion accepts all formats supported by the selected knowledge base. Mutation inputs must be a current-turn input-file id or a `create_artifact` file token, never a filesystem path or URL.
"""


KNOWLEDGE_MANAGER_INSPECT_SCRIPT = r'''#!/usr/bin/env python3
from __future__ import annotations

import argparse
import hashlib
import json
import mimetypes
from pathlib import Path

TEXT_EXTENSIONS = {".txt", ".md", ".markdown", ".csv", ".tsv", ".json", ".jsonl", ".xml", ".html", ".htm", ".yaml", ".yml", ".log"}


def main() -> None:
    parser = argparse.ArgumentParser(description="Profile a candidate knowledge document without mutating WeKnora.")
    parser.add_argument("file")
    args = parser.parse_args()
    path = Path(args.file).expanduser().resolve()
    if not path.is_file():
        raise SystemExit(f"not a regular file: {path}")
    data = path.read_bytes()
    result = {
        "file_name": path.name,
        "extension": path.suffix.lower().lstrip("."),
        "mime_type_guess": mimetypes.guess_type(path.name)[0] or "application/octet-stream",
        "file_size": len(data),
        "sha256": hashlib.sha256(data).hexdigest(),
        "empty": len(data) == 0,
    }
    if path.suffix.lower() in TEXT_EXTENSIONS:
        text = data.decode("utf-8-sig", errors="replace")
        result.update({
            "text_characters": len(text),
            "text_lines": len(text.splitlines()),
            "replacement_characters": text.count("\ufffd"),
        })
    print(json.dumps(result, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
'''


KNOWLEDGE_MANAGER_COMPARE_SCRIPT = r'''#!/usr/bin/env python3
from __future__ import annotations

import argparse
import difflib
import json
from pathlib import Path


def read_text(path: Path) -> str:
    if not path.is_file():
        raise SystemExit(f"not a regular file: {path}")
    return path.read_text(encoding="utf-8-sig", errors="replace")


def main() -> None:
    parser = argparse.ArgumentParser(description="Compare two UTF-8 text representations without mutating WeKnora.")
    parser.add_argument("old")
    parser.add_argument("new")
    parser.add_argument("--diff-output", default="")
    args = parser.parse_args()
    old_path = Path(args.old).expanduser().resolve()
    new_path = Path(args.new).expanduser().resolve()
    old = read_text(old_path)
    new = read_text(new_path)
    diff_lines = list(difflib.unified_diff(
        old.splitlines(), new.splitlines(), fromfile=old_path.name, tofile=new_path.name, lineterm=""
    ))
    if args.diff_output:
        output = Path(args.diff_output).expanduser().resolve()
        output.parent.mkdir(parents=True, exist_ok=True)
        output.write_text("\n".join(diff_lines) + ("\n" if diff_lines else ""), encoding="utf-8")
    print(json.dumps({
        "old_file": old_path.name,
        "new_file": new_path.name,
        "identical": old == new,
        "similarity": round(difflib.SequenceMatcher(None, old, new).ratio(), 6),
        "old_lines": len(old.splitlines()),
        "new_lines": len(new.splitlines()),
        "diff_lines": len(diff_lines),
        "diff_output": str(Path(args.diff_output).resolve()) if args.diff_output else "",
    }, ensure_ascii=False, indent=2))


if __name__ == "__main__":
    main()
'''


def prepare_knowledge_manager_workspace(payload: ChatPayload, run_dir: Path) -> str:
    if payload.runtime_config.agent_type != "knowledge-base-manager":
        return ""
    root = run_dir / "generated" / "kb_manager"
    root.mkdir(parents=True, exist_ok=True)
    (root / "README.md").write_text(KNOWLEDGE_MANAGER_WORKSPACE_README, encoding="utf-8")
    (root / "inspect_candidate.py").write_text(KNOWLEDGE_MANAGER_INSPECT_SCRIPT, encoding="utf-8")
    (root / "compare_text.py").write_text(KNOWLEDGE_MANAGER_COMPARE_SCRIPT, encoding="utf-8")
    (root / "operation_plan.template.json").write_text(
        json.dumps(
            {
                "intent": "inspect|add|replace|delete",
                "knowledge_base_name": "",
                "old_document_name": "",
                "candidate_file_name": "",
                "evidence": [],
                "notes": "Scratch planning only; runtime scope and tools remain authoritative.",
            },
            ensure_ascii=False,
            indent=2,
        )
        + "\n",
        encoding="utf-8",
    )
    return _relative_path(root / "README.md", run_dir)


def prepare_data_analysis_reference_doc(payload: ChatPayload, run_dir: Path) -> PreparedDataAnalysisReferenceDoc | None:
    if not is_structured_analysis_payload(payload):
        return None
    if not DATA_ANALYSIS_REFERENCE_SOURCE.is_file():
        return None
    target_dir = run_dir / "generated" / "data_analysis"
    target_dir.mkdir(parents=True, exist_ok=True)
    target = target_dir / "runtime_reference.md"
    target.write_text(DATA_ANALYSIS_REFERENCE_SOURCE.read_text(encoding="utf-8"), encoding="utf-8")
    return PreparedDataAnalysisReferenceDoc(
        path=_relative_path(target, run_dir),
        absolute_path=str(target),
    )


def replace_data_analysis_reference_placeholders(text: str, prepared: PreparedDataAnalysisReferenceDoc | None) -> str:
    if not text:
        return text
    replacements = {
        "data_analysis_runtime_reference_path": prepared.path if prepared else "Data analysis runtime reference is not available.",
        "data_analysis_runtime_reference_absolute_path": prepared.absolute_path if prepared else "Data analysis runtime reference is not available.",
    }
    for key in DATA_ANALYSIS_REFERENCE_VARIABLES:
        text = text.replace("{{" + key + "}}", replacements.get(key, f"{{{{{key}}}}}"))
    return text


def build_system_prompt(
    payload: ChatPayload,
    document_templates: PreparedDocumentTemplateContext | None = None,
    data_analysis_reference: PreparedDataAnalysisReferenceDoc | None = None,
) -> str:
    base = replace_document_template_placeholders((payload.system_prompt or "").strip(), document_templates, inline_context=False)
    base = replace_data_analysis_reference_placeholders(base, data_analysis_reference)
    catalog = [{"key":x.key,"name":x.name,"description":x.description,"selected_by_user":x.selected_by_user} for x in payload.lightweight_skills]
    professional = [{"name":x.name,"description":x.description,"path":f".claude/skills/{x.name}/SKILL.md"} for x in payload.professional_skills]
    # The platform supplies the shared conversation/evidence/protocol contract.
    # This adapter adds only its workspace and skill capabilities.
    policy = """Files and commands use this run's workspace. Use original files for transformations and versioned updates. Generated visual files are rendered and inspected with the workspace tools; page observations inform your judgment, not an automatic approval. Register requested output files after checking them against the request.
Skills are scoped capabilities: read the relevant lightweight body with read_skill or a professional SKILL.md with read_file when needed. Template requirements and reference files are supplied as paths; use applicable templates for the requested format."""
    return "\n\n".join(part for part in (base, policy,
        "<skill_catalog>"+json.dumps(catalog,ensure_ascii=False)+"</skill_catalog>",
        "<professional_skills>"+json.dumps(professional,ensure_ascii=False)+"</professional_skills>") if part)


def current_user_turn_source_id(payload: ChatPayload) -> str:
    return f"user_message_{payload.user_message_id}" if payload.user_message_id else "current_user_message"


@lru_cache(maxsize=1)
def runtime_environment_facts() -> dict[str, Any]:
    """Expose installed execution capabilities once, independent of the task."""
    modules = [name for name in ("docx", "pptx", "openpyxl", "pypdf", "fitz", "PIL", "pandas", "matplotlib") if importlib.util.find_spec(name) is not None]
    commands = {name: path for name in ("python3", "libreoffice", "pdftoppm", "pdftotext", "fc-list") if (path := shutil.which(name))}
    fonts: list[str] = []
    if "fc-list" in commands:
        result = subprocess.run([commands["fc-list"], "--format=%{family}\n"], capture_output=True, text=True, timeout=10, check=True)
        fonts = sorted(set(result.stdout.splitlines()))
    return {"python_executable":sys.executable, "python_modules":modules, "commands":commands, "font_families":fonts}


def build_prompt(
    payload: ChatPayload,
    document_templates: PreparedDocumentTemplateContext | None = None,
    working_directory: str = "",
) -> str:
    parts: list[str] = []
    current_source_id = current_user_turn_source_id(payload)
    parts.append(
        f'<user_request verbatim="true" priority="highest" source_id="{current_source_id}" '
        'authority="current_user">'
    )
    parts.append(payload.query)
    parts.append("</user_request>")
    if payload.enable_artifacts:
        environment = dict(runtime_environment_facts())
        if working_directory:
            environment["working_directory"] = working_directory
        environment["chat_model_supports_images"] = payload.llm.supports_vision
        parts.append("<runtime_environment source=\"installed capabilities\">" + json.dumps(environment, ensure_ascii=False) + "</runtime_environment>")
    if document_templates and document_templates.xml:
        parts.append(document_templates.xml)
    parts.append("<weknora_context>")
    parts.append(
        "This payload comes from the WeKnora frontend and agent configuration. "
        "Only the <user_request verbatim=\"true\" priority=\"highest\"> block is the user's exact current chat input; "
        "all other blocks are contextual information with their own source labels."
    )
    from .working_context import visible_context_view
    visible_context = visible_context_view(prompt_visible_context(payload.visible_context), working_directory)
    if visible_context:
        parts.append('<visible_context source="WeKnora frontend-visible state and effective agent configuration" role="user_visible_context">')
        parts.append(json.dumps(visible_context, ensure_ascii=False, separators=(",", ":")))
        parts.append("</visible_context>")
    if payload.history and payload.llm.runtime_adapter == "claude-sdk":
        parts.append('<conversation_history source="WeKnora session history" role="background_context">')
        for msg in payload.history:
            source_attr = f" source_id={json.dumps(msg.source_id)}" if msg.source_id else ""
            authority = (
                "user_authored_fact_source"
                if msg.role == "user"
                else "non_factual_unless_later_user_confirmed"
            )
            parts.append(
                f"<message role={json.dumps(msg.role)}{source_attr} authority={json.dumps(authority)}>"
            )
            if msg.mentioned_items:
                parts.append("<visible_mentions>")
                parts.append(json.dumps(msg.mentioned_items, ensure_ascii=False))
                parts.append("</visible_mentions>")
            if msg.images:
                parts.append("<visible_images>")
                parts.append(json.dumps([img.model_dump() for img in msg.images], ensure_ascii=False))
                parts.append("</visible_images>")
            if msg.attachments:
                parts.append("<visible_attachments>")
                parts.append(json.dumps([att.model_dump(exclude={"content"}) for att in msg.attachments], ensure_ascii=False))
                parts.append("</visible_attachments>")
            parts.append(msg.content)
            parts.append("</message>")
        parts.append("</conversation_history>")
    if payload.quoted_context:
        parts.append('<quoted_context source="WeKnora quote reply" role="reference_context">')
        parts.append(payload.quoted_context)
        parts.append("</quoted_context>")
    if payload.image_description:
        parts.append('<image_description source="WeKnora image analysis" role="derived_visual_context">')
        parts.append(payload.image_description)
        parts.append("</image_description>")
    if payload.attachments:
        parts.append('<attachments source="WeKnora uploaded files" role="file_context">')
        for att in payload.attachments:
            parts.append(
                f"<attachment name={json.dumps(att.file_name)} type={json.dumps(att.file_type)} "
                f"size_bytes={att.file_size} extracted_text_available={json.dumps(bool(att.content))}>"
            )
            if att.content:
                parts.append(att.content)
                if att.is_truncated:
                    parts.append("[attachment content truncated]")
            else:
                parts.append("[no extracted text available]")
            parts.append("</attachment>")
        parts.append("</attachments>")
    if payload.image_urls:
        parts.append("<image_urls>")
        for url in payload.image_urls:
            parts.append(prompt_media_reference(url))
        parts.append("</image_urls>")
    parts.append("</weknora_context>")
    return "\n".join(parts)


def build_prompt_observation(payload: ChatPayload, rendered_prompt: str) -> dict[str, Any]:
    if not payload.eval_observability:
        return {}
    history_role_counts: dict[str, int] = {}
    history_role_chars: dict[str, int] = {}
    for message in payload.history:
        history_role_counts[message.role] = history_role_counts.get(message.role, 0) + 1
        history_role_chars[message.role] = history_role_chars.get(message.role, 0) + len(message.content)
    return {
        "eval_only": True,
        "configured_history_rounds": payload.runtime_config.history_turns,
        "actual_history_messages": len(payload.history),
        "history_message_count_by_role": history_role_counts,
        "history_message_chars_by_role": history_role_chars,
        "current_query_chars": len(payload.query),
        "system_prompt_chars": len(payload.system_prompt),
        "quoted_context_chars": len(payload.quoted_context),
        "attachment_count": len(payload.attachments),
        "original_input_file_count": len(payload.original_input_files),
        "tool_count": len(payload.tools),
        "rendered_prompt_chars": len(rendered_prompt),
    }


@dataclass(frozen=True)
class ToolUseFragment:
    tool_use_id: str
    name: str
    input: Any


@dataclass(frozen=True)
class ToolResultFragment:
    tool_use_id: str
    is_error: bool
    content: str = ""


@dataclass(frozen=True)
class PreparedDocumentTemplateContext:
    xml: str
    replacements: dict[str, str]


@dataclass(frozen=True)
class PreparedDataAnalysisReferenceDoc:
    path: str
    absolute_path: str


def block_value(block: Any, name: str, default: Any = None) -> Any:
    if isinstance(block, dict):
        return block.get(name, default)
    return getattr(block, name, default)


def block_type(block: Any) -> str:
    if isinstance(block, dict):
        return str(block.get("type") or "")
    return block.__class__.__name__


def tool_use_fragments(message: Any) -> list[ToolUseFragment]:
    out: list[ToolUseFragment] = []
    content = getattr(message, "content", None)
    if isinstance(content, list):
        for block in content:
            block_kind = block_type(block)
            if block_kind == "ToolUseBlock" or block_value(block, "type") == "tool_use":
                out.append(
                    ToolUseFragment(
                        tool_use_id=str(block_value(block, "id", "") or ""),
                        name=str(block_value(block, "name", "") or ""),
                        input=block_value(block, "input", {}) or {},
                    )
                )
    return out


def tool_result_fragments(message: Any) -> list[ToolResultFragment]:
    out: list[ToolResultFragment] = []
    content = getattr(message, "content", None)
    if isinstance(content, list):
        for block in content:
            block_kind = block_type(block)
            if block_kind != "ToolResultBlock" and block_value(block, "type") != "tool_result":
                continue
            result_content = block_value(block, "content", "")
            if isinstance(result_content, str):
                result_text = result_content
            else:
                try:
                    result_text = json.dumps(result_content, ensure_ascii=False, default=str)
                except TypeError:
                    result_text = str(result_content)
            out.append(
                ToolResultFragment(
                    tool_use_id=str(block_value(block, "tool_use_id", "") or ""),
                    is_error=bool(block_value(block, "is_error", False))
                    or bool(re.search(r"\bExit code\s+[1-9]\d*\b", result_text)),
                    content=result_text,
                )
            )
    return out


DATA_ANALYSIS_AGENT_TYPE = "data-analysis"
TABLE_ANALYSIS_AGENT_TYPE = "table-analysis"
STRUCTURED_ANALYSIS_AGENT_TYPES = {DATA_ANALYSIS_AGENT_TYPE, TABLE_ANALYSIS_AGENT_TYPE}


def is_structured_analysis_payload(payload: ChatPayload) -> bool:
    return payload.runtime_config.agent_type in STRUCTURED_ANALYSIS_AGENT_TYPES


MAX_TURNS_USER_MESSAGE = "任务过于复杂，请将任务拆分为具体子任务逐个执行，或提高智能体最大迭代次数"
TIMEOUT_USER_MESSAGE = "任务耗时过长，请将任务拆分为具体子任务逐个执行，或提高智能体LLM调用超时时间"


def raw_sdk_error_text(error: Any) -> str:
    parts: list[str] = []
    if error is not None and error.__class__.__name__ != "ResultMessage":
        parts.append(str(error))
    for attr in ("subtype", "stop_reason", "result", "api_error_status"):
        value = getattr(error, attr, None)
        if value is not None:
            parts.append(str(value))
    errors = getattr(error, "errors", None)
    if isinstance(errors, list):
        parts.extend(str(item) for item in errors if item is not None)
    return " ".join(parts).strip()


def user_facing_error_message(error: Any) -> str:
    raw = raw_sdk_error_text(error)
    lowered = raw.lower()
    if any(token in lowered for token in ("max_turn", "max turns", "maxturns", "turncount")):
        return MAX_TURNS_USER_MESSAGE
    if any(token in lowered for token in ("timeout", "timed out", "deadline exceeded", "api_timeout_ms")):
        return TIMEOUT_USER_MESSAGE
    if any(token in lowered for token in ("private artifact upload", "artifact persistence metadata")):
        return "智能体产物保存失败，本次任务未完成，请稍后重试"
    if "without a valid terminal answer" in lowered:
        return "模型未提供可明确交付的最终正文，本次回答未完成。"
    return raw or "General agent runtime returned an error"


class GeneralAgentRunner:
    def __init__(self, run_root: Path, payload: ChatPayload) -> None:
        self.payload = payload
        self.run_dir = run_root / payload.run_id
        self.run_dir.mkdir(parents=True, exist_ok=True)
        self.config_dir = self.run_dir / "claude-config"
        self.config_dir.mkdir(parents=True, exist_ok=True)
        self.artifacts = ArtifactStore(self.run_dir, payload, upload_artifact_before_completion)
        self.knowledge_manager_workspace = prepare_knowledge_manager_workspace(payload, self.run_dir)
        self.document_templates = prepare_document_template_context(payload, self.run_dir)
        self.data_analysis_reference = prepare_data_analysis_reference_doc(payload, self.run_dir)
        self.professional_skill_names = materialize_professional_skills(payload, self.run_dir)

    def cleanup(self) -> None:
        root = self.run_dir.parent.resolve()
        target = self.run_dir.resolve()
        if target != root and root in target.parents:
            shutil.rmtree(target, ignore_errors=True)

    async def run(self) -> AsyncIterator[RunEvent]:
        from .runtime_tools import RuntimeTools
        from .platform_runtime import run_platform
        from .sdk_runtime import run_sdk
        status_id = "runtime-status-" + self.payload.run_id
        status_data = {"progress_kind": "assistant_status", "progress_id": status_id,
            "phase": "start", "transient": True, "answer_contract": "runtime-terminal-v1"}
        yield RunEvent(type="progress", id=status_id, content="正在准备任务上下文", data=status_data)
        tools = RuntimeTools(self.payload, self.artifacts)
        system_prompt = build_system_prompt(self.payload, self.document_templates, self.data_analysis_reference)
        prompt = build_prompt(self.payload, self.document_templates,
            working_directory=str(self.run_dir.resolve()))
        prompt += tools.originals.prompt()
        observation = build_prompt_observation(self.payload, prompt)
        observation["effective_system_prompt_chars"] = len(system_prompt)
        if self.payload.llm.runtime_adapter == "platform":
            stream = run_platform(self.payload, system_prompt, prompt, tools, observation)
        elif self.payload.llm.runtime_adapter == "claude-sdk":
            stream = run_sdk(self.payload, system_prompt, prompt, tools, observation, self.config_dir)
        else:
            raise ValueError("unsupported runtime adapter: " + self.payload.llm.runtime_adapter)
        emitted = ""
        yield RunEvent(type="progress", id=status_id, content="正在分析上下文和可用工具", data=status_data)
        waiting_for_first_action = True
        async for evt in stream:
            if waiting_for_first_action and evt.type in {"progress", "answer_delta", "terminal_answer"}:
                waiting_for_first_action = False
                yield RunEvent(type="progress", id=status_id, content="已完成初始分析", done=True,
                    data={**status_data, "phase": "success"})
            if evt.type == "terminal_answer":
                emitted = evt.content
            else:
                yield evt
        if not emitted.strip():
            raise RuntimeError("runtime ended without a terminal answer")
        # create_artifact returns only after durable storage. Its actual result
        # is already visible to the model and UI; completion adds no hidden I/O.
        persisted_artifacts = self.artifacts.finalize_for_result()
        yield RunEvent(type="result", data=ChatResult(
            run_id=self.payload.run_id, answer=emitted, artifacts=persisted_artifacts,
            artifact_notice=self.artifacts.notice, artifact_original_count=self.artifacts.original_count,
            artifact_returned_count=self.artifacts.returned_count, artifact_dropped_count=self.artifacts.dropped_count,
            artifact_returned_size=self.artifacts.returned_size, artifact_limit_bytes=ARTIFACT_RETURN_LIMIT_BYTES,
            prompt_observation=observation,
        ).model_dump())


def read_artifact(run_root: Path, run_id: str, token: str) -> tuple[Path, bytes]:
    if not SAFE_ID_RE.fullmatch(run_id or "") or not SAFE_ID_RE.fullmatch(token or ""):
        raise FileNotFoundError(token)
    root = run_root.resolve()
    path = (root / run_id / "artifacts" / token).resolve()
    if root not in path.parents:
        raise FileNotFoundError(token)
    if not path.is_file():
        raise FileNotFoundError(token)
    return path, path.read_bytes()
