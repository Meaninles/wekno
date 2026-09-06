from __future__ import annotations

import argparse
import json
import os
import sys
import threading
import time
from collections import Counter
from concurrent.futures import ThreadPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from weknora_eval.client import (
    WeKnoraAPIError,
    WeKnoraAssistantPersistenceTimeout,
    WeKnoraClient,
    WeKnoraResponseDeadlineExceeded,
    event_tool_name,
    event_type,
    streamed_production_candidate,
)


REQUIRED_MODEL_ID = "prod-deepseek-v4-flash-int8-chat"
PROFILES = {
    "quick-answer": {
        "display_name": "快速问答",
        "agent_id": "builtin-quick-answer",
        "endpoint": "knowledge-chat",
        "agent_enabled": False,
    },
    "rag-reasoning": {
        "display_name": "RAG推理",
        "agent_id": "builtin-smart-reasoning",
        "endpoint": "agent-chat",
        "agent_enabled": True,
    },
    "general-agent": {
        "display_name": "通用智能体",
        "agent_id": "builtin-general-agent",
        "endpoint": "agent-chat",
        "agent_enabled": True,
    },
}
UNSAFE_PLAN_KEYS = {
    "answer",
    "answers",
    "expected_answer",
    "gold_answer",
    "reference_answer",
    "references",
    "required_claims",
    "judge_feedback",
    "judge_rubric",
    "rubric",
}


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


def load_json(path: Path) -> dict[str, Any]:
    value = json.loads(path.read_text(encoding="utf-8"))
    if not isinstance(value, dict):
        raise ValueError(f"expected a JSON object: {path}")
    return value


def load_env(path: Path) -> dict[str, str]:
    values: dict[str, str] = {}
    for raw in path.read_text(encoding="utf-8").splitlines():
        line = raw.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, value = line.split("=", 1)
        values[key.strip()] = value
    return values


def assert_sut_safe(value: Any, *, path: str = "plan") -> None:
    if isinstance(value, dict):
        for key, item in value.items():
            normalized = str(key).strip().lower()
            if normalized in UNSAFE_PLAN_KEYS:
                raise ValueError(f"unsafe SUT replay field at {path}.{key}")
            assert_sut_safe(item, path=f"{path}.{key}")
    elif isinstance(value, list):
        for index, item in enumerate(value):
            assert_sut_safe(item, path=f"{path}[{index}]")


def compact_step_tools(value: Any) -> list[str]:
    names: list[str] = []
    if isinstance(value, dict):
        for key, item in value.items():
            if key in {"tool", "tool_name", "name"} and isinstance(item, str):
                if item in {
                    "knowledge_search",
                    "grep_chunks",
                    "list_knowledge_chunks",
                    "query_knowledge_graph",
                    "web_search",
                    "web_fetch",
                }:
                    names.append(item)
            names.extend(compact_step_tools(item))
    elif isinstance(value, list):
        for item in value:
            names.extend(compact_step_tools(item))
    return names


def stream_with_queue_retry(
    client: WeKnoraClient,
    path: str,
    payload: dict[str, Any],
    *,
    max_queue_wait_seconds: float = 3600.0,
) -> tuple[list[dict[str, Any]], int, int, int, int]:
    """Honor bounded harness concurrency without treating server backpressure as SUT failure."""

    queue_started = time.perf_counter()
    retries = 0
    while True:
        try:
            events, ttfb_ms, total_latency_ms = client.stream(path, payload)
            queue_wait_ms = int((time.perf_counter() - queue_started) * 1000) - total_latency_ms
            return events, ttfb_ms, total_latency_ms, retries, max(0, queue_wait_ms)
        except WeKnoraResponseDeadlineExceeded:
            raise
        except WeKnoraAPIError as exc:
            message = str(exc)
            if "CHAT_QUEUE_USER_LIMIT" not in message and "CHAT_QUEUE_FULL" not in message:
                raise
            elapsed = time.perf_counter() - queue_started
            if elapsed >= max_queue_wait_seconds:
                raise
            retries += 1
            time.sleep(min(10.0, 1.0 + retries))


