from __future__ import annotations

import json
import time
import uuid
from copy import deepcopy
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from .client import WeKnoraClient, event_tool_name, event_type
from .dataset import resolve_env
from .runner import EvalModeRequired


def _now() -> str:
    return datetime.now(timezone.utc).isoformat()


def load_discovery_scenario(path: str | Path) -> dict[str, Any]:
    source = Path(path)
    with source.open("r", encoding="utf-8") as handle:
        payload = resolve_env(json.load(handle))
    if payload.get("schema_version") != 1:
        raise ValueError("discovery scenario schema_version must be 1")
    if not str(payload.get("model_id") or "").strip():
        raise ValueError("discovery scenario requires model_id")
    sessions = payload.get("sessions")
    if not isinstance(sessions, list) or not sessions:
        raise ValueError("discovery scenario requires at least one session")
    profile_ids: set[str] = set()
    for session in sessions:
        if not isinstance(session, dict):
            raise ValueError("each discovery session must be an object")
        profile_id = str(session.get("profile_id") or "").strip()
        if not profile_id or profile_id in profile_ids:
            raise ValueError(f"invalid or duplicate profile_id: {profile_id!r}")
        profile_ids.add(profile_id)
        history_turns = int(session.get("history_turns") or 0)
        minimum_turns = int(session.get("minimum_turns") or 0)
        if history_turns < 1 or minimum_turns < history_turns + 2:
            raise ValueError(
                f"{profile_id}: minimum_turns must be at least history_turns + 2"
            )
        turns = session.get("turns")
        if not isinstance(turns, list) or len(turns) != 1:
            raise ValueError(
                f"{profile_id}: discovery scenario must contain exactly one seed turn; "
                "all later turns must come from a reviewed Codex adaptive-turn plan"
            )
        turn_ids = [str(turn.get("turn_id") or "") for turn in turns]
        if any(not item for item in turn_ids) or len(turn_ids) != len(set(turn_ids)):
            raise ValueError(f"{profile_id}: turn_id values must be non-empty and unique")
    return payload


ADAPTIVE_STRATEGIES = {
    "continue",
    "correct_and_continue",
    "retry_same_question",
    "resync_state",
}


def load_adaptive_turn_plan(
    path: str | Path,
    prior: dict[str, Any],
    *,
    profile_id: str,
) -> dict[str, Any]:
    """Load one Codex-authored continuation after validating its review link.

    The plan is intentionally a one-turn object. It cannot be used to queue a
    script of future questions, and it must point at the exact reviewed turn
    that currently ends the persisted discovery conversation.
    """

    source = Path(path)
    with source.open("r", encoding="utf-8") as handle:
        payload = resolve_env(json.load(handle))
    if payload.get("schema_version") != 1:
        raise ValueError("adaptive turn plan schema_version must be 1")
    if str(payload.get("profile_id") or "") != profile_id:
        raise ValueError("adaptive turn plan profile_id does not match --profile")

    sessions = [
        session
        for session in prior.get("sessions") or []
        if isinstance(session, dict) and str(session.get("profile_id") or "") == profile_id
    ]
    if len(sessions) != 1:
        raise ValueError(f"expected exactly one prior discovery session for {profile_id}")
    completed = [turn for turn in sessions[0].get("turns") or [] if isinstance(turn, dict)]
    if not completed:
        raise ValueError("adaptive continuation requires a completed seed turn")
    parent = completed[-1]
    parent_id = str(parent.get("turn_id") or "")
    if str(payload.get("parent_turn_id") or "") != parent_id:
        raise ValueError(
            f"adaptive turn parent must be the latest completed turn {parent_id}"
        )
    review = parent.get("human_review")
    if not isinstance(review, dict):
        raise ValueError(f"review {profile_id}/{parent_id} before planning the next turn")

    review_basis = payload.get("review_basis")
    if not isinstance(review_basis, dict):
        raise ValueError("adaptive turn plan requires review_basis")
    disposition = str(review.get("disposition") or "")
    if str(review_basis.get("previous_disposition") or "") != disposition:
        raise ValueError("adaptive turn plan disposition does not match the recorded review")
    strategy = str(review_basis.get("strategy") or "")
    if strategy not in ADAPTIVE_STRATEGIES:
        raise ValueError(f"unsupported adaptive strategy: {strategy!r}")
    if disposition == "rejected_answer" and strategy == "continue":
        raise ValueError(
            "a rejected answer must be corrected, retried, or state-resynchronized before continuing"
        )
    if not str(review_basis.get("reason") or "").strip():
        raise ValueError("adaptive turn plan requires a non-empty review_basis.reason")

    checks = payload.get("semantic_checks")
    if not isinstance(checks, dict):
        raise ValueError("adaptive turn plan requires semantic_checks")
    required_true = (
        "previous_answer_manually_reviewed",
        "current_prompt_matches_review_next_action",
        "does_not_assume_unverified_assistant_claims",
        "authoritative_user_state_reconciled",
    )
    failed_checks = [name for name in required_true if checks.get(name) is not True]
    if failed_checks:
        raise ValueError(
            "adaptive turn semantic checks must be explicitly true: " + ", ".join(failed_checks)
        )
    if str(checks.get("checked_by") or "").strip().lower() != "codex":
        raise ValueError("adaptive turn semantic_checks.checked_by must be codex")

    turn = payload.get("turn")
    if not isinstance(turn, dict):
        raise ValueError("adaptive turn plan requires exactly one turn object")
    expected_turn_id = f"turn-{len(completed) + 1:03d}"
    if str(turn.get("turn_id") or "") != expected_turn_id:
        raise ValueError(f"adaptive turn id must be {expected_turn_id}")
    if not str(turn.get("purpose") or "").strip():
        raise ValueError("adaptive turn requires a purpose")
    if not str(turn.get("user_message") or "").strip():
        raise ValueError("adaptive turn requires a user_message")

    return {
        "turn": deepcopy(turn),
        "adaptive_parent": {
            "turn_id": parent_id,
            "reviewed_at": str(review.get("reviewed_at") or ""),
            "disposition": disposition,
            "strategy": strategy,
            "reason": str(review_basis["reason"]),
            "semantic_checks": deepcopy(checks),
            "plan_source": source.name,
        },
    }


