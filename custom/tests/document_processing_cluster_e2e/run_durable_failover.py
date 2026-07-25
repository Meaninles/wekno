from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
import time
import urllib.error
from dataclasses import asdict, dataclass
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

if __package__ in {None, ""}:
    sys.path.insert(0, str(Path(__file__).resolve().parent))
    from cluster_e2e import (  # type: ignore
        APIClient,
        APIError,
        ClusterE2ERunner,
        DockerController,
        E2EFailure,
        JsonlRecorder,
        QueueSnapshot,
        WorkerInstance,
        utc_now,
        validate_performance,
    )
else:
    from .cluster_e2e import (
        APIClient,
        APIError,
        ClusterE2ERunner,
        DockerController,
        E2EFailure,
        JsonlRecorder,
        QueueSnapshot,
        WorkerInstance,
        utc_now,
        validate_performance,
    )


REPO_ROOT = Path(__file__).resolve().parents[3]
SCENARIOS = (
    "stable-reboot",
    "cross-instance-takeover",
    "paused-old-owner",
    "redis-restart",
    "fleet-restart",
    "api-restart",
)
READY_INSTANCE_STATES = {"ready", "healthy", "active", "running"}


@dataclass(frozen=True)
class WorkerTarget:
    instance_id: str
    container: str


@dataclass
class CommandResult:
    name: str
    command: list[str]
    status: str
    return_code: int
    elapsed_seconds: float
    log: str


class SplitScopeAPIClient:
    """Separate tenant workload calls from system-admin control observations.

    Read-only probes receive a short, narrowly-scoped transport retry window.
    A killed API container can refuse or reset one connection while coming
    back, and a durability test must not confuse that expected cold-start
    edge with a lost document. Mutating calls are deliberately delegated
    without retries: upload/delete/attestation must never be duplicated by
    this adapter.
    """

    def __init__(
        self,
        workload: APIClient,
        control: APIClient,
        *,
        read_retry_timeout: float = 30.0,
        read_retry_interval: float = 0.5,
    ) -> None:
        self.workload = workload
        self.control = control
        self.read_retry_timeout = read_retry_timeout
        self.read_retry_interval = read_retry_interval

    def __getattr__(self, name: str) -> Any:
        return getattr(self.workload, name)

    @staticmethod
    def _transient_read_error(exc: Exception) -> bool:
        if isinstance(exc, APIError):
            return exc.status in {502, 503, 504}
        current: BaseException | None = exc
        while current is not None:
            if isinstance(
                current,
                (urllib.error.URLError, ConnectionError, TimeoutError),
            ):
                return True
            current = current.__cause__
        return False

    def _retry_read(self, operation: str, call: Callable[[], Any]) -> Any:
        deadline = time.monotonic() + self.read_retry_timeout
        while True:
            try:
                return call()
            except Exception as exc:
                if (
                    not self._transient_read_error(exc)
                    or time.monotonic() >= deadline
                ):
                    raise
                time.sleep(self.read_retry_interval)

    def system_info(self) -> Any:
        return self._retry_read("system_info", self.workload.system_info)

    def get_knowledge_base(self, kb_id: str) -> Mapping[str, Any]:
        return self._retry_read(
            "get_knowledge_base",
            lambda: self.workload.get_knowledge_base(kb_id),
        )

    def get_knowledge(self, knowledge_id: str) -> Mapping[str, Any]:
        return self._retry_read(
            "get_knowledge",
            lambda: self.workload.get_knowledge(knowledge_id),
        )

    def list_all_knowledge(self, kb_id: str) -> list[Mapping[str, Any]]:
        return self._retry_read(
            "list_all_knowledge",
            lambda: self.workload.list_all_knowledge(kb_id),
        )

    def get_queue(self, knowledge_ids: Sequence[str]) -> QueueSnapshot:
        return self._retry_read(
            "get_queue",
            lambda: self.workload.get_queue(knowledge_ids),
        )

    def get_instances(self, *, optional: bool = False) -> tuple[WorkerInstance, ...]:
        return self._retry_read(
            "get_instances",
            lambda: self.control.get_instances(optional=optional),
        )

    def attest_instance_termination(self, instance_id: str, boot_id: str, proof: str) -> None:
        self.control.attest_instance_termination(instance_id, boot_id, proof)

    def system_setting(self, key: str) -> Mapping[str, Any]:
        return self._retry_read(
            "system_setting",
            lambda: self.control.system_setting(key),
        )

    def get_spans(self, knowledge_id: str) -> Mapping[str, Any]:
        return self._retry_read(
            "get_spans",
            lambda: self.workload.get_spans(knowledge_id),
        )

    def list_chunks(
        self,
        knowledge_id: str,
        chunk_types: Sequence[str],
    ) -> list[Mapping[str, Any]]:
        return self._retry_read(
            "list_chunks",
            lambda: self.workload.list_chunks(knowledge_id, chunk_types),
        )

    def hybrid_search(
        self,
        kb_id: str,
        query_text: str,
        knowledge_ids: Sequence[str],
    ) -> list[Mapping[str, Any]]:
        return self._retry_read(
            "hybrid_search",
            lambda: self.workload.hybrid_search(
                kb_id,
                query_text,
                knowledge_ids,
            ),
        )

    def list_wiki_pages(self, kb_id: str) -> list[Mapping[str, Any]]:
        return self._retry_read(
            "list_wiki_pages",
            lambda: self.workload.list_wiki_pages(kb_id),
        )


