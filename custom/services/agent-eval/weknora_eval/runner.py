from __future__ import annotations

import os
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from .client import WeKnoraClient, event_tool_name, event_type
from .dataset import dataset_sha256
from .models import (
    CaseRun,
    CaseSetup,
    CaseSpec,
    ExperimentRun,
    ObservedTurn,
    SUTFingerprint,
    Split,
    Verdict,
)
from .scoring import score_case


class EvalModeRequired(RuntimeError):
    pass


def _merge_setup(base: CaseSetup, override: CaseSetup | None) -> CaseSetup:
    if override is None:
        return base
    return base.model_copy(update=override.model_dump(exclude_unset=True))


def _tools_from_steps(steps: list[dict[str, Any]]) -> list[str]:
    result: list[str] = []
    for step in steps:
        for call in step.get("tool_calls") or []:
            if isinstance(call, dict) and call.get("name"):
                result.append(str(call["name"]))
    return result


class EvalRunner:
    def __init__(self, client: WeKnoraClient) -> None:
        self.client = client

    def doctor(self) -> SUTFingerprint:
        raw = self.client.capabilities()
        mode = str(raw.get("mode") or "")
        if mode != "eval":
            raise EvalModeRequired(
                f"refusing evaluation execution against mode={mode!r}; production is record-only"
            )
        if raw.get("recorder_enabled") is not True:
            raise EvalModeRequired("refusing evaluation execution because the OTLP recorder is disabled")
        return SUTFingerprint(
            mode=mode,
            release=str(raw.get("release") or ""),
            commit=str(raw.get("commit") or ""),
            environment=str(raw.get("environment") or ""),
            capabilities=[str(item) for item in raw.get("capabilities") or []],
            raw=raw,
        )

    def run_case(self, spec: CaseSpec) -> CaseRun:
        case_run = CaseRun(
            case_id=spec.case_id,
            family_id=spec.family_id,
            split=spec.split,
            verdict=Verdict.INVALID,
        )
        if not spec.enabled:
            return case_run.model_copy(update={"error": "case disabled"})
        try:
            session_id = self.client.create_session()
            seen_message_ids: set[str] = set()
            observed_turns: list[ObservedTurn] = []
            for turn_spec in spec.turns:
                setup = _merge_setup(spec.setup, turn_spec.setup_override)
                model_id = setup.summary_model_id or os.environ.get("AGENT_EVAL_SUMMARY_MODEL_ID", "").strip()
                if not model_id:
                    raise ValueError(
                        f"{spec.case_id}/{turn_spec.turn_id}: summary_model_id is required in setup or AGENT_EVAL_SUMMARY_MODEL_ID"
                    )
                payload = {
                    "query": turn_spec.query,
                    "knowledge_base_ids": setup.knowledge_base_ids,
                    "knowledge_ids": setup.knowledge_ids,
                    "agent_enabled": spec.agent.endpoint == "agent-chat",
                    "agent_id": spec.agent.agent_id,
                    "web_search_enabled": setup.web_search_enabled,
                    "summary_model_id": model_id,
                    "disable_title": False,
                    "channel": setup.channel,
                }
                events, ttfb_ms, total_latency_ms = self.client.stream(
                    f"/{spec.agent.endpoint}/{session_id}", payload
                )
                stream_errors = [
                    event for event in events if event_type(event) == "error" and event.get("done") is True
                ]
                message = self.client.load_completed_assistant(
                    session_id, exclude_message_ids=seen_message_ids
                )
                message_id = str(message.get("id") or "")
                if message_id:
                    seen_message_ids.add(message_id)
                steps = [item for item in message.get("agent_steps") or [] if isinstance(item, dict)]
                event_tools = [name for event in events if (name := event_tool_name(event))]
                tools = list(dict.fromkeys([*event_tools, *_tools_from_steps(steps)]))
                error = str(stream_errors[-1]) if stream_errors else None
                observed = ObservedTurn(
                    turn_id=turn_spec.turn_id,
                    session_id=session_id,
                    message_id=message_id,
                    content=str(message.get("content") or ""),
                    references=[item for item in message.get("knowledge_references") or [] if isinstance(item, dict)],
                    retrieval_stats=message.get("retrieval_stats") if isinstance(message.get("retrieval_stats"), dict) else {},
                    tools=tools,
                    agent_steps=steps,
                    agent_mode=bool(message.get("agent_mode")),
                    agent_tool_count=int(message.get("agent_tool_count") or 0),
                    is_completed=bool(message.get("is_completed")),
                    ttfb_ms=ttfb_ms,
                    total_latency_ms=total_latency_ms,
                    event_count=len(events),
                    error=error,
                )
                observed_turns.append(observed)
                if error:
                    break
            case_run = case_run.model_copy(update={"turns": observed_turns})
            return score_case(spec, case_run)
        except Exception as exc:
            return case_run.model_copy(update={"error": str(exc), "verdict": Verdict.INVALID})

    def run_suite(
        self,
        cases: list[CaseSpec],
        *,
        selected_splits: set[Split],
        max_concurrency: int = 1,
        metadata: dict[str, Any] | None = None,
    ) -> ExperimentRun:
        sut = self.doctor()
        selected = [case for case in cases if case.enabled and case.split in selected_splits]
        results_by_id: dict[str, CaseRun] = {}
        if max_concurrency <= 1:
            for case in selected:
                results_by_id[case.case_id] = self.run_case(case)
        else:
            with ThreadPoolExecutor(max_workers=max_concurrency) as executor:
                futures = {executor.submit(self.run_case, case): case.case_id for case in selected}
                for future in as_completed(futures):
                    results_by_id[futures[future]] = future.result()
        ordered = [results_by_id[case.case_id] for case in selected]
        suites = {case.suite for case in selected}
        suite = next(iter(suites)) if len(suites) == 1 else "mixed"
        return ExperimentRun(
            run_id=f"run-{uuid.uuid4()}",
            suite=suite,
            dataset_sha256=dataset_sha256(cases),
            splits=sorted(selected_splits, key=lambda item: item.value),
            sut=sut,
            cases=ordered,
            metadata=metadata or {},
        )