def _assistant_ids(turns: list[dict[str, Any]]) -> set[str]:
    return {
        str(turn.get("message_id"))
        for turn in turns
        if str(turn.get("message_id") or "").strip()
    }


def _event_usage(value: Any, result: dict[str, Any] | None = None) -> dict[str, Any]:
    found = result if result is not None else {}
    if isinstance(value, dict):
        for key, item in value.items():
            normalized = str(key).lower()
            if normalized in {
                "input_tokens",
                "output_tokens",
                "prompt_tokens",
                "completion_tokens",
                "total_tokens",
                "cached_tokens",
            } and isinstance(item, (int, float)):
                found[normalized] = max(float(item), float(found.get(normalized) or 0))
            elif normalized == "usage" or isinstance(item, (dict, list)):
                _event_usage(item, found)
    elif isinstance(value, list):
        for item in value:
            _event_usage(item, found)
    return found


def _tool_calls_from_steps(steps: list[dict[str, Any]]) -> list[dict[str, Any]]:
    calls: list[dict[str, Any]] = []
    for step in steps:
        iteration = step.get("iteration")
        for call in step.get("tool_calls") or []:
            if not isinstance(call, dict):
                continue
            calls.append(
                {
                    "iteration": iteration,
                    "id": str(call.get("id") or ""),
                    "name": str(call.get("name") or ""),
                    "args": deepcopy(call.get("args")),
                    "result": deepcopy(call.get("result")),
                    "duration_ms": int(call.get("duration") or 0),
                }
            )
    return calls


def _event_inventory(events: list[dict[str, Any]]) -> dict[str, Any]:
    event_types: dict[str, int] = {}
    event_tools: list[str] = []
    errors: list[dict[str, Any]] = []
    for event in events:
        kind = event_type(event) or "unknown"
        event_types[kind] = event_types.get(kind, 0) + 1
        if name := event_tool_name(event):
            event_tools.append(name)
        if kind == "error":
            errors.append(deepcopy(event))
    return {
        "event_types": event_types,
        "event_tools": list(dict.fromkeys(event_tools)),
        "errors": errors,
        "usage": _event_usage(events),
    }


