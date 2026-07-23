from __future__ import annotations

import json
import tempfile
import threading
import unittest
import urllib.parse
from dataclasses import asdict
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

from custom.tests.document_processing_cluster_e2e.cluster_e2e import (
    APIClient,
    E2EFailure,
    QueueSnapshot,
    RunResult,
    WorkerInstance,
    build_workload_profile,
    generated_questions,
    graph_artifact_counts,
    missing_chunk_texts,
    percentile,
    source_refs_include,
    summarize_instance_topology,
    validate_instance_topology,
    validate_performance,
    workload_profile_fingerprint,
)


class _ContractHandler(BaseHTTPRequestHandler):
    requests: list[tuple[str, str]] = []

    def do_GET(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler contract
        type(self).requests.append((self.path, self.headers.get("X-API-Key", "")))
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path == "/api/v1/system/info":
            payload = {"success": True, "data": {"version": "test"}}
        elif parsed.path == "/api/v1/custom/document-queue/status":
            ids = urllib.parse.parse_qs(parsed.query).get("knowledge_ids", [""])[0].split(",")
            payload = {
                "success": True,
                "data": {
                    "waiting_total": 1,
                    "active_total": 0,
                    "items": {knowledge_id: {"position": 1, "state": "waiting"} for knowledge_id in ids},
                },
            }
        else:
            self.send_error(404)
            return
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self) -> None:  # noqa: N802 - BaseHTTPRequestHandler contract
        type(self).requests.append((self.path, self.headers.get("X-API-Key", "")))
        parsed = urllib.parse.urlparse(self.path)
        if parsed.path != "/api/v1/custom/document-queue/status":
            self.send_error(404)
            return
        length = int(self.headers.get("Content-Length", "0"))
        request = json.loads(self.rfile.read(length) or b"{}")
        ids = request.get("knowledge_ids", [])
        payload = {
            "success": True,
            "data": {
                "waiting_total": len(ids),
                "active_total": 0,
                "items": {
                    knowledge_id: {"position": index + 1, "state": "waiting"}
                    for index, knowledge_id in enumerate(ids)
                },
            },
        }
        body = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, _format: str, *args: object) -> None:
        return


class APIClientContractTests(unittest.TestCase):
    def test_real_http_contract_and_auth_header(self) -> None:
        _ContractHandler.requests = []
        server = ThreadingHTTPServer(("127.0.0.1", 0), _ContractHandler)
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            host, port = server.server_address
            client = APIClient(f"http://{host}:{port}", "test-token", timeout=2)
            self.assertTrue(client.system_info()["success"])
            snapshot = client.get_queue(["k1"])
            self.assertEqual(snapshot.items["k1"].position, 1)
            large = client.get_queue([f"k{index}" for index in range(205)])
            self.assertEqual(len(large.items), 205)
            self.assertEqual(len({item.position for item in large.items.values()}), 205)
            self.assertTrue(all(token == "test-token" for _, token in _ContractHandler.requests))
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=2)


class QueueContractTests(unittest.TestCase):
    def test_normalizes_map_contract(self) -> None:
        snapshot = QueueSnapshot.from_payload(
            {
                "success": True,
                "data": {
                    "waiting_total": 9,
                    "active_total": 2,
                    "items": {
                        "k1": {"position": 3, "state": "waiting"},
                        "k2": {"state": "active", "instance_id": "worker-b", "epoch": 4},
                    },
                },
            }
        )
        self.assertEqual(snapshot.waiting_total, 9)
        self.assertEqual(snapshot.active_total, 2)
        self.assertEqual(snapshot.items["k1"].position, 3)
        self.assertEqual(snapshot.items["k2"].owner_instance_id, "worker-b")
        self.assertEqual(snapshot.items["k2"].execution_epoch, 4)

    def test_normalizes_array_contract_and_instances(self) -> None:
        snapshot = QueueSnapshot.from_payload(
            {
                "documents": [
                    {"knowledge_id": "k1", "queue_position": 1, "queue_state": "queued"},
                    {"knowledge_id": "k2", "status": "processing"},
                ],
                "instances": [
                    {
                        "id": "worker-a",
                        "status": "healthy",
                        "concurrency": 2,
                        "active_documents": ["k2"],
                    }
                ],
            }
        )
        self.assertEqual(snapshot.waiting_total, 1)
        self.assertEqual(snapshot.active_total, 1)
        self.assertEqual(snapshot.items["k1"].position, 1)
        self.assertEqual(snapshot.instances[0].active_documents, ("k2",))
        self.assertEqual(snapshot.instances[0].active_count, 1)

    def test_worker_numeric_active_count(self) -> None:
        worker = WorkerInstance.from_mapping(
            {"worker_id": "w1", "state": "ready", "max_active": "4", "active_documents": "3"}
        )
        self.assertEqual(worker.capacity, 4)
        self.assertEqual(worker.active_count, 3)

    def test_stale_ready_instance_is_not_healthy(self) -> None:
        stale = WorkerInstance.from_mapping(
            {"instance_id": "old-worker", "state": "ready", "healthy": False, "capacity": 4}
        )
        current = WorkerInstance.from_mapping(
            {"instance_id": "worker-a", "state": "ready", "healthy": True, "capacity": 4}
        )
        legacy = WorkerInstance.from_mapping(
            {"instance_id": "legacy-worker", "state": "ready", "capacity": 4}
        )
        self.assertFalse(stale.is_ready)
        self.assertTrue(current.is_ready)
        self.assertTrue(legacy.is_ready)
        self.assertFalse(stale.is_healthy_ready)
        self.assertTrue(current.is_healthy_ready)
        self.assertFalse(legacy.is_healthy_ready)

    def test_worker_boot_identity_is_preserved(self) -> None:
        worker = WorkerInstance.from_mapping(
            {"instance_id": "w1", "boot_id": "boot-new", "state": "ready", "capacity": 2}
        )
        self.assertEqual(worker.boot_id, "boot-new")


