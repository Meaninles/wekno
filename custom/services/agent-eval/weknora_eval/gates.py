from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from pathlib import Path
from typing import Iterable

from .codex_review import review_for_track, validate_codex_review
from .dataset import dataset_sha256
from .models import (
    AnswerTrack,
    CaseRun,
    CaseSpec,
    ExperimentRun,
    GateCheck,
    GatePolicy,
    GateResult,
    GateKind,
    ReviewMode,
    Split,
    Verdict,
)
from .scoring import project_case_to_answer_track, score_case


def load_policy(path: str | Path) -> GatePolicy:
    with Path(path).open("r", encoding="utf-8") as handle:
        return GatePolicy.model_validate(json.load(handle))


def _check(name: str, verdict: Verdict, comment: str, **details: object) -> GateCheck:
    return GateCheck(name=name, verdict=verdict, comment=comment, details=details)


def _p95(values: Iterable[int]) -> float:
    ordered = sorted(values)
    if not ordered:
        return 0.0
    index = max(0, math.ceil(len(ordered) * 0.95) - 1)
    return float(ordered[index])


def _selected_specs(dataset: list[CaseSpec], policy: GatePolicy) -> list[CaseSpec]:
    required = set(policy.required_splits)
    return [case for case in dataset if case.enabled and case.split in required]


def _metric_rate_for_prefix(cases: Iterable[CaseRun], prefix: str) -> float | None:
    values = [
        score.passed
        for case in cases
        for score in case.scores
        if score.passed is not None and _matches_prefix(score.name, [prefix])
    ]
    if not values:
        return None
    return sum(values) / len(values)


def _run_key(case: CaseRun) -> tuple[str, int]:
    return case.case_id, case.attempt_index


def _key_label(key: tuple[str, int]) -> str:
    return f"{key[0]}#attempt-{key[1]}"


def _matches_prefix(name: str, prefixes: list[str]) -> bool:
    return any(name == prefix or name.startswith(f"{prefix}.") for prefix in prefixes)


def _sut_identity_value(sut: object, field: str) -> object:
    if field in {"commit", "release", "environment"}:
        return getattr(sut, field, None)
    if field.startswith("raw."):
        raw = getattr(sut, "raw", {})
        return raw.get(field.removeprefix("raw.")) if isinstance(raw, dict) else None
    return None


def _identity_field_present(identity: dict[str, object], field: str) -> bool:
    """Identity booleans may legitimately be false (assistance disabled)."""

    if field not in identity:
        return False
    value = identity[field]
    return value is not None and value != "" and value != [] and value != {}


def _with_preserved_judge_scores(raw_case: CaseRun, rescored: CaseRun) -> CaseRun:
    judge_scores = [
        score for score in raw_case.scores if score.name == "judge.contract_satisfaction"
    ]
    return rescored.model_copy(update={"scores": [*rescored.scores, *judge_scores]})


def _stored_track_case(case: CaseRun, answer_track: AnswerTrack) -> CaseRun:
    projected = project_case_to_answer_track(case, answer_track)
    if answer_track == AnswerTrack.PRODUCTION_CANDIDATE:
        return projected.model_copy(
            update={
                "verdict": case.production_verdict or case.verdict,
                "scores": case.production_scores or case.scores,
            }
        )
    if answer_track == AnswerTrack.EVAL_ASSISTED_ANSWER:
        return projected.model_copy(
            update={
                "verdict": case.eval_assisted_verdict or Verdict.INVALID,
                "scores": case.eval_assisted_scores,
            }
        )
    return projected.model_copy(update={"verdict": case.verdict, "scores": case.scores})


def _review_repair_only_pass(case: CaseRun) -> bool:
    production_review = case.production_codex_review
    assisted_review = case.eval_assisted_codex_review
    if production_review is not None and assisted_review is not None:
        return (
            production_review.verdict != Verdict.PASS
            and assisted_review.verdict == Verdict.PASS
        )
    return case.repair_only_pass or (
        case.production_verdict is not None
        and case.production_verdict != Verdict.PASS
        and case.eval_assisted_verdict == Verdict.PASS
    )


def _repair_dependency_stats(cases: Iterable[CaseRun]) -> dict[str, float | int]:
    case_list = list(cases)
    turns = [turn for case in case_list for turn in case.turns]
    triggered = [turn for turn in turns if turn.repair.triggered]
    successful = [turn for turn in triggered if turn.repair.succeeded]
    repair_only = [
        case
        for case in case_list
        if _review_repair_only_pass(case)
    ]
    turn_count = len(turns)
    trigger_count = len(triggered)
    case_count = len(case_list)
    return {
        "turn_count": turn_count,
        "case_count": case_count,
        "repair_trigger_count": trigger_count,
        "repair_success_count": len(successful),
        "repair_only_pass_count": len(repair_only),
        "repair_trigger_rate": trigger_count / turn_count if turn_count else 0.0,
        "repair_success_rate": len(successful) / trigger_count if trigger_count else 0.0,
        "repair_only_pass_rate": len(repair_only) / case_count if case_count else 0.0,
        "average_repair_attempts": (
            sum(turn.repair.attempts for turn in triggered) / trigger_count
            if trigger_count
            else 0.0
        ),
        "added_model_calls": sum(turn.repair.added_model_calls for turn in turns),
        "added_tool_calls": sum(turn.repair.added_tool_calls for turn in turns),
        "added_latency_ms": sum(turn.repair.added_latency_ms for turn in turns),
        "added_model_calls_per_turn": (
            sum(turn.repair.added_model_calls for turn in turns) / turn_count
            if turn_count
            else 0.0
        ),
        "added_tool_calls_per_turn": (
            sum(turn.repair.added_tool_calls for turn in turns) / turn_count
            if turn_count
            else 0.0
        ),
        "average_added_latency_ms": (
            sum(turn.repair.added_latency_ms for turn in triggered) / trigger_count
            if trigger_count
            else 0.0
        ),
    }


