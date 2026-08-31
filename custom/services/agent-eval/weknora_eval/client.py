from __future__ import annotations

import json
import threading
import time
import urllib.error
import urllib.request
from typing import Any, Callable


class WeKnoraAPIError(RuntimeError):
    pass


class WeKnoraResponseDeadlineExceeded(WeKnoraAPIError):
    """A valid observation that the SUT did not finish within the wall deadline."""

    def __init__(
        self,
        path: str,
        timeout_seconds: float,
        events: list[dict[str, Any]],
        ttfb_ms: int,
        total_latency_ms: int,
    ) -> None:
        super().__init__(
            f"POST {path} exceeded the {timeout_seconds:g}s total response deadline"
        )
        self.events = events
        self.ttfb_ms = ttfb_ms
        self.total_latency_ms = total_latency_ms


class WeKnoraAssistantPersistenceTimeout(WeKnoraAPIError):
    """The SUT stream ended but no assistant message became observable."""


def unwrap_data(value: Any) -> Any:
    return value.get("data") if isinstance(value, dict) and "data" in value else value


def event_type(event: dict[str, Any]) -> str:
    return str(event.get("response_type") or event.get("type") or "")


def event_tool_name(event: dict[str, Any]) -> str:
    data = event.get("data") if isinstance(event.get("data"), dict) else {}
    return str(event.get("tool_name") or data.get("tool_name") or "").strip()


def streamed_production_candidate(events: list[dict[str, Any]]) -> str | None:
    """Reconstruct the user-visible answer surface from canonical SSE events.

    Answer fragments are accumulated by event id. A later non-preserving tool
    call supersedes earlier answer fragments in the same way as WeKnora's
    stream handler; post-answer artifact calls explicitly preserve them. The
    completion payload is used only when no answer fragment was emitted.
    ``None`` means the supplied event list cannot prove an SSE answer surface.
    """

    segments: list[dict[str, Any]] = []
    positions: dict[str, int] = {}
    completion_fallback: str | None = None
    saw_answer = False
    for event in events:
        kind = event_type(event)
        data = event.get("data") if isinstance(event.get("data"), dict) else {}
        if kind == "answer":
            saw_answer = True
            event_id = str(
                event.get("id") or data.get("event_id") or f"answer-{len(segments)}"
            )
            index = positions.get(event_id)
            if index is None:
                index = len(segments)
                positions[event_id] = index
                segments.append({"content": "", "superseded": False})
            segments[index]["content"] += str(event.get("content") or "")
        elif kind == "tool_call" and not bool(data.get("preserve_answer")):
            for segment in segments:
                if segment["content"]:
                    segment["superseded"] = True
        elif kind == "complete":
            value = data.get("final_answer")
            if isinstance(value, str):
                completion_fallback = value
    if saw_answer:
        return "".join(
            str(segment["content"])
            for segment in segments
            if not segment["superseded"]
        )
    return completion_fallback


class WeKnoraClient:
    def __init__(self, base_url: str, api_key: str, timeout: float = 600.0) -> None:
        if timeout <= 0:
            raise ValueError("timeout must be greater than zero")
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
        deadline = started + self.timeout
        first_event_at: float | None = None
        events: list[dict[str, Any]] = []

        def deadline_error() -> WeKnoraResponseDeadlineExceeded:
            ended = time.perf_counter()
            first = first_event_at if first_event_at is not None else ended
            return WeKnoraResponseDeadlineExceeded(
                path,
                self.timeout,
                list(events),
                round((first - started) * 1000),
                round((ended - started) * 1000),
            )

        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                deadline_expired = threading.Event()

                def expire_response() -> None:
                    deadline_expired.set()
                    try:
                        response.close()
                    except Exception:
                        pass

                timer = threading.Timer(
                    max(0.001, deadline - time.perf_counter()), expire_response
                )
                timer.daemon = True
                timer.start()
                data_lines: list[str] = []
                try:
                    while True:
                        remaining = deadline - time.perf_counter()
                        if remaining <= 0 or deadline_expired.is_set():
                            raise deadline_error()

                        # urllib's timeout is otherwise only an inactivity
                        # timeout. Bound the next blocking read by the remaining
                        # wall-clock budget as well. The timer is a fallback for
                        # transports that do not expose their socket object.
                        fp = getattr(response, "fp", None)
                        raw_stream = getattr(fp, "raw", None)
                        sock = getattr(raw_stream, "_sock", None)
                        if sock is not None and callable(getattr(sock, "settimeout", None)):
                            sock.settimeout(max(0.001, remaining))

                        raw = response.readline()
                        if not raw:
                            if deadline_expired.is_set():
                                raise deadline_error()
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
                except (TimeoutError, OSError, ValueError) as exc:
                    if deadline_expired.is_set() or time.perf_counter() >= deadline:
                        raise deadline_error() from exc
                    raise
                finally:
                    timer.cancel()
        except WeKnoraResponseDeadlineExceeded:
            raise
        except urllib.error.HTTPError as exc:
            detail = exc.read()[:2000].decode("utf-8", errors="replace")
            raise WeKnoraAPIError(f"POST {path} failed: HTTP {exc.code}: {detail}") from exc
        except (urllib.error.URLError, TimeoutError, OSError, ValueError) as exc:
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

    def search_knowledge(
        self,
        query: str,
        *,
        knowledge_base_ids: list[str],
        knowledge_ids: list[str],
    ) -> list[dict[str, Any]]:
        """Run one read-only retrieval without creating a conversation turn."""

        payload = unwrap_data(
            self.request(
                "POST",
                "/knowledge-search",
                {
                    "query": query,
                    "knowledge_base_ids": knowledge_base_ids,
                    "knowledge_ids": knowledge_ids,
                },
            )
        )
        if not isinstance(payload, list):
            raise WeKnoraAPIError("knowledge search returned a non-list payload")
        return [item for item in payload if isinstance(item, dict)]

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
                raise WeKnoraAssistantPersistenceTimeout(
                    "no new persisted assistant message"
                )
            time.sleep(0.2)
