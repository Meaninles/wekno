from __future__ import annotations

import json
import unittest
import urllib.error
from unittest import mock

from custom.tests.document_processing_cluster_e2e.cluster_e2e import (
    APIError,
    ClusterE2ERunner,
    DockerController,
    E2EFailure,
    QueueItem,
    QueueSnapshot,
    WorkerInstance,
)
from custom.tests.document_processing_cluster_e2e.run_durable_failover import (
    SplitScopeAPIClient,
    WorkerTarget,
    assert_no_takeover_while_paused,
    validate_worker_mappings,
    wait_for_instances_ready,
    wait_for_stable_resume,
    wait_for_takeover,
)


class _Recorder:
    def __init__(self) -> None:
        self.events: list[tuple[str, dict[str, object]]] = []

    def emit(self, event: str, **fields: object) -> None:
        self.events.append((event, fields))


class _Observation:
    def __init__(self, *, status: str = "", owners: set[str] | None = None) -> None:
        self.final_status = status
        self.owners = set(owners or set())


class _Client:
    def __init__(self, instances: list[WorkerInstance] | None = None) -> None:
        self.instances = instances or []

    def system_info(self) -> dict[str, bool]:
        return {"ok": True}

    def get_instances(self, *, optional: bool = False) -> tuple[WorkerInstance, ...]:
        del optional
        return tuple(self.instances)


class _Runner:
    def __init__(
        self,
        snapshots: list[QueueSnapshot],
        observations: dict[str, _Observation],
        instances: list[WorkerInstance] | None = None,
    ) -> None:
        self.snapshots = list(snapshots)
        self.observations = observations
        self.poll_interval = 0
        self.client = _Client(instances)

    def sample_queue(self) -> QueueSnapshot:
        snapshot = self.snapshots.pop(0) if len(self.snapshots) > 1 else self.snapshots[0]
        for knowledge_id, item in snapshot.items.items():
            if knowledge_id in self.observations and item.owner_instance_id:
                self.observations[knowledge_id].owners.add(item.owner_instance_id)
        return snapshot

    def _refresh_terminal_states(self) -> None:
        return


def _item(
    knowledge_id: str,
    owner: str,
    *,
    epoch: int | None = None,
    state: str = "processing",
    progress: str = "",
) -> QueueItem:
    return QueueItem(
        knowledge_id=knowledge_id,
        state=state,
        owner_instance_id=owner,
        execution_epoch=epoch,
        last_progress_at=progress,
    )


def _snapshot(
    items: list[QueueItem],
    instances: tuple[WorkerInstance, ...] = (),
) -> QueueSnapshot:
    return QueueSnapshot(
        waiting_total=0,
        active_total=len(items),
        items={item.knowledge_id: item for item in items},
        instances=instances,
    )


class DockerEvidenceTests(unittest.TestCase):
    def test_stop_and_start_inspect_the_actual_running_state(self) -> None:
        recorder = _Recorder()
        controller = DockerController(["worker-a", "worker-b"], recorder)  # type: ignore[arg-type]
        running = True

        def fake_run(arguments: list[str], timeout: float = 60.0) -> str:
            nonlocal running
            del timeout
            if arguments[:2] == ["docker", "kill"]:
                running = False
                return "worker-a"
            if arguments[:2] == ["docker", "start"]:
                running = True
                return "worker-a"
            if arguments[:4] == ["docker", "inspect", "--format", "{{json .State}}"]:
                return json.dumps(
                    {"Running": running, "Paused": False, "Status": "running" if running else "exited"}
                )
            raise AssertionError(arguments)

        with mock.patch.object(controller, "_run", side_effect=fake_run):
            controller.stop("worker-a", hard_kill=True)
            self.assertFalse(running)
            controller.start("worker-a")
            self.assertTrue(running)

        verified = [fields["expected_running"] for event, fields in recorder.events if event == "chaos.container_state_verified"]
        self.assertEqual(verified, [False, True])

    def test_mapping_rejects_container_instance_mismatch(self) -> None:
        controller = DockerController(["c1", "c2"], _Recorder())  # type: ignore[arg-type]
        instances = {
            "worker-a": WorkerInstance("worker-a", "boot-a", "ready", 1, 0),
            "worker-b": WorkerInstance("worker-b", "boot-b", "ready", 1, 0),
        }
        with (
            mock.patch.object(controller, "assert_present"),
            mock.patch.object(
                controller,
                "configured_instance_identity",
                side_effect=[("wrong-worker", "worker-a"), ("worker-b", "worker-b")],
            ),
        ):
            with self.assertRaisesRegex(E2EFailure, "mapping mismatch"):
                validate_worker_mappings(
                    controller,
                    [WorkerTarget("worker-a", "c1"), WorkerTarget("worker-b", "c2")],
                    instances,
                )