def _apply_state_updates(
    active: dict[str, dict[str, Any]],
    retired: list[dict[str, Any]],
    turn: dict[str, Any],
) -> None:
    turn_id = str(turn["turn_id"])
    for update in turn.get("state_updates") or []:
        if not isinstance(update, dict):
            continue
        key = str(update.get("key") or "").strip()
        operation = str(update.get("operation") or "set").strip()
        if not key:
            raise ValueError(f"{turn_id}: state update key is required")
        previous = active.get(key)
        if operation in {"supersede", "clear"} and previous is not None:
            retired.append(
                {
                    **deepcopy(previous),
                    "retired_by_turn": turn_id,
                    "retired_reason": str(update.get("reason") or operation),
                }
            )
        if operation == "clear":
            active.pop(key, None)
            continue
        if operation not in {"set", "supersede"}:
            raise ValueError(f"{turn_id}: unsupported state operation {operation!r}")
        active[key] = {
            "key": key,
            "value": deepcopy(update.get("value")),
            "class": str(update.get("class") or "durable"),
            "origin_turn": turn_id,
            "note": str(update.get("note") or ""),
        }


def _context_observation(
    *,
    absolute_turn: int,
    history_turns: int,
    active_state: dict[str, dict[str, Any]],
    turn_positions: dict[str, int],
    requested_model_id: str,
    usage: dict[str, Any],
) -> dict[str, Any]:
    visible_start = max(1, absolute_turn - history_turns)
    outside: list[str] = []
    for key, fact in active_state.items():
        origin = turn_positions.get(str(fact.get("origin_turn") or ""), absolute_turn)
        if origin < visible_start and fact.get("class") == "durable":
            outside.append(key)
    return {
        "absolute_turn": absolute_turn,
        "configured_history_turns": history_turns,
        "crossed_history_window": absolute_turn > history_turns,
        "turns_beyond_history_window": max(0, absolute_turn - history_turns),
        "expected_visible_history_start_turn": visible_start,
        "durable_state_outside_window": sorted(outside),
        "requested_model_id": requested_model_id,
        "token_usage_observed": usage,
        "token_usage_available": bool(usage),
    }


def _prior_by_profile(payload: dict[str, Any] | None) -> dict[str, dict[str, Any]]:
    if not payload:
        return {}
    result: dict[str, dict[str, Any]] = {}
    for session in payload.get("sessions") or []:
        if isinstance(session, dict) and session.get("profile_id"):
            result[str(session["profile_id"])] = session
    return result


REVIEW_DISPOSITIONS = {
    "accepted_observation",
    "accepted_with_findings",
    "rejected_answer",
}


def assert_discovery_reviewed(
    payload: dict[str, Any],
    selected_profiles: set[str] | None = None,
) -> None:
    missing: list[str] = []
    for session in payload.get("sessions") or []:
        if not isinstance(session, dict):
            continue
        profile_id = str(session.get("profile_id") or "")
        if selected_profiles and profile_id not in selected_profiles:
            continue
        for turn in session.get("turns") or []:
            if isinstance(turn, dict) and not isinstance(turn.get("human_review"), dict):
                missing.append(f"{profile_id}/{turn.get('turn_id')}")
    if missing:
        raise ValueError(
            "review completed discovery turns before continuing: " + ", ".join(missing)
        )


def review_discovery_turn(
    payload: dict[str, Any],
    *,
    profile_id: str,
    turn_id: str,
    disposition: str,
    findings: list[str] | None = None,
    next_action: str = "",
) -> dict[str, Any]:
    if disposition not in REVIEW_DISPOSITIONS:
        raise ValueError(f"unsupported review disposition: {disposition}")
    matches: list[dict[str, Any]] = []
    for session in payload.get("sessions") or []:
        if not isinstance(session, dict) or str(session.get("profile_id") or "") != profile_id:
            continue
        matches.extend(
            turn
            for turn in session.get("turns") or []
            if isinstance(turn, dict) and str(turn.get("turn_id") or "") == turn_id
        )
    if len(matches) != 1:
        raise ValueError(f"expected exactly one completed turn for {profile_id}/{turn_id}")
    matches[0]["human_review"] = {
        "reviewer": "codex",
        "reviewed_at": _now(),
        "disposition": disposition,
        "findings": [str(item) for item in findings or [] if str(item).strip()],
        "next_action": str(next_action or ""),
        "eligible_as_gold": False,
    }
    payload["last_reviewed_at"] = _now()
    return payload


def _stream_error_text(inventory: dict[str, Any]) -> str | None:
    errors = inventory.get("errors") or []
    if not errors:
        return None
    latest = errors[-1]
    data = latest.get("data") if isinstance(latest.get("data"), dict) else {}
    return str(data.get("error") or latest.get("content") or latest)


