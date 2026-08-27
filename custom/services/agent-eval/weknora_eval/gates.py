from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from pathlib import Path
from typing import Iterable

from .dataset import dataset_sha256
from .models import (
    CaseRun,
    CaseSpec,
    ExperimentRun,
    GateCheck,
    GatePolicy,
    GateResult,
    Split,
    Verdict,
)
from .scoring import score_case


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


def _metric_rates(cases: Iterable[CaseRun]) -> dict[str, float]:
    values: dict[str, list[bool]] = defaultdict(list)
    for case in cases:
        for score in case.scores:
            if score.passed is not None:
                values[score.name].append(score.passed)
    return {name: sum(items) / len(items) for name, items in values.items() if items}


def evaluate_gate(
    dataset: list[CaseSpec],
    candidate: ExperimentRun,
    policy: GatePolicy,
    baseline: ExperimentRun | None = None,
) -> GateResult:
    checks: list[GateCheck] = []
    selected = _selected_specs(dataset, policy)
    expected_ids = {case.case_id for case in selected}
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
        and not missing_sut_capabilities
    )
    checks.append(
        _check(
            "sut_handshake",
            Verdict.PASS if sut_ok else Verdict.INVALID,
            "eval-mode recorder and capabilities verified" if sut_ok else "invalid eval-mode SUT fingerprint",
            mode=candidate.sut.mode,
            recorder_enabled=candidate.sut.raw.get("recorder_enabled"),
            missing_capabilities=missing_sut_capabilities,
        )
    )

    if not expected_ids:
        checks.append(_check("dataset_selection", Verdict.INVALID, "no enabled cases match required gate splits"))
    else:
        checks.append(
            _check(
                "dataset_selection",
                Verdict.PASS,
                f"selected {len(expected_ids)} gate cases",
                splits=[split.value for split in policy.required_splits],
            )
        )

    candidate_ids = [case.case_id for case in candidate.cases if case.case_id in expected_ids]
    duplicate_candidate_ids = sorted(case_id for case_id, count in Counter(candidate_ids).items() if count > 1)
    raw_candidate_by_id = {case.case_id: case for case in candidate.cases if case.case_id in expected_ids}
    missing = sorted(expected_ids - set(raw_candidate_by_id))
    coverage = len(raw_candidate_by_id) / len(expected_ids) if expected_ids else 0.0
    coverage_ok = not duplicate_candidate_ids and coverage >= policy.min_case_coverage
    checks.append(
        _check(
            "case_coverage",
            Verdict.PASS if coverage_ok else Verdict.INVALID,
            f"coverage={coverage:.3f}, required>={policy.min_case_coverage:.3f}",
            missing=missing,
            duplicates=duplicate_candidate_ids,
        )
    )

    specs_by_id = {case.case_id: case for case in selected}
    artifact_errors: dict[str, list[str]] = {}
    candidate_by_id: dict[str, CaseRun] = {}
    for case_id, raw_case in raw_candidate_by_id.items():
        spec = specs_by_id[case_id]
        errors: list[str] = []
        if raw_case.family_id != spec.family_id or raw_case.split != spec.split:
            errors.append("family/split mismatch")
        expected_turn_ids = [turn.turn_id for turn in spec.turns]
        actual_turn_ids = [turn.turn_id for turn in raw_case.turns]
        if actual_turn_ids != expected_turn_ids:
            errors.append("turn coverage/order mismatch")
        rescored = score_case(spec, raw_case.model_copy(update={"scores": []}))
        if raw_case.verdict != rescored.verdict:
            errors.append(f"stored verdict {raw_case.verdict.value} != recomputed {rescored.verdict.value}")
        if errors:
            artifact_errors[case_id] = errors
        candidate_by_id[case_id] = rescored
    checks.append(
        _check(
            "candidate_artifact_integrity",
            Verdict.PASS if not artifact_errors else Verdict.INVALID,
            "candidate observations deterministically rescored" if not artifact_errors else "candidate artifact integrity failed",
            errors=artifact_errors,
        )
    )

    invalid = [case.case_id for case in candidate_by_id.values() if case.verdict == Verdict.INVALID]
    invalid_rate = len(invalid) / len(candidate_by_id) if candidate_by_id else 1.0
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
        if case.case_id in candidate_by_id and candidate_by_id[case.case_id].verdict != Verdict.INVALID
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

    hard_failures = sorted(
        case.case_id
        for case in candidate_by_id.values()
        if case.verdict == Verdict.FAIL
        or any(score.hard and score.passed is False for score in case.scores)
    )
    hard_ok = not policy.forbid_hard_failures or not hard_failures
    checks.append(
        _check(
            "hard_constraints",
            Verdict.PASS if hard_ok else Verdict.FAIL,
            "all hard constraints passed" if hard_ok else "hard constraint failures detected",
            cases=hard_failures,
        )
    )

    if baseline is not None and candidate_dataset_ok and baseline_dataset_ok:
        baseline_ids = [case.case_id for case in baseline.cases if case.case_id in expected_ids]
        duplicate_baseline_ids = sorted(case_id for case_id, count in Counter(baseline_ids).items() if count > 1)
        raw_baseline_by_id = {case.case_id: case for case in baseline.cases if case.case_id in expected_ids}
        missing_baseline = sorted(expected_ids - set(raw_baseline_by_id))
        baseline_errors: dict[str, list[str]] = {}
        baseline_by_id: dict[str, CaseRun] = {}
        for case_id, raw_case in raw_baseline_by_id.items():
            spec = specs_by_id[case_id]
            errors: list[str] = []
            if raw_case.family_id != spec.family_id or raw_case.split != spec.split:
                errors.append("family/split mismatch")
            if [turn.turn_id for turn in raw_case.turns] != [turn.turn_id for turn in spec.turns]:
                errors.append("turn coverage/order mismatch")
            rescored = score_case(spec, raw_case.model_copy(update={"scores": []}))
            if raw_case.verdict != rescored.verdict:
                errors.append(f"stored verdict {raw_case.verdict.value} != recomputed {rescored.verdict.value}")
            if errors:
                baseline_errors[case_id] = errors
            baseline_by_id[case_id] = rescored
        if missing_baseline:
            checks.append(
                _check(
                    "baseline_coverage",
                    Verdict.INVALID,
                    "baseline does not cover the paired gate cases",
                    missing=missing_baseline,
                    duplicates=duplicate_baseline_ids,
                )
            )
        elif duplicate_baseline_ids or baseline_errors:
            checks.append(
                _check(
                    "baseline_artifact_integrity",
                    Verdict.INVALID,
                    "baseline artifact integrity failed",
                    duplicates=duplicate_baseline_ids,
                    errors=baseline_errors,
                )
            )
        else:
            checks.append(
                _check(
                    "baseline_artifact_integrity",
                    Verdict.PASS,
                    "baseline observations deterministically rescored",
                )
            )
            regressions = sorted(
                case_id
                for case_id in expected_ids
                if baseline_by_id[case_id].verdict == Verdict.PASS
                and candidate_by_id.get(case_id, CaseRun(
                    case_id=case_id,
                    family_id="missing",
                    split=Split.GATE,
                    verdict=Verdict.INVALID,
                )).verdict != Verdict.PASS
            )
            regression_ok = not policy.forbid_pass_to_fail_regressions or not regressions
            checks.append(
                _check(
                    "paired_non_regression",
                    Verdict.PASS if regression_ok else Verdict.FAIL,
                    "no baseline PASS regressed" if regression_ok else "baseline PASS cases regressed",
                    cases=regressions,
                )
            )

            candidate_rates = _metric_rates(candidate_by_id.values())
            baseline_rates = _metric_rates(baseline_by_id.values())
            metric_regressions: dict[str, dict[str, float]] = {}
            for metric, tolerance in policy.max_metric_rate_regression.items():
                if metric not in baseline_rates or metric not in candidate_rates:
                    metric_regressions[metric] = {
                        "baseline": baseline_rates.get(metric, -1.0),
                        "candidate": candidate_rates.get(metric, -1.0),
                        "tolerance": tolerance,
                    }
                    continue
                if candidate_rates[metric] + tolerance < baseline_rates[metric]:
                    metric_regressions[metric] = {
                        "baseline": baseline_rates[metric],
                        "candidate": candidate_rates[metric],
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

            candidate_p95 = _p95(
                turn.total_latency_ms for case in candidate_by_id.values() for turn in case.turns
            )
            baseline_p95 = _p95(
                turn.total_latency_ms for case in baseline_by_id.values() for turn in case.turns
            )
            latency_limit = baseline_p95 * (1 + policy.max_p95_latency_regression_ratio) + policy.max_p95_latency_regression_ms
            latency_ok = candidate_p95 <= latency_limit
            checks.append(
                _check(
                    "latency_non_regression",
                    Verdict.PASS if latency_ok else Verdict.FAIL,
                    f"candidate p95={candidate_p95:.0f}ms, limit={latency_limit:.0f}ms",
                    candidate_p95_ms=candidate_p95,
                    baseline_p95_ms=baseline_p95,
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
    )