def atomic_write_json(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    temporary = path.with_suffix(path.suffix + ".tmp")
    temporary.write_text(
        json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    os.replace(temporary, path)


def validate_environment(
    client: WeKnoraClient,
    plan: dict[str, Any],
    settings: dict[str, str],
    state: dict[str, Any],
) -> dict[str, str]:
    assert_sut_safe({"cases": plan.get("cases")})
    required_false_flags = (
        "source_answers_in_sut_input",
        "source_references_in_sut_input",
        "reference_answers_in_sut_input",
        "required_claims_in_sut_input",
        "judge_feedback_in_sut_input",
        "sealed_holdout_accessed",
        "eval_assistance_enabled",
    )
    for key in required_false_flags:
        if plan.get(key) is not False:
            raise ValueError(f"replay plan requires {key}=false")
    if plan.get("required_model_id") != REQUIRED_MODEL_ID:
        raise ValueError("replay plan does not require DS V4 Flash")
    if settings.get("AGENT_EVAL_SUMMARY_MODEL_ID") != REQUIRED_MODEL_ID:
        raise ValueError("runner.env is not pinned to DS V4 Flash")

    capabilities = client.capabilities()
    if capabilities.get("mode") != "production":
        raise ValueError("target is not running in production mode")
    if capabilities.get("recorder_enabled") is not False:
        raise ValueError("production target unexpectedly has Eval recorder enabled")

    model = client.request("GET", f"/models/{REQUIRED_MODEL_ID}")
    model_data = model.get("data") if isinstance(model, dict) else None
    if not isinstance(model_data, dict):
        raise ValueError("DS V4 Flash model is not inspectable")
    if str(model_data.get("id") or "") != REQUIRED_MODEL_ID:
        raise ValueError("resolved model ID differs from DS V4 Flash")
    if str(model_data.get("status") or "") != "active":
        raise ValueError("DS V4 Flash model is not active")

    for profile in PROFILES.values():
        agent_id = str(profile["agent_id"])
        agent = client.get_agent(agent_id)
        config = agent.get("config")
        if not isinstance(config, dict):
            raise ValueError(f"agent has no inspectable config: {agent_id}")
        query_model_id = str(config.get("query_understand_model_id") or "")
        if query_model_id != REQUIRED_MODEL_ID:
            raise ValueError(
                f"agent query-understand model is not DS V4 Flash: {agent_id}"
            )

    bindings: dict[str, str] = {}
    knowledge_bases = state.get("knowledge_bases")
    if not isinstance(knowledge_bases, dict):
        raise ValueError("import state has no knowledge base bindings")
    for case in plan.get("cases") or []:
        if not isinstance(case, dict):
            raise ValueError("replay case is not an object")
        slug = str(case.get("kb_slug") or "")
        binding = knowledge_bases.get(slug)
        if not isinstance(binding, dict):
            raise ValueError(f"missing local knowledge base binding: {slug}")
        local_id = str(binding.get("local_knowledge_base_id") or "")
        if not local_id:
            raise ValueError(f"empty local knowledge base binding: {slug}")
        bindings[slug] = local_id
        turns = case.get("turns")
        if not isinstance(turns, list) or not turns:
            raise ValueError(f"case has no turns: {case.get('case_id')}")
        for turn in turns:
            if set(turn) - {"source_ordinal", "query"}:
                raise ValueError(
                    f"turn contains non-production-input fields: {case.get('case_id')}"
                )
            if not str(turn.get("query") or "").strip():
                raise ValueError(f"case contains an empty query: {case.get('case_id')}")
    return bindings


def run_conversation(
    *,
    base_url: str,
    api_key: str,
    timeout: float,
    case: dict[str, Any],
    profile_id: str,
    repetition: int,
    knowledge_base_id: str,
) -> dict[str, Any]:
    profile = PROFILES[profile_id]
    client = WeKnoraClient(base_url, api_key, timeout=timeout)
    started_at = utc_now()
    result: dict[str, Any] = {
        "run_id": f"{case['case_id']}::{profile_id}::r{repetition}",
        "case_id": case["case_id"],
        "source_session_alias": case["source_session_alias"],
        "source_agent_kind": case["source_agent_kind"],
        "kb_slug": case["kb_slug"],
        "family": case["family"],
        "high_usage_user": bool(case.get("high_usage_user")),
        "profile_id": profile_id,
        "profile_display_name": profile["display_name"],
        "agent_id": profile["agent_id"],
        "model_id": REQUIRED_MODEL_ID,
        "repetition": repetition,
        "started_at": started_at,
        "turns": [],
        "error": None,
    }
    try:
        session_id = client.create_session()
        result["session_id"] = session_id
        seen_message_ids: set[str] = set()
        for turn_index, turn in enumerate(case["turns"], start=1):
            payload = {
                "query": str(turn["query"]),
                "knowledge_base_ids": [knowledge_base_id],
                "knowledge_ids": [],
                "agent_enabled": bool(profile["agent_enabled"]),
                "agent_id": profile["agent_id"],
                "web_search_enabled": False,
                "summary_model_id": REQUIRED_MODEL_ID,
                "disable_title": False,
                "channel": "web",
            }
            observed: dict[str, Any] = {
                "turn_index": turn_index,
                "source_ordinal": int(turn["source_ordinal"]),
                "query": str(turn["query"]),
                "request_contract": {
                    "knowledge_base_ids": [knowledge_base_id],
                    "knowledge_ids": [],
                    "agent_enabled": bool(profile["agent_enabled"]),
                    "agent_id": profile["agent_id"],
                    "web_search_enabled": False,
                    "summary_model_id": REQUIRED_MODEL_ID,
                },
            }
            try:
                (
                    events,
                    ttfb_ms,
                    total_latency_ms,
                    queue_retry_count,
                    queue_wait_ms,
                ) = stream_with_queue_retry(
                    client, f"/{profile['endpoint']}/{session_id}", payload
                )
                streamed_content = streamed_production_candidate(events)
                try:
                    message = client.load_completed_assistant(
                        session_id,
                        exclude_message_ids=seen_message_ids,
                        wait_seconds=15.0,
                    )
                except WeKnoraAssistantPersistenceTimeout:
                    message = {}
                message_id = str(message.get("id") or "")
                if message_id:
                    seen_message_ids.add(message_id)
                content = str(message.get("content") or "")
                references = [
                    item
                    for item in message.get("knowledge_references") or []
                    if isinstance(item, dict)
                ]
                steps = [
                    item
                    for item in message.get("agent_steps") or []
                    if isinstance(item, dict)
                ]
                event_tools = [
                    name for event in events if (name := event_tool_name(event))
                ]
                tools = list(
                    dict.fromkeys(event_tools + compact_step_tools(steps))
                )
                errors = [
                    event
                    for event in events
                    if event_type(event) == "error"
                ]
                terminal_errors = [
                    event
                    for event in errors
                    if event.get("done") is True
                    and isinstance(event.get("data"), dict)
                    and bool(str(event["data"].get("stage") or "").strip())
                ]
                complete = bool(message.get("is_completed")) and bool(content.strip())
                normalized_content = content.lstrip()
                persisted_runtime_error = (
                    normalized_content.startswith("ResultMessage(")
                    and "is_error=True" in normalized_content
                ) or normalized_content.startswith("API Error:")
                if terminal_errors:
                    observation_error = "terminal_stream_error"
                elif persisted_runtime_error:
                    observation_error = "persisted_runtime_error"
                elif not complete:
                    observation_error = "response_incomplete"
                else:
                    observation_error = None
                observed.update(
                    {
                        "message_id": message_id,
                        "content": content,
                        "streamed_content": streamed_content,
                        "stream_persistence_match": (
                            streamed_content == content
                            if streamed_content is not None and complete
                            else None
                        ),
                        "is_completed": bool(message.get("is_completed")),
                        "references": references,
                        "retrieval_stats": (
                            message.get("retrieval_stats")
                            if isinstance(message.get("retrieval_stats"), dict)
                            else {}
                        ),
                        "tools": tools,
                        "agent_mode": bool(message.get("agent_mode")),
                        "agent_tool_count": int(message.get("agent_tool_count") or 0),
                        "ttfb_ms": ttfb_ms,
                        "total_latency_ms": total_latency_ms,
                        "queue_retry_count": queue_retry_count,
                        "queue_wait_ms": queue_wait_ms,
                        "event_count": len(events),
                        "event_type_counts": dict(
                            Counter(event_type(event) for event in events)
                        ),
                        "stream_errors": errors,
                        "terminal_stream_errors": terminal_errors,
                        "error": observation_error,
                    }
                )
            except WeKnoraResponseDeadlineExceeded as exc:
                observed.update(
                    {
                        "content": "",
                        "streamed_content": streamed_production_candidate(exc.events),
                        "stream_persistence_match": None,
                        "is_completed": False,
                        "references": [],
                        "retrieval_stats": {},
                        "tools": list(
                            dict.fromkeys(
                                name
                                for event in exc.events
                                if (name := event_tool_name(event))
                            )
                        ),
                        "agent_mode": bool(profile["agent_enabled"]),
                        "agent_tool_count": 0,
                        "ttfb_ms": exc.ttfb_ms,
                        "total_latency_ms": exc.total_latency_ms,
                        "event_count": len(exc.events),
                        "event_type_counts": dict(
                            Counter(event_type(event) for event in exc.events)
                        ),
                        "stream_errors": [],
                        "error": "response_deadline_exceeded",
                    }
                )
            except Exception as exc:  # observation must survive one failed turn
                observed.update(
                    {
                        "content": "",
                        "streamed_content": None,
                        "stream_persistence_match": None,
                        "is_completed": False,
                        "references": [],
                        "retrieval_stats": {},
                        "tools": [],
                        "agent_mode": bool(profile["agent_enabled"]),
                        "agent_tool_count": 0,
                        "ttfb_ms": None,
                        "total_latency_ms": None,
                        "event_count": 0,
                        "event_type_counts": {},
                        "stream_errors": [],
                        "error": f"{type(exc).__name__}: {exc}",
                    }
                )
            result["turns"].append(observed)
            if observed.get("error"):
                break
    except Exception as exc:
        result["error"] = f"{type(exc).__name__}: {exc}"
    result["finished_at"] = utc_now()
    return result


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser(
        description="Replay production conversations through the unassisted production path."
    )
    root = Path(__file__).resolve().parent
    parser.add_argument(
        "--plan",
        type=Path,
        default=root
        / "artifacts/production-replay-v2/tests/production-equivalent-replay-plan.v1.json",
    )
    parser.add_argument("--env", type=Path, default=root / "runner.env")
    parser.add_argument(
        "--state",
        type=Path,
        default=root
        / "artifacts/production-replay-v2/kb-manifests/import-state.json",
    )
    parser.add_argument(
        "--output",
        type=Path,
        default=root
        / "artifacts/production-replay-v2/tests/production-equivalent-replay-run.v1.json",
    )
    parser.add_argument("--base-url", default="http://localhost:8080/api/v1")
    parser.add_argument("--repetitions", type=int, default=3)
    parser.add_argument("--concurrency", type=int, default=8)
    parser.add_argument("--timeout-seconds", type=float, default=420.0)
    parser.add_argument(
        "--max-cases",
        type=int,
        default=0,
        help="Smoke-test only; zero runs the complete plan.",
    )
    parser.add_argument(
        "--case-id",
        action="append",
        default=[],
        help="Run only an exact case_id; repeat the option to select multiple cases.",
    )
    parser.add_argument(
        "--validate-only",
        action="store_true",
        help="Validate isolation, model, agents, and KB bindings without sending a chat.",
    )
    parser.add_argument(
        "--retry-queue-failures",
        action="store_true",
        help=(
            "Replace only conversations that failed with an explicit chat queue "
            "capacity error in an existing output artifact."
        ),
    )
    return parser.parse_args()


def main() -> int:
    args = parse_args()
    if args.repetitions < 3:
        raise ValueError("production reproduction requires at least three repetitions")
    if args.concurrency < 1:
        raise ValueError("concurrency must be positive")
    plan = load_json(args.plan)
    settings = load_env(args.env)
    state = load_json(args.state)
    api_key = settings.get("WEKNORA_E2E_TENANT_API_KEY", "").strip()
    if not api_key:
        raise ValueError("runner.env has no tenant API key")
    validator = WeKnoraClient(args.base_url, api_key, timeout=30.0)
    bindings = validate_environment(validator, plan, settings, state)

    cases = list(plan.get("cases") or [])
    if args.case_id:
        selected = set(args.case_id)
        known = {str(case.get("case_id") or "") for case in cases}
        unknown = sorted(selected - known)
        if unknown:
            raise ValueError(f"unknown case_id selection: {', '.join(unknown)}")
        cases = [case for case in cases if str(case.get("case_id") or "") in selected]
    if args.max_cases:
        cases = cases[: args.max_cases]
    profiles = list(PROFILES)
    if args.validate_only:
        print(
            "Replay validation complete: "
            f"cases={len(cases)} turns={sum(len(case['turns']) for case in cases)} "
            f"profiles={len(profiles)} knowledge_bases={len(bindings)} "
            f"mode=production recorder=false "
            f"answer_model={REQUIRED_MODEL_ID} "
            f"query_understand_model={REQUIRED_MODEL_ID}",
            flush=True,
        )
        return 0
    if args.retry_queue_failures:
        artifact = load_json(args.output)
        case_by_id = {str(case["case_id"]): case for case in cases}
        queue_markers = ("CHAT_QUEUE_USER_LIMIT", "CHAT_QUEUE_FULL")
        retry_indexes = [
            index
            for index, item in enumerate(artifact.get("results") or [])
            if any(
                any(marker in str(turn.get("error") or "") for marker in queue_markers)
                for turn in item.get("turns") or []
            )
        ]
        if not retry_indexes:
            print("No explicit queue-capacity failures found to retry.", flush=True)
            return 0
        replacements: list[dict[str, Any]] = []
        for index in retry_indexes:
            previous = artifact["results"][index]
            case_id = str(previous["case_id"])
            case = case_by_id[case_id]
            print(f"Retrying queue-capacity failure: {previous['run_id']}", flush=True)
            replacement = run_conversation(
                base_url=args.base_url,
                api_key=api_key,
                timeout=args.timeout_seconds,
                case=case,
                profile_id=str(previous["profile_id"]),
                repetition=int(previous["repetition"]),
                knowledge_base_id=bindings[str(case["kb_slug"])],
            )
            artifact["results"][index] = replacement
            replacements.append(
                {
                    "run_id": replacement["run_id"],
                    "retried_at": replacement["finished_at"],
                    "replacement_error": replacement.get("error")
                    or next(
                        (
                            turn.get("error")
                            for turn in replacement.get("turns") or []
                            if turn.get("error")
                        ),
                        None,
                    ),
                }
            )
            atomic_write_json(args.output, artifact)
        artifact.setdefault("execution", {})["queue_capacity_retries"] = replacements
        artifact["finished_at"] = utc_now()
        atomic_write_json(args.output, artifact)
        remaining = sum(
            1
            for item in artifact.get("results") or []
            for turn in item.get("turns") or []
            if any(marker in str(turn.get("error") or "") for marker in queue_markers)
        )
        print(
            f"Queue-capacity retry complete: replaced={len(replacements)} "
            f"remaining_queue_failures={remaining}",
            flush=True,
        )
        return 0
    work = [
        (case, profile_id, repetition)
        for case in cases
        for profile_id in profiles
        for repetition in range(1, args.repetitions + 1)
    ]
    artifact: dict[str, Any] = {
        "schema_version": 1,
        "started_at": utc_now(),
        "finished_at": None,
        "status": "running",
        "purpose": "production-equivalent reproduction and qualitative analysis only",
        "target": {
            "base_url": args.base_url,
            "mode": "production",
            "eval_recorder_enabled": False,
            "model_id": REQUIRED_MODEL_ID,
            "query_understand_model_id": REQUIRED_MODEL_ID,
            "eval_assistance_enabled": False,
            "web_search_enabled": False,
        },
        "execution": {
            "case_count": len(cases),
            "profile_count": len(profiles),
            "repetitions": args.repetitions,
            "concurrency": args.concurrency,
            "conversation_count": len(work),
            "smoke_limited": bool(args.max_cases),
            "selected_case_ids": list(args.case_id),
        },
        "plan_path": str(args.plan.resolve()),
        "results": [],
    }
    atomic_write_json(args.output, artifact)
    print(
        "Starting production-equivalent replay: "
        f"cases={len(cases)} profiles={len(profiles)} "
        f"repetitions={args.repetitions} conversations={len(work)} "
        f"concurrency={args.concurrency} model={REQUIRED_MODEL_ID}",
        flush=True,
    )

    write_lock = threading.Lock()
    completed = 0
    with ThreadPoolExecutor(max_workers=args.concurrency) as executor:
        futures = {
            executor.submit(
                run_conversation,
                base_url=args.base_url,
                api_key=api_key,
                timeout=args.timeout_seconds,
                case=case,
                profile_id=profile_id,
                repetition=repetition,
                knowledge_base_id=bindings[str(case["kb_slug"])],
            ): (case["case_id"], profile_id, repetition)
            for case, profile_id, repetition in work
        }
        for future in as_completed(futures):
            result = future.result()
            with write_lock:
                artifact["results"].append(result)
                completed += 1
                if completed % 6 == 0 or completed == len(work):
                    errors = sum(
                        1
                        for item in artifact["results"]
                        if item.get("error")
                        or any(turn.get("error") for turn in item.get("turns") or [])
                    )
                    print(
                        f"Progress {completed}/{len(work)} conversations; "
                        f"observed failures={errors}",
                        flush=True,
                    )
                atomic_write_json(args.output, artifact)

    artifact["results"].sort(
        key=lambda item: (
            item["case_id"], item["profile_id"], item["repetition"]
        )
    )
    artifact["finished_at"] = utc_now()
    artifact["status"] = "completed"
    atomic_write_json(args.output, artifact)
    observed_turns = sum(len(item.get("turns") or []) for item in artifact["results"])
    failures = sum(
        1
        for item in artifact["results"]
        if item.get("error")
        or any(turn.get("error") for turn in item.get("turns") or [])
    )
    print(
        f"Replay complete: conversations={len(work)} turns={observed_turns} "
        f"observed_failures={failures} output={args.output.resolve()}",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
