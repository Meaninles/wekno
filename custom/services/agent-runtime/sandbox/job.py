"""Container-side execution receipts, process-group cancellation and bounded logs.

Runs as the container supervisor user; arbitrary commands run as uid 1000.
The private receipt directory is inaccessible to generated programs.
"""
import base64
import fcntl
import hashlib
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time

LIMIT = 1024 * 1024


def atomic_json(path, value):
    temporary = path.with_suffix(".tmp")
    with temporary.open("w") as stream:
        json.dump(value, stream)
        stream.flush()
        os.fsync(stream.fileno())
    temporary.replace(path)
    descriptor = os.open(path.parent, os.O_DIRECTORY)
    try:
        os.fsync(descriptor)
    finally:
        os.close(descriptor)


def as_user():
    os.setgroups([])
    os.setgid(1000)
    os.setuid(1000)
    os.umask(0o077)


def main():
    request = json.loads(base64.b64decode(sys.argv[1]))
    root = Path("/control")
    root.mkdir(exist_ok=True, mode=0o700)
    with (root / "epoch.lock").open("a+") as epoch_lock:
        fcntl.flock(epoch_lock, fcntl.LOCK_EX)
        epoch_path = root / "epoch"
        epoch = int(epoch_path.read_text()) if epoch_path.exists() else 0
        if request["epoch"] < epoch:
            raise RuntimeError("Stale workspace owner")
        epoch_path.write_text(str(request["epoch"]))
    identity = hashlib.sha256(request["id"].encode()).hexdigest()
    folder = root / identity
    folder.mkdir(exist_ok=True, mode=0o700)
    with (folder / "lock").open("a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        result_path = folder / "result.json"
        if result_path.exists():
            print(result_path.read_text())
            return
        if (folder / "started").exists():
            raise RuntimeError("Execution was interrupted; its side-effect outcome is uncertain")
        atomic_json(folder / "started", {"time":time.time()})
        stdout_path, stderr_path = folder / "stdout", folder / "stderr"
        with stdout_path.open("wb") as stdout, stderr_path.open("wb") as stderr:
            command = request["command"]
            if isinstance(command, str):
                command = ["/bin/bash", "-lc", command]
            process = subprocess.Popen(command, cwd=request.get("cwd") or "/workspace",
                                       stdout=subprocess.PIPE, stderr=subprocess.PIPE, start_new_session=True,
                                       preexec_fn=as_user, env={**os.environ,"HOME":"/workspace","TMPDIR":"/tmp"})
            def drain(source, destination):
                remaining = LIMIT
                truncated = False
                with source:
                    while chunk := source.read(65536):
                        destination.write(chunk[:remaining])
                        if len(chunk) > remaining:
                            truncated = True
                        remaining = max(0, remaining - len(chunk))
                if truncated:
                    destination.write(b"\n[output truncated; save detailed results to a workspace file]")

            drains = [threading.Thread(target=drain, args=(source, target), daemon=True)
                      for source, target in ((process.stdout, stdout), (process.stderr, stderr))]
            for thread in drains:
                thread.start()
            try:
                code = process.wait(timeout=min(float(request.get("timeout") or 300), 7200))
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait()
                code = 124
            finally:
                # Reap detached children in the original group even when the
                # parent exits successfully. The container bounds setsid children.
                try:
                    os.killpg(process.pid, signal.SIGKILL)
                except ProcessLookupError:
                    pass
            for thread in drains:
                thread.join(timeout=2)
                if thread.is_alive():
                    raise RuntimeError("A detached process retained execution output handles")
        def bounded(path):
            with path.open("rb") as stream:
                data = stream.read(LIMIT)
            if path.stat().st_size > LIMIT:
                data += b"\n[output truncated; save detailed results to a workspace file]"
            return base64.b64encode(data).decode()
        result = {"exit_code":code, "stdout":bounded(stdout_path), "stderr":bounded(stderr_path)}
        atomic_json(result_path,result)
        print(json.dumps(result))


if __name__ == "__main__":
    main()
