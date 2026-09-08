"""Authenticated access to the Go-owned run, tool and event records."""
from __future__ import annotations

import asyncio
from typing import Any

import httpx

from .contracts import RunRequest


class ControlError(RuntimeError):
    def __init__(self, code: str, message: str):
        super().__init__(message)
        self.code = code


class ControlUnavailable(ControlError):
    pass


def control_cause(exc):
    seen = set()
    while exc is not None and id(exc) not in seen:
        seen.add(id(exc))
        if isinstance(exc, ControlError):
            return exc
        exc = exc.__cause__ or exc.__context__
    return None


class Control:
    def __init__(self, payload: RunRequest, client: httpx.AsyncClient):
        self.payload = payload
        self.client = client
        self.base_url = payload.tool_callback_url.rsplit("/tools/call", 1)[0]

    async def post(self, path: str, **data: Any) -> dict[str, Any]:
        for attempt in range(3):
            try:
                response = await self.client.post(
                    self.base_url + "/" + path,
                    headers={"Authorization": "Bearer " + self.payload.tool_callback_api_key},
                    json={**data, "run_id": self.payload.run_id, "owner_epoch": self.payload.owner_epoch},
                )
                if response.status_code not in (502, 503, 504):
                    break
            except httpx.TransportError:
                response = None
            # Tool execution has a persistent receipt but can still be in
            # flight. Recover through its SDK checkpoint, never a fresh call ID.
            if path == "tools/call" or attempt == 2:
                raise ControlUnavailable("control_unavailable", "Run service temporarily unavailable")
            await asyncio.sleep(.2 * (attempt+1))
        if response.status_code in (409, 410):
            # A stale worker must unwind the SDK, not feed a tool error to a
            # further reasoning round after losing execution ownership.
            raise asyncio.CancelledError("run ownership lost or run cancelled")
        if not response.is_success:
            try:
                problem = response.json()
            except ValueError:
                problem = {}
            raise ControlError(problem.get("code", "control_unavailable"),
                               problem.get("error", f"Run service returned HTTP {response.status_code}"))
        return response.json()

    async def checkpoint(self, state: dict[str, Any], *, boundary: str) -> None:
        await self.post("runs/checkpoint", checkpoint=state, boundary=boundary)

    async def budget(self, role="") -> dict[str, Any]:
        return await self.post("runs/budget", model_role=role)

    async def events(self, events: list[dict[str, Any]]) -> None:
        await self.post("runs/events", events=events)

    async def tool(self, name: str, arguments: dict[str, Any], call_id: str) -> dict[str, Any]:
        return await self.post("tools/call", tool_name=name, arguments=arguments, tool_call_id=call_id)

    async def validate(self, result: dict[str, Any]) -> dict[str, Any]:
        return await self.post("runs/validate", result=result)

    async def commit(self, result: dict[str, Any]) -> dict[str, Any]:
        return await self.post("runs/commit", result=result)