def _failure_class(error: str) -> tuple[str, bool]:
    normalized = error.lower()
    if "403" in normalized and ("authenticate" in normalized or "forbidden" in normalized):
        return "upstream_authentication", True
    if "timeout" in normalized or "timed out" in normalized:
        return "timeout", True
    if "connection" in normalized or "unexpected eof" in normalized or "broken pipe" in normalized:
        return "transport", True
    if "not in search target scope" in normalized or "tool" in normalized and "scope" in normalized:
        return "tool_scope", False
    if "no new persisted assistant message" in normalized:
        return "persistence", True
    return "unknown", False


class DiscoveryCollector:
    """Runs unscored, human-led discovery conversations in the eval SUT.

    This intentionally has no dependency on scoring, judges, Langfuse datasets,
    experiments, or release gates. Its output is evidence for later Codex
    curation, not an evaluation verdict.
    """

    def __init__(self, client: WeKnoraClient) -> None:
        self.client = client

    def _assert_eval_environment(self) -> dict[str, Any]:
        capabilities = self.client.capabilities()
        if str(capabilities.get("mode") or "") != "eval":
            raise EvalModeRequired("discovery conversations may only run in the isolated eval environment")
        return capabilities

    def collect(
        self,
        scenario: dict[str, Any],
        *,
        prior: dict[str, Any] | None = None,
        selected_profiles: set[str] | None = None,
        adaptive_turns: dict[str, dict[str, Any]] | None = None,
        progress: Any | None = None,
    ) -> dict[str, Any]:
        capabilities = self._assert_eval_environment()
        model_id = str(scenario["model_id"])
        default_knowledge_ids = [str(item) for item in scenario.get("knowledge_ids") or []]
        prior_sessions = _prior_by_profile(prior)
        output: dict[str, Any] = {
            "schema_version": 1,
            "discovery_id": f"discovery-{uuid.uuid4()}",
            "scenario_id": str(scenario.get("scenario_id") or ""),
            "phase": str(scenario.get("phase") or "problem-finding"),
            "started_at": _now(),
            "completed_at": None,
            "formal_eval_executed": False,
            "model_id": model_id,
            "sut": capabilities,
            "sessions": [],
        }
        for session_spec in scenario["sessions"]:
            profile_id = str(session_spec["profile_id"])
            if selected_profiles and profile_id not in selected_profiles:
                continue
            previous = prior_sessions.get(profile_id)
            previous_turns = deepcopy(previous.get("turns") or []) if previous else []
            invalid_attempts = deepcopy(previous.get("invalid_attempts") or []) if previous else []
            active_state = deepcopy(previous.get("state_ledger", {}).get("active") or {}) if previous else {}
            retired_state = deepcopy(previous.get("state_ledger", {}).get("retired") or []) if previous else []
            session_id = str(previous.get("session_id") or "") if previous else ""
            if not session_id:
                session_id = self.client.create_session()
            seen_ids = _assistant_ids(previous_turns)
            history_turns = int(session_spec["history_turns"])
            absolute_offset = len(previous_turns)
            turn_positions = {
                str(turn.get("turn_id") or ""): index
                for index, turn in enumerate(previous_turns, 1)
            }
            session_result: dict[str, Any] = {
                "profile_id": profile_id,
                "agent_id": str(session_spec["agent_id"]),
                "agent_type": session_spec.get("agent_type"),
                "display_name": str(session_spec.get("display_name") or profile_id),
                "endpoint": str(session_spec["endpoint"]),
                "history_turns": history_turns,
                "minimum_turns": int(session_spec["minimum_turns"]),
                "business_goal": str(session_spec.get("business_goal") or ""),
                "session_id": session_id,
                "turns": previous_turns,
                "invalid_attempts": invalid_attempts,
                "state_ledger": {"active": active_state, "retired": retired_state},
                "error": None,
            }
            output["sessions"].append(session_result)
            completed_turn_ids = {
                str(turn.get("turn_id") or "")
                for turn in previous_turns
                if not turn.get("error")
            }
            adaptive_plan = (adaptive_turns or {}).get(profile_id)
            if previous:
                if not adaptive_plan:
                    raise ValueError(
                        f"{profile_id}: resumed discovery requires one reviewed Codex adaptive-turn plan"
                    )
                pending_turns = [adaptive_plan["turn"]]
            else:
                if adaptive_plan:
                    raise ValueError(f"{profile_id}: the first turn must use the scenario seed")
                pending_turns = [
                    turn for turn in session_spec["turns"]
                    if str(turn.get("turn_id") or "") not in completed_turn_ids
                ][:1]
            for relative_index, turn_spec in enumerate(pending_turns, 1):
                absolute_turn = absolute_offset + relative_index
                turn_id = str(turn_spec["turn_id"])
                turn_positions[turn_id] = absolute_turn
                active_before = deepcopy(active_state)
                retired_before = deepcopy(retired_state)
                _apply_state_updates(active_state, retired_state, turn_spec)
                setup = {
                    "knowledge_base_ids": [str(item) for item in session_spec.get("knowledge_base_ids") or []],
                    "knowledge_ids": [str(item) for item in session_spec.get("knowledge_ids") or default_knowledge_ids],
                    "web_search_enabled": bool(session_spec.get("web_search_enabled", False)),
                    "channel": str(session_spec.get("channel") or "agent-eval-discovery"),
                }
                if isinstance(turn_spec.get("setup_override"), dict):
                    setup.update(turn_spec["setup_override"])
                payload = {
                    "query": str(turn_spec["user_message"]),
                    "knowledge_base_ids": setup["knowledge_base_ids"],
                    "knowledge_ids": setup["knowledge_ids"],
                    "agent_enabled": session_spec["endpoint"] == "agent-chat",
                    "agent_id": str(session_spec["agent_id"]),
                    "web_search_enabled": setup["web_search_enabled"],
                    "summary_model_id": model_id,
                    "disable_title": absolute_turn > 1,
                    "channel": setup["channel"],
                }
                started = time.perf_counter()
                message_ids_before: set[str] | None = None
                if progress is not None:
                    progress(profile_id, turn_id, absolute_turn, "started", None)
                try:
                    message_ids_before = self._session_message_ids(session_id)
                    events, ttfb_ms, total_latency_ms = self.client.stream(
                        f"/{session_spec['endpoint']}/{session_id}", payload
                    )
                    message = self.client.load_completed_assistant(
                        session_id,
                        exclude_message_ids=seen_ids,
                        wait_seconds=30.0,
                    )
                    message_id = str(message.get("id") or "")
                    if message_id:
                        seen_ids.add(message_id)
                    steps = [item for item in message.get("agent_steps") or [] if isinstance(item, dict)]
                    inventory = _event_inventory(events)
                    stream_error = _stream_error_text(inventory)
                    result = {
                        "turn_id": turn_id,
                        "purpose": str(turn_spec.get("purpose") or ""),
                        "user_message": str(turn_spec["user_message"]),
                        "prompt_source": "codex-adaptive" if adaptive_plan else "scenario-seed",
                        "adaptive_parent": deepcopy(adaptive_plan.get("adaptive_parent")) if adaptive_plan else None,
                        "message_id": message_id,
                        "assistant_message": str(message.get("content") or ""),
                        "is_completed": bool(message.get("is_completed")),
                        "references": deepcopy(message.get("knowledge_references") or []),
                        "retrieval_stats": deepcopy(message.get("retrieval_stats") or {}),
                        "agent_steps": steps,
                        "tool_calls": _tool_calls_from_steps(steps),
                        "event_inventory": inventory,
                        "context_observation": _context_observation(
                            absolute_turn=absolute_turn,
                            history_turns=history_turns,
                            active_state=active_state,
                            turn_positions=turn_positions,
                            requested_model_id=model_id,
                            usage=inventory["usage"],
                        ),
                        "state_after_turn": deepcopy(active_state),
                        "ttfb_ms": ttfb_ms,
                        "total_latency_ms": total_latency_ms,
                        "event_count": len(events),
                        "error": stream_error,
                    }
                    if stream_error:
                        active_state = active_before
                        retired_state = retired_before
                        failure_class, retryable = _failure_class(stream_error)
                        result["failure_class"] = failure_class
                        result["retryable"] = retryable
                        rollback = self._rollback_failed_request(session_id, message)
                        result["rollback"] = rollback
                        invalid_attempts.append(result)
                        session_result["invalid_attempts"] = invalid_attempts
                        session_result["error"] = stream_error
                        if progress is not None:
                            progress(profile_id, turn_id, absolute_turn, "failed", result)
                        break
                    session_result["turns"].append(result)
                    if progress is not None:
                        progress(profile_id, turn_id, absolute_turn, "completed", result)
                except Exception as exc:
                    active_state = active_before
                    retired_state = retired_before
                    failure_class, retryable = _failure_class(str(exc))
                    result = {
                        "turn_id": turn_id,
                        "purpose": str(turn_spec.get("purpose") or ""),
                        "user_message": str(turn_spec["user_message"]),
                        "prompt_source": "codex-adaptive" if adaptive_plan else "scenario-seed",
                        "adaptive_parent": deepcopy(adaptive_plan.get("adaptive_parent")) if adaptive_plan else None,
                        "assistant_message": "",
                        "context_observation": _context_observation(
                            absolute_turn=absolute_turn,
                            history_turns=history_turns,
                            active_state=active_before,
                            turn_positions=turn_positions,
                            requested_model_id=model_id,
                            usage={},
                        ),
                        "state_after_turn": deepcopy(active_before),
                        "ttfb_ms": 0,
                        "total_latency_ms": round((time.perf_counter() - started) * 1000),
                        "event_count": 0,
                        "error": str(exc),
                        "failure_class": failure_class,
                        "retryable": retryable,
                        "rollback": self._rollback_new_messages(
                            session_id,
                            message_ids_before,
                        ),
                    }
                    invalid_attempts.append(result)
                    session_result["invalid_attempts"] = invalid_attempts
                    session_result["error"] = str(exc)
                    if progress is not None:
                        progress(profile_id, turn_id, absolute_turn, "failed", result)
                    break
            session_result["state_ledger"] = {
                "active": deepcopy(active_state),
                "retired": deepcopy(retired_state),
            }
        output["completed_at"] = _now()
        return output

    def _rollback_failed_request(self, session_id: str, assistant: dict[str, Any]) -> dict[str, Any]:
        """Remove only the failed eval request pair so it cannot poison history."""

        request_id = str(assistant.get("request_id") or "")
        if not request_id:
            return {"attempted": False, "reason": "assistant request_id missing"}
        try:
            payload = self.client.request("GET", f"/messages/{session_id}/load?limit=500")
            messages = payload.get("data") if isinstance(payload, dict) else payload
            targets = [
                str(item.get("id") or "")
                for item in messages or []
                if isinstance(item, dict) and str(item.get("request_id") or "") == request_id
            ]
            deleted: list[str] = []
            for message_id in targets:
                if not message_id:
                    continue
                self.client.request("DELETE", f"/messages/{session_id}/{message_id}")
                deleted.append(message_id)
            return {
                "attempted": True,
                "request_id": request_id,
                "deleted_message_ids": deleted,
                "recoverable": True,
            }
        except Exception as exc:
            return {
                "attempted": True,
                "request_id": request_id,
                "deleted_message_ids": [],
                "recoverable": False,
                "error": str(exc),
            }

    def _session_message_ids(self, session_id: str) -> set[str]:
        payload = self.client.request("GET", f"/messages/{session_id}/load?limit=500")
        messages = payload.get("data") if isinstance(payload, dict) else payload
        return {
            str(item.get("id") or "")
            for item in messages or []
            if isinstance(item, dict) and str(item.get("id") or "").strip()
        }

    def _rollback_new_messages(
        self,
        session_id: str,
        message_ids_before: set[str] | None,
    ) -> dict[str, Any]:
        """Best-effort cleanup when transport/persistence raised before a request id was available."""

        if message_ids_before is None:
            return {
                "attempted": False,
                "recoverable": False,
                "reason": "pre-turn message snapshot unavailable",
            }
        try:
            current_ids = self._session_message_ids(session_id)
            targets = sorted(current_ids - message_ids_before)
            deleted: list[str] = []
            for message_id in targets:
                self.client.request("DELETE", f"/messages/{session_id}/{message_id}")
                deleted.append(message_id)
            return {
                "attempted": True,
                "deleted_message_ids": deleted,
                "recoverable": True,
            }
        except Exception as exc:
            return {
                "attempted": True,
                "deleted_message_ids": [],
                "recoverable": False,
                "error": str(exc),
            }

    def export_existing_session(
        self,
        scenario: dict[str, Any],
        *,
        profile_id: str,
        session_id: str,
    ) -> dict[str, Any]:
        """Recover a checkpoint from messages already persisted by WeKnora."""

        capabilities = self._assert_eval_environment()
        matches = [item for item in scenario["sessions"] if item["profile_id"] == profile_id]
        if len(matches) != 1:
            raise ValueError(f"profile {profile_id!r} was not found exactly once")
        session_spec = matches[0]
        payload = self.client.request("GET", f"/messages/{session_id}/load?limit=500")
        messages = payload.get("data") if isinstance(payload, dict) else payload
        if not isinstance(messages, list):
            raise ValueError("message export returned a non-list")
        ordered = sorted(
            [item for item in messages if isinstance(item, dict)],
            key=lambda item: str(item.get("created_at") or item.get("id") or ""),
        )
        specs = {str(item["user_message"]): item for item in session_spec["turns"]}
        history_turns = int(session_spec["history_turns"])
        active_state: dict[str, dict[str, Any]] = {}
        retired_state: list[dict[str, Any]] = []
        turn_positions: dict[str, int] = {}
        recovered: list[dict[str, Any]] = []
        pending_user: dict[str, Any] | None = None
        for message in ordered:
            role = str(message.get("role") or "")
            if role == "user":
                pending_user = message
                continue
            if role != "assistant" or pending_user is None:
                continue
            query = str(pending_user.get("content") or "")
            turn_spec = specs.get(query)
            pending_user = None
            if turn_spec is None:
                raise ValueError(f"persisted user message was not found in scenario: {query[:120]!r}")
            turn_id = str(turn_spec["turn_id"])
            absolute_turn = len(recovered) + 1
            turn_positions[turn_id] = absolute_turn
            _apply_state_updates(active_state, retired_state, turn_spec)
            steps = [item for item in message.get("agent_steps") or [] if isinstance(item, dict)]
            recovered.append(
                {
                    "turn_id": turn_id,
                    "purpose": str(turn_spec.get("purpose") or ""),
                    "user_message": query,
                    "message_id": str(message.get("id") or ""),
                    "assistant_message": str(message.get("content") or ""),
                    "is_completed": bool(message.get("is_completed")),
                    "references": deepcopy(message.get("knowledge_references") or []),
                    "retrieval_stats": deepcopy(message.get("retrieval_stats") or {}),
                    "agent_steps": steps,
                    "tool_calls": _tool_calls_from_steps(steps),
                    "event_inventory": {"event_types": {}, "event_tools": [], "errors": [], "usage": {}},
                    "context_observation": _context_observation(
                        absolute_turn=absolute_turn,
                        history_turns=history_turns,
                        active_state=active_state,
                        turn_positions=turn_positions,
                        requested_model_id=str(scenario["model_id"]),
                        usage={},
                    ),
                    "state_after_turn": deepcopy(active_state),
                    "ttfb_ms": 0,
                    "total_latency_ms": int(message.get("agent_duration_ms") or 0),
                    "event_count": 0,
                    "error": None if message.get("is_completed") else "persisted assistant message is incomplete",
                }
            )
        return {
            "schema_version": 1,
            "discovery_id": f"discovery-{uuid.uuid4()}",
            "scenario_id": str(scenario.get("scenario_id") or ""),
            "phase": str(scenario.get("phase") or "problem-finding"),
            "started_at": str(ordered[0].get("created_at") or _now()) if ordered else _now(),
            "completed_at": _now(),
            "formal_eval_executed": False,
            "model_id": str(scenario["model_id"]),
            "sut": capabilities,
            "sessions": [
                {
                    "profile_id": profile_id,
                    "agent_id": str(session_spec["agent_id"]),
                    "agent_type": session_spec.get("agent_type"),
                    "display_name": str(session_spec.get("display_name") or profile_id),
                    "endpoint": str(session_spec["endpoint"]),
                    "history_turns": history_turns,
                    "minimum_turns": int(session_spec["minimum_turns"]),
                    "business_goal": str(session_spec.get("business_goal") or ""),
                    "session_id": session_id,
                    "turns": recovered,
                    "invalid_attempts": [],
                    "state_ledger": {"active": active_state, "retired": retired_state},
                    "error": None,
                }
            ],
        }


def write_discovery_result(path: str | Path, value: dict[str, Any]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(
        json.dumps(value, ensure_ascii=False, indent=2) + "\n",
        encoding="utf-8",
        newline="\n",
    )
