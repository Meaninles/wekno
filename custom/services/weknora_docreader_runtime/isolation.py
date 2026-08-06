"""Killable process isolation for untrusted document parsers.

Document parsers call native libraries, Java, LibreOffice and nested worker
processes.  A gRPC deadline can stop the caller, but it cannot stop a Python
thread blocked inside one of those components.  This module therefore runs one
document in its own OS process group and gives the parent a hard deadline.  On
timeout or client cancellation, the whole process group is terminated.
"""

from __future__ import annotations

import logging
import multiprocessing
import os
import pickle
import shutil
import signal
import tempfile
import time
import traceback
import uuid
from dataclasses import dataclass
from multiprocessing.connection import Connection
from pathlib import Path
from typing import Any, Callable, Optional

logger = logging.getLogger(__name__)
_SIGTERM = getattr(signal, "SIGTERM", 15)
_SIGKILL = getattr(signal, "SIGKILL", 9)


class ParseTimeoutError(TimeoutError):
    """The parser exceeded its DocReader-side hard deadline."""


class ParseCancelledError(RuntimeError):
    """The upstream gRPC request ended while parsing was still active."""


class ParseExecutionError(RuntimeError):
    """The isolated parser process failed."""


@dataclass
class _ChildMessage:
    kind: str
    detail: str = ""


def sys_platform_linux() -> bool:
    return os.name == "posix" and hasattr(os, "setsid")


def _start_process_group() -> None:
    if sys_platform_linux():
        os.setsid()


def _atomic_pickle(path: Path, value: Any) -> None:
    temporary = path.with_suffix(path.suffix + ".tmp")
    with temporary.open("wb") as stream:
        pickle.dump(value, stream, protocol=pickle.HIGHEST_PROTOCOL)
        stream.flush()
        os.fsync(stream.fileno())
    os.replace(temporary, path)


def _parse_worker(job_dir: str, status: Connection) -> None:
    """Child entry point. It must stay at module scope for ``spawn``."""

    _start_process_group()
    job_path = Path(job_dir)
    try:
        status.send(_ChildMessage("ready"))
        with (job_path / "request.pkl").open("rb") as stream:
            payload = pickle.load(stream)

        # Import inside the isolated process. This prevents parser/native
        # library state from leaking into the long-lived gRPC process.
        from docreader.parser import Parser
        from docreader.utils.request import request_id_context

        with request_id_context(payload["request_id"]):
            parser = Parser()
            if payload["mode"] == "url":
                document = parser.parse_url(
                    payload["url"],
                    payload["title"],
                    parser_engine=payload["parser_engine"],
                    engine_overrides=payload["engine_overrides"],
                )
            else:
                document = parser.parse_file(
                    payload["file_name"],
                    payload["file_type"],
                    payload["file_content"],
                    parser_engine=payload["parser_engine"],
                    engine_overrides=payload["engine_overrides"],
                )

        # Pydantic's Python dump preserves bytes and ordinary metadata without
        # inflating base64 images again. The filesystem avoids a large Pipe
        # message deadlocking a child while the parent waits for completion.
        _atomic_pickle(job_path / "result.pkl", document.model_dump(mode="python"))
        status.send(_ChildMessage("complete"))
    except BaseException as exc:
        detail = f"{type(exc).__name__}: {exc}\n{traceback.format_exc()}"
        try:
            status.send(_ChildMessage("error", detail))
        except Exception:
            pass
    finally:
        status.close()


def _signal_process_group(process: multiprocessing.Process, sig: int) -> None:
    if process.pid is None:
        return
    if sys_platform_linux():
        try:
            os.killpg(process.pid, sig)
            return
        except ProcessLookupError:
            return
        except OSError:
            # The child may have failed before setsid(). Fall back to the
            # direct process handle in that narrow startup race.
            pass
    try:
        if sig == _SIGKILL and hasattr(process, "kill"):
            process.kill()
        else:
            process.terminate()
    except (OSError, ProcessLookupError):
        pass


def _terminate_process_tree(
    process: multiprocessing.Process, grace_seconds: float
) -> None:
    """Terminate the parser and every descendant in its process group."""

    if not process.is_alive():
        # The direct child may have crashed after creating native/Java/Office
        # descendants. Its process group can still exist even though the
        # multiprocessing.Process itself has exited.
        if sys_platform_linux():
            _signal_process_group(process, _SIGKILL)
        process.join(timeout=0)
        return
    _signal_process_group(process, _SIGTERM)
    process.join(timeout=max(0.0, grace_seconds))
    if process.is_alive():
        _signal_process_group(process, _SIGKILL)
        process.join(timeout=max(1.0, grace_seconds))