def _worker(
    instance_id: str,
    *,
    boot_id: str | None = None,
    healthy: bool | None = True,
    state: str = "ready",
    capacity: int = 4,
) -> WorkerInstance:
    return WorkerInstance(
        instance_id=instance_id,
        boot_id=boot_id or f"boot-{instance_id}",
        state=state,
        capacity=capacity,
        active_count=0,
        healthy=healthy,
    )


def _topology(count: int, *, capacity: int = 4, prefix: str = "worker") -> dict[str, object]:
    workers = tuple(_worker(f"{prefix}-{index}", capacity=capacity) for index in range(count))
    return validate_instance_topology(
        workers,
        workers,
        expected_count=count,
        required=True,
    )


def _workload_profile(
    *,
    documents: int = 500,
    generated_size_kib: int = 64,
    process_config: dict[str, object] | None = None,
) -> dict[str, object]:
    return build_workload_profile(
        kb_id="kb-performance",
        kb_snapshot={
            "type": "document",
            "chunking_config": {"chunk_size": 512},
            "embedding_model_id": "embedding-a",
            "summary_model_id": "chat-a",
            "indexing_strategy": {"vector": True, "knowledge_graph": True},
        },
        documents=documents,
        upload_concurrency=32,
        generated_size_kib=generated_size_kib,
        fixture_paths=(),
        process_config=process_config,
        expected_derivatives={"summary", "questions", "graph", "wiki"},
        expected_chunk_text=("acceptance marker",),
        verify_sample=3,
        wiki_timeout=1800,
        poll_interval=2,
        skip_card_contract=False,
    )


def _run_result(throughput: float, *, documents: int = 500) -> RunResult:
    return RunResult(
        run_id="test-run",
        started_at="2026-01-01T00:00:00+00:00",
        finished_at="2026-01-01T00:10:00+00:00",
        documents=documents,
        completed=documents,
        failed=0,
        cancelled=0,
        wall_seconds=documents / throughput,
        throughput_docs_per_second=throughput,
        queue_wait_p50_seconds=1,
        queue_wait_p95_seconds=2,
        processing_p50_seconds=10,
        processing_p95_seconds=20,
        max_waiting_total=documents,
        max_active_total=4,
        max_active_by_instance={},
        owner_distribution={},
        errors=[],
    )


class InstanceTopologyTests(unittest.TestCase):
    def test_counts_only_explicitly_healthy_runnable_instances(self) -> None:
        topology = summarize_instance_topology(
            (
                _worker("live"),
                _worker("stale", healthy=False),
                _worker("legacy", healthy=None),
                _worker("draining", state="draining"),
            )
        )
        self.assertEqual(topology["api_instances_total"], 4)
        self.assertEqual(topology["healthy_ready_count"], 1)
        self.assertEqual(topology["healthy_ready_instances"][0]["instance_id"], "live")

    def test_rejects_declared_count_that_is_not_actually_live(self) -> None:
        workers = (_worker("worker-a"), _worker("worker-b"))
        with self.assertRaisesRegex(E2EFailure, "does not match --instance-count"):
            validate_instance_topology(
                workers,
                workers,
                expected_count=3,
                required=True,
            )

    def test_rejects_boot_change_during_measurement(self) -> None:
        start = (_worker("worker-a", boot_id="boot-old"),)
        end = (_worker("worker-a", boot_id="boot-new"),)
        with self.assertRaisesRegex(E2EFailure, "changed during the measured run"):
            validate_instance_topology(start, end, expected_count=1, required=True)

    def test_stable_topology_records_actual_capacity(self) -> None:
        topology = _topology(2, capacity=6)
        self.assertEqual(topology["effective_healthy_ready_count"], 2)
        self.assertEqual(topology["per_instance_capacity"], [6])


