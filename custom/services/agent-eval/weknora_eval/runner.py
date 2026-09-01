from __future__ import annotations

import os
import uuid
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

from .assistance import BoundedEvalAssistant, EvalAssistant, EvalAssistantConfig
from .client import (
    WeKnoraClient,
    WeKnoraAssistantPersistenceTimeout,
    WeKnoraResponseDeadlineExceeded,
    event_tool_name,
    event_type,
    streamed_production_candidate,
)
from .dataset import dataset_sha256
from .models import (
    AnswerSnapshot,
    CaseRun,
    CaseSetup,
    CaseSpec,
    ExperimentRun,
    KnowledgeSelectionMode,
    ObservedTurn,
    RepairTrace,
    ReviewMode,
    SUT_RESPONSE_DEADLINE_EXCEEDED,
    SUT_RESPONSE_INCOMPLETE,
    SUT_STREAM_ERROR,
    SUT_TURN_SKIPPED_AFTER_DEADLINE,
    SUT_TURN_SKIPPED_AFTER_FAILURE,
    SUTFingerprint,
    Split,
    Verdict,
)
from .scoring import score_case, score_case_tracks


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
    def __init__(
        self,
        client: WeKnoraClient,
        assistant: EvalAssistant | None = None,
    ) -> None:
        self.client = client
        if assistant is None:
            assistant_config = EvalAssistantConfig.from_env()
            assistant = (
                BoundedEvalAssistant(client, assistant_config)
                if assistant_config is not None
                else None
            )
        self.assistant = assistant

    def _assert_knowledge_selection(self, spec: CaseSpec) -> None:
        """Fail closed when the runtime agent contradicts the Eval KB contract."""

        setups = [
            _merge_setup(spec.setup, turn.setup_override)
            for turn in spec.turns
        ]
        constrained = {
            setup.knowledge_selection_mode
            for setup in setups
            if setup.knowledge_selection_mode != KnowledgeSelectionMode.AGENT_DEFAULT
        }
        if not constrained:
            return

        agent = self.client.get_agent(spec.agent.agent_id)
        config = agent.get("config")
        if not isinstance(config, dict):
            raise EvalModeRequired(
                f"agent {spec.agent.agent_id!r} has no inspectable runtime config"
            )
        runtime_mode = str(config.get("kb_selection_mode") or "").strip()
        configured_kbs = [str(item) for item in config.get("knowledge_bases") or []]

        if KnowledgeSelectionMode.NONE in constrained:
            if runtime_mode != "none" or configured_kbs:
                raise EvalModeRequired(
                    "knowledge_selection_mode=none requires an agent configured with "
                    "kb_selection_mode=none and no bound knowledge bases"
                )
        if KnowledgeSelectionMode.EXPLICIT in constrained and runtime_mode == "none":
            raise EvalModeRequired(
                "knowledge_selection_mode=explicit cannot use an agent configured with "
                "kb_selection_mode=none"
            )

    def doctor(self) -> SUTFingerprint:
        raw = dict(self.client.capabilities())
        mode = str(raw.get("mode") or "")
        if mode != "eval":
            raise EvalModeRequired(
                f"refusing evaluation execution against mode={mode!r}; production is record-only"
            )
        if raw.get("recorder_enabled") is not True:
            raise EvalModeRequired("refusing evaluation execution because the OTLP recorder is disabled")
        if str(raw.get("capture_policy") or "") != "full":
            raise EvalModeRequired(
                "refusing evaluation execution because eval mode requires full capture"
            )
        reported_commit = str(raw.get("commit") or "").strip()
        source_commit = os.environ.get("AGENT_EVAL_SUT_COMMIT", "").strip()
        if reported_commit and source_commit and reported_commit != source_commit:
            raise EvalModeRequired(
                "refusing evaluation execution because the running SUT commit "
                f"{reported_commit!r} differs from the orchestrated source commit {source_commit!r}"
            )
        raw.update(
            {
                "reported_commit": reported_commit,
                "source_commit": source_commit,
                "worktree_dirty": os.environ.get(
                    "AGENT_EVAL_SUT_WORKTREE_DIRTY", ""
                ).strip(),
                "runtime_image_id": os.environ.get(
                    "AGENT_EVAL_RUNTIME_IMAGE_ID", ""
                ).strip(),
                "general_agent_image_id": os.environ.get(
                    "AGENT_EVAL_GENERAL_AGENT_IMAGE_ID", ""
                ).strip(),
            }
        )
        return SUTFingerprint(
            mode=mode,
            release=str(raw.get("release") or ""),
            commit=reported_commit or source_commit,
            environment=str(raw.get("environment") or ""),
            capabilities=[str(item) for item in raw.get("capabilities") or []],
            raw=raw,
        )

    def run_case(self, spec: CaseSpec, *, attempt_index: int = 1) -> CaseRun:
        case_run = CaseRun(
            case_id=spec.case_id,
            family_id=spec.family_id,
            split=spec.split,
            agent_profile_id=spec.agent_profile_id,
            attempt_index=attempt_index,
            verdict=Verdict.INVALID,
        )
        if not spec.enabled:
            return case_run.model_copy(update={"error": "case disabled"})
        try:
            self._assert_knowledge_selection(spec)
            session_id = self.client.create_session()
            seen_message_ids: set[str] = set()
            observed_turns: list[ObservedTurn] = []
            for turn_index, turn_spec in enumerate(spec.turns):
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
                try:
                    events, ttfb_ms, total_latency_ms = self.client.stream(
                        f"/{spec.agent.endpoint}/{session_id}", payload
                    )
                except WeKnoraResponseDeadlineExceeded as exc:
                    event_tools = [
                        name for event in exc.events if (name := event_tool_name(event))
                    ]
                    observed_turns.append(
                        ObservedTurn(
                            turn_id=turn_spec.turn_id,
                            session_id=session_id,
                            tools=list(dict.fromkeys(event_tools)),
                            ttfb_ms=exc.ttfb_ms,
                            total_latency_ms=exc.total_latency_ms,
                            event_count=len(exc.events),
                            error=SUT_RESPONSE_DEADLINE_EXCEEDED,
                            production_candidate=AnswerSnapshot(),
                            eval_assisted_answer=AnswerSnapshot(),
                        )
                    )
                    observed_turns.extend(
                        ObservedTurn(
                            turn_id=remaining.turn_id,
                            session_id=session_id,
                            error=SUT_TURN_SKIPPED_AFTER_DEADLINE,
                            production_candidate=AnswerSnapshot(),
                            eval_assisted_answer=AnswerSnapshot(),
                        )
                        for remaining in spec.turns[turn_index + 1 :]
                    )
                    break
                stream_errors = [
                    event for event in events if event_type(event) == "error" and event.get("done") is True
                ]
                streamed_content = streamed_production_candidate(events)
                terminal_stream_errors = [
                    event
                    for event in stream_errors
                    if isinstance(event.get("data"), dict)
                    and bool(str(event["data"].get("stage") or "").strip())
                ]
                try:
                    message = self.client.load_completed_assistant(
                        session_id, exclude_message_ids=seen_message_ids
                    )
                except WeKnoraAssistantPersistenceTimeout:
                    message = {}
                message_id = str(message.get("id") or "")
                if message_id:
                    seen_message_ids.add(message_id)
                steps = [item for item in message.get("agent_steps") or [] if isinstance(item, dict)]
                event_tools = [name for event in events if (name := event_tool_name(event))]
                tools = list(dict.fromkeys([*event_tools, *_tools_from_steps(steps)]))
                content = str(message.get("content") or "")
                completed = bool(message.get("is_completed")) and bool(content.strip())
                normalized_content = content.lstrip()
                persisted_runtime_error = (
                    normalized_content.startswith("ResultMessage(")
                    and "is_error=True" in normalized_content
                ) or normalized_content.startswith("API Error:")
                if terminal_stream_errors:
                    # WeKnora's top-level execution errors carry a stage and
                    # terminate the SSE stream. The handler may still persist a
                    # completed assistant row containing a user-facing error;
                    # that row is not a recovered business answer and must not
                    # masquerade as one in the eval artifact.
                    error = f"{SUT_STREAM_ERROR}: {terminal_stream_errors[-1]}"
                elif completed and not persisted_runtime_error:
                    # A tool/model step may emit an error event and then recover.
                    # The persisted completed answer is the source of truth.
                    error = None
                elif stream_errors:
                    error = f"{SUT_STREAM_ERROR}: {stream_errors[-1]}"
                elif persisted_runtime_error:
                    error = f"{SUT_STREAM_ERROR}: persisted runtime error payload"
                else:
                    error = SUT_RESPONSE_INCOMPLETE
                observed = ObservedTurn(
                    turn_id=turn_spec.turn_id,
                    session_id=session_id,
                    message_id=message_id,
                    content=content,
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
                    streamed_content=streamed_content,
                    production_surface_equivalent=(
                        streamed_content == content
                        if streamed_content is not None and completed
                        else None
                    ),
                    production_candidate=AnswerSnapshot(
                        content=content,
                        references=[
                            item
                            for item in message.get("knowledge_references") or []
                            if isinstance(item, dict)
                        ],
                        retrieval_stats=(
                            message.get("retrieval_stats")
                            if isinstance(message.get("retrieval_stats"), dict)
                            else {}
                        ),
                    ),
                    eval_assisted_answer=AnswerSnapshot(
                        content=content,
                        references=[
                            item
                            for item in message.get("knowledge_references") or []
                            if isinstance(item, dict)
                        ],
                        retrieval_stats=(
                            message.get("retrieval_stats")
                            if isinstance(message.get("retrieval_stats"), dict)
                            else {}
                        ),
                    ),
                )
                observed_turns.append(observed)
                if error:
                    observed_turns.extend(
                        ObservedTurn(
                            turn_id=remaining.turn_id,
                            session_id=session_id,
                            error=SUT_TURN_SKIPPED_AFTER_FAILURE,
                            production_candidate=AnswerSnapshot(),
                            eval_assisted_answer=AnswerSnapshot(),
                        )
                        for remaining in spec.turns[turn_index + 1 :]
                    )
                    break
            case_run = case_run.model_copy(update={"turns": observed_turns})
            production_scored = score_case(spec, case_run)
            if self.assistant is not None and not case_run.error:
                assisted_turns: list[ObservedTurn] = []
                for turn_index, observed in enumerate(observed_turns):
                    production = observed.production_candidate or AnswerSnapshot(
                        content=observed.content,
                        references=observed.references,
                        retrieval_stats=observed.retrieval_stats,
                    )
                    hard_failure = any(
                        score.turn_id == observed.turn_id
                        and score.hard
                        and score.passed is False
                        for score in production_scored.scores
                    ) and spec.review_mode != ReviewMode.CODEX_CONVERSATION
                    if observed.error or not production.content.strip():
                        assisted_turns.append(observed)
                        continue
                    setup = _merge_setup(
                        spec.setup,
                        spec.turns[turn_index].setup_override,
                    )
                    try:
                        assisted, trace = self.assistant.assist(
                            query=spec.turns[turn_index].query,
                            prior_user_statements=[
                                item.query for item in spec.turns[:turn_index]
                            ],
                            setup=setup,
                            production=production,
                            max_response_chars=spec.turns[
                                turn_index
                            ].contract.max_response_chars,
                            force=hard_failure,
                        )
                    except Exception as exc:
                        assisted = production
                        trace = RepairTrace(
                            triggered=True,
                            failure_reason=(
                                "Eval assistance failed open: "
                                f"{type(exc).__name__}: {exc}"
                            ),
                        )
                    assisted_turns.append(
                        observed.model_copy(
                            update={
                                "eval_assisted_answer": assisted,
                                "repair": trace,
                            }
                        )
                    )
                case_run = case_run.model_copy(update={"turns": assisted_turns})
            return score_case_tracks(spec, case_run)
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
        planned = [
            (case, attempt_index)
            for case in selected
            for attempt_index in range(1, case.repetitions + 1)
        ]
        results_by_key: dict[tuple[str, int], CaseRun] = {}
        if max_concurrency <= 1:
            for index, (case, attempt_index) in enumerate(planned, start=1):
                result = self.run_case(
                    case,
                    attempt_index=attempt_index,
                )
                results_by_key[(case.case_id, attempt_index)] = result
                print(
                    f"EVAL_PROGRESS completed={index}/{len(planned)} "
                    f"case={case.case_id} attempt={attempt_index} "
                    f"verdict={result.verdict.value}",
                    flush=True,
                )
        else:
            with ThreadPoolExecutor(max_workers=max_concurrency) as executor:
                futures = {
                    executor.submit(self.run_case, case, attempt_index=attempt_index): (
                        case.case_id,
                        attempt_index,
                    )
                    for case, attempt_index in planned
                }
                for future in as_completed(futures):
                    key = futures[future]
                    result = future.result()
                    results_by_key[key] = result
                    print(
                        f"EVAL_PROGRESS completed={len(results_by_key)}/{len(planned)} "
                        f"case={key[0]} attempt={key[1]} "
                        f"verdict={result.verdict.value}",
                        flush=True,
                    )
        ordered = [
            results_by_key[(case.case_id, attempt_index)]
            for case, attempt_index in planned
        ]
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
