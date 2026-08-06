import os
import subprocess
import sys
import tempfile
import time
import unittest
from pathlib import Path
from unittest.mock import patch

from custom.services.weknora_docreader_runtime.isolation import (
    IsolatedParseRunner,
    ParseCancelledError,
    ParseExecutionError,
    ParseTimeoutError,
    _ChildMessage,
    _start_process_group,
)


def _hanging_worker(job_dir, status):
    _start_process_group()
    status.send(_ChildMessage("ready"))
    if os.name == "posix":
        child = subprocess.Popen(
            [sys.executable, "-c", "import time; time.sleep(60)"],
        )
        pid_file = os.environ.get("DOCREADER_TEST_DESCENDANT_PID_FILE")
        if pid_file:
            Path(pid_file).write_text(str(child.pid), encoding="ascii")
    time.sleep(60)


def _crashing_worker(job_dir, status):
    _start_process_group()
    status.send(_ChildMessage("ready"))
    child = subprocess.Popen(
        [sys.executable, "-c", "import time; time.sleep(60)"],
    )
    pid_file = os.environ.get("DOCREADER_TEST_DESCENDANT_PID_FILE")
    if pid_file:
        Path(pid_file).write_text(str(child.pid), encoding="ascii")
    os._exit(7)


def _assert_process_stopped(test_case: unittest.TestCase, pid: int) -> None:
    deadline = time.monotonic() + 3
    while time.monotonic() < deadline:
        proc_stat = Path(f"/proc/{pid}/stat")
        if not proc_stat.exists():
            return
        # A zombie is no longer running and retains no parser resources; PID 1
        # will reap it.
        if proc_stat.read_text().split()[2] == "Z":
            return
        time.sleep(0.05)
    test_case.fail(f"descendant {pid} survived process-group kill")


class IsolationTest(unittest.TestCase):
    def test_real_text_parse_runs_in_isolated_process(self):
        with tempfile.TemporaryDirectory() as tmp_path:
            runner = IsolatedParseRunner(
                timeout_seconds=15,
                kill_grace_seconds=0.2,
                work_root=tmp_path,
            )

            document = runner.parse_file(
                request_id="isolation-success",
                file_name="sample.md",
                file_type="md",
                file_content=b"# isolated parser works",
                parser_engine="",
                engine_overrides={},
                is_active=lambda: True,
            )

            self.assertIn("isolated parser works", document.content)
            self.assertEqual(list(Path(tmp_path).iterdir()), [])

    def test_timeout_kills_parser_process_group(self):
        with tempfile.TemporaryDirectory() as tmp_path:
            jobs_path = Path(tmp_path, "jobs")
            jobs_path.mkdir()
            descendant_pid_file = Path(tmp_path, "descendant.pid")
            runner = IsolatedParseRunner(
                timeout_seconds=0.5,
                kill_grace_seconds=0.2,
                work_root=str(jobs_path),
                process_target=_hanging_worker,
            )

            started = time.monotonic()
            with patch.dict(
                os.environ,
                {
                    "DOCREADER_TEST_DESCENDANT_PID_FILE": str(
                        descendant_pid_file
                    )
                },
            ):
                with self.assertRaisesRegex(ParseTimeoutError, "hard timeout"):
                    runner.parse_file(
                        request_id="isolation-timeout",
                        file_name="hang.pdf",
                        file_type="pdf",
                        file_content=b"not-used",
                        parser_engine="",
                        engine_overrides={},
                        is_active=lambda: True,
                    )
            self.assertLess(time.monotonic() - started, 4)
            self.assertEqual(list(jobs_path.iterdir()), [])
            if os.name == "posix":
                descendant_pid = int(
                    descendant_pid_file.read_text(encoding="ascii")
                )
                _assert_process_stopped(self, descendant_pid)

    @unittest.skipUnless(os.name == "posix", "requires POSIX process groups")
    def test_child_crash_still_kills_native_descendants(self):
        with tempfile.TemporaryDirectory() as tmp_path:
            jobs_path = Path(tmp_path, "jobs")
            jobs_path.mkdir()
            descendant_pid_file = Path(tmp_path, "crash-descendant.pid")
            runner = IsolatedParseRunner(
                timeout_seconds=10,
                kill_grace_seconds=0.2,
                work_root=str(jobs_path),
                process_target=_crashing_worker,
            )

            with patch.dict(
                os.environ,
                {
                    "DOCREADER_TEST_DESCENDANT_PID_FILE": str(
                        descendant_pid_file
                    )
                },
            ):
                with self.assertRaisesRegex(ParseExecutionError, "code 7"):
                    runner.parse_file(
                        request_id="isolation-crash",
                        file_name="crash.pdf",
                        file_type="pdf",
                        file_content=b"not-used",
                        parser_engine="",
                        engine_overrides={},
                        is_active=lambda: True,
                    )

            descendant_pid = int(
                descendant_pid_file.read_text(encoding="ascii")
            )
            _assert_process_stopped(self, descendant_pid)
            self.assertEqual(list(jobs_path.iterdir()), [])

    def test_cancellation_kills_parser_without_waiting_for_timeout(self):
        with tempfile.TemporaryDirectory() as tmp_path:
            runner = IsolatedParseRunner(
                timeout_seconds=30,
                kill_grace_seconds=0.2,
                work_root=tmp_path,
                process_target=_hanging_worker,
            )
            started = time.monotonic()

            with self.assertRaisesRegex(ParseCancelledError, "cancelled"):
                runner.parse_file(
                    request_id="isolation-cancel",
                    file_name="hang.pdf",
                    file_type="pdf",
                    file_content=b"not-used",
                    parser_engine="",
                    engine_overrides={},
                    is_active=lambda: time.monotonic() - started < 0.5,
                )

            self.assertLess(time.monotonic() - started, 4)
            self.assertEqual(list(Path(tmp_path).iterdir()), [])


if __name__ == "__main__":
    unittest.main()
