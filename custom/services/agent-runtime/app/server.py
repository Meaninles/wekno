"""Stateless worker processes claim durable runs; HTTP clients never own them."""
from __future__ import annotations

import asyncio
import contextlib
import logging
import os
import socket
import time
import uuid
import traceback
from contextlib import asynccontextmanager

import httpx
from fastapi import FastAPI

from .contracts import RunRequest
from .control import Control, ControlUnavailable, control_cause
from .models import model_for
from .run import execute

log = logging.getLogger("agent-runtime")
worker_contact: dict[int, float] = {}
claim_lock = asyncio.Lock()


def exception_frames(exc):
    # Types and code locations are useful operational diagnostics without
    # printing prompts, provider bodies or authentication material.
    return {"type":type(exc).__name__,
            "frames":[f"{f.filename}:{f.lineno}:{f.name}" for f in traceback.extract_tb(exc.__traceback__)],
            "children":[exception_frames(e) for e in getattr(exc,"exceptions",[])]}


async def heartbeat(control: Control, owner: asyncio.Task):
    try:
        while True:
            await asyncio.sleep(5)
            await control.post("runs/heartbeat")
    except BaseException:
        if not asyncio.current_task().cancelling():
            owner.cancel("Run ownership heartbeat failed")
        raise


async def handle(payload: RunRequest, client: httpx.AsyncClient):
    control = Control(payload, client)
    beat = asyncio.create_task(heartbeat(control, asyncio.current_task()))
    try:
        from .workspace import workspace_for
        from .artifact_delivery import finalize, start_finalization
        async with workspace_for(payload, control) as workspace:
            if payload.finalization is None:
                try:
                    async with model_for(payload, control) as model:
                        candidate = await execute(payload, control, model, extra_tools=workspace.tools,
                            skills=workspace.skills, offloader=workspace.offloader)
                except ControlUnavailable:
                    raise
                except Exception as exc:
                    if isinstance(control_cause(exc), ControlUnavailable):
                        raise control_cause(exc)
                    log.error("run=%s epoch=%s phase=execution failed=%s", payload.run_id,
                              payload.owner_epoch, exception_frames(exc))
                    await start_finalization(payload, control, error=exc)
                else:
                    await start_finalization(payload, control, result=candidate)
            # No model or SDK invocation exists in this phase or its recovery.
            async with asyncio.timeout(max(1, payload.deadline_unix - time.time())):
                await finalize(payload, control, workspace)
    except asyncio.CancelledError:
        # Shutdown and ownership loss leave the durable run recoverable. A
        # user stop has already changed its authoritative row to cancelled.
        raise
    except ControlUnavailable:
        # The lease expires and a worker resumes the last durable boundary.
        log.warning("run=%s control_unavailable; awaiting recovery", payload.run_id)
    except Exception as exc:
        exc = control_cause(exc) or exc
        if isinstance(exc, ControlUnavailable):
            log.warning("run=%s control_unavailable; awaiting recovery", payload.run_id)
            return
        # Do not log prompt content, credentials or provider response bodies.
        log.error("run=%s epoch=%s failed=%s", payload.run_id, payload.owner_epoch, exception_frames(exc))
        if payload.finalization is not None:
            # Keep its manifest, files and candidate for code-only recovery.
            return
        with contextlib.suppress(BaseException):
            async with asyncio.timeout(10):
                from .failures import error_code
                await control.post("runs/fail", error=f"Runtime failed: {type(exc).__name__}",
                                   error_code=error_code(exc))
        raise
    finally:
        beat.cancel()
        with contextlib.suppress(Exception, asyncio.CancelledError):
            await beat


async def worker(index: int):
    base = os.environ["AGENT_RUNTIME_CONTROL_URL"].rstrip("/")
    key = os.environ["AGENT_RUNTIME_API_KEY"]
    identity = f"{socket.gethostname()}-{index}-{uuid.uuid4().hex}"
    # Tool calls may wait on a user approval. Each run and operation retains
    # its explicit deadline; heartbeat and admission use separate connections.
    async with httpx.AsyncClient(timeout=httpx.Timeout(7200,connect=10,pool=10),
                                 limits=httpx.Limits(max_connections=64,max_keepalive_connections=32)) as client:
        while True:
            try:
                async with claim_lock:
                    response = await client.post(base + "/runs/claim", json={"worker_id":identity},
                                                 headers={"Authorization":"Bearer "+key}, timeout=15)
                    response.raise_for_status()
                    worker_contact[index] = time.monotonic()
                    data = response.json().get("run")
                    if data is None:
                        await asyncio.sleep(.1)
                        continue
                try:
                    payload = RunRequest.model_validate(data)
                except ValueError:
                    await client.post(base + "/runs/fail", json={
                        "run_id": data["run_id"], "owner_epoch": data["owner_epoch"],
                        "error": "Invalid runtime request configuration", "error_code": "service_configuration"},
                        headers={"Authorization": "Bearer " + key}, timeout=15)
                    continue
                # Distinct task: SDK cancellation must end this run without
                # stopping a healthy worker from claiming unrelated work.
                task = asyncio.create_task(handle(payload, client))
                try:
                    await task
                except asyncio.CancelledError:
                    if asyncio.current_task().cancelling():
                        task.cancel()
                        raise
                except Exception:
                    pass
            except asyncio.CancelledError:
                raise
            except Exception as exc:
                worker_contact.pop(index, None)
                status = exc.response.status_code if isinstance(exc, httpx.HTTPStatusError) else None
                log.error("worker=%s claim_failed=%s status=%s", index, type(exc).__name__, status)
                await asyncio.sleep(5)


@asynccontextmanager
async def lifespan(app: FastAPI):
    for key in ("AGENT_RUNTIME_CONTROL_URL","AGENT_RUNTIME_API_KEY"):
        if not os.environ.get(key):
            raise RuntimeError(f"{key} is required")
    count = int(os.environ.get("AGENT_RUNTIME_CONCURRENCY", "8"))
    if not 1 <= count <= 128:
        raise ValueError("AGENT_RUNTIME_CONCURRENCY must be 1..128")
    tasks = [asyncio.create_task(worker(i)) for i in range(count)]
    app.state.workers = tasks
    from .housekeeping import housekeeping
    cleanup = asyncio.create_task(housekeeping())
    try:
        yield
    finally:
        for task in tasks:
            task.cancel()
        cleanup.cancel()
        await asyncio.gather(*tasks, cleanup, return_exceptions=True)


app = FastAPI(lifespan=lifespan, docs_url=None, redoc_url=None)


@app.get("/healthz")
async def health():
    return {"status":"ok", "harness":"agentscope", "protocol_version":1}


@app.get("/readyz")
async def ready():
    from fastapi.responses import JSONResponse
    alive = all(not task.done() for task in app.state.workers) and bool(worker_contact)
    return JSONResponse({"ready":alive},status_code=200 if alive else 503)
