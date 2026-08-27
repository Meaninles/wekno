from __future__ import annotations

import hashlib
import json
import os
import re
from collections import Counter, defaultdict
from pathlib import Path
from typing import Any, Iterable

from .models import (
    AgentSelector,
    Capability,
    CaseSetup,
    CaseSpec,
    FrozenDatasetManifest,
    Provenance,
    Split,
    TurnContract,
    TurnSpec,
)


ENV_RE = re.compile(r"^\$\{([A-Z][A-Z0-9_]*)\}$")
EMAIL_RE = re.compile(r"\b[A-Z0-9._%+-]+@[A-Z0-9.-]+\.[A-Z]{2,}\b", re.IGNORECASE)
PHONE_RE = re.compile(r"(?<!\d)(?:\+?86[- ]?)?1[3-9]\d{9}(?!\d)")
TOKEN_RE = re.compile(r"(?i)\b(?:sk|pk|token|apikey|api_key)[-_a-z0-9]{12,}\b")


class DatasetError(ValueError):
    pass


def canonical_json(value: Any) -> str:
    return json.dumps(value, ensure_ascii=False, sort_keys=True, separators=(",", ":"))


def dataset_sha256(cases: Iterable[CaseSpec]) -> str:
    rows = sorted(canonical_json(case.model_dump(mode="json")) for case in cases)
    return hashlib.sha256(("\n".join(rows) + "\n").encode("utf-8")).hexdigest()


def load_jsonl(path: str | Path, *, resolve_variables: bool = False) -> list[CaseSpec]:
    result: list[CaseSpec] = []
    source = Path(path)
    with source.open("r", encoding="utf-8") as handle:
        for line_number, raw in enumerate(handle, 1):
            raw = raw.strip()
            if not raw or raw.startswith("#"):
                continue
            try:
                payload = json.loads(raw)
                if resolve_variables:
                    payload = resolve_env(payload)
                result.append(CaseSpec.model_validate(payload))
            except Exception as exc:
                raise DatasetError(f"{source}:{line_number}: {exc}") from exc
    return result


def write_jsonl(path: str | Path, cases: Iterable[CaseSpec]) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    with target.open("w", encoding="utf-8", newline="\n") as handle:
        for case in cases:
            handle.write(canonical_json(case.model_dump(mode="json")) + "\n")


def resolve_env(value: Any) -> Any:
    if isinstance(value, str):
        match = ENV_RE.fullmatch(value)
        if not match:
            return value
        name = match.group(1)
        resolved = os.environ.get(name)
        if resolved is None or not resolved.strip():
            raise DatasetError(f"required environment variable {name} is not set")
        return resolved.strip()
    if isinstance(value, list):
        return [resolve_env(item) for item in value]
    if isinstance(value, dict):
        return {key: resolve_env(item) for key, item in value.items()}
    return value


