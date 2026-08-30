from __future__ import annotations

import hashlib
import json
import os
from collections import defaultdict
from pathlib import Path
from typing import Any

from .dataset import canonical_json, dataset_sha256, load_jsonl, validate_dataset
from .gates import load_policy
from .models import Capability, CaseSpec, SUTFingerprint, Split


class ReadinessError(ValueError):
    pass


def file_sha256(path: str | Path) -> str:
    source = Path(path)
    if source.suffix.lower() == ".json":
        with source.open("r", encoding="utf-8") as handle:
            payload = json.load(handle)
        return hashlib.sha256(canonical_json(payload).encode("utf-8")).hexdigest()
    if source.suffix.lower() in {
        ".jsonl", ".md", ".py", ".ps1", ".txt", ".toml", ".yaml", ".yml",
    }:
        # Git may materialize the same committed text as LF on Linux and CRLF
        # on Windows. Eval identity must describe semantic source bytes, not
        # the checkout platform, otherwise a clean Windows worktree fails a
        # manifest frozen on Linux before any session runs.
        payload = source.read_bytes().replace(b"\r\n", b"\n").replace(b"\r", b"\n")
        return hashlib.sha256(payload).hexdigest()
    digest = hashlib.sha256()
    with source.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def _load_object(path: str | Path) -> dict[str, Any]:
    source = Path(path)
    with source.open("r", encoding="utf-8") as handle:
        value = json.load(handle)
    if not isinstance(value, dict):
        raise ReadinessError(f"{source}: expected a JSON object")
    return value


def _check(checks: list[dict[str, Any]], name: str, passed: bool, detail: str, **data: Any) -> None:
    checks.append({"name": name, "passed": passed, "detail": detail, "data": data})


def _profile_map(profile_set: dict[str, Any]) -> dict[str, dict[str, Any]]:
    rows = profile_set.get("profiles")
    if not isinstance(rows, list) or not rows:
        raise ReadinessError("profile set has no profiles")
    result: dict[str, dict[str, Any]] = {}
    for row in rows:
        if not isinstance(row, dict):
            raise ReadinessError("profile row must be an object")
        profile_id = str(row.get("profile_id") or "").strip()
        if not profile_id or profile_id in result:
            raise ReadinessError(f"invalid or duplicate profile_id: {profile_id!r}")
        result[profile_id] = row
    return result


def _selected(cases: list[CaseSpec], split: Split) -> list[CaseSpec]:
    return [case for case in cases if case.enabled and case.split == split]


def _sut_identity_value(sut: SUTFingerprint, field: str) -> Any:
    if field == "commit":
        return sut.commit
    if field == "release":
        return sut.release
    if field == "environment":
        return sut.environment
    if field.startswith("raw."):
        return sut.raw.get(field.removeprefix("raw."))
    return None