def parse_worker(value: str) -> WorkerTarget:
    instance_id, separator, container = value.partition("=")
    if not separator or not instance_id.strip() or not container.strip():
        raise argparse.ArgumentTypeError("--worker must be INSTANCE_ID=CONTAINER")
    return WorkerTarget(instance_id.strip(), container.strip())


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        description=(
            "Independent durability/failover suite for the document workflow queue. "
            "Safe state-machine and race contracts run by default; Docker faults are opt-in."
        )
    )
    p.add_argument("--go-container", default=os.getenv("WEKNORA_E2E_GO_CONTAINER", ""))
    p.add_argument("--go-binary", default="go")
    p.add_argument("--container-go-binary", default="/usr/local/go/bin/go")
    p.add_argument("--skip-contracts", action="store_true")
    p.add_argument("--no-race", action="store_true", help="disable go test -race for safe contracts")
    p.add_argument("--postgres-contract", action="store_true")
    p.add_argument("--redis-contract", action="store_true")
    p.add_argument("--redis-contract-db", type=int, default=14)

    p.add_argument("--scenario", action="append", choices=SCENARIOS, default=[])
    p.add_argument("--allow-chaos", action="store_true")
    p.add_argument(
        "--allow-infrastructure-chaos",
        action="store_true",
        help="additional acknowledgement required before stopping Redis",
    )
    p.add_argument(
        "--allow-full-worker-outage",
        action="store_true",
        help="acknowledge that every listed worker will be stopped together",
    )
    p.add_argument("--base-url", default=os.getenv("WEKNORA_E2E_HOST", "http://localhost:8080"))
    p.add_argument("--token", default=os.getenv("WEKNORA_E2E_TOKEN", ""))
    p.add_argument("--auth-mode", choices=("api-key", "bearer"), default="api-key")
    p.add_argument(
        "--admin-token",
        default=os.getenv("WEKNORA_E2E_ADMIN_TOKEN", ""),
        help="system-admin token used only for control-plane observation; never persisted",
    )
    p.add_argument(
        "--admin-auth-mode",
        choices=("api-key", "bearer"),
        default=os.getenv("WEKNORA_E2E_ADMIN_AUTH_MODE", "bearer"),
    )
    p.add_argument("--kb-id", default=os.getenv("WEKNORA_E2E_KB_ID", ""))
    p.add_argument("--worker", action="append", type=parse_worker, default=[])
    p.add_argument("--fault-instance", default="")
    p.add_argument("--redis-container", default="")
    p.add_argument("--documents", type=int, default=8)
    p.add_argument("--upload-concurrency", type=int, default=8)
    p.add_argument("--generated-size-kib", type=int, default=64)
    p.add_argument("--poll-interval", type=float, default=1.0)
    p.add_argument("--activity-timeout", type=float, default=120.0)
    p.add_argument("--takeover-timeout", type=float, default=180.0)
    p.add_argument("--completion-timeout", type=float, default=1800.0)
    p.add_argument("--fault-down-seconds", type=float, default=15.0)
    p.add_argument(
        "--pause-seconds",
        type=float,
        default=90.0,
        help="must exceed workflow lease plus Asynq active-task recovery delay",
    )
    p.add_argument("--keep-data", action="store_true")
    p.add_argument(
        "--output-dir",
        type=Path,
        default=Path("custom/tests/document_processing_cluster_e2e/durable_failover_outputs"),
    )
    return p


def command_environment(args: argparse.Namespace) -> dict[str, str]:
    environment = dict(os.environ)
    if args.postgres_contract:
        environment["WEKNORA_DOCUMENT_QUEUE_POSTGRES_CONTRACT"] = "1"
    if args.redis_contract:
        environment["WEKNORA_DURABLE_FAILOVER_REDIS_CONTRACT"] = "1"
        environment["WEKNORA_DURABLE_FAILOVER_REDIS_DB"] = str(args.redis_contract_db)
    return environment