def validate_dataset(cases: list[CaseSpec]) -> list[str]:
    errors: list[str] = []
    seen_case_ids: set[str] = set()
    family_splits: dict[str, set[Split]] = defaultdict(set)
    suites = {case.suite for case in cases}
    if not cases:
        errors.append("dataset is empty")
    if len(suites) > 1:
        errors.append(f"one dataset file must contain one suite, got {sorted(suites)}")
    for case in cases:
        if case.case_id in seen_case_ids:
            errors.append(f"duplicate case_id: {case.case_id}")
        seen_case_ids.add(case.case_id)
        family_splits[case.family_id].add(case.split)
        if case.split != Split.QUARANTINE and case.provenance.needs_codex_review:
            errors.append(f"{case.case_id}: unreviewed case must remain in quarantine")
        if (
            case.split != Split.QUARANTINE
            and case.provenance.metadata.get("requires_branch_curation") is True
        ):
            errors.append(
                f"{case.case_id}: adaptive conversation requires branch-safe curation before promotion"
            )
        if not case.enabled:
            continue
        if case.split != Split.QUARANTINE:
            for turn in case.turns:
                contract = turn.contract
                deterministic_signal_count = sum(
                    [
                        bool(contract.required_claims),
                        bool(contract.forbidden_claims),
                        bool(contract.conversation_state.active_facts),
                        bool(contract.conversation_state.retired_facts),
                        bool(contract.conversation_state.unknown_facts),
                        bool(contract.conversation_state.forbidden_inferences),
                        bool(contract.conversation_state.action_boundaries),
                        bool(
                            contract.decision
                            and (
                                contract.decision.required_unknowns
                                or contract.decision.required_defer_claims
                                or contract.decision.forbidden_recommendations
                            )
                        ),
                        bool(contract.evidence_anchors),
                        bool(contract.evidence_claims),
                        bool(contract.min_retrieved_sources),
                        contract.citation_required,
                        bool(contract.tool_policy.required_tools),
                        bool(contract.tool_policy.forbidden_tools),
                        contract.max_tool_calls is not None,
                        contract.max_total_latency_ms is not None,
                    ]
                )
                if deterministic_signal_count == 0 and case.split in {Split.GATE, Split.SEALED_HOLDOUT}:
                    errors.append(
                        f"{case.case_id}/{turn.turn_id}: release-gated split requires deterministic hard constraints"
                    )
                elif deterministic_signal_count == 0 and not contract.judge_rubric:
                    errors.append(
                        f"{case.case_id}/{turn.turn_id}: reviewed case has no executable contract"
                    )
    for family_id, splits in family_splits.items():
        non_quarantine = splits - {Split.QUARANTINE}
        if len(non_quarantine) > 1:
            errors.append(
                f"family leakage: {family_id} appears in {sorted(split.value for split in non_quarantine)}"
            )
    return errors


def _family_order(family_id: str, salt: str) -> str:
    return hashlib.sha256(f"{salt}\x00{family_id}".encode("utf-8")).hexdigest()


def split_by_family(
    cases: list[CaseSpec],
    *,
    salt: str,
    dev_ratio: float = 0.50,
    gate_ratio: float = 0.25,
    holdout_ratio: float = 0.15,
) -> list[CaseSpec]:
    if not 0 < dev_ratio < 1 or not 0 < gate_ratio < 1 or not 0 < holdout_ratio < 1:
        raise DatasetError("dev/gate/holdout ratios must be between 0 and 1")
    calibration_ratio = 1.0 - dev_ratio - gate_ratio - holdout_ratio
    if calibration_ratio < 0:
        raise DatasetError("split ratios exceed 1.0")
    families = sorted({case.family_id for case in cases}, key=lambda item: _family_order(item, salt))
    count = len(families)
    if count == 0:
        return []

    ratios = [
        (Split.DEV, dev_ratio),
        (Split.GATE, gate_ratio),
        (Split.SEALED_HOLDOUT, holdout_ratio),
        (Split.GRADER_CALIBRATION, calibration_ratio),
    ]
    raw_counts = [(split, ratio * count) for split, ratio in ratios]
    allocations = {split: int(raw) for split, raw in raw_counts}
    remaining = count - sum(allocations.values())
    ranked_remainders = sorted(
        raw_counts,
        key=lambda item: (item[1] - int(item[1]), item[0].value),
        reverse=True,
    )
    for split, _ in ranked_remainders[:remaining]:
        allocations[split] += 1

    # With four or more families, reserve at least one family for every
    # non-zero split. This avoids a tiny suite accidentally becoming all-dev.
    non_zero = [split for split, ratio in ratios if ratio > 0]
    if count >= len(non_zero):
        for split in non_zero:
            if allocations[split] > 0:
                continue
            donor = max(non_zero, key=lambda item: allocations[item])
            if allocations[donor] > 1:
                allocations[donor] -= 1
                allocations[split] += 1

    assigned: dict[str, Split] = {}
    offset = 0
    for split, _ in ratios:
        for family_id in families[offset : offset + allocations[split]]:
            assigned[family_id] = split
        offset += allocations[split]

    return [case.model_copy(update={"split": assigned[case.family_id]}) for case in cases]


