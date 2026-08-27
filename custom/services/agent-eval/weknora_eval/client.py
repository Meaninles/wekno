from __future__ import annotations

import json
import time
import urllib.error
import urllib.request
from typing import Any, Callable


class WeKnoraAPIError(RuntimeError):
    pass


def unwrap_data(value: Any) -> Any:
    return value.get("data") if isinstance(value, dict) and "data" in value else value


def event_type(event: dict[str, Any]) -> str:
    return str(event.get("response_type") or event.get("type") or "")


def event_tool_name(event: dict[str, Any]) -> str:
    data = event.get("data") if isinstance(event.get("data"), dict) else {}
    return str(event.get("tool_name") or data.get("tool_name") or "").strip()


class WeKnoraClient:
    def __init__(self, base_url: str, api_key: str, timeout: float = 600.0) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def _headers(self, *, stream: bool = False) -> dict[str, str]:
        headers = {"X-API-Key": self.api_key, "Content-Type": "application/json"}
        if stream:
            headers["Accept"] = "text/event-stream"
        return headers

    def request(self, method: str, path: str, body: Any | None = None) -> Any:
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
        request = urllib.request.Request(
            self.base_url + path,
            data=data,
            method=method,
            headers=self._headers(),
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                raw = response.read()
        except urllib.error.HTTPError as exc:
            detail = exc.read()[:2000].decode("utf-8", errors="replace")
            raise WeKnoraAPIError(f"{method} {path} failed: HTTP {exc.code}: {detail}") from exc
        except urllib.error.URLError as exc:
            raise WeKnoraAPIError(f"{method} {path} failed: {exc}") from exc
        return json.loads(raw) if raw else None

    def stream(
        self,
        path: str,
        body: dict[str, Any],
        on_event: Callable[[dict[str, Any]], None] | None = None,
    ) -> tuple[list[dict[str, Any]], int, int]:
        request = urllib.request.Request(
            self.base_url + path,
            data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
            method="POST",
            headers=self._headers(stream=True),
        )
        started = time.perf_counter()
        first_event_at: float | None = None
        events: list[dict[str, Any]] = []
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                data_lines: list[str] = []
                while True:
                    raw = response.readline()
                    if not raw:
                        break
                    line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                    if line.startswith("data:"):
                        data_lines.append(line[5:].lstrip())
                        continue
                    if line or not data_lines:
                        continue
                    payload = "\n".join(data_lines)
                    data_lines = []
                    if payload == "[DONE]":
                        continue
                    event = json.loads(payload)
                    if first_event_at is None:
                        first_event_at = time.perf_counter()
                    events.append(event)
                    if on_event is not None:
                        on_event(event)
        except urllib.error.HTTPError as exc:
            detail = exc.read()[:2000].decode("utf-8", errors="replace")
            raise WeKnoraAPIError(f"POST {path} failed: HTTP {exc.code}: {detail}") from exc
        except (urllib.error.URLError, TimeoutError) as exc:
            raise WeKnoraAPIError(f"POST {path} failed: {exc}") from exc
        ended = time.perf_counter()
        first = first_event_at if first_event_at is not None else ended
        return events, round((first - started) * 1000), round((ended - started) * 1000)

    def capabilities(self) -> dict[str, Any]:
        payload = unwrap_data(self.request("GET", "/custom/agent-eval/capabilities"))
        if not isinstance(payload, dict):
            raise WeKnoraAPIError("agent-eval capabilities endpoint returned a non-object")
        return payload

    def create_session(self) -> str:
        payload = unwrap_data(self.request("POST", "/sessions", {"title": ""}))
        if not isinstance(payload, dict) or not payload.get("id"):
            raise WeKnoraAPIError("session create response has no id")
        return str(payload["id"])

    def load_completed_assistant(
        self,
        session_id: str,
        *,
        exclude_message_ids: set[str] | None = None,
        wait_seconds: float = 10.0,
    ) -> dict[str, Any]:
        excluded = exclude_message_ids or set()
        deadline = time.monotonic() + wait_seconds
        last: dict[str, Any] | None = None
        while True:
            payload = unwrap_data(self.request("GET", f"/messages/{session_id}/load?limit=200"))
            messages = payload if isinstance(payload, list) else []
            assistants = [
                message
                for message in messages
                if isinstance(message, dict)
                and message.get("role") == "assistant"
                and str(message.get("id") or "") not in excluded
            ]
            if assistants:
                last = max(assistants, key=lambda item: str(item.get("created_at") or item.get("id") or ""))
                if last.get("is_completed") and str(last.get("content") or "").strip():
                    return last
            if time.monotonic() >= deadline:
                if last is not None:
                    return last
                raise WeKnoraAPIError("no new persisted assistant message")
            time.sleep(0.2)