def execute_contract(
    args: argparse.Namespace,
    run_dir: Path,
    *,
    name: str,
    package: str,
    pattern: str,
    race: bool,
) -> CommandResult:
    go_args = ["test"]
    if race:
        go_args.append("-race")
    go_args.extend([package, "-run", pattern, "-count=1", "-v"])
    environment = command_environment(args)
    if args.go_container:
        command = ["docker", "exec", "-w", "/workspace"]
        for variable in (
            "WEKNORA_DOCUMENT_QUEUE_POSTGRES_CONTRACT",
            "WEKNORA_DURABLE_FAILOVER_REDIS_CONTRACT",
            "WEKNORA_DURABLE_FAILOVER_REDIS_DB",
        ):
            if variable in environment:
                command.extend(["-e", f"{variable}={environment[variable]}"])
        command.extend([args.go_container, args.container_go_binary, *go_args])
    else:
        command = [args.go_binary, *go_args]

    started = time.monotonic()
    process = subprocess.run(
        command,
        cwd=REPO_ROOT,
        env=environment,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    elapsed = time.monotonic() - started
    run_dir.mkdir(parents=True, exist_ok=True)
    log_path = run_dir / f"{name}.log"
    log_path.write_text(process.stdout, encoding="utf-8")
    return CommandResult(
        name=name,
        command=command,
        status="passed" if process.returncode == 0 else "failed",
        return_code=process.returncode,
        elapsed_seconds=elapsed,
        log=str(log_path.resolve()),
    )


def execute_python_contracts(run_dir: Path) -> CommandResult:
    command = [
        sys.executable,
        "-m",
        "unittest",
        "custom.tests.document_processing_cluster_e2e.test_cluster_e2e",
        "custom.tests.document_processing_cluster_e2e.test_durable_failover",
    ]
    started = time.monotonic()
    process = subprocess.run(
        command,
        cwd=REPO_ROOT,
        text=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        check=False,
    )
    elapsed = time.monotonic() - started
    log_path = run_dir / "python_harness_contracts.log"
    log_path.write_text(process.stdout, encoding="utf-8")
    return CommandResult(
        name="python-harness-contracts",
        command=command,
        status="passed" if process.returncode == 0 else "failed",
        return_code=process.returncode,
        elapsed_seconds=elapsed,
        log=str(log_path.resolve()),
    )


def run_contracts(args: argparse.Namespace, run_dir: Path) -> list[CommandResult]:
    race = not args.no_race
    core_pattern = "^(" + "|".join(
        (
            "TestPreparingIsInvisibleAndCannotDispatchOrClaim",
            "TestPrepareRejectsSameGenerationDifferentPayloadOrOptions",
            "TestWrongKnowledgeBindingCannotActivate",
            "TestConcurrentActivationHasOneCASWinner",
            "TestRecoverPreparingActivatesOnlyExactCommittedBinding",
            "TestClaimRevalidatesKnowledgeWorkflowBinding",
            "TestClaimAcceptsDurableResumeBoundariesAfterOwnerTermination",
            "TestResumeActivatesPersistedPlanWithoutReconstruction",
            "TestResumeQueuedAcceptsEveryClaimableRecoveryBoundary",
            "TestResumeQueuedRejectsUncommittedOwnerlessProcessing",
            "TestResumeLeasedAndTerminalStatesRequireExactGenerationWorkflowBinding",
            "TestStableInstanceRestartAdoptsAndFencesPreviousBoot",
            "TestCrossInstanceTakeoverRequiresTerminationProofBeyondStaleHeartbeat",
            "TestTerminationAttestationRejectsFreshOrWrongBoot",
            "TestFencedBootCannotClaimOrRenew",
            "TestConcurrentRegisterAndClaimHasExactlyOneWinner",
            "TestSlowRecoveryDoesNotStarveHeartbeat",
            "TestExpiredRecoveryFixedSweepRevisitsOldRowsDespiteGrowingTail",
            "TestExpiredRecoveryFixedSweepUsesIDAtEqualLeaseBoundary",
            "TestRecoverExpiredLeasesCursorPassesMoreThanScanBudgetOfUnprovenWork",
            "TestConcurrentExpiredRecoveryRequeuesEligibleWorkflowOnlyOnce",
            "TestDeadlineIgnoringDelegateTripsAndClearsLiveness",
            "TestKubernetesInstanceIdentityRoundTrip",
            "TestKubernetesRuntimeVerifierRequiresPositiveExactTermination",
            "TestKubernetesRuntimeVerifierRejectsUnsupportedIdentityWithoutCallingAPI",
            "TestKubernetesRuntimeRecoveryRequiresEveryTakeoverGate",
            "TestKubernetesRuntimeRecoveryFailsClosedOnVerifierError",
        )
    ) + ")$"
    results = [
        execute_contract(
            args,
            run_dir,
            name="core-state-machine-race",
            package="./internal/custom/modules/documentqueue",
            pattern=core_pattern,
            race=race,
        ),
        execute_contract(
            args,
            run_dir,
            name="durable-failover-contracts",
            package="./custom/tests/document_processing_cluster_e2e",
            pattern="^TestDurableFailover",
            race=race,
        ),
        execute_python_contracts(run_dir),
    ]
    return results


def wait_for_api(client: APIClient, timeout: float, poll_interval: float) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            client.system_info()
            client.get_queue(["00000000-0000-0000-0000-000000000000"])
            return
        except Exception as exc:  # expected while an API-owning worker reboots
            last_error = exc
            time.sleep(poll_interval)
    raise E2EFailure(f"API did not recover within {timeout}s: {last_error}")


def wait_for_instances_ready(
    client: APIClient,
    targets: Sequence[WorkerTarget],
    timeout: float,
    poll_interval: float,
    *,
    previous_boots: Mapping[str, str] | None = None,
) -> dict[str, WorkerInstance]:
    expected = {target.instance_id for target in targets}
    if not expected:
        raise E2EFailure("no worker instances were supplied for readiness verification")
    if previous_boots is not None:
        missing_boots = sorted(instance_id for instance_id in expected if not previous_boots.get(instance_id))
        if missing_boots:
            raise E2EFailure(f"cannot prove boot changes; previous boot_id missing for {missing_boots}")
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    last_seen: dict[str, WorkerInstance] = {}
    while time.monotonic() < deadline:
        try:
            instances = client.get_instances(optional=False)
            last_seen = {instance.instance_id: instance for instance in instances}
            ready: dict[str, WorkerInstance] = {}
            for instance_id in expected:
                instance = last_seen.get(instance_id)
                if instance is None or not instance.is_ready or not instance.boot_id:
                    break
                if previous_boots is not None and instance.boot_id == previous_boots[instance_id]:
                    break
                ready[instance_id] = instance
            else:
                return ready
        except Exception as exc:  # API cold-start and connection-refused are expected here
            last_error = exc
        time.sleep(poll_interval)
    summary = {
        instance_id: {"boot_id": item.boot_id, "state": item.state}
        for instance_id, item in last_seen.items()
        if instance_id in expected
    }
    raise E2EFailure(
        f"instances did not become ready with the required boot identities within {timeout}s; "
        f"seen={summary!r}, last_error={last_error}"
    )


def validate_worker_mappings(
    controller: DockerController,
    workers: Sequence[WorkerTarget],
    instances: Mapping[str, WorkerInstance],
) -> list[dict[str, str]]:
    controller.assert_present()
    evidence: list[dict[str, str]] = []
    for worker in workers:
        instance = instances.get(worker.instance_id)
        if instance is None:
            raise E2EFailure(
                f"mapped instance {worker.instance_id} is not present in the instances API"
            )
        explicit, hostname = controller.configured_instance_identity(worker.container)
        if explicit:
            if explicit != worker.instance_id:
                raise E2EFailure(
                    f"container mapping mismatch for {worker.container}: "
                    f"configured instance_id={explicit!r}, requested={worker.instance_id!r}"
                )
            source = "container-environment"
        elif hostname == worker.instance_id:
            source = "container-hostname"
        else:
            raise E2EFailure(
                f"cannot prove instance/container mapping for {worker.instance_id}={worker.container}; "
                f"container instance env is empty and hostname is {hostname!r}"
            )
        evidence.append(
            {
                "instance_id": worker.instance_id,
                "container": worker.container,
                "boot_id": instance.boot_id,
                "source": source,
            }
        )
    return evidence


def target_instance(
    runner: ClusterE2ERunner,
    workers: list[WorkerTarget],
    requested: str,
    timeout: float,
) -> tuple[WorkerTarget, WorkerInstance, QueueSnapshot, set[str]]:
    by_id = {worker.instance_id: worker for worker in workers}
    if requested and requested not in by_id:
        raise E2EFailure(f"--fault-instance {requested!r} is not present in --worker mappings")
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = runner.sample_queue()
        instances = snapshot.instances or runner.client.get_instances(optional=False)
        candidates = [instance for instance in instances if instance.instance_id in by_id]
        if requested:
            candidates = [instance for instance in candidates if instance.instance_id == requested]
        for instance in candidates:
            owned = set(instance.active_documents) & set(runner.observations)
            if not owned:
                owned = {
                    knowledge_id
                    for knowledge_id, item in snapshot.items.items()
                    if knowledge_id in runner.observations and item.owner_instance_id == instance.instance_id
                }
            if owned:
                return by_id[instance.instance_id], instance, snapshot, owned
        time.sleep(runner.poll_interval)
    raise E2EFailure(
        "no mapped worker owned a tracked active document before fault injection; "
        "increase --documents or --generated-size-kib"
    )


def wait_for_boot_change(
    runner: ClusterE2ERunner,
    target: WorkerTarget,
    old_boot: str,
    timeout: float,
) -> WorkerInstance:
    return wait_for_instances_ready(
        runner.client,
        [target],
        timeout,
        runner.poll_interval,
        previous_boots={target.instance_id: old_boot},
    )[target.instance_id]


def unfinished_owned_at_fault(
    runner: ClusterE2ERunner,
    owner_instance: str,
    candidates: set[str],
    before: QueueSnapshot,
    boundary: str,
) -> set[str]:
    runner._refresh_terminal_states()
    unfinished: set[str] = set()
    invalid: list[str] = []
    for knowledge_id in candidates:
        observation = runner.observations.get(knowledge_id)
        if observation is None:
            invalid.append(f"{knowledge_id}:not-tracked")
            continue
        if observation.final_status:
            continue
        item = before.items.get(knowledge_id)
        if item is None:
            invalid.append(f"{knowledge_id}:queue-item-missing")
            continue
        if item.owner_instance_id != owner_instance:
            invalid.append(
                f"{knowledge_id}:owner={item.owner_instance_id or '<empty>'},expected={owner_instance}"
            )
            continue
        unfinished.add(knowledge_id)
    if invalid:
        raise E2EFailure(
            f"{boundary} boundary ownership could not be proven for mapped worker: {invalid}"
        )
    if not unfinished:
        raise E2EFailure(
            f"all target-owned documents completed at the {boundary} boundary; "
            "increase --documents or --generated-size-kib"
        )
    return unfinished


def _item_progress_changed(before: Any, after: Any) -> bool:
    if before is None or after is None:
        return False
    return any(
        getattr(before, field, None) != getattr(after, field, None)
        for field in ("stage", "lease_until", "last_progress_at", "execution_epoch")
    )


def wait_for_stable_resume(
    runner: ClusterE2ERunner,
    target: WorkerTarget,
    new_boot: str,
    formerly_owned: set[str],
    before: QueueSnapshot,
    timeout: float,
) -> dict[str, Any]:
    evidence: dict[str, dict[str, Any]] = {}
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = runner.sample_queue()
        runner._refresh_terminal_states()
        instances = snapshot.instances or runner.client.get_instances(optional=False)
        current = next(
            (
                instance for instance in instances
                if instance.instance_id == target.instance_id
                and instance.boot_id == new_boot
                and instance.is_ready
            ),
            None,
        )
        if current is None:
            time.sleep(runner.poll_interval)
            continue
        for knowledge_id in formerly_owned:
            if knowledge_id in evidence:
                continue
            observation = runner.observations[knowledge_id]
            foreign_owners = sorted(observation.owners - {target.instance_id})
            if foreign_owners:
                raise E2EFailure(
                    f"stable restart document {knowledge_id} was claimed by another instance: "
                    f"{foreign_owners}"
                )
            item = snapshot.items.get(knowledge_id)
            if item is not None and item.owner_instance_id not in {"", target.instance_id}:
                raise E2EFailure(
                    f"stable restart changed owner for {knowledge_id}: "
                    f"{target.instance_id} -> {item.owner_instance_id}"
                )
            if observation.final_status:
                if observation.final_status != "completed":
                    raise E2EFailure(
                        f"stable restart ended {knowledge_id} as {observation.final_status}"
                    )
                evidence[knowledge_id] = {
                    "proof": "terminal-after-new-boot-without-foreign-owner",
                    "instance_id": target.instance_id,
                    "boot_id": new_boot,
                    "final_status": observation.final_status,
                }
                continue
            if item is None or item.owner_instance_id != target.instance_id:
                continue
            if knowledge_id in current.active_documents:
                evidence[knowledge_id] = {
                    "proof": "active-on-new-boot",
                    "instance_id": target.instance_id,
                    "boot_id": new_boot,
                    "execution_epoch": item.execution_epoch,
                }
                continue
            before_item = before.items.get(knowledge_id)
            if _item_progress_changed(before_item, item):
                evidence[knowledge_id] = {
                    "proof": "owner-progress-on-new-boot",
                    "instance_id": target.instance_id,
                    "boot_id": new_boot,
                    "before_epoch": before_item.execution_epoch if before_item else None,
                    "after_epoch": item.execution_epoch,
                }
        if len(evidence) == len(formerly_owned):
            return {"documents": evidence}
        time.sleep(runner.poll_interval)
    missing = sorted(formerly_owned - set(evidence))
    raise E2EFailure(
        f"new boot {target.instance_id}/{new_boot} did not prove resume or terminal state "
        f"for every kill-boundary document within {timeout}s: {missing}"
    )


def wait_for_takeover(
    runner: ClusterE2ERunner,
    failed_instance: str,
    formerly_owned: set[str],
    before: QueueSnapshot,
    timeout: float,
) -> dict[str, Any]:
    before_epochs = {
        knowledge_id: before.items.get(knowledge_id).execution_epoch
        if before.items.get(knowledge_id)
        else None
        for knowledge_id in formerly_owned
    }
    takeover_seen: dict[str, dict[str, Any]] = {}
    owner_changed: dict[str, str] = {}
    epoch_advanced: dict[str, tuple[int, int]] = {}
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = runner.sample_queue()
        runner._refresh_terminal_states()
        for knowledge_id in formerly_owned:
            if knowledge_id in takeover_seen:
                continue
            observation = runner.observations[knowledge_id]
            item = snapshot.items.get(knowledge_id)
            if item and item.owner_instance_id and item.owner_instance_id != failed_instance:
                owner_changed[knowledge_id] = item.owner_instance_id
            before_epoch = before_epochs.get(knowledge_id)
            if item and before_epoch is not None and item.execution_epoch is not None and item.execution_epoch > before_epoch:
                epoch_advanced[knowledge_id] = (before_epoch, item.execution_epoch)
            if knowledge_id in owner_changed and knowledge_id in epoch_advanced:
                before_epoch_value, after_epoch = epoch_advanced[knowledge_id]
                takeover_seen[knowledge_id] = {
                    "proof": "owner-changed-and-epoch-advanced",
                    "new_owner": owner_changed[knowledge_id],
                    "before_epoch": before_epoch_value,
                    "after_epoch": after_epoch,
                }
                continue
            if observation.final_status:
                if observation.final_status != "completed":
                    raise E2EFailure(
                        f"takeover document {knowledge_id} ended as {observation.final_status}"
                    )
                survivor_owners = sorted(observation.owners - {failed_instance})
                if not survivor_owners:
                    raise E2EFailure(
                        f"{knowledge_id} completed after owner termination, but no surviving owner "
                        "was ever observed; survivor completion cannot be proven"
                    )
                takeover_seen[knowledge_id] = {
                    "proof": "completed-by-observed-survivor",
                    "survivor_owners": survivor_owners,
                    "before_epoch": before_epoch,
                }
        if len(takeover_seen) == len(formerly_owned):
            return {"documents": takeover_seen}
        time.sleep(runner.poll_interval)
    missing = sorted(formerly_owned - set(takeover_seen))
    raise E2EFailure(
        f"takeover was not proven for every document formerly owned by {failed_instance} "
        f"within {timeout}s: {missing}"
    )


def wait_for_termination_attestation(
    client: APIClient,
    target: WorkerTarget,
    boot_id: str,
    timeout: float,
    poll_interval: float,
) -> None:
    deadline = time.monotonic() + timeout
    last_error: Exception | None = None
    while time.monotonic() < deadline:
        try:
            client.attest_instance_termination(
                target.instance_id,
                boot_id,
                f"docker-inspect-confirmed-not-running:{target.container}",
            )
            return
        except APIError as exc:
            last_error = exc
            if exc.status not in {409, 423}:
                raise
        time.sleep(poll_interval)
    raise E2EFailure(
        f"exact termination attestation for {target.instance_id}/{boot_id} "
        f"was not accepted within {timeout}s: {last_error}"
    )


def assert_no_takeover_while_paused(
    runner: ClusterE2ERunner,
    owner_instance: str,
    formerly_owned: set[str],
    before: QueueSnapshot,
    duration: float,
) -> dict[str, Any]:
    before_epochs = {
        knowledge_id: before.items.get(knowledge_id).execution_epoch
        if before.items.get(knowledge_id)
        else None
        for knowledge_id in formerly_owned
    }
    deadline = time.monotonic() + duration
    samples = 0
    terminal_during_pause: set[str] = set()
    while time.monotonic() < deadline:
        runner.client.system_info()
        snapshot = runner.sample_queue()
        runner._refresh_terminal_states()
        samples += 1
        for knowledge_id in formerly_owned:
            observation = runner.observations[knowledge_id]
            if observation.final_status:
                terminal_during_pause.add(knowledge_id)
                continue
            item = snapshot.items.get(knowledge_id)
            if item is None:
                raise E2EFailure(
                    f"paused non-terminal document {knowledge_id} disappeared from the queue; "
                    "ownership safety cannot be proven"
                )
            if item.owner_instance_id != owner_instance:
                raise E2EFailure(
                    f"paused owner was lost or replaced for {knowledge_id}: "
                    f"{owner_instance} -> {item.owner_instance_id or '<empty>'}"
                )
            before_epoch = before_epochs.get(knowledge_id)
            if before_epoch is not None and item.execution_epoch is None:
                raise E2EFailure(
                    f"paused document {knowledge_id} lost execution_epoch evidence "
                    f"(before={before_epoch})"
                )
            if before_epoch is not None and item.execution_epoch != before_epoch:
                raise E2EFailure(
                    f"paused owner epoch changed without termination proof for {knowledge_id}: "
                    f"{before_epoch} -> {item.execution_epoch}"
                )
        time.sleep(runner.poll_interval)
    return {
        "samples": samples,
        "takeover_blocked": True,
        "terminal_during_pause": sorted(terminal_during_pause),
        "continuously_owned": sorted(formerly_owned - terminal_during_pause),
    }


def wait_downtime(seconds: float) -> None:
    if seconds <= 0:
        return
    deadline = time.monotonic() + seconds
    while time.monotonic() < deadline:
        time.sleep(min(1.0, deadline - time.monotonic()))


def wait_for_survivor_owned(
    runner: ClusterE2ERunner,
    api_instance: str,
    timeout: float,
) -> tuple[QueueSnapshot, set[str]]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = runner.sample_queue()
        runner._refresh_terminal_states()
        owned = {
            knowledge_id
            for knowledge_id, item in snapshot.items.items()
            if knowledge_id in runner.observations
            and not runner.observations[knowledge_id].final_status
            and item.owner_instance_id
            and item.owner_instance_id != api_instance
        }
        if owned:
            return snapshot, owned
        time.sleep(runner.poll_interval)
    raise E2EFailure(
        "no non-terminal document was owned by a surviving worker before the API restart; "
        "increase --documents or --generated-size-kib"
    )


def wait_for_survivor_progress(
    runner: ClusterE2ERunner,
    api_instance: str,
    candidates: set[str],
    before: QueueSnapshot,
    timeout: float,
) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        snapshot = runner.sample_queue()
        runner._refresh_terminal_states()
        evidence: dict[str, dict[str, Any]] = {}
        for knowledge_id in candidates:
            observation = runner.observations[knowledge_id]
            old_item = before.items.get(knowledge_id)
            item = snapshot.items.get(knowledge_id)
            survivor_owners = sorted(observation.owners - {api_instance})
            if observation.final_status:
                if observation.final_status != "completed" or not survivor_owners:
                    continue
                evidence[knowledge_id] = {
                    "proof": "survivor-terminal-across-api-restart",
                    "survivor_owners": survivor_owners,
                }
                continue
            if item is None or not item.owner_instance_id or item.owner_instance_id == api_instance:
                continue
            if item.owner_instance_id != (old_item.owner_instance_id if old_item else ""):
                evidence[knowledge_id] = {
                    "proof": "survivor-owner-changed-across-api-restart",
                    "owner": item.owner_instance_id,
                }
                continue
            if _item_progress_changed(old_item, item):
                evidence[knowledge_id] = {
                    "proof": "survivor-progress-across-api-restart",
                    "owner": item.owner_instance_id,
                    "before_epoch": old_item.execution_epoch if old_item else None,
                    "after_epoch": item.execution_epoch,
                }
        if evidence:
            return {"documents": evidence}
        time.sleep(runner.poll_interval)
    raise E2EFailure(
        f"no surviving worker progress was proven for API-outage documents within {timeout}s: "
        f"{sorted(candidates)}"
    )


def verify_scenario(
    runner: ClusterE2ERunner,
    ids: list[str],
    started: float,
    started_at: str,
    args: argparse.Namespace,
) -> dict[str, Any]:
    runner.wait_for_completion(args.completion_timeout)
    expected = {"summary", "questions", "graph", "wiki"}
    # Verify every faulted document, not a sample: duplicate IDs, post-terminal
    # count drift and failed vector retrieval are all double-write/lost-write evidence.
    runner.verify_document_outputs(
        ids,
        expected=expected,
        sample_limit=len(ids),
        wiki_timeout=args.completion_timeout,
    )
    result = runner.result(started, started_at)
    validate_performance(
        result,
        min_throughput=0,
        max_p95_processing_seconds=0,
        baseline_report=None,
        min_scaling_efficiency=0,
    )
    return asdict(result)


def run_scenario(
    args: argparse.Namespace,
    scenario: str,
    suite_dir: Path,
    client: APIClient,
) -> dict[str, Any]:
    scenario_dir = suite_dir / scenario
    recorder = JsonlRecorder(scenario_dir / "events.jsonl")
    runner = ClusterE2ERunner(
        client,
        args.kb_id,
        recorder,
        run_id=f"durable-{scenario}-{int(time.time())}",
        poll_interval=args.poll_interval,
        expected_derivatives={"summary", "questions", "graph", "wiki"},
    )
    workers: list[WorkerTarget] = args.worker
    controller = DockerController([worker.container for worker in workers], recorder)
    started_at = utc_now()
    started = time.monotonic()
    ids: list[str] = []
    stopped: set[str] = set()
    stopped_worker_boots: dict[str, tuple[WorkerTarget, str]] = {}
    paused: set[str] = set()
    redis_controller: DockerController | None = None
    evidence: dict[str, Any] = {}
    try:
        runner.api_smoke()
        initial_instances = wait_for_instances_ready(
            client,
            workers,
            args.activity_timeout,
            args.poll_interval,
        )
        evidence["instance_container_mappings"] = validate_worker_mappings(
            controller,
            workers,
            initial_instances,
        )
        ids = runner.upload_batch(
            args.documents,
            upload_concurrency=args.upload_concurrency,
            process_config=None,
            generated_size_kib=args.generated_size_kib,
        )
        initial = runner.sample_queue()
        runner.assert_queue_positions(initial)

        if scenario == "stable-reboot":
            target, instance, before, owned = target_instance(
                runner, workers, args.fault_instance, args.activity_timeout,
            )
            controller.stop(target.container, hard_kill=True)
            stopped.add(target.container)
            stopped_worker_boots[target.container] = (target, instance.boot_id)
            owned = unfinished_owned_at_fault(
                runner, target.instance_id, owned, before, "kill",
            )
            controller.start(target.container)
            stopped.remove(target.container)
            new_instance = wait_for_boot_change(
                runner, target, instance.boot_id, args.takeover_timeout,
            )
            stopped_worker_boots.pop(target.container, None)
            resume_evidence = wait_for_stable_resume(
                runner,
                target,
                new_instance.boot_id,
                owned,
                before,
                args.takeover_timeout,
            )
            evidence.update(
                {
                    "stable_instance_id": target.instance_id,
                    "old_boot_id": instance.boot_id,
                    "new_boot_id": new_instance.boot_id,
                    "owned_at_kill": sorted(owned),
                    "epochs_at_kill": {
                        key: before.items[key].execution_epoch
                        for key in owned
                    },
                    "resume": resume_evidence,
                }
            )
        elif scenario == "cross-instance-takeover":
            target, instance, before, owned = target_instance(
                runner, workers, args.fault_instance, args.activity_timeout,
            )
            controller.stop(target.container, hard_kill=True)
            stopped.add(target.container)
            stopped_worker_boots[target.container] = (target, instance.boot_id)
            owned = unfinished_owned_at_fault(
                runner, target.instance_id, owned, before, "kill",
            )
            wait_for_termination_attestation(
                client,
                target,
                instance.boot_id,
                args.takeover_timeout,
                args.poll_interval,
            )
            takeover_evidence = wait_for_takeover(
                runner, target.instance_id, owned, before, args.takeover_timeout,
            )
            evidence.update(takeover_evidence)
            evidence.update(
                {
                    "failed_instance": target.instance_id,
                    "failed_boot_id": instance.boot_id,
                    "owned_at_kill": sorted(owned),
                    "termination_attested_after_stopped_inspect": True,
                }
            )
        elif scenario == "paused-old-owner":
            target, _instance, before, owned = target_instance(
                runner, workers, args.fault_instance, args.activity_timeout,
            )
            controller.pause(target.container)
            paused.add(target.container)
            pause_started = time.monotonic()
            safety_evidence: dict[str, Any] | None = None
            try:
                owned = unfinished_owned_at_fault(
                    runner, target.instance_id, owned, before, "pause",
                )
                safety_evidence = assert_no_takeover_while_paused(
                    runner, target.instance_id, owned, before, args.pause_seconds,
                )
            finally:
                controller.unpause(target.container)
                paused.remove(target.container)
            evidence.update(
                {
                    "paused_instance": target.instance_id,
                    "paused_seconds": time.monotonic() - pause_started,
                    "owned_at_pause": sorted(owned),
                    "safety": safety_evidence,
                }
            )
        elif scenario == "redis-restart":
            if not args.redis_container:
                raise E2EFailure("--redis-container is required for redis-restart")
            runner.wait_for_some_activity(args.activity_timeout)
            redis_controller = DockerController([args.redis_container], recorder)
            redis_controller.stop(args.redis_container, hard_kill=True)
            stopped.add(args.redis_container)
            wait_downtime(args.fault_down_seconds)
            redis_controller.start(args.redis_container)
            stopped.remove(args.redis_container)
            redis_controller.wait_redis_ping(
                args.redis_container,
                timeout=args.takeover_timeout,
            )
            wait_for_api(client, args.takeover_timeout, args.poll_interval)
            evidence.update(
                {
                    "redis_container": args.redis_container,
                    "down_seconds": args.fault_down_seconds,
                    "stopped_inspect_verified": True,
                    "running_inspect_verified": True,
                    "redis_ping": "PONG",
                    "api_recovered": True,
                    "durable_source": "PostgreSQL workflow outbox",
                }
            )
        elif scenario == "fleet-restart":
            runner.wait_for_some_activity(args.activity_timeout)
            before_fleet = wait_for_instances_ready(
                client, workers, args.activity_timeout, args.poll_interval,
            )
            previous_boots = {
                instance_id: instance.boot_id
                for instance_id, instance in before_fleet.items()
            }
            for worker in workers:
                controller.stop(worker.container, hard_kill=True)
                stopped.add(worker.container)
                stopped_worker_boots[worker.container] = (
                    worker,
                    previous_boots[worker.instance_id],
                )
            wait_downtime(args.fault_down_seconds)
            for worker in workers:
                controller.start(worker.container)
                stopped.remove(worker.container)
            wait_for_api(client, args.takeover_timeout, args.poll_interval)
            after_fleet = wait_for_instances_ready(
                client,
                workers,
                args.takeover_timeout,
                args.poll_interval,
                previous_boots=previous_boots,
            )
            stopped_worker_boots.clear()
            evidence.update(
                {
                    "restarted_instances": [worker.instance_id for worker in workers],
                    "down_seconds": args.fault_down_seconds,
                    "old_boot_ids": previous_boots,
                    "new_boot_ids": {
                        instance_id: instance.boot_id
                        for instance_id, instance in after_fleet.items()
                    },
                    "all_instances_ready": True,
                    "api_cold_start_recovered": True,
                }
            )
        elif scenario == "api-restart":
            if not args.fault_instance:
                raise E2EFailure("api-restart requires --fault-instance for the API-owning worker")
            by_id = {worker.instance_id: worker for worker in workers}
            target = by_id.get(args.fault_instance)
            if target is None:
                raise E2EFailure("api-restart --fault-instance must be present in --worker mappings")
            before_api, survivor_owned = wait_for_survivor_owned(
                runner, target.instance_id, args.activity_timeout,
            )
            old_instance = wait_for_instances_ready(
                client,
                [target],
                args.activity_timeout,
                args.poll_interval,
            )[target.instance_id]
            controller.stop(target.container, hard_kill=True)
            stopped.add(target.container)
            stopped_worker_boots[target.container] = (target, old_instance.boot_id)
            wait_downtime(args.fault_down_seconds)
            controller.start(target.container)
            stopped.remove(target.container)
            wait_for_api(client, args.takeover_timeout, args.poll_interval)
            new_instance = wait_for_boot_change(
                runner, target, old_instance.boot_id, args.takeover_timeout,
            )
            stopped_worker_boots.pop(target.container, None)
            survivor_evidence = wait_for_survivor_progress(
                runner,
                target.instance_id,
                survivor_owned,
                before_api,
                args.takeover_timeout,
            )
            evidence.update(
                {
                    "api_instance": target.instance_id,
                    "old_boot_id": old_instance.boot_id,
                    "new_boot_id": new_instance.boot_id,
                    "api_cold_start_recovered": True,
                    "survivor_owned_at_kill": sorted(survivor_owned),
                    "survivor_progress": survivor_evidence,
                }
            )
        else:  # pragma: no cover - argparse prevents this
            raise E2EFailure(f"unknown durability scenario {scenario}")

        result = verify_scenario(runner, ids, started, started_at, args)
        recorder.emit("durable_scenario.passed", scenario=scenario, evidence=evidence, result=result)
        return {
            "scenario": scenario,
            "status": "passed",
            "evidence": evidence,
            "result": result,
            "events": str((scenario_dir / "events.jsonl").resolve()),
        }
    finally:
        cleanup_failures: list[str] = []
        for container in list(paused):
            try:
                controller.unpause(container)
            except Exception as exc:
                recorder.emit("cleanup.unpause_failed", container=container, error=str(exc))
                cleanup_failures.append(f"unpause {container}: {exc}")
        for container in list(stopped):
            try:
                active_controller = redis_controller if container == args.redis_container else controller
                if active_controller is not None:
                    active_controller.start(container)
                    if container == args.redis_container and redis_controller is not None:
                        redis_controller.wait_redis_ping(
                            container,
                            timeout=args.takeover_timeout,
                        )
            except Exception as exc:
                recorder.emit("cleanup.restart_failed", container=container, error=str(exc))
                cleanup_failures.append(f"restart {container}: {exc}")
        if stopped_worker_boots:
            try:
                wait_for_api(client, args.takeover_timeout, args.poll_interval)
                recovered: dict[str, dict[str, str]] = {}
                for target, old_boot in stopped_worker_boots.values():
                    current = wait_for_boot_change(
                        runner, target, old_boot, args.takeover_timeout,
                    )
                    recovered[target.instance_id] = {
                        "old_boot_id": old_boot,
                        "new_boot_id": current.boot_id,
                        "state": current.state,
                    }
                evidence["cleanup_restarted_instances"] = recovered
            except Exception as exc:
                recorder.emit("cleanup.instance_readiness_failed", error=str(exc))
                cleanup_failures.append(f"worker readiness after cleanup restart: {exc}")
        elif args.redis_container in stopped:
            try:
                wait_for_api(client, args.takeover_timeout, args.poll_interval)
            except Exception as exc:
                recorder.emit("cleanup.api_readiness_failed", error=str(exc))
                cleanup_failures.append(f"API readiness after Redis cleanup restart: {exc}")
        if runner.observations and not args.keep_data:
            try:
                runner.cleanup()
            except Exception as exc:
                recorder.emit("cleanup.documents_failed", error=str(exc))
                cleanup_failures.append(f"document cleanup: {exc}")
        if cleanup_failures:
            raise E2EFailure("chaos cleanup/recovery verification failed: " + "; ".join(cleanup_failures))


def validate_args(args: argparse.Namespace) -> None:
    if args.skip_contracts and not args.scenario:
        raise E2EFailure("nothing selected: remove --skip-contracts or add at least one --scenario")
    if args.scenario and not args.allow_chaos:
        raise E2EFailure("--scenario requires --allow-chaos")
    if args.scenario and (not args.token or not args.kb_id):
        raise E2EFailure("chaos scenarios require --token and --kb-id")
    if args.scenario and len(args.worker) < 2:
        raise E2EFailure("chaos scenarios require at least two --worker INSTANCE_ID=CONTAINER mappings")
    if "redis-restart" in args.scenario and not args.allow_infrastructure_chaos:
        raise E2EFailure("redis-restart requires --allow-infrastructure-chaos")
    if "fleet-restart" in args.scenario and not args.allow_full_worker_outage:
        raise E2EFailure("fleet-restart requires --allow-full-worker-outage")
    if "fleet-restart" in args.scenario and len(args.worker) < 3:
        raise E2EFailure("fleet-restart requires mappings for all three document-processing instances")
    if "api-restart" in args.scenario and not args.fault_instance:
        raise E2EFailure("api-restart requires --fault-instance for the API-owning instance")
    if args.pause_seconds <= 0:
        raise E2EFailure("--pause-seconds must be positive")
    containers = [worker.container for worker in args.worker]
    instances = [worker.instance_id for worker in args.worker]
    if len(containers) != len(set(containers)) or len(instances) != len(set(instances)):
        raise E2EFailure("--worker instance IDs and container names must be unique")


def main() -> int:
    args = parser().parse_args()
    run_stamp = time.strftime("%Y%m%d-%H%M%S")
    run_dir = (REPO_ROOT / args.output_dir / run_stamp).resolve()
    run_dir.mkdir(parents=True, exist_ok=True)
    report: dict[str, Any] = {
        "suite": "document-workflow-durable-failover",
        "started_at": utc_now(),
        "config": {
            "contracts": not args.skip_contracts,
            "race": not args.no_race,
            "postgres_contract": args.postgres_contract,
            "redis_contract": args.redis_contract,
            "redis_contract_db": args.redis_contract_db,
            "scenarios": args.scenario,
            "workers": [asdict(worker) for worker in args.worker],
            "base_url": args.base_url,
            "kb_id": args.kb_id,
            "auth_mode": args.auth_mode,
            "admin_auth_mode": args.admin_auth_mode,
            "split_scope_auth": bool(args.admin_token),
            "fault_instance": args.fault_instance,
            "redis_container": args.redis_container,
            "documents": args.documents,
            "upload_concurrency": args.upload_concurrency,
            "generated_size_kib": args.generated_size_kib,
            "poll_interval_seconds": args.poll_interval,
            "activity_timeout_seconds": args.activity_timeout,
            "takeover_timeout_seconds": args.takeover_timeout,
            "completion_timeout_seconds": args.completion_timeout,
            "fault_down_seconds": args.fault_down_seconds,
            "pause_seconds": args.pause_seconds,
            "keep_data": args.keep_data,
            "allow_chaos": args.allow_chaos,
            "allow_infrastructure_chaos": args.allow_infrastructure_chaos,
            "allow_full_worker_outage": args.allow_full_worker_outage,
        },
        "contracts": [],
        "scenarios": [],
    }
    return_code = 0
    try:
        validate_args(args)
        if not args.skip_contracts:
            contract_results = run_contracts(args, run_dir)
            report["contracts"] = [asdict(result) for result in contract_results]
            failed = [result.name for result in contract_results if result.status != "passed"]
            if failed:
                raise E2EFailure(f"durability contracts failed: {failed}")

        if args.scenario:
            workload_client = APIClient(
                args.base_url,
                args.token,
                auth_mode=args.auth_mode,
                timeout=60,
            )
            control_client = APIClient(
                args.base_url,
                args.admin_token or args.token,
                auth_mode=args.admin_auth_mode if args.admin_token else args.auth_mode,
                timeout=60,
            )
            client = SplitScopeAPIClient(workload_client, control_client)
            for scenario in args.scenario:
                result = run_scenario(args, scenario, run_dir, client)
                report["scenarios"].append(result)
        report["status"] = "passed"
    except Exception as exc:
        report["status"] = "failed"
        report["error"] = str(exc)
        return_code = 1
    finally:
        report["finished_at"] = utc_now()
        report_path = run_dir / "durable_failover_report.json"
        report_path.write_text(
            json.dumps(report, ensure_ascii=False, indent=2, default=str) + "\n",
            encoding="utf-8",
        )
        print(f"report: {report_path}")
        if report.get("status") != "passed":
            print(f"ERROR: {report.get('error', 'suite failed')}", file=sys.stderr)
    return return_code


if __name__ == "__main__":
    raise SystemExit(main())