def freeze_dataset(
    cases: list[CaseSpec],
    source_files: list[str],
    dependency_sha256: dict[str, str] | None = None,
) -> FrozenDatasetManifest:
    if errors := validate_dataset(cases):
        raise DatasetError("cannot freeze invalid dataset: " + "; ".join(errors))
    counts = Counter(case.split.value for case in cases)
    return FrozenDatasetManifest(
        suite=cases[0].suite,
        dataset_sha256=dataset_sha256(cases),
        case_count=len(cases),
        family_count=len({case.family_id for case in cases}),
        split_counts=dict(sorted(counts.items())),
        source_files=source_files,
        dependency_sha256=dict(sorted((dependency_sha256 or {}).items())),
    )


def redact_text(value: str) -> str:
    value = EMAIL_RE.sub("<EMAIL>", value)
    value = PHONE_RE.sub("<PHONE>", value)
    value = TOKEN_RE.sub("<SECRET>", value)
    return value


def _messages_from_transcript(payload: Any) -> list[dict[str, Any]]:
    if isinstance(payload, list):
        return [item for item in payload if isinstance(item, dict)]
    if not isinstance(payload, dict):
        return []
    for key in ("messages", "conversation", "turns"):
        value = payload.get(key)
        if isinstance(value, list):
            if key == "turns" and any(
                isinstance(item, dict) and "user_message" in item for item in value
            ):
                normalized: list[dict[str, Any]] = []
                for item in value:
                    if not isinstance(item, dict):
                        continue
                    normalized.append(
                        {
                            "role": "user",
                            "content": item.get("user_message") or "",
                        }
                    )
                    normalized.append(
                        {
                            "role": "assistant",
                            "content": item.get("assistant_message") or "",
                            "references": item.get("references") or [],
                        }
                    )
                return normalized
            return [item for item in value if isinstance(item, dict)]
    return []


def _load_transcript_payloads(path: str | Path) -> list[Any]:
    source = Path(path)
    if source.suffix.lower() == ".jsonl":
        values: list[Any] = []
        with source.open("r", encoding="utf-8") as handle:
            for line_number, raw in enumerate(handle, 1):
                raw = raw.strip()
                if not raw:
                    continue
                try:
                    values.append(json.loads(raw))
                except json.JSONDecodeError as exc:
                    raise DatasetError(f"{source}:{line_number}: {exc}") from exc
        return values
    with source.open("r", encoding="utf-8") as handle:
        payload = json.load(handle)
    if isinstance(payload, dict) and isinstance(payload.get("sessions"), list):
        return payload["sessions"]
    return payload if isinstance(payload, list) else [payload]


def _safe_reference_metadata(value: Any) -> dict[str, Any]:
    if not isinstance(value, dict):
        return {}
    allowed = {
        "citation_id",
        "chunk_id",
        "knowledge_id",
        "file_name",
        "source_type",
    }
    return {
        str(key): redact_text(str(item))
        for key, item in value.items()
        if str(key) in allowed and item is not None
    }