def _repair_dependency_check(
    candidate_cases: Iterable[CaseRun],
    policy: GatePolicy,
    baseline_cases: Iterable[CaseRun] | None,
) -> GateCheck:
    candidate_stats = _repair_dependency_stats(candidate_cases)
    baseline_stats = (
        _repair_dependency_stats(baseline_cases)
        if baseline_cases is not None
        else None
    )
    failures: dict[str, object] = {}
    thresholds = {
        "repair_trigger_rate": policy.max_repair_trigger_rate,
        "repair_only_pass_rate": policy.max_repair_only_pass_rate,
        "average_repair_attempts": policy.max_average_repair_attempts,
        "added_model_calls_per_turn": policy.max_added_model_calls_per_turn,
        "added_tool_calls_per_turn": policy.max_added_tool_calls_per_turn,
        "average_added_latency_ms": policy.max_average_added_latency_ms,
    }
    for metric, limit in thresholds.items():
        value = float(candidate_stats[metric])
        if limit is not None and value > limit:
            failures[metric] = {"candidate": value, "maximum": limit}
    if policy.forbid_repair_dependency_regression:
        if baseline_stats is None:
            failures["baseline"] = "repair dependency regression requires a baseline"
        else:
            for metric in (
                "repair_trigger_rate",
                "repair_only_pass_rate",
                "average_repair_attempts",
                "added_model_calls_per_turn",
                "added_tool_calls_per_turn",
                "average_added_latency_ms",
            ):
                candidate_value = float(candidate_stats[metric])
                baseline_value = float(baseline_stats[metric])
                if candidate_value > baseline_value:
                    failures[f"regression.{metric}"] = {
                        "candidate": candidate_value,
                        "baseline": baseline_value,
                    }
    return _check(
        "repair_dependency",
        Verdict.PASS if not failures else Verdict.FAIL,
        "repair dependency is reported and within policy"
        if not failures
        else "repair dependency exceeded policy or regressed",
        candidate=candidate_stats,
        baseline=baseline_stats,
        failures=failures,
    )


def _adjudicate_case(case: CaseRun, policy: GatePolicy) -> tuple[CaseRun, dict[str, object]]:
    if not policy.require_judge or case.verdict == Verdict.INVALID:
        return case, {"required": policy.require_judge, "effective": case.verdict.value}

    judge_by_turn = {
        score.turn_id: score
        for score in case.scores
        if score.name == "judge.contract_satisfaction" and score.turn_id
    }
    expected_turns = {turn.turn_id for turn in case.turns}
    missing = sorted(expected_turns - set(judge_by_turn))
    low_confidence: list[str] = []
    invalid_judgements: list[str] = []
    judge_failures: list[str] = []
    for turn_id, score in judge_by_turn.items():
        measured_sut_failure = any(
            deterministic.name == "execution_valid"
            and deterministic.turn_id == turn_id
            and deterministic.passed is False
            and deterministic.metadata.get("failure_origin") == "sut"
            for deterministic in case.scores
        )
        confidence = float(score.metadata.get("confidence", -1))
        if confidence < policy.min_judge_confidence and not measured_sut_failure:
            low_confidence.append(turn_id)
        if score.value == "invalid":
            if measured_sut_failure:
                judge_failures.append(turn_id)
            else:
                invalid_judgements.append(turn_id)
        elif score.value == "fail":
            judge_failures.append(turn_id)

    failed_hard = [score for score in case.scores if score.hard and score.passed is False]
    non_reviewable = [
        score.name
        for score in failed_hard
        if not _matches_prefix(score.name, policy.judge_reviewable_metric_prefixes)
    ]
    reviewable_turns = {
        score.turn_id
        for score in failed_hard
        if _matches_prefix(score.name, policy.judge_reviewable_metric_prefixes)
    }
    reviewable_not_passed = sorted(
        turn_id
        for turn_id in reviewable_turns
        if turn_id not in judge_by_turn or judge_by_turn[turn_id].value != "pass"
    )

    if missing or low_confidence or invalid_judgements:
        verdict = Verdict.INVALID
    elif non_reviewable or judge_failures or reviewable_not_passed:
        verdict = Verdict.FAIL
    else:
        verdict = Verdict.PASS
    detail: dict[str, object] = {
        "required": True,
        "effective": verdict.value,
        "missing": missing,
        "low_confidence": sorted(low_confidence),
        "invalid": sorted(invalid_judgements),
        "judge_failures": sorted(judge_failures),
        "non_reviewable_failures": sorted(non_reviewable),
        "reviewable_not_passed": reviewable_not_passed,
    }
    return case.model_copy(update={"verdict": verdict}), detail


def _adjudicate_codex_review(
    spec: CaseSpec,
    case: CaseRun,
    policy: GatePolicy,
) -> tuple[CaseRun, dict[str, object]]:
    errors: list[str] = []
    if spec.review_mode != ReviewMode.CODEX_CONVERSATION:
        errors.append("dataset case is not declared for Codex conversation review")
    errors.extend(
        validate_codex_review(
            spec,
            case,
            policy.answer_track,
            policy.required_codex_review_dimensions,
        )
    )
    review = review_for_track(case, policy.answer_track)
    if errors or review is None:
        return (
            case.model_copy(update={"verdict": Verdict.INVALID}),
            {"valid": False, "errors": errors},
        )
    return (
        case.model_copy(update={"verdict": review.verdict}),
        {
            "valid": True,
            "verdict": review.verdict.value,
            "summary": review.summary,
            "dimensions": {
                name: item.rating.value
                for name, item in review.dimensions.items()
            },
            "findings": review.findings,
            "critical_findings": review.critical_findings,
        },
    )


