from __future__ import annotations

import json
import os
from pathlib import Path
from urllib.parse import quote

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import Response, StreamingResponse
from pydantic import ValidationError

from .runner import GeneralAgentRunner, read_artifact, user_facing_error_message
from .schemas import ChatPayload, RunEvent


RUN_ROOT = Path(os.getenv("GENERAL_AGENT_RUN_ROOT", "/tmp/weknora-general-agent-runs"))
RUN_ROOT.mkdir(parents=True, exist_ok=True)
API_KEY = os.getenv("CUSTOM_GENERAL_AGENT_API_KEY", "")

app = FastAPI(title="WeKnora General Agent", version="1.0.0")


def authorize(authorization: str | None = Header(default=None), x_api_key: str | None = Header(default=None)) -> None:
    if not API_KEY:
        raise HTTPException(status_code=503, detail="CUSTOM_GENERAL_AGENT_API_KEY is not configured")
    token = (authorization or "").strip()
    if token.lower().startswith("bearer "):
        token = token[7:].strip()
    if token == API_KEY or (x_api_key or "").strip() == API_KEY:
        return
    raise HTTPException(status_code=401, detail="unauthorized")


@app.get("/health")
async def health():
    return {"ok": True}


@app.post("/v1/chat/stream")
async def chat_stream(request: Request, authorization: str | None = Header(default=None), x_api_key: str | None = Header(default=None)):
    authorize(authorization, x_api_key)
    try:
        payload = ChatPayload.model_validate(await request.json())
    except json.JSONDecodeError:
        raise HTTPException(status_code=400, detail="invalid json body")
    except ValidationError as exc:
        raise HTTPException(status_code=422, detail=exc.errors())
    runner = GeneralAgentRunner(RUN_ROOT, payload)

    async def generate():
        try:
            async for evt in runner.run():
                line = evt.model_dump_json(exclude_none=True)
                yield line + "\n"
        except Exception as exc:
            err = RunEvent(type="error", message=user_facing_error_message(exc))
            yield err.model_dump_json(exclude_none=True) + "\n"

    return StreamingResponse(generate(), media_type="application/x-ndjson")


@app.get("/v1/runs/{run_id}/files/{file_token}")
async def download(run_id: str, file_token: str, authorization: str | None = Header(default=None), x_api_key: str | None = Header(default=None)):
    authorize(authorization, x_api_key)
    try:
        path, data = read_artifact(RUN_ROOT, run_id, file_token)
    except FileNotFoundError:
        raise HTTPException(status_code=404, detail="file not found")
    meta_path = path.with_suffix(".json")
    content_type = "application/octet-stream"
    filename = file_token
    if meta_path.is_file():
        try:
            meta = json.loads(meta_path.read_text(encoding="utf-8"))
            content_type = meta.get("content_type") or content_type
            filename = meta.get("filename") or filename
        except Exception:
            pass
    ascii_fallback = "".join(ch if ord(ch) < 128 and ch not in {'"', "\\", ";"} else "_" for ch in filename).strip("_")
    if not ascii_fallback:
        ascii_fallback = "artifact"
    disposition = f"attachment; filename=\"{ascii_fallback}\"; filename*=UTF-8''{quote(filename, safe='')}"
    return Response(
        data,
        media_type=content_type,
        headers={"Content-Disposition": disposition},
    )