class IsolatedParseRunner:
    """Execute one parser request in a killable, disposable process."""

    def __init__(
        self,
        *,
        timeout_seconds: float,
        kill_grace_seconds: float = 3.0,
        work_root: Optional[str] = None,
        process_target: Callable[[str, Connection], None] = _parse_worker,
        process_start_method: str = "spawn",
    ) -> None:
        if timeout_seconds <= 0:
            raise ValueError("timeout_seconds must be positive")
        self.timeout_seconds = float(timeout_seconds)
        self.kill_grace_seconds = max(0.0, float(kill_grace_seconds))
        self.work_root = work_root
        self.process_target = process_target
        self.context = multiprocessing.get_context(process_start_method)

    def parse_file(
        self,
        *,
        request_id: str,
        file_name: str,
        file_type: str,
        file_content: bytes,
        parser_engine: str,
        engine_overrides: dict[str, Any],
        is_active: Optional[Callable[[], bool]] = None,
    ):
        payload = {
            "mode": "file",
            "request_id": request_id,
            "file_name": file_name,
            "file_type": file_type,
            "file_content": bytes(file_content),
            "parser_engine": parser_engine,
            "engine_overrides": engine_overrides,
        }
        return self._run(payload, is_active=is_active)

    def parse_url(
        self,
        *,
        request_id: str,
        url: str,
        title: str,
        parser_engine: str,
        engine_overrides: dict[str, Any],
        is_active: Optional[Callable[[], bool]] = None,
    ):
        payload = {
            "mode": "url",
            "request_id": request_id,
            "url": url,
            "title": title,
            "parser_engine": parser_engine,
            "engine_overrides": engine_overrides,
        }
        return self._run(payload, is_active=is_active)

    def _run(
        self,
        payload: dict[str, Any],
        *,
        is_active: Optional[Callable[[], bool]],
    ):
        from docreader.models.document import Document

        root = self.work_root
        if root:
            os.makedirs(root, mode=0o700, exist_ok=True)
        job_dir = tempfile.mkdtemp(prefix="parse-", dir=root)
        os.chmod(job_dir, 0o700)
        job_path = Path(job_dir)
        _atomic_pickle(job_path / "request.pkl", payload)

        receive_status, send_status = self.context.Pipe(duplex=False)
        process = self.context.Process(
            target=self.process_target,
            args=(job_dir, send_status),
            name=f"docreader-parse-{uuid.uuid4().hex[:8]}",
        )
        deadline = time.monotonic() + self.timeout_seconds
        last_message: Optional[_ChildMessage] = None
        try:
            process.start()
            send_status.close()
            while True:
                if is_active is not None and not is_active():
                    logger.warning(
                        "Cancelling isolated parser pid=%s after upstream disconnect",
                        process.pid,
                    )
                    _terminate_process_tree(process, self.kill_grace_seconds)
                    raise ParseCancelledError("document parse cancelled by caller")

                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    logger.error(
                        "Hard timeout after %.1fs; terminating parser process group pid=%s",
                        self.timeout_seconds,
                        process.pid,
                    )
                    _terminate_process_tree(process, self.kill_grace_seconds)
                    raise ParseTimeoutError(
                        f"document parse exceeded hard timeout of "
                        f"{self.timeout_seconds:g} seconds"
                    )

                if receive_status.poll(min(0.1, remaining)):
                    try:
                        last_message = receive_status.recv()
                    except EOFError:
                        last_message = None
                    if last_message is not None and last_message.kind == "complete":
                        process.join(timeout=1.0)
                        with (job_path / "result.pkl").open("rb") as stream:
                            return Document.model_validate(pickle.load(stream))
                    if last_message is not None and last_message.kind == "error":
                        process.join(timeout=1.0)
                        logger.error(
                            "Isolated parser failed: %s", last_message.detail
                        )
                        raise ParseExecutionError(
                            last_message.detail.splitlines()[0]
                        )

                if not process.is_alive():
                    # Drain a final message sent immediately before exit.
                    if receive_status.poll(0):
                        try:
                            last_message = receive_status.recv()
                        except EOFError:
                            last_message = None
                        if last_message is not None and last_message.kind == "complete":
                            with (job_path / "result.pkl").open("rb") as stream:
                                return Document.model_validate(pickle.load(stream))
                        if last_message is not None and last_message.kind == "error":
                            logger.error(
                                "Isolated parser failed: %s", last_message.detail
                            )
                            raise ParseExecutionError(
                                last_message.detail.splitlines()[0]
                            )
                    raise ParseExecutionError(
                        f"isolated parser exited unexpectedly with code {process.exitcode}"
                    )
        finally:
            receive_status.close()
            send_status.close()
            if process.pid is not None:
                _terminate_process_tree(process, self.kill_grace_seconds)
            shutil.rmtree(job_dir, ignore_errors=True)