def _case_pass_rates(cases: dict[tuple[str, int], CaseRun]) -> dict[str, float]:
    values: dict[str, list[bool]] = defaultdict(list)
    for (case_id, _), case in cases.items():
        values[case_id].append(case.verdict == Verdict.PASS)
    return {case_id: sum(items) / len(items) for case_id, items in values.items()}


def evaluate_gate(
    dataset: list[CaseSpec],
    candidate: ExperimentRun,
    policy: GatePolicy,
    baseline: ExperimentRun | None = None,
) -> GateResult:
    checks: list[GateCheck] = []
    expected_track = {
        GateKind.PRODUCTION_RELEASE: AnswerTrack.PRODUCTION_CANDIDATE,
        GateKind.EVAL_OPTIMIZATION: AnswerTrack.EVAL_ASSISTED_ANSWER,
    }.get(policy.gate_kind)
    track_ok = expected_track is None or policy.answer_track == expected_track
    checks.append(
        _check(
            "answer_track_contract",
            Verdict.PASS if track_ok else Verdict.INVALID,
            f"gate scores only {policy.answer_track.value}"
            if track_ok
            else f"{policy.gate_kind.value} requires {expected_track.value}",
            gate_kind=policy.gate_kind.value,
            answer_track=policy.answer_track.value,
        )
    )
    selected = _selected_specs(dataset, policy)
    required_splits = set(policy.required_splits)
    dataset_splits = {case.split for case in dataset if case.enabled}
    candidate_splits = set(candidate.splits)
    baseline_splits = set(baseline.splits) if baseline is not None else required_splits
    missing_dataset_splits = sorted(
        split.value for split in required_splits - dataset_splits
    )
    missing_candidate_splits = sorted(
        split.value for split in required_splits - candidate_splits
    )
    missing_baseline_splits = sorted(
        split.value for split in required_splits - baseline_splits
    )
    split_coverage_ok = not (
        missing_dataset_splits
        or missing_candidate_splits
        or missing_baseline_splits
    )
    checks.append(
        _check(
            "required_split_coverage",
            Verdict.PASS if split_coverage_ok else Verdict.INVALID,
            "every required formal split is present in the dataset and run artifacts"
            if split_coverage_ok
            else "one or more required formal splits are absent",
            required=sorted(split.value for split in required_splits),
            dataset_missing=missing_dataset_splits,
            candidate_missing=missing_candidate_splits,
            baseline_missing=missing_baseline_splits,
        )
    )
    expected_ids = {case.case_id for case in selected}
    expected_keys = {
        (case.case_id, attempt_index)
        for case in selected
        for attempt_index in range(1, case.repetitions + 1)
    }
    expected_dataset_sha256 = dataset_sha256(dataset)

    candidate_dataset_ok = candidate.dataset_sha256 == expected_dataset_sha256
    baseline_dataset_ok = baseline is None or baseline.dataset_sha256 == expected_dataset_sha256
    if not candidate_dataset_ok or not baseline_dataset_ok:
        checks.append(
            _check(
                "dataset_identity",
                Verdict.INVALID,
                "run artifact does not match the supplied frozen dataset",
                expected_sha256=expected_dataset_sha256,
                candidate_sha256=candidate.dataset_sha256,
                baseline_sha256=baseline.dataset_sha256 if baseline else None,
            )
        )
    elif policy.require_baseline and baseline is None:
        checks.append(_check("baseline_present", Verdict.INVALID, "policy requires a frozen baseline"))
    else:
        checks.append(_check("dataset_identity", Verdict.PASS, "dataset identity is comparable"))

    sut_capabilities = set(candidate.sut.capabilities)
    required_sut_capabilities = {cap.value for cap in policy.required_capabilities}
    missing_sut_capabilities = sorted(required_sut_capabilities - sut_capabilities)
    sut_ok = (
        candidate.sut.mode == "eval"
        and candidate.sut.raw.get("recorder_enabled") is True
        and candidate.sut.raw.get("capture_policy") == "full"
        and not missing_sut_capabilities
    )
    checks.append(
        _check(
            "sut_handshake",
            Verdict.PASS if sut_ok else Verdict.INVALID,
            "eval-mode recorder and capabilities verified" if sut_ok else "invalid eval-mode SUT fingerprint",
            mode=candidate.sut.mode,
            recorder_enabled=candidate.sut.raw.get("recorder_enabled"),
            capture_policy=candidate.sut.raw.get("capture_policy"),
            missing_capabilities=missing_sut_capabilities,
        )
    )

    required_sut_identity = set(policy.required_sut_identity_fields)
    missing_candidate_sut_identity = sorted(
        field
        for field in required_sut_identity
        if not _sut_identity_value(candidate.sut, field)
    )
    missing_baseline_sut_identity = sorted(
        field
        for field in required_sut_identity
        if baseline is not None and not _sut_identity_value(baseline.sut, field)
    )
    checks.append(
        _check(
            "sut_provenance",
            Verdict.PASS
            if not missing_candidate_sut_identity and not missing_baseline_sut_identity
            else Verdict.INVALID,
            "candidate and baseline SUT provenance are complete"
            if not missing_candidate_sut_identity and not missing_baseline_sut_identity
            else "candidate or baseline SUT provenance is incomplete",
            candidate_missing=missing_candidate_sut_identity,
            baseline_missing=missing_baseline_sut_identity,
            candidate_commit=candidate.sut.commit,
            baseline_commit=baseline.sut.commit if baseline else None,
        )
    )
    if policy.require_clean_sut:
        candidate_clean = candidate.sut.raw.get("worktree_dirty") == "false"
        baseline_clean = (
            baseline is None or baseline.sut.raw.get("worktree_dirty") == "false"
        )
        checks.append(
            _check(
                "clean_sut_worktrees",
                Verdict.PASS if candidate_clean and baseline_clean else Verdict.INVALID,
                "candidate and baseline were executed from clean SUT commits"
                if candidate_clean and baseline_clean
                else "formal gate cannot compare a dirty SUT worktree",
                candidate_dirty=candidate.sut.raw.get("worktree_dirty"),
                baseline_dirty=baseline.sut.raw.get("worktree_dirty") if baseline else None,
            )
        )

    if not expected_keys:
        checks.append(_check("dataset_selection", Verdict.INVALID, "no enabled cases match required gate splits"))
    else:
        checks.append(
            _check(
                "dataset_selection",
                Verdict.PASS,
                f"selected {len(expected_ids)} gate cases / {len(expected_keys)} session executions",
                splits=[split.value for split in policy.required_splits],
            )
        )

    under_repeated = sorted(
        case.case_id
        for case in selected
        if case.repetitions < policy.min_repetitions_per_case
    )
    checks.append(
        _check(
            "repetition_contract",
            Verdict.PASS if not under_repeated else Verdict.INVALID,
            "dataset declares enough independent session repetitions"
            if not under_repeated
            else "dataset repetition count is below policy",
            minimum=policy.min_repetitions_per_case,
            cases=under_repeated,
        )
    )

    dataset_profiles = {case.agent_profile_id for case in selected if case.agent_profile_id}
    missing_dataset_profiles = sorted(set(policy.required_agent_profiles) - dataset_profiles)
    checks.append(
        _check(
            "dataset_agent_profile_coverage",
            Verdict.PASS if not missing_dataset_profiles else Verdict.INVALID,
            "required WeKnora agent profiles are present in the selected dataset"
            if not missing_dataset_profiles
            else "selected dataset omits required WeKnora agent profiles",
            missing=missing_dataset_profiles,
            covered=sorted(dataset_profiles),
        )
    )

    candidate_runs = [case for case in candidate.cases if case.case_id in expected_ids]
    candidate_keys = [_run_key(case) for case in candidate_runs]
    duplicate_candidate_keys = sorted(
        (_key_label(key) for key, count in Counter(candidate_keys).items() if count > 1)
    )
    unexpected_candidate_keys = sorted(
        _key_label(key) for key in set(candidate_keys) - expected_keys
    )
    raw_candidate_by_key = {
        _run_key(case): case
        for case in candidate_runs
        if _run_key(case) in expected_keys
    }
    missing_keys = expected_keys - set(raw_candidate_by_key)
    missing = sorted(_key_label(key) for key in missing_keys)
    coverage = len(raw_candidate_by_key) / len(expected_keys) if expected_keys else 0.0
    coverage_ok = (
        not duplicate_candidate_keys
        and not unexpected_candidate_keys
        and coverage >= policy.min_case_coverage
    )
    checks.append(
        _check(
            "case_coverage",
            Verdict.PASS if coverage_ok else Verdict.INVALID,
            f"coverage={coverage:.3f}, required>={policy.min_case_coverage:.3f}",
            missing=missing,
            duplicates=duplicate_candidate_keys,
            unexpected=unexpected_candidate_keys,
        )
    )

    specs_by_id = {case.case_id: case for case in selected}
    artifact_errors: dict[str, list[str]] = {}
    candidate_by_key: dict[tuple[str, int], CaseRun] = {}
    for key, raw_case in raw_candidate_by_key.items():
        spec = specs_by_id[key[0]]
        errors: list[str] = []
        if raw_case.family_id != spec.family_id or raw_case.split != spec.split:
            errors.append("family/split mismatch")
        if raw_case.agent_profile_id != spec.agent_profile_id:
            errors.append("agent profile mismatch")
        expected_turn_ids = [turn.turn_id for turn in spec.turns]
        actual_turn_ids = [turn.turn_id for turn in raw_case.turns]
        if actual_turn_ids != expected_turn_ids:
            errors.append("turn coverage/order mismatch")
        if policy.require_production_surface_equivalence:
            divergent_turns = sorted(
                turn.turn_id
                for turn in raw_case.turns
                if turn.error is None
                and turn.is_completed
                and turn.production_surface_equivalent is not True
            )
            if divergent_turns:
                errors.append(
                    "SSE/persisted production candidate equivalence is unproven "
                    "or false for turns: " + ", ".join(divergent_turns)
                )
        tracked_case = _stored_track_case(raw_case, policy.answer_track)
        if tracked_case.error and not raw_case.error:
            errors.append(tracked_case.error)
        rescored = score_case(spec, tracked_case.model_copy(update={"scores": []}))
        if tracked_case.verdict != rescored.verdict:
            errors.append(
                f"stored {policy.answer_track.value} verdict "
                f"{tracked_case.verdict.value} != recomputed {rescored.verdict.value}"
            )
        if errors:
            artifact_errors[_key_label(key)] = errors
        candidate_by_key[key] = _with_preserved_judge_scores(tracked_case, rescored)
    checks.append(
        _check(
            "candidate_artifact_integrity",
            Verdict.PASS if not artifact_errors else Verdict.INVALID,
            "candidate observations deterministically rescored" if not artifact_errors else "candidate artifact integrity failed",
            errors=artifact_errors,
        )
    )

    adjudication_details: dict[str, dict[str, object]] = {}
    adjudication_invalid: list[str] = []
    if policy.require_judge:
        for key, case in list(candidate_by_key.items()):
            adjudicated, detail = _adjudicate_case(case, policy)
            candidate_by_key[key] = adjudicated
            adjudication_details[_key_label(key)] = detail
            if adjudicated.verdict == Verdict.INVALID:
                adjudication_invalid.append(_key_label(key))
        checks.append(
            _check(
                "semantic_adjudication",
                Verdict.PASS if not adjudication_invalid else Verdict.INVALID,
                "calibrated judge coverage is complete"
                if not adjudication_invalid
                else "judge result is missing, invalid, or below confidence threshold",
                cases=adjudication_details,
            )
        )

    codex_review_details: dict[str, dict[str, object]] = {}
    codex_review_invalid: list[str] = []
    if policy.require_codex_review:
        for key, case in list(candidate_by_key.items()):
            reviewed, detail = _adjudicate_codex_review(
                specs_by_id[key[0]], case, policy
            )
            candidate_by_key[key] = reviewed
            codex_review_details[_key_label(key)] = detail
            if reviewed.verdict == Verdict.INVALID:
                codex_review_invalid.append(_key_label(key))
        checks.append(
            _check(
                "codex_conversation_review",
                Verdict.PASS if not codex_review_invalid else Verdict.INVALID,
                "every complete conversation has an exact-track Codex review"
                if not codex_review_invalid
                else "Codex review is missing, stale, or incomplete",
                minimum_standard="materially correct and useful; minor issues allowed",
                cases=codex_review_details,
            )
        )

    dual_track_review_details: dict[str, dict[str, object]] = {}
    dual_track_review_invalid: list[str] = []
    if policy.require_dual_track_codex_review:
        for key, case in list(candidate_by_key.items()):
            track_errors = {
                track.value: validate_codex_review(
                    specs_by_id[key[0]],
                    case,
                    track,
                    policy.required_codex_review_dimensions,
                )
                for track in (
                    AnswerTrack.PRODUCTION_CANDIDATE,
                    AnswerTrack.EVAL_ASSISTED_ANSWER,
                )
            }
            track_errors = {
                track: errors for track, errors in track_errors.items() if errors
            }
            dual_track_review_details[_key_label(key)] = {
                "valid": not track_errors,
                "errors": track_errors,
            }
            if track_errors:
                candidate_by_key[key] = case.model_copy(update={"verdict": Verdict.INVALID})
                dual_track_review_invalid.append(_key_label(key))
        checks.append(
            _check(
                "dual_track_codex_review",
                Verdict.PASS if not dual_track_review_invalid else Verdict.INVALID,
                "production and assisted tracks both have exact-conversation Codex reviews"
                if not dual_track_review_invalid
                else "repair dependency cannot use missing or stale track reviews",
                cases=dual_track_review_details,
            )
        )

    invalid = sorted(
        [*(_key_label(key) for key in missing_keys), *(
            _key_label(key)
            for key, case in candidate_by_key.items()
            if case.verdict == Verdict.INVALID
        )]
    )
    invalid_rate = len(set(invalid)) / len(expected_keys) if expected_keys else 1.0
    invalid_ok = invalid_rate <= policy.max_invalid_rate
    checks.append(
        _check(
            "invalid_rate",
            Verdict.PASS if invalid_ok else Verdict.INVALID,
            f"invalid_rate={invalid_rate:.3f}, allowed<={policy.max_invalid_rate:.3f}",
            invalid_cases=invalid,
        )
    )

    required_capabilities = {cap.value for cap in policy.required_capabilities}
    covered_capabilities = {
        capability.value
        for case in selected
        if all(
            (case.case_id, attempt_index) in candidate_by_key
            and candidate_by_key[(case.case_id, attempt_index)].verdict != Verdict.INVALID
            for attempt_index in range(1, case.repetitions + 1)
        )
        for capability in case.capabilities
    }
    missing_capabilities = sorted(required_capabilities - covered_capabilities)
    checks.append(
        _check(
            "capability_coverage",
            Verdict.PASS if not missing_capabilities else Verdict.INVALID,
            "required capability families executed" if not missing_capabilities else "capability coverage incomplete",
            missing=missing_capabilities,
            covered=sorted(covered_capabilities),
        )
    )

    covered_profiles = {
        case.agent_profile_id
        for case in selected
        if case.agent_profile_id
        and all(
            (case.case_id, attempt_index) in candidate_by_key
            and candidate_by_key[(case.case_id, attempt_index)].verdict != Verdict.INVALID
            for attempt_index in range(1, case.repetitions + 1)
        )
    }
    missing_profiles = sorted(set(policy.required_agent_profiles) - covered_profiles)
    checks.append(
        _check(
            "agent_profile_coverage",
            Verdict.PASS if not missing_profiles else Verdict.INVALID,
            "required WeKnora agents completed every declared session repetition"
            if not missing_profiles
            else "required WeKnora agent execution coverage is incomplete",
            missing=missing_profiles,
            covered=sorted(covered_profiles),
        )
    )

    hard_failures = sorted(
        _key_label(key)
        for key, case in candidate_by_key.items()
        if (
            policy.forbid_hard_failures
            and (
                case.verdict == Verdict.FAIL
                or any(score.hard and score.passed is False for score in case.scores)
            )
        )
        or any(
            score.passed is False
            and _matches_prefix(score.name, policy.critical_metric_prefixes)
            for score in case.scores
        )
    )
    # When broad hard-failure blocking is disabled, explicitly critical metrics
    # are still release blockers.  The collected list already reflects both
    # policy branches, so its emptiness is the single source of truth.
    hard_ok = not hard_failures
    checks.append(
        _check(
            "hard_constraints",
            Verdict.PASS if hard_ok else Verdict.FAIL,
            "all hard constraints passed" if hard_ok else "hard constraint failures detected",
            cases=hard_failures,
        )
    )

    candidate_pass_rates = _case_pass_rates(candidate_by_key)
    unstable_cases = {
        case_id: rate
        for case_id, rate in sorted(candidate_pass_rates.items())
        if rate < policy.min_pass_rate_per_case
    }
    checks.append(
        _check(
            "repetition_pass_rate",
            Verdict.PASS if not unstable_cases else Verdict.FAIL,
            "each case meets the minimum repeated-session pass rate"
            if not unstable_cases
            else "one or more cases are too unstable across repetitions",
            minimum=policy.min_pass_rate_per_case,
            rates=candidate_pass_rates,
            failures=unstable_cases,
        )
    )

    latency_breaches: dict[str, dict[str, float]] = {}
    for profile in sorted(
        set(policy.max_p95_latency_ms_by_agent) | set(policy.max_latency_ms_by_agent)
    ):
        values = [
            turn.total_latency_ms
            for case in candidate_by_key.values()
            if case.agent_profile_id == profile
            for turn in case.turns
        ]
        if not values:
            latency_breaches[profile] = {"missing": 1.0}
            continue
        p95 = _p95(values)
        maximum = float(max(values))
        p95_limit = policy.max_p95_latency_ms_by_agent.get(profile)
        max_limit = policy.max_latency_ms_by_agent.get(profile)
        if (p95_limit is not None and p95 > p95_limit) or (
            max_limit is not None and maximum > max_limit
        ):
            latency_breaches[profile] = {
                "p95_ms": p95,
                "p95_limit_ms": float(p95_limit or 0),
                "max_ms": maximum,
                "max_limit_ms": float(max_limit or 0),
            }
    if policy.max_p95_latency_ms_by_agent or policy.max_latency_ms_by_agent:
        checks.append(
            _check(
                "absolute_latency_by_agent",
                Verdict.PASS if not latency_breaches else Verdict.FAIL,
                "per-agent absolute latency budgets passed"
                if not latency_breaches
                else "per-agent absolute latency budget exceeded",
                breaches=latency_breaches,
            )
        )

    required_identity_fields = set(policy.required_execution_identity_fields)
    candidate_identity = (
        candidate.metadata.get("execution_contract")
        if isinstance(candidate.metadata.get("execution_contract"), dict)
        else {}
    )
    missing_candidate_identity = sorted(
        field
        for field in required_identity_fields
        if not _identity_field_present(candidate_identity, field)
    )
    checks.append(
        _check(
            "candidate_execution_identity",
            Verdict.PASS if not missing_candidate_identity else Verdict.INVALID,
            "candidate model/corpus/profile identity is frozen"
            if not missing_candidate_identity
            else "candidate execution identity is incomplete",
            missing=missing_candidate_identity,
            identity=candidate_identity,
        )
    )
    if policy.require_clean_framework:
        clean_framework = candidate_identity.get("framework_worktree_dirty") == "false"
        checks.append(
            _check(
                "clean_eval_framework",
                Verdict.PASS if clean_framework else Verdict.INVALID,
                "eval framework was executed from a clean commit"
                if clean_framework
                else "formal gate cannot run from a dirty eval framework worktree",
                framework_commit=candidate_identity.get("framework_commit"),
                worktree_dirty=candidate_identity.get("framework_worktree_dirty"),
            )
        )

    raw_baseline_for_repair: list[CaseRun] = []
    if baseline is not None and candidate_dataset_ok and baseline_dataset_ok:
        baseline_identity = (
            baseline.metadata.get("execution_contract")
            if isinstance(baseline.metadata.get("execution_contract"), dict)
            else {}
        )
        identity_mismatches = {
            field: {
                "baseline": baseline_identity.get(field),
                "candidate": candidate_identity.get(field),
            }
            for field in sorted(required_identity_fields)
            if not _identity_field_present(baseline_identity, field)
            or baseline_identity.get(field) != candidate_identity.get(field)
        }
        checks.append(
            _check(
                "paired_execution_identity",
                Verdict.PASS if not identity_mismatches else Verdict.INVALID,
                "baseline and candidate use the same model, corpus and agent profiles"
                if not identity_mismatches
                else "baseline and candidate execution identities are not comparable",
                mismatches=identity_mismatches,
            )
        )
        baseline_runs = [case for case in baseline.cases if case.case_id in expected_ids]
        baseline_keys = [_run_key(case) for case in baseline_runs]
        duplicate_baseline_keys = sorted(
            _key_label(key) for key, count in Counter(baseline_keys).items() if count > 1
        )
        unexpected_baseline_keys = sorted(_key_label(key) for key in set(baseline_keys) - expected_keys)
        raw_baseline_by_key = {
            _run_key(case): case
            for case in baseline_runs
            if _run_key(case) in expected_keys
        }
        raw_baseline_for_repair = list(raw_baseline_by_key.values())
        missing_baseline_keys = expected_keys - set(raw_baseline_by_key)
        missing_baseline = sorted(_key_label(key) for key in missing_baseline_keys)
        baseline_errors: dict[str, list[str]] = {}
        baseline_by_key: dict[tuple[str, int], CaseRun] = {}
        for key, raw_case in raw_baseline_by_key.items():
            spec = specs_by_id[key[0]]
            errors: list[str] = []
            if raw_case.family_id != spec.family_id or raw_case.split != spec.split:
                errors.append("family/split mismatch")
            if raw_case.agent_profile_id != spec.agent_profile_id:
                errors.append("agent profile mismatch")
            if [turn.turn_id for turn in raw_case.turns] != [turn.turn_id for turn in spec.turns]:
                errors.append("turn coverage/order mismatch")
            if policy.require_production_surface_equivalence:
                divergent_turns = sorted(
                    turn.turn_id
                    for turn in raw_case.turns
                    if turn.error is None
                    and turn.is_completed
                    and turn.production_surface_equivalent is not True
                )
                if divergent_turns:
                    errors.append(
                        "SSE/persisted production candidate equivalence is unproven "
                        "or false for turns: " + ", ".join(divergent_turns)
                    )
            tracked_case = _stored_track_case(raw_case, policy.answer_track)
            if tracked_case.error and not raw_case.error:
                errors.append(tracked_case.error)
            rescored = score_case(spec, tracked_case.model_copy(update={"scores": []}))
            if tracked_case.verdict != rescored.verdict:
                errors.append(
                    f"stored {policy.answer_track.value} verdict "
                    f"{tracked_case.verdict.value} != recomputed {rescored.verdict.value}"
                )
            if errors:
                baseline_errors[_key_label(key)] = errors
            baseline_by_key[key] = _with_preserved_judge_scores(tracked_case, rescored)
        if missing_baseline or duplicate_baseline_keys or unexpected_baseline_keys:
            checks.append(
                _check(
                    "baseline_coverage",
                    Verdict.INVALID,
                    "baseline does not exactly cover the paired session repetitions",
                    missing=missing_baseline,
                    duplicates=duplicate_baseline_keys,
                    unexpected=unexpected_baseline_keys,
                )
            )
        elif baseline_errors:
            checks.append(
                _check(
                    "baseline_artifact_integrity",
                    Verdict.INVALID,
                    "baseline artifact integrity failed",
                    errors=baseline_errors,
                )
            )
        else:
            baseline_adjudication_invalid: list[str] = []
            if policy.require_judge:
                for key, case in list(baseline_by_key.items()):
                    adjudicated, _ = _adjudicate_case(case, policy)
                    baseline_by_key[key] = adjudicated
                    if adjudicated.verdict == Verdict.INVALID:
                        baseline_adjudication_invalid.append(_key_label(key))
            baseline_codex_details: dict[str, dict[str, object]] = {}
            if policy.require_codex_review:
                for key, case in list(baseline_by_key.items()):
                    reviewed, detail = _adjudicate_codex_review(
                        specs_by_id[key[0]], case, policy
                    )
                    baseline_by_key[key] = reviewed
                    baseline_codex_details[_key_label(key)] = detail
                    if reviewed.verdict == Verdict.INVALID:
                        baseline_adjudication_invalid.append(_key_label(key))
                checks.append(
                    _check(
                        "baseline_codex_conversation_review",
                        Verdict.PASS
                        if not baseline_adjudication_invalid
                        else Verdict.INVALID,
                        "baseline conversations have comparable Codex reviews"
                        if not baseline_adjudication_invalid
                        else "baseline Codex review is missing, stale, or incomplete",
                        cases=baseline_codex_details,
                    )
                )
            if policy.require_dual_track_codex_review:
                baseline_dual_details: dict[str, dict[str, object]] = {}
                for key, case in list(baseline_by_key.items()):
                    track_errors = {
                        track.value: validate_codex_review(
                            specs_by_id[key[0]],
                            case,
                            track,
                            policy.required_codex_review_dimensions,
                        )
                        for track in (
                            AnswerTrack.PRODUCTION_CANDIDATE,
                            AnswerTrack.EVAL_ASSISTED_ANSWER,
                        )
                    }
                    track_errors = {
                        track: errors
                        for track, errors in track_errors.items()
                        if errors
                    }
                    baseline_dual_details[_key_label(key)] = {
                        "valid": not track_errors,
                        "errors": track_errors,
                    }
                    if track_errors:
                        baseline_by_key[key] = case.model_copy(
                            update={"verdict": Verdict.INVALID}
                        )
                        baseline_adjudication_invalid.append(_key_label(key))
                checks.append(
                    _check(
                        "baseline_dual_track_codex_review",
                        Verdict.PASS
                        if not any(
                            not detail["valid"]
                            for detail in baseline_dual_details.values()
                        )
                        else Verdict.INVALID,
                        "baseline production and assisted tracks have comparable Codex reviews"
                        if not any(
                            not detail["valid"]
                            for detail in baseline_dual_details.values()
                        )
                        else "baseline repair dependency reviews are incomplete or stale",
                        cases=baseline_dual_details,
                    )
                )
            if baseline_adjudication_invalid:
                checks.append(
                    _check(
                        "baseline_semantic_adjudication",
                        Verdict.INVALID,
                        "baseline judge result is missing, invalid, or below confidence threshold",
                        cases=baseline_adjudication_invalid,
                    )
                )
            checks.append(
                _check(
                    "baseline_artifact_integrity",
                    Verdict.PASS if not baseline_adjudication_invalid else Verdict.INVALID,
                    "baseline observations deterministically rescored"
                    if not baseline_adjudication_invalid
                    else "baseline semantic adjudication is not comparable",
                )
            )
            if policy.pair_repetitions_by_attempt:
                regressions: object = sorted(
                    _key_label(key)
                    for key in expected_keys
                    if baseline_by_key[key].verdict == Verdict.PASS
                    and (
                        key not in candidate_by_key
                        or candidate_by_key[key].verdict != Verdict.PASS
                    )
                )
            else:
                baseline_case_rates = _case_pass_rates(baseline_by_key)
                candidate_case_rates = _case_pass_rates(candidate_by_key)
                regressions = {
                    case_id: {
                        "baseline": baseline_rate,
                        "candidate": candidate_case_rates.get(case_id, 0.0),
                        "tolerance": policy.max_case_pass_rate_regression,
                    }
                    for case_id, baseline_rate in sorted(baseline_case_rates.items())
                    if candidate_case_rates.get(case_id, 0.0)
                    + policy.max_case_pass_rate_regression
                    < baseline_rate
                }
            regression_ok = not policy.forbid_pass_to_fail_regressions or not regressions
            if not policy.forbid_pass_to_fail_regressions:
                regression_comment = (
                    "Codex-reviewed baseline deltas are reported; the absolute "
                    "repetition standard decides quality"
                )
            elif regression_ok:
                regression_comment = "no baseline PASS rate regressed"
            else:
                regression_comment = "baseline PASS rate regressed"
            checks.append(
                _check(
                    "paired_non_regression",
                    Verdict.PASS if regression_ok else Verdict.FAIL,
                    regression_comment,
                    pairing="attempt"
                    if policy.pair_repetitions_by_attempt
                    else "case_pass_rate",
                    blocking=policy.forbid_pass_to_fail_regressions,
                    cases=regressions,
                )
            )

            baseline_case_rates = _case_pass_rates(baseline_by_key)
            candidate_case_rates = _case_pass_rates(candidate_by_key)
            improved_cases = {
                case_id: {
                    "baseline": baseline_rate,
                    "candidate": candidate_case_rates.get(case_id, 0.0),
                }
                for case_id, baseline_rate in sorted(baseline_case_rates.items())
                if candidate_case_rates.get(case_id, 0.0) > baseline_rate
            }
            if policy.min_improved_case_count:
                improvement_ok = (
                    len(improved_cases) >= policy.min_improved_case_count
                )
                checks.append(
                    _check(
                        "minimum_case_improvement",
                        Verdict.PASS if improvement_ok else Verdict.FAIL,
                        "candidate improves enough case-level repetition rates"
                        if improvement_ok
                        else "candidate has no sufficient case-level improvement",
                        minimum=policy.min_improved_case_count,
                        count=len(improved_cases),
                        cases=improved_cases,
                    )
                )

            metric_regressions: dict[str, dict[str, float]] = {}
            for metric, tolerance in policy.max_metric_rate_regression.items():
                baseline_rate = _metric_rate_for_prefix(
                    baseline_by_key.values(), metric
                )
                candidate_rate = _metric_rate_for_prefix(
                    candidate_by_key.values(), metric
                )
                if baseline_rate is None or candidate_rate is None:
                    metric_regressions[metric] = {
                        "baseline": baseline_rate if baseline_rate is not None else -1.0,
                        "candidate": candidate_rate if candidate_rate is not None else -1.0,
                        "tolerance": tolerance,
                    }
                    continue
                if candidate_rate + tolerance < baseline_rate:
                    metric_regressions[metric] = {
                        "baseline": baseline_rate,
                        "candidate": candidate_rate,
                        "tolerance": tolerance,
                    }
            checks.append(
                _check(
                    "metric_non_regression",
                    Verdict.PASS if not metric_regressions else Verdict.FAIL,
                    "metric rates are within regression budgets" if not metric_regressions else "metric regression budget exceeded",
                    regressions=metric_regressions,
                )
            )

            profiles = sorted(
                {
                    case.agent_profile_id or "unassigned"
                    for case in [*candidate_by_key.values(), *baseline_by_key.values()]
                }
            )
            latency_regressions: dict[str, dict[str, float]] = {}
            for profile in profiles:
                candidate_p95 = _p95(
                    turn.total_latency_ms
                    for case in candidate_by_key.values()
                    if (case.agent_profile_id or "unassigned") == profile
                    for turn in case.turns
                )
                baseline_p95 = _p95(
                    turn.total_latency_ms
                    for case in baseline_by_key.values()
                    if (case.agent_profile_id or "unassigned") == profile
                    for turn in case.turns
                )
                latency_limit = (
                    baseline_p95 * (1 + policy.max_p95_latency_regression_ratio)
                    + policy.max_p95_latency_regression_ms
                )
                if candidate_p95 > latency_limit:
                    latency_regressions[profile] = {
                        "candidate_p95_ms": candidate_p95,
                        "baseline_p95_ms": baseline_p95,
                        "limit_ms": latency_limit,
                    }
            latency_ok = not latency_regressions
            checks.append(
                _check(
                    "latency_non_regression",
                    Verdict.PASS if latency_ok else Verdict.FAIL,
                    "per-agent p95 latency is within regression budgets"
                    if latency_ok
                    else "per-agent p95 latency regression budget exceeded",
                    regressions=latency_regressions,
                )
            )

    if policy.gate_kind != GateKind.LEGACY:
        checks.append(
            _repair_dependency_check(
                raw_candidate_by_key.values(),
                policy,
                raw_baseline_for_repair if baseline is not None else None,
            )
        )

    if policy.gate_kind == GateKind.PRODUCTION_RELEASE:
        repair_only_cases = sorted(
            _key_label(key)
            for key, case in raw_candidate_by_key.items()
            if _review_repair_only_pass(case)
        )
        checks.append(
            _check(
                "eval_assistance_cannot_rescue_release",
                Verdict.PASS,
                "repair-only successes are reported but never substituted for "
                "production-candidate Codex verdicts",
                cases=repair_only_cases,
                counted_as_production_pass=False,
            )
        )

    if any(check.verdict == Verdict.INVALID for check in checks):
        verdict = Verdict.INVALID
    elif any(check.verdict == Verdict.FAIL for check in checks):
        verdict = Verdict.FAIL
    else:
        verdict = Verdict.PASS
    return GateResult(
        policy_id=policy.policy_id,
        candidate_run_id=candidate.run_id,
        baseline_run_id=baseline.run_id if baseline else None,
        verdict=verdict,
        checks=checks,
        gate_kind=policy.gate_kind,
        answer_track=policy.answer_track,
    )
