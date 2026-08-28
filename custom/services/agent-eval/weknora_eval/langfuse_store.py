from __future__ import annotations

import os
import threading
import uuid
from typing import Any

from .dataset import dataset_sha256
from .models import CaseRun, CaseSpec, ExperimentRun, Split
from .runner import EvalRunner


class LangfuseStoreError(RuntimeError):
    pass


def _client():
    try:
        from langfuse import Langfuse
    except ImportError as exc:
        raise LangfuseStoreError("langfuse dependency is not installed") from exc
    host = os.environ.get("LANGFUSE_HOST", "").strip()
    public_key = os.environ.get("LANGFUSE_PUBLIC_KEY", "").strip()
    secret_key = os.environ.get("LANGFUSE_SECRET_KEY", "").strip()
    if not host or not public_key or not secret_key:
        raise LangfuseStoreError("LANGFUSE_HOST, LANGFUSE_PUBLIC_KEY and LANGFUSE_SECRET_KEY are required")
    return Langfuse(
        host=host,
        public_key=public_key,
        secret_key=secret_key,
        environment="agent-eval",
        release=os.environ.get("LANGFUSE_RELEASE", "agent-eval"),
        sample_rate=1.0,
    )


def default_dataset_name(cases: list[CaseSpec]) -> str:
    suite = cases[0].suite if cases else "empty"
    return f"weknora-agent-eval/{suite}/{dataset_sha256(cases)[:16]}"


def publish_dataset(cases: list[CaseSpec], name: str | None = None) -> str:
    if not cases:
        raise LangfuseStoreError("cannot publish an empty dataset")
    dataset_name = name or default_dataset_name(cases)
    client = _client()
    digest = dataset_sha256(cases)
    client.create_dataset(
        name=dataset_name,
        description="Immutable acceptable-answer contracts for WeKnora agent evaluation",
        metadata={
            "schema_version": 1,
            "dataset_sha256": digest,
            "suite": cases[0].suite,
            "owner": "codex",
        },
    )
    for case in cases:
        for attempt_index in range(1, case.repetitions + 1):
            client.create_dataset_item(
                id=str(
                    uuid.uuid5(
                        uuid.NAMESPACE_URL,
                        f"{dataset_name}:{case.case_id}:attempt-{attempt_index}",
                    )
                ),
                dataset_name=dataset_name,
                input={
                    "case": case.model_dump(mode="json"),
                    "attempt_index": attempt_index,
                },
                expected_output={
                    "contract_type": "acceptable-answer-contract",
                    "turns": [
                        {
                            "turn_id": turn.turn_id,
                            "contract": turn.contract.model_dump(mode="json"),
                        }
                        for turn in case.turns
                    ],
                },
                metadata={
                    "case_id": case.case_id,
                    "attempt_index": attempt_index,
                    "family_id": case.family_id,
                    "split": case.split.value,
                    "capabilities": [
                        capability.value for capability in case.capabilities
                    ],
                    "dataset_sha256": digest,
                },
            )
    client.flush()
    return dataset_name


def run_langfuse_experiment(
    cases: list[CaseSpec],
    runner: EvalRunner,
    *,
    selected_splits: set[Split],
    name: str,
    dataset_name: str | None = None,
    max_concurrency: int = 1,
) -> ExperimentRun:
    selected = [case for case in cases if case.enabled and case.split in selected_splits]
    if not selected:
        raise LangfuseStoreError("no cases selected for experiment")
    sut = runner.doctor()
    published_name = publish_dataset(selected, dataset_name)
    client = _client()
    dataset = client.get_dataset(published_name)
    lock = threading.Lock()
    results: dict[tuple[str, int], CaseRun] = {}
    planned_count = sum(case.repetitions for case in selected)

    def task(*, item: Any, **_: Any) -> dict[str, Any]:
        payload = item.input if isinstance(item.input, dict) else {}
        spec = CaseSpec.model_validate(payload["case"])
        attempt_index = int(payload.get("attempt_index") or 1)
        case_run = runner.run_case(spec, attempt_index=attempt_index)
        with lock:
            results[(spec.case_id, attempt_index)] = case_run
            completed_count = len(results)
        print(
            f"EVAL_PROGRESS completed={completed_count}/{planned_count} "
            f"case={spec.case_id} attempt={attempt_index} "
            f"verdict={case_run.verdict.value}",
            flush=True,
        )
        return case_run.model_dump(mode="json")

    def evaluator(*, output: Any, **_: Any) -> list[dict[str, Any]]:
        case_run = CaseRun.model_validate(output)
        values: list[dict[str, Any]] = [
            {
                "name": "weknora.verdict",
                "value": case_run.verdict.value,
                "comment": case_run.error or "three-state gate verdict",
            }
        ]
        for score in case_run.scores:
            values.append(
                {
                    "name": f"weknora.{score.name}",
                    "value": score.value,
                    "comment": score.comment,
                    "metadata": {
                        "hard": score.hard,
                        "passed": score.passed,
                        "turn_id": score.turn_id,
                    },
                }
            )
        return values

    experiment = dataset.run_experiment(
        name=name,
        run_name=name,
        description="Codex-operated WeKnora agent evaluation; WeKnora is the SUT and recorder only",
        task=task,
        evaluators=[evaluator],
        max_concurrency=max_concurrency,
        metadata={
            "dataset_sha256": dataset_sha256(cases),
            "sut_release": sut.release,
            "sut_commit": sut.commit,
            "mode": sut.mode,
        },
    )
    client.flush()
    ordered = [
        results[(case.case_id, attempt_index)]
        for case in selected
        for attempt_index in range(1, case.repetitions + 1)
        if (case.case_id, attempt_index) in results
    ]
    return ExperimentRun(
        run_id=str(experiment.dataset_run_id or f"run-{uuid.uuid4()}"),
        suite=selected[0].suite,
        dataset_sha256=dataset_sha256(cases),
        splits=sorted(selected_splits, key=lambda item: item.value),
        sut=sut,
        cases=ordered,
        metadata={
            "langfuse_dataset": published_name,
            "langfuse_dataset_run_id": experiment.dataset_run_id,
            "langfuse_dataset_run_url": experiment.dataset_run_url,
        },
    )