class SplitScopeAPIClientTests(unittest.TestCase):
    def test_admin_only_calls_do_not_use_the_tenant_workload_credential(self) -> None:
        workload = mock.Mock()
        control = mock.Mock()
        expected = (WorkerInstance("worker-a", "boot-a", "ready", 1, 0),)
        control.get_instances.return_value = expected
        client = SplitScopeAPIClient(workload, control)

        self.assertEqual(client.get_instances(optional=False), expected)
        client.attest_instance_termination("worker-a", "boot-a", "proof")
        self.assertEqual(
            client.system_setting("asynq.concurrency"),
            control.system_setting.return_value,
        )
        self.assertEqual(client.get_queue(["k1"]), workload.get_queue.return_value)

        workload.get_instances.assert_not_called()
        workload.attest_instance_termination.assert_not_called()
        workload.system_setting.assert_not_called()
        control.get_instances.assert_called_once_with(optional=False)
        control.attest_instance_termination.assert_called_once_with(
            "worker-a", "boot-a", "proof"
        )
        control.system_setting.assert_called_once_with("asynq.concurrency")

    def test_read_only_call_retries_a_cold_start_transport_failure(self) -> None:
        refused = E2EFailure("GET failed during cold start")
        refused.__cause__ = urllib.error.URLError("connection refused")

        workload = mock.Mock()
        workload.list_all_knowledge.side_effect = [
            refused,
            [{"id": "k1"}],
        ]
        client = SplitScopeAPIClient(
            workload,
            mock.Mock(),
            read_retry_timeout=1,
            read_retry_interval=0,
        )

        self.assertEqual(client.list_all_knowledge("kb-1"), [{"id": "k1"}])
        self.assertEqual(workload.list_all_knowledge.call_count, 2)

    def test_read_only_call_retries_503_but_not_500(self) -> None:
        workload = mock.Mock()
        workload.get_knowledge.side_effect = [
            APIError("GET", "http://test/k1", 503, "starting"),
            {"id": "k1"},
        ]
        client = SplitScopeAPIClient(
            workload,
            mock.Mock(),
            read_retry_timeout=1,
            read_retry_interval=0,
        )
        self.assertEqual(client.get_knowledge("k1"), {"id": "k1"})

        workload.get_knowledge.side_effect = APIError(
            "GET", "http://test/k2", 500, "product failure"
        )
        with self.assertRaises(APIError):
            client.get_knowledge("k2")
        self.assertEqual(workload.get_knowledge.call_count, 3)

    def test_mutating_calls_are_never_retried(self) -> None:
        workload = mock.Mock()
        transient = E2EFailure("connection reset")
        transient.__cause__ = urllib.error.URLError("connection reset")
        workload.delete_knowledge.side_effect = transient
        client = SplitScopeAPIClient(
            workload,
            mock.Mock(),
            read_retry_timeout=1,
            read_retry_interval=0,
        )

        with self.assertRaises(E2EFailure):
            client.delete_knowledge("k1")
        workload.delete_knowledge.assert_called_once_with("k1")