class WorkloadProfileTests(unittest.TestCase):
    def test_generated_profile_changes_with_document_count_size_and_process_config(self) -> None:
        base = _workload_profile()
        reordered = _workload_profile(process_config={"b": 2, "a": 1})
        same_reordered = _workload_profile(process_config={"a": 1, "b": 2})
        self.assertEqual(
            workload_profile_fingerprint(reordered),
            workload_profile_fingerprint(same_reordered),
        )
        self.assertNotEqual(
            workload_profile_fingerprint(base),
            workload_profile_fingerprint(_workload_profile(documents=499)),
        )
        self.assertNotEqual(
            workload_profile_fingerprint(base),
            workload_profile_fingerprint(_workload_profile(generated_size_kib=32)),
        )
        self.assertNotEqual(
            workload_profile_fingerprint(base),
            workload_profile_fingerprint(reordered),
        )

    def test_fixture_profile_uses_content_hash_and_size(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            first = Path(temp_dir) / "fixture.md"
            second = Path(temp_dir) / "fixture-copy.md"
            first.write_bytes(b"alpha workload")
            second.write_bytes(b"bravo workload")
            common = dict(
                kb_id="kb-performance",
                kb_snapshot={"type": "document"},
                documents=1,
                upload_concurrency=1,
                generated_size_kib=64,
                process_config=None,
                expected_derivatives=(),
                expected_chunk_text=(),
                verify_sample=1,
                wiki_timeout=0,
                poll_interval=2,
                skip_card_contract=False,
            )
            first_profile = build_workload_profile(fixture_paths=(first,), **common)
            second_profile = build_workload_profile(fixture_paths=(second,), **common)
        descriptor = first_profile["input"]["fixtures"][0]
        self.assertEqual(descriptor["size_bytes"], len(b"alpha workload"))
        self.assertEqual(len(descriptor["sha256"]), 64)
        self.assertNotEqual(
            workload_profile_fingerprint(first_profile),
            workload_profile_fingerprint(second_profile),
        )


class PerformanceComparisonTests(unittest.TestCase):
    def _write_baseline(
        self,
        directory: str,
        profile: dict[str, object],
        *,
        throughput: float = 1.0,
        instances: int = 1,
        capacity: int = 4,
    ) -> Path:
        report = Path(directory) / "baseline.json"
        report.write_text(
            json.dumps(
                {
                    "status": "passed",
                    "workload_profile": profile,
                    "workload_fingerprint": workload_profile_fingerprint(profile),
                    "instance_topology": _topology(instances, capacity=capacity, prefix="baseline"),
                    "result": asdict(_run_result(throughput, documents=int(profile["documents"]))),
                }
            ),
            encoding="utf-8",
        )
        return report

    def test_scaling_uses_api_observed_instance_ratio(self) -> None:
        profile = _workload_profile()
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = self._write_baseline(temp_dir, profile, throughput=1.0, instances=1)
            scaling = validate_performance(
                _run_result(2.4),
                min_throughput=0,
                max_p95_processing_seconds=0,
                baseline_report=baseline,
                min_scaling_efficiency=0.75,
                workload_profile=profile,
                instance_topology=_topology(3, prefix="scaled"),
            )
        self.assertAlmostEqual(scaling["speedup"], 2.4)
        self.assertAlmostEqual(scaling["instance_expansion_factor"], 3.0)
        self.assertAlmostEqual(scaling["scaling_efficiency"], 0.8)
        self.assertEqual(scaling["baseline_healthy_ready_instances"], 1)
        self.assertEqual(scaling["scaled_healthy_ready_instances"], 3)

    def test_rejects_incomparable_document_count_before_reporting_speedup(self) -> None:
        baseline_profile = _workload_profile(documents=500)
        current_profile = _workload_profile(documents=499)
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = self._write_baseline(temp_dir, baseline_profile)
            with self.assertRaisesRegex(E2EFailure, r"workload_profile\.documents"):
                validate_performance(
                    _run_result(2.0, documents=499),
                    min_throughput=0,
                    max_p95_processing_seconds=0,
                    baseline_report=baseline,
                    min_scaling_efficiency=0,
                    workload_profile=current_profile,
                    instance_topology=_topology(2, prefix="scaled"),
                )

    def test_rejects_size_derivative_and_kb_configuration_changes(self) -> None:
        baseline_profile = _workload_profile()
        changed_derivatives = json.loads(json.dumps(baseline_profile))
        changed_derivatives["expected_derivatives"].remove("wiki")
        changed_kb = json.loads(json.dumps(baseline_profile))
        changed_kb["knowledge_base"]["config_sha256"] = "changed"
        cases = {
            "generated size": _workload_profile(generated_size_kib=32),
            "derivatives": changed_derivatives,
            "knowledge-base config": changed_kb,
        }
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = self._write_baseline(temp_dir, baseline_profile)
            for label, current_profile in cases.items():
                with self.subTest(label=label), self.assertRaisesRegex(
                    E2EFailure,
                    "not comparable",
                ):
                    validate_performance(
                        _run_result(2.0),
                        min_throughput=0,
                        max_p95_processing_seconds=0,
                        baseline_report=baseline,
                        min_scaling_efficiency=0,
                        workload_profile=current_profile,
                        instance_topology=_topology(2, prefix="scaled"),
                    )

    def test_rejects_legacy_baseline_without_workload_identity(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = Path(temp_dir) / "legacy.json"
            baseline.write_text(
                json.dumps({"status": "passed", "result": asdict(_run_result(1.0))}),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(E2EFailure, "rerun the baseline with the current harness"):
                validate_performance(
                    _run_result(2.0),
                    min_throughput=0,
                    max_p95_processing_seconds=0,
                    baseline_report=baseline,
                    min_scaling_efficiency=0,
                    workload_profile=_workload_profile(),
                    instance_topology=_topology(2, prefix="scaled"),
                )

    def test_rejects_baseline_without_api_topology_evidence(self) -> None:
        profile = _workload_profile()
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = Path(temp_dir) / "no-topology.json"
            baseline.write_text(
                json.dumps(
                    {
                        "status": "passed",
                        "workload_profile": profile,
                        "result": asdict(_run_result(1.0)),
                    }
                ),
                encoding="utf-8",
            )
            with self.assertRaisesRegex(E2EFailure, "no instance_topology"):
                validate_performance(
                    _run_result(2.0),
                    min_throughput=0,
                    max_p95_processing_seconds=0,
                    baseline_report=baseline,
                    min_scaling_efficiency=0,
                    workload_profile=profile,
                    instance_topology=_topology(2, prefix="scaled"),
                )

    def test_rejects_changed_per_instance_concurrency(self) -> None:
        profile = _workload_profile()
        with tempfile.TemporaryDirectory() as temp_dir:
            baseline = self._write_baseline(temp_dir, profile, capacity=4)
            with self.assertRaisesRegex(E2EFailure, "per-instance concurrency differs"):
                validate_performance(
                    _run_result(2.0),
                    min_throughput=0,
                    max_p95_processing_seconds=0,
                    baseline_report=baseline,
                    min_scaling_efficiency=0,
                    workload_profile=profile,
                    instance_topology=_topology(2, capacity=8, prefix="scaled"),
                )


class OutputEvidenceTests(unittest.TestCase):
    def test_missing_chunk_texts_combines_chunk_types_case_insensitively(self) -> None:
        chunks = [
            {"chunk_type": "image_ocr", "content": "VISION CODE: 7319"},
            {"chunk_type": "image_caption", "content": "A Blue Box and Red Circle"},
        ]
        self.assertEqual(missing_chunk_texts(chunks, ["7319", "blue box"]), [])
        self.assertEqual(missing_chunk_texts(chunks, ["StandardReply"]), ["StandardReply"])

    def test_generated_questions_accepts_json_metadata(self) -> None:
        chunk = {"metadata": '{"generated_questions":[{"id":"q1","question":"why"}]}' }
        self.assertEqual(len(generated_questions(chunk)), 1)

    def test_graph_artifact_counts_require_successful_graph_span_output(self) -> None:
        spans = [
            {"name": "postprocess.graph.chunk[0]", "status": "done", "output": {"nodes_added": 2, "relations_added": 1}},
            {"name": "postprocess.graph.chunk[1]", "status": "failed", "output": {"nodes_added": 99}},
            {"name": "postprocess.summary", "status": "done", "output": {"nodes_added": 99}},
            {"name": "postprocess.graph.chunk[2]", "status": "completed", "output": {"nodes_added": "3", "relations_added": "bad"}},
        ]
        self.assertEqual(graph_artifact_counts(spans), (5, 1))

    def test_graph_artifact_counts_do_not_treat_completed_empty_span_as_artifact(self) -> None:
        spans = [{"name": "postprocess.graph.chunk[0]", "status": "done", "output": {}}]
        self.assertEqual(graph_artifact_counts(spans), (0, 0))

    def test_source_refs_match_only_id_prefix(self) -> None:
        page = {"source_refs": ["k1|Document one", "k2|Document two"]}
        self.assertTrue(source_refs_include(page, "k1"))
        self.assertFalse(source_refs_include(page, "k"))

    def test_percentile_interpolates(self) -> None:
        self.assertEqual(percentile([], 95), None)
        self.assertEqual(percentile([10], 95), 10)
        self.assertAlmostEqual(percentile([0, 10], 50) or 0, 5.0)


if __name__ == "__main__":
    unittest.main()