def evaluate_readiness(
    *,
    dataset_path: str | Path,
    manifest_path: str | Path,
    profile_path: str | Path,
    policy_path: str | Path,
    calibration_path: str | Path,
    split: Split,
    sut: SUTFingerprint,
) -> dict[str, Any]:
    """Fail-closed, read-only validation performed before any eval session exists."""

    unresolved = load_jsonl(dataset_path)
    dataset_errors = validate_dataset(unresolved)
    selected = _selected(unresolved, split)
    manifest = _load_object(manifest_path)
    profile_set = _load_object(profile_path)
    profiles = _profile_map(profile_set)
    policy = load_policy(policy_path)
    checks: list[dict[str, Any]] = []

    _check(
        checks,
        "dataset_contracts",
        not dataset_errors,
        "dataset contracts are executable and family-isolated" if not dataset_errors else "dataset validation failed",
        errors=dataset_errors,
    )
    _check(
        checks,
        "split_selection",
        bool(selected),
        f"selected {len(selected)} enabled {split.value} cases",
        case_count=len(selected),
    )

    digest = dataset_sha256(unresolved)
    manifest_ok = (
        manifest.get("dataset_sha256") == digest
        and manifest.get("suite") == (unresolved[0].suite if unresolved else "")
        and int(manifest.get("case_count") or 0) == len(unresolved)
        and int(manifest.get("family_count") or 0)
        == len({case.family_id for case in unresolved})
    )
    _check(
        checks,
        "frozen_identity",
        manifest_ok,
        "dataset hash and frozen manifest match" if manifest_ok else "dataset differs from its frozen manifest",
        dataset_sha256=digest,
        manifest_sha256=manifest.get("dataset_sha256"),
    )

    known_dependencies = {
        "profiles": file_sha256(profile_path),
        "policy": file_sha256(policy_path),
        "judge_calibration": file_sha256(calibration_path),
        "scorer": file_sha256(Path(__file__).with_name("scoring.py")),
        "gate": file_sha256(Path(__file__).with_name("gates.py")),
        "judge": file_sha256(Path(__file__).with_name("judge.py")),
        "calibrator": file_sha256(Path(__file__).with_name("calibration.py")),
    }
    required_dependency_names = {
        "profiles",
        "policy",
        "judge_calibration",
        *policy.required_frozen_dependencies,
    }
    unknown_dependency_names = sorted(required_dependency_names - set(known_dependencies))
    expected_dependencies = {
        name: known_dependencies[name]
        for name in sorted(required_dependency_names & set(known_dependencies))
    }
    manifest_dependencies = manifest.get("dependency_sha256")
    dependency_ok = (
        not unknown_dependency_names
        and isinstance(manifest_dependencies, dict)
        and manifest_dependencies == expected_dependencies
    )
    _check(
        checks,
        "frozen_dependencies",
        dependency_ok,
        "all required dataset and evaluator dependency hashes match the manifest"
        if dependency_ok
        else "a frozen eval dependency changed",
        expected=expected_dependencies,
        manifest=manifest_dependencies,
        unknown=unknown_dependency_names,
    )

    required_model = str((profile_set.get("model") or {}).get("required_model_id") or "").strip()
    actual_model = os.environ.get("AGENT_EVAL_SUMMARY_MODEL_ID", "").strip()
    model_ok = bool(required_model) and actual_model == required_model
    _check(
        checks,
        "model_binding",
        model_ok,
        "DeepSeek model binding is exact" if model_ok else "runtime model does not match the frozen profile set",
        required_model_id=required_model,
        actual_model_id=actual_model,
    )

    judge_environment = {
        name: bool(os.environ.get(name, "").strip())
        for name in (
            "AGENT_EVAL_JUDGE_BASE_URL",
            "AGENT_EVAL_JUDGE_API_KEY",
            "AGENT_EVAL_JUDGE_MODEL",
        )
    }
    judge_binding_ok = not policy.require_judge or all(judge_environment.values())
    _check(
        checks,
        "judge_binding",
        judge_binding_ok,
        "calibrated Judge runtime binding is complete"
        if judge_binding_ok
        else "formal gate requires a complete Judge runtime binding",
        configured=judge_environment,
    )

    required_profile_ids = set(policy.required_agent_profiles)
    selected_profile_ids = {case.agent_profile_id for case in selected if case.agent_profile_id}
    known_profile_ids = set(profiles)
    matrix_ok = required_profile_ids == selected_profile_ids == known_profile_ids
    _check(
        checks,
        "three_agent_matrix",
        matrix_ok,
        "selected split covers exactly the frozen three-agent matrix" if matrix_ok else "three-agent matrix differs across dataset, profile and policy",
        required=sorted(required_profile_ids),
        selected=sorted(selected_profile_ids),
        configured=sorted(known_profile_ids),
    )

    binding_errors: list[str] = []
    long_context_max_turns: dict[str, int] = defaultdict(int)
    for case in selected:
        profile = profiles.get(case.agent_profile_id or "")
        if profile is None:
            binding_errors.append(f"{case.case_id}: unknown agent_profile_id")
            continue
        expected = {
            "agent_id": str(profile.get("agent_id") or ""),
            "agent_type": str(profile.get("agent_type") or ""),
            "endpoint": str(profile.get("endpoint") or ""),
        }
        actual = {
            "agent_id": case.agent.agent_id,
            "agent_type": case.agent.agent_type or "",
            "endpoint": case.agent.endpoint,
        }
        if actual != expected:
            binding_errors.append(f"{case.case_id}: agent binding {actual} != {expected}")
        declared_history = case.provenance.metadata.get("configured_history_turns")
        if declared_history != profile.get("configured_history_turns"):
            binding_errors.append(
                f"{case.case_id}: history declaration {declared_history!r} != {profile.get('configured_history_turns')!r}"
            )
        if Capability.LONG_CONTEXT_DIALOGUE in case.capabilities:
            long_context_max_turns[case.agent_profile_id or ""] = max(
                long_context_max_turns[case.agent_profile_id or ""], len(case.turns)
            )
    _check(
        checks,
        "agent_bindings",
        not binding_errors,
        "every case is bound to its frozen WeKnora agent profile" if not binding_errors else "case/profile binding mismatch",
        errors=binding_errors,
    )

    overflow_errors: list[str] = []
    for profile_id in required_profile_ids:
        configured = int(profiles.get(profile_id, {}).get("configured_history_turns") or 0)
        observed = long_context_max_turns.get(profile_id, 0)
        if observed < configured + 2:
            overflow_errors.append(
                f"{profile_id}: longest case has {observed} turns; requires >= {configured + 2}"
            )
    _check(
        checks,
        "history_window_overflow",
        not overflow_errors,
        "each agent has a branch-safe case at least two turns beyond its configured window" if not overflow_errors else "long-context depth is insufficient",
        maximum_turns=dict(sorted(long_context_max_turns.items())),
        errors=overflow_errors,
    )

    repetition_errors = [
        f"{case.case_id}: {case.repetitions} < {policy.min_repetitions_per_case}"
        for case in selected
        if split in set(policy.required_splits)
        and case.repetitions < policy.min_repetitions_per_case
    ]
    _check(
        checks,
        "repetition_plan",
        not repetition_errors,
        "gate repetitions satisfy the release policy" if not repetition_errors else "gate repetition plan is insufficient",
        errors=repetition_errors,
    )

    covered_capabilities = {capability for case in selected for capability in case.capabilities}
    missing_capabilities = sorted(
        capability.value
        for capability in set(policy.required_capabilities) - covered_capabilities
    )
    _check(
        checks,
        "capability_coverage",
        not missing_capabilities,
        "selected data covers every policy capability" if not missing_capabilities else "selected data misses policy capabilities",
        missing=missing_capabilities,
        covered=sorted(capability.value for capability in covered_capabilities),
    )

    required_sut_capabilities = {capability.value for capability in policy.required_capabilities}
    actual_sut_capabilities = set(sut.capabilities)
    sut_ok = (
        sut.mode == "eval"
        and sut.raw.get("recorder_enabled") is True
        and sut.raw.get("capture_policy") == "full"
        and required_sut_capabilities <= actual_sut_capabilities
    )
    _check(
        checks,
        "eval_sut_handshake",
        sut_ok,
        "eval mode and fail-open recorder are available" if sut_ok else "SUT is not safe for eval execution",
        mode=sut.mode,
        recorder_enabled=sut.raw.get("recorder_enabled"),
        capture_policy=sut.raw.get("capture_policy"),
        missing_capabilities=sorted(required_sut_capabilities - actual_sut_capabilities),
    )

    missing_sut_identity = sorted(
        field
        for field in policy.required_sut_identity_fields
        if not _sut_identity_value(sut, field)
    )
    _check(
        checks,
        "sut_provenance",
        not missing_sut_identity,
        "SUT source and running image provenance are complete"
        if not missing_sut_identity
        else "SUT provenance is incomplete",
        missing=missing_sut_identity,
        commit=sut.commit,
        runtime_image_id=sut.raw.get("runtime_image_id"),
        general_agent_image_id=sut.raw.get("general_agent_image_id"),
    )
    if policy.require_clean_sut:
        clean_sut = sut.raw.get("worktree_dirty") == "false"
        _check(
            checks,
            "clean_sut_worktree",
            clean_sut,
            "SUT was built and executed from a clean source commit"
            if clean_sut
            else "formal evaluation cannot attribute a dirty SUT worktree",
            commit=sut.commit,
            worktree_dirty=sut.raw.get("worktree_dirty"),
        )

    unresolved_vars: list[str] = []
    try:
        load_jsonl(dataset_path, resolve_variables=True)
    except Exception as exc:
        unresolved_vars.append(str(exc))
    _check(
        checks,
        "runtime_variables",
        not unresolved_vars,
        "dataset variables resolve inside the isolated runner" if not unresolved_vars else "dataset runtime variables are incomplete",
        errors=unresolved_vars,
    )

    ready = all(check["passed"] for check in checks)
    return {
        "schema_version": 1,
        "status": "READY" if ready else "NOT_READY",
        "formal_eval_executed": False,
        "split": split.value,
        "suite": unresolved[0].suite if unresolved else "",
        "dataset_sha256": digest,
        "dataset_file_sha256": file_sha256(dataset_path),
        "case_count": len(selected),
        "planned_session_count": sum(case.repetitions for case in selected),
        "model_id": actual_model,
        "sut": sut.model_dump(mode="json"),
        "checks": checks,
    }


def assert_ready(report: dict[str, Any]) -> None:
    if report.get("status") == "READY":
        return
    failures = [
        f"{item.get('name')}: {item.get('detail')}"
        for item in report.get("checks") or []
        if isinstance(item, dict) and item.get("passed") is not True
    ]
    raise ReadinessError("preflight failed: " + "; ".join(failures))