class FailoverEvidenceTests(unittest.TestCase):
    def test_stable_resume_uses_new_boot_active_owner_when_epoch_is_absent(self) -> None:
        before = _snapshot([_item("k1", "worker-a")])
        instance = WorkerInstance(
            "worker-a", "boot-new", "ready", 2, 1, active_documents=("k1",)
        )
        after = _snapshot([_item("k1", "worker-a")], (instance,))
        runner = _Runner([after], {"k1": _Observation(owners={"worker-a"})})
        result = wait_for_stable_resume(
            runner,  # type: ignore[arg-type]
            WorkerTarget("worker-a", "container-a"),
            "boot-new",
            {"k1"},
            before,
            1,
        )
        self.assertEqual(result["documents"]["k1"]["proof"], "active-on-new-boot")
        self.assertEqual(result["documents"]["k1"]["boot_id"], "boot-new")

    def test_stable_resume_does_not_empty_pass_without_owner_or_terminal_evidence(self) -> None:
        before = _snapshot([_item("k1", "worker-a")])
        instance = WorkerInstance("worker-a", "boot-new", "ready", 2, 0)
        ownerless = _snapshot([_item("k1", "")], (instance,))
        runner = _Runner([ownerless], {"k1": _Observation(owners={"worker-a"})})
        with mock.patch(
            "custom.tests.document_processing_cluster_e2e.run_durable_failover.time.monotonic",
            side_effect=[0, 0, 2],
        ):
            with self.assertRaisesRegex(E2EFailure, "k1"):
                wait_for_stable_resume(
                    runner,  # type: ignore[arg-type]
                    WorkerTarget("worker-a", "container-a"),
                    "boot-new",
                    {"k1"},
                    before,
                    1,
                )

    def test_takeover_requires_evidence_for_every_document(self) -> None:
        before = _snapshot(
            [_item("k1", "worker-a", epoch=3), _item("k2", "worker-a", epoch=8)]
        )
        partial = _snapshot(
            [_item("k1", "worker-b", epoch=4), _item("k2", "worker-a", epoch=8)]
        )
        runner = _Runner(
            [partial],
            {
                "k1": _Observation(owners={"worker-a"}),
                "k2": _Observation(owners={"worker-a"}),
            },
        )
        with mock.patch(
            "custom.tests.document_processing_cluster_e2e.run_durable_failover.time.monotonic",
            side_effect=[0, 0, 2],
        ):
            with self.assertRaisesRegex(E2EFailure, "k2"):
                wait_for_takeover(
                    runner,  # type: ignore[arg-type]
                    "worker-a",
                    {"k1", "k2"},
                    before,
                    1,
                )

    def test_takeover_accepts_all_owner_and_epoch_transitions(self) -> None:
        before = _snapshot(
            [_item("k1", "worker-a", epoch=3), _item("k2", "worker-a", epoch=8)]
        )
        after = _snapshot(
            [_item("k1", "worker-b", epoch=4), _item("k2", "worker-c", epoch=9)]
        )
        runner = _Runner(
            [after],
            {
                "k1": _Observation(owners={"worker-a"}),
                "k2": _Observation(owners={"worker-a"}),
            },
        )
        result = wait_for_takeover(
            runner,  # type: ignore[arg-type]
            "worker-a",
            {"k1", "k2"},
            before,
            1,
        )
        self.assertEqual(set(result["documents"]), {"k1", "k2"})
        self.assertTrue(
            all(
                value["proof"] == "owner-changed-and-epoch-advanced"
                for value in result["documents"].values()
            )
        )

    def test_terminal_takeover_without_observed_survivor_is_rejected(self) -> None:
        before = _snapshot([_item("k1", "worker-a", epoch=3)])
        terminal = _snapshot([])
        runner = _Runner(
            [terminal],
            {"k1": _Observation(status="completed", owners={"worker-a"})},
        )
        with self.assertRaisesRegex(E2EFailure, "no surviving owner"):
            wait_for_takeover(
                runner,  # type: ignore[arg-type]
                "worker-a",
                {"k1"},
                before,
                1,
            )

    def test_paused_nonterminal_missing_item_is_a_failure(self) -> None:
        before = _snapshot([_item("k1", "worker-a", epoch=4)])
        missing = _snapshot([])
        runner = _Runner(
            [missing],
            {"k1": _Observation(owners={"worker-a"})},
        )
        with mock.patch(
            "custom.tests.document_processing_cluster_e2e.run_durable_failover.time.monotonic",
            side_effect=[0, 0],
        ):
            with self.assertRaisesRegex(E2EFailure, "disappeared"):
                assert_no_takeover_while_paused(
                    runner,  # type: ignore[arg-type]
                    "worker-a",
                    {"k1"},
                    before,
                    1,
                )

    def test_paused_missing_item_is_allowed_only_after_terminal(self) -> None:
        before = _snapshot([_item("k1", "worker-a", epoch=4)])
        missing = _snapshot([])
        runner = _Runner(
            [missing],
            {"k1": _Observation(status="completed", owners={"worker-a"})},
        )
        with mock.patch(
            "custom.tests.document_processing_cluster_e2e.run_durable_failover.time.monotonic",
            side_effect=[0, 0, 2],
        ):
            result = assert_no_takeover_while_paused(
                runner,  # type: ignore[arg-type]
                "worker-a",
                {"k1"},
                before,
                1,
            )
        self.assertEqual(result["terminal_during_pause"], ["k1"])

    def test_instance_readiness_retries_cold_api_and_requires_new_boot(self) -> None:
        target = WorkerTarget("worker-a", "container-a")
        client = mock.Mock()
        client.get_instances.side_effect = [
            E2EFailure("connection refused"),
            (WorkerInstance("worker-a", "boot-new", "ready", 1, 0),),
        ]
        result = wait_for_instances_ready(
            client,
            [target],
            1,
            0,
            previous_boots={"worker-a": "boot-old"},
        )
        self.assertEqual(result["worker-a"].boot_id, "boot-new")


class PartialUploadCleanupTests(unittest.TestCase):
    def test_successful_uploads_remain_registered_when_one_upload_fails(self) -> None:
        class Client:
            def __init__(self) -> None:
                self.deleted: list[str] = []

            def upload_document(
                self,
                kb_id: str,
                filename: str,
                content: bytes,
                *,
                process_config: object,
                metadata: object,
            ) -> dict[str, str]:
                del kb_id, content, process_config, metadata
                if "00001" in filename:
                    raise E2EFailure("injected upload failure")
                return {"id": filename}

            def delete_knowledge(self, knowledge_id: str) -> None:
                self.deleted.append(knowledge_id)

        client = Client()
        runner = ClusterE2ERunner(
            client,  # type: ignore[arg-type]
            "kb-test",
            _Recorder(),  # type: ignore[arg-type]
            run_id="partial-upload",
            poll_interval=0,
        )
        with self.assertRaisesRegex(E2EFailure, "successful uploads remain registered"):
            runner.upload_batch(
                3,
                upload_concurrency=3,
                process_config=None,
                generated_size_kib=1,
            )
        self.assertEqual(len(runner.observations), 2)
        runner.cleanup()
        self.assertEqual(set(client.deleted), set(runner.observations))


if __name__ == "__main__":
    unittest.main()