def build_quarantine_from_transcripts(
    paths: list[str | Path],
    *,
    suite: str,
    agent_id: str,
    endpoint: str = "agent-chat",
) -> list[CaseSpec]:
    """Harvest real conversations without pretending their answers are gold.

    The output is always quarantined and marked for Codex review. Existing
    assistant answers and exact evidence are retained only as provenance so
    Codex can turn them into acceptable-answer contracts later.
    """

    result: list[CaseSpec] = []
    for source_path in paths:
        for index, payload in enumerate(_load_transcript_payloads(source_path), 1):
            discovery_turns: list[dict[str, Any]] = []
            if isinstance(payload, dict) and str(payload.get("profile_id") or "").strip():
                discovery_turns = [
                    turn
                    for turn in payload.get("turns") or []
                    if isinstance(turn, dict) and "user_message" in turn
                ]
                missing_reviews = [
                    str(turn.get("turn_id") or "<unknown>")
                    for turn in discovery_turns
                    if not isinstance(turn.get("human_review"), dict)
                ]
                if missing_reviews:
                    raise DatasetError(
                        f"{source_path}: adaptive discovery turns must be reviewed before harvest: "
                        + ", ".join(missing_reviews)
                    )
            messages = _messages_from_transcript(payload)
            if not messages:
                continue
            raw_identity = canonical_json(payload)
            source_hash = hashlib.sha256(raw_identity.encode("utf-8")).hexdigest()
            turns: list[TurnSpec] = []
            reference_answers: dict[str, str] = {}
            reference_evidence: list[dict[str, Any]] = []
            pending_user: dict[str, Any] | None = None
            for message in messages:
                role = str(message.get("role") or "").lower()
                if role == "user":
                    pending_user = message
                    continue
                if role != "assistant" or pending_user is None:
                    continue
                query = redact_text(str(pending_user.get("content") or pending_user.get("query") or "")).strip()
                answer = redact_text(str(message.get("content") or message.get("answer") or "")).strip()
                if not query:
                    pending_user = None
                    continue
                turn_id = f"turn-{len(turns) + 1:03d}"
                turns.append(TurnSpec(turn_id=turn_id, query=query, contract=TurnContract()))
                reference_answers[turn_id] = answer
                for reference in message.get("knowledge_references") or message.get("references") or []:
                    if not isinstance(reference, dict):
                        continue
                    reference_evidence.append(
                        {
                            "turn_id": turn_id,
                            "id": str(reference.get("id") or ""),
                            "evidence_content": redact_text(str(reference.get("evidence_content") or reference.get("content") or "")),
                            "metadata": _safe_reference_metadata(reference.get("metadata")),
                        }
                    )
                pending_user = None
            if not turns:
                continue
            short_hash = source_hash[:12]
            resolved_agent_id = str(payload.get("agent_id") or agent_id) if isinstance(payload, dict) else agent_id
            resolved_endpoint = str(payload.get("endpoint") or endpoint) if isinstance(payload, dict) else endpoint
            profile_id = str(payload.get("profile_id") or "") if isinstance(payload, dict) else ""
            provenance_metadata: dict[str, Any] = {}
            if isinstance(payload, dict) and profile_id:
                turn_reviews = {
                    str(turn.get("turn_id") or ""): {
                        "disposition": str(turn["human_review"].get("disposition") or ""),
                        "findings": [
                            redact_text(str(item))
                            for item in turn["human_review"].get("findings") or []
                            if str(item).strip()
                        ],
                        "next_action": redact_text(
                            str(turn["human_review"].get("next_action") or "")
                        ),
                        "eligible_as_gold": False,
                    }
                    for turn in discovery_turns
                }
                rejected_turns = [
                    turn_id
                    for turn_id, review in turn_reviews.items()
                    if review["disposition"] == "rejected_answer"
                ]
                finding_turns = [
                    turn_id
                    for turn_id, review in turn_reviews.items()
                    if review["disposition"] == "accepted_with_findings"
                ]
                provenance_metadata = {
                    "discovery_profile_id": profile_id,
                    "history_turns": int(payload.get("history_turns") or 0),
                    "invalid_attempt_count": len(payload.get("invalid_attempts") or []),
                    "source_kind": "adaptive-discovery",
                    "all_completed_turns_codex_reviewed": True,
                    "turn_reviews": turn_reviews,
                    "rejected_answer_turns": rejected_turns,
                    "accepted_with_findings_turns": finding_turns,
                    "requires_branch_curation": bool(rejected_turns or finding_turns),
                    "conversation_validity": "reviewed-observation-only",
                }
            result.append(
                CaseSpec(
                    case_id=f"harvest-{short_hash}-{index:03d}",
                    family_id=f"harvest-{short_hash}",
                    suite=suite,
                    split=Split.QUARANTINE,
                    enabled=False,
                    capabilities=[Capability.LONG_CONTEXT_DIALOGUE] if len(turns) > 1 else [Capability.RAG_RETRIEVAL],
                    agent=AgentSelector(endpoint=resolved_endpoint, agent_id=resolved_agent_id),
                    setup=CaseSetup(),
                    turns=turns,
                    tags=[
                        "real-conversation",
                        "needs-codex-curation",
                        *([f"profile:{profile_id}"] if profile_id else []),
                    ],
                    provenance=Provenance(
                        source=Path(source_path).name,
                        source_hash=source_hash,
                        needs_codex_review=True,
                        reference_answers=reference_answers,
                        reference_evidence=reference_evidence,
                        metadata=provenance_metadata,
                    ),
                )
            )
    return result
