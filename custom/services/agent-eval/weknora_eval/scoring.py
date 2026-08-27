from __future__ import annotations

import re
from collections import Counter
from typing import Any

from .models import CaseRun, CaseSpec, MetricScore, ObservedTurn, TextRule, Verdict


CITATION_RE = re.compile(r'<src id="(S[1-9][0-9]*)"\s*/>')
MUTATION_HINTS = (
    "delete", "remove", "create", "update", "replace", "write", "upload", "insert",
    "删除", "新增", "创建", "修改", "替换", "写入", "上传",
)
STATE_SECTION_ALIASES = {
    "active": ("当前有效事实", "当前事实", "已确认事实"),
    "retired": ("已废弃事实", "废弃事实", "失效事实"),
    "unknown": ("待确认事实", "待确认事项", "待确认项", "未知事实", "未确认事实"),
    "action_boundary": ("行动边界", "操作边界", "权限边界"),
}


def _normal(value: str, case_sensitive: bool) -> str:
    collapsed = " ".join(value.split())
    return collapsed if case_sensitive else collapsed.casefold()


def _rule_matches(rule: TextRule, text: str) -> bool:
    value = _normal(text, rule.case_sensitive)
    any_ok = not rule.any_of or any(_normal(item, rule.case_sensitive) in value for item in rule.any_of)
    all_ok = all(_normal(item, rule.case_sensitive) in value for item in rule.all_of)
    excluded = any(
        _normal(item, rule.case_sensitive) in value for item in rule.unless_any_of
    )
    return any_ok and all_ok and not excluded


def _state_section_key(value: str) -> str | None:
    normalized = _normal(value, False)
    for key, aliases in STATE_SECTION_ALIASES.items():
        if any(_normal(alias, False) in normalized for alias in aliases):
            return key
    return None


def _state_section_texts(text: str) -> tuple[dict[str, str], set[str]]:
    """Extract common Markdown heading/table layouts without requiring one template."""

    sections: dict[str, list[str]] = {key: [] for key in STATE_SECTION_ALIASES}
    found: set[str] = set()
    current: str | None = None
    table_columns: dict[int, str] | None = None

    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line:
            continue

        if "|" in line:
            cells = [cell.strip() for cell in line.strip("|").split("|")]
            mapped = {
                index: key
                for index, cell in enumerate(cells)
                if (key := _state_section_key(cell)) is not None
            }
            if len(mapped) >= 2:
                table_columns = mapped
                found.update(mapped.values())
                current = None
                continue
            if table_columns is not None:
                if all(not cell.strip(" :-") for cell in cells):
                    continue
                for index, key in table_columns.items():
                    if index < len(cells):
                        sections[key].append(cells[index])
                continue
            if len(mapped) == 1:
                index, key = next(iter(mapped.items()))
                found.add(key)
                sections[key].extend(
                    cell for cell_index, cell in enumerate(cells) if cell_index != index
                )
                current = None
                continue
        else:
            table_columns = None

        key = _state_section_key(line)
        if key is not None:
            found.add(key)
            current = key
            sections[key].append(line)
            continue
        if current is not None:
            sections[current].append(line)

    return {key: "\n".join(lines) for key, lines in sections.items()}, found


def _score(
    name: str,
    value: float | bool | str,
    passed: bool | None,
    comment: str,
    *,
    turn_id: str,
    hard: bool = True,
    metadata: dict[str, Any] | None = None,
) -> MetricScore:
    return MetricScore(
        name=name,
        value=value,
        passed=passed,
        hard=hard,
        comment=comment,
        turn_id=turn_id,
        metadata=metadata or {},
    )


def _score_required_rule_group(
    scores: list[MetricScore],
    *,
    prefix: str,
    rules: list[TextRule],
    text: str,
    turn_id: str,
) -> None:
    for rule in rules:
        passed = _rule_matches(rule, text)
        scores.append(
            _score(
                f"{prefix}.{rule.rule_id}",
                passed,
                passed,
                rule.description or "required acceptable state",
                turn_id=turn_id,
            )
        )


def _score_forbidden_rule_group(
    scores: list[MetricScore],
    *,
    prefix: str,
    rules: list[TextRule],
    text: str,
    turn_id: str,
) -> None:
    for rule in rules:
        absent = not _rule_matches(rule, text)
        scores.append(
            _score(
                f"{prefix}.{rule.rule_id}",
                absent,
                absent,
                rule.description or "forbidden state must be absent",
                turn_id=turn_id,
            )
        )


def citation_ids(turn: ObservedTurn) -> list[str]:
    return list(dict.fromkeys(CITATION_RE.findall(turn.content)))


def _reference_citation_id(reference: dict[str, Any]) -> str:
    metadata = reference.get("metadata") if isinstance(reference.get("metadata"), dict) else {}
    return str(metadata.get("citation_id") or "")


def _reference_source_id(reference: dict[str, Any]) -> str:
    metadata = reference.get("metadata") if isinstance(reference.get("metadata"), dict) else {}
    for value in (
        metadata.get("chunk_id"),
        reference.get("id"),
        reference.get("knowledge_id"),
        metadata.get("knowledge_id"),
        metadata.get("url"),
    ):
        if value:
            return str(value)
    return ""


def _evidence_text(reference: dict[str, Any]) -> str:
    return str(reference.get("evidence_content") or reference.get("content") or "")


def score_turn(spec: CaseSpec, turn_index: int, observed: ObservedTurn) -> list[MetricScore]:
    turn_spec = spec.turns[turn_index]
    contract = turn_spec.contract
    turn_id = turn_spec.turn_id
    scores: list[MetricScore] = []

    execution_ok = observed.error is None and observed.is_completed and bool(observed.content.strip())
    scores.append(
        _score(
            "execution_valid",
            execution_ok,
            execution_ok,
            "completed persisted assistant response" if execution_ok else (observed.error or "response incomplete or empty"),
            turn_id=turn_id,
        )
    )
    if not execution_ok:
        return scores

    response_length = len(observed.content)
    min_ok = response_length >= contract.min_response_chars
    scores.append(
        _score(
            "response_min_length",
            response_length,
            min_ok,
            f"response chars {response_length}, required >= {contract.min_response_chars}",
            turn_id=turn_id,
        )
    )
    if contract.max_response_chars is not None:
        max_ok = response_length <= contract.max_response_chars
        scores.append(
            _score(
                "response_max_length",
                response_length,
                max_ok,
                f"response chars {response_length}, required <= {contract.max_response_chars}",
                turn_id=turn_id,
            )
        )

    for rule in contract.required_claims:
        passed = _rule_matches(rule, observed.content)
        scores.append(
            _score(
                f"required_claim.{rule.rule_id}",
                passed,
                passed,
                rule.description or "required acceptable claim",
                turn_id=turn_id,
            )
        )
    for rule in contract.forbidden_claims:
        absent = not _rule_matches(rule, observed.content)
        scores.append(
            _score(
                f"forbidden_claim.{rule.rule_id}",
                absent,
                absent,
                rule.description or "forbidden claim must be absent",
                turn_id=turn_id,
            )
        )

    state = contract.conversation_state
    state_texts = {key: observed.content for key in STATE_SECTION_ALIASES}
    if state.require_scoped_sections:
        state_texts, found_sections = _state_section_texts(observed.content)
        required_sections = {
            key
            for key, rules in {
                "active": state.active_facts,
                "retired": state.retired_facts,
                "unknown": state.unknown_facts,
                "action_boundary": state.action_boundaries,
            }.items()
            if rules
        }
        section_structure_ok = required_sections <= found_sections
        scores.append(
            _score(
                "state.section_structure",
                section_structure_ok,
                section_structure_ok,
                "required state sections must be present and independently scoped",
                turn_id=turn_id,
                metadata={
                    "required_sections": sorted(required_sections),
                    "found_sections": sorted(found_sections),
                },
            )
        )
    _score_required_rule_group(
        scores,
        prefix="state.active",
        rules=state.active_facts,
        text=state_texts["active"],
        turn_id=turn_id,
    )
    _score_required_rule_group(
        scores,
        prefix="state.retired",
        rules=state.retired_facts,
        text=state_texts["retired"],
        turn_id=turn_id,
    )
    _score_required_rule_group(
        scores,
        prefix="state.unknown",
        rules=state.unknown_facts,
        text=state_texts["unknown"],
        turn_id=turn_id,
    )
    _score_required_rule_group(
        scores,
        prefix="state.action_boundary",
        rules=state.action_boundaries,
        text=state_texts["action_boundary"],
        turn_id=turn_id,
    )
    _score_forbidden_rule_group(
        scores,
        prefix="state.forbidden_inference",
        rules=state.forbidden_inferences,
        text=observed.content,
        turn_id=turn_id,
    )

    if contract.decision is not None:
        _score_required_rule_group(
            scores,
            prefix="decision.unknown",
            rules=contract.decision.required_unknowns,
            text=observed.content,
            turn_id=turn_id,
        )
        _score_required_rule_group(
            scores,
            prefix="decision.defer",
            rules=contract.decision.required_defer_claims,
            text=observed.content,
            turn_id=turn_id,
        )
        _score_forbidden_rule_group(
            scores,
            prefix="decision.forbidden_recommendation",
            rules=contract.decision.forbidden_recommendations,
            text=observed.content,
            turn_id=turn_id,
        )

    used_citations = citation_ids(observed)
    reference_ids = [_reference_citation_id(reference) for reference in observed.references]
    reference_ids = [item for item in reference_ids if item]
    citation_integrity = (
        len(reference_ids) == len(set(reference_ids))
        and used_citations == reference_ids
        and all(_evidence_text(reference).strip() for reference in observed.references)
    )
    if used_citations or observed.references or contract.citation_required:
        scores.append(
            _score(
                "citation_integrity",
                citation_integrity,
                citation_integrity,
                f"body citations={used_citations}, persisted references={reference_ids}",
                turn_id=turn_id,
            )
        )
    citation_floor_ok = len(used_citations) >= contract.min_citations
    if contract.citation_required or contract.min_citations:
        scores.append(
            _score(
                "citation_minimum",
                len(used_citations),
                citation_floor_ok,
                f"citations {len(used_citations)}, required >= {contract.min_citations}",
                turn_id=turn_id,
            )
        )
    if contract.max_citations is not None:
        citation_ceiling_ok = len(used_citations) <= contract.max_citations
        scores.append(
            _score(
                "citation_maximum",
                len(used_citations),
                citation_ceiling_ok,
                f"citations {len(used_citations)}, required <= {contract.max_citations}",
                turn_id=turn_id,
            )
        )

    matched_anchors = 0
    anchor_citation_ids: dict[str, set[str]] = {}
    for anchor in contract.evidence_anchors:
        matches = 0
        matched_citations: set[str] = set()
        for reference in observed.references:
            text = _normal(_evidence_text(reference), False)
            source_id = _reference_source_id(reference)
            any_ok = not anchor.any_of or any(_normal(item, False) in text for item in anchor.any_of)
            all_ok = all(_normal(item, False) in text for item in anchor.all_of)
            source_ok = not anchor.source_ids or source_id in anchor.source_ids
            if any_ok and all_ok and source_ok:
                matches += 1
                citation_id = _reference_citation_id(reference)
                if citation_id:
                    matched_citations.add(citation_id)
        anchor_citation_ids[anchor.anchor_id] = matched_citations
        passed = matches >= anchor.min_matching_fragments
        matched_anchors += int(passed)
        scores.append(
            _score(
                f"evidence_anchor.{anchor.anchor_id}",
                matches,
                passed,
                anchor.description or f"matching evidence fragments >= {anchor.min_matching_fragments}",
                turn_id=turn_id,
            )
        )

    if contract.evidence_claims:
        answer_segments = [
            segment.strip()
            for segment in re.split(r"(?:\r?\n)+", observed.content)
            if segment.strip()
        ]
        for claim_rule in contract.evidence_claims:
            claim_segments = [
                segment
                for segment in answer_segments
                if _rule_matches(claim_rule.claim, segment)
            ]
            acceptable_citations: set[str] = set()
            for anchor_id in claim_rule.anchor_ids:
                acceptable_citations.update(anchor_citation_ids.get(anchor_id, set()))
            adjacent_citations = {
                citation_id
                for segment in claim_segments
                for citation_id in CITATION_RE.findall(segment)
            }
            if claim_rule.require_adjacent_citation:
                passed = bool(claim_segments) and bool(
                    acceptable_citations & adjacent_citations
                )
            else:
                passed = bool(claim_segments) and bool(acceptable_citations)
            scores.append(
                _score(
                    f"evidence_claim.{claim_rule.rule_id}",
                    passed,
                    passed,
                    claim_rule.description
                    or "claim must be supported by an allowed adjacent citation",
                    turn_id=turn_id,
                    metadata={
                        "claim_segments": len(claim_segments),
                        "acceptable_citations": sorted(acceptable_citations),
                        "adjacent_citations": sorted(adjacent_citations),
                    },
                )
            )
    if contract.evidence_anchors:
        anchor_floor_ok = matched_anchors >= contract.min_evidence_anchors
        scores.append(
            _score(
                "evidence_anchor_coverage",
                matched_anchors / len(contract.evidence_anchors),
                anchor_floor_ok,
                f"matched {matched_anchors}/{len(contract.evidence_anchors)} anchors; floor={contract.min_evidence_anchors}",
                turn_id=turn_id,
            )
        )

    stats = observed.retrieval_stats
    for source_type, minimum in sorted(contract.min_retrieved_sources.items()):
        actual = int(stats.get(source_type, 0) or 0)
        passed = actual >= minimum
        scores.append(
            _score(
                f"retrieval_floor.{source_type}",
                actual,
                passed,
                f"retrieved {actual} {source_type}, required >= {minimum}",
                turn_id=turn_id,
            )
        )
    if contract.forbid_stale_citations_without_retrieval:
        total = int(stats.get("total", 0) or 0)
        no_stale = total > 0 or (not used_citations and not observed.references)
        scores.append(
            _score(
                "no_stale_citations",
                no_stale,
                no_stale,
                f"retrieved total={total}, citations={len(used_citations)}, references={len(observed.references)}",
                turn_id=turn_id,
            )
        )

    tools = [name.casefold() for name in observed.tools]
    tool_counts = Counter(tools)
    for required in contract.tool_policy.required_tools:
        passed = required.casefold() in tool_counts
        scores.append(
            _score(
                f"required_tool.{required}",
                tool_counts[required.casefold()],
                passed,
                "required tool invoked",
                turn_id=turn_id,
            )
        )
    for forbidden in contract.tool_policy.forbidden_tools:
        passed = forbidden.casefold() not in tool_counts
        scores.append(
            _score(
                f"forbidden_tool.{forbidden}",
                tool_counts[forbidden.casefold()],
                passed,
                "forbidden tool not invoked",
                turn_id=turn_id,
            )
        )
    if contract.tool_policy.allowed_tools:
        allowed = {name.casefold() for name in contract.tool_policy.allowed_tools}
        unexpected = sorted({name for name in tools if name not in allowed})
        scores.append(
            _score(
                "tool_allowlist",
                len(unexpected),
                not unexpected,
                f"unexpected tools={unexpected}",
                turn_id=turn_id,
            )
        )
    if contract.tool_policy.read_only:
        mutation_tools = sorted({name for name in tools if any(hint in name for hint in MUTATION_HINTS)})
        scores.append(
            _score(
                "read_only_tool_policy",
                len(mutation_tools),
                not mutation_tools,
                f"mutation-like tools={mutation_tools}",
                turn_id=turn_id,
            )
        )

    if contract.max_tool_calls is not None:
        actual_tool_calls = (
            observed.agent_tool_count
            if observed.agent_tool_count > 0
            else len(observed.tools)
        )
        tool_budget_ok = actual_tool_calls <= contract.max_tool_calls
        scores.append(
            _score(
                "tool_call_ceiling",
                actual_tool_calls,
                tool_budget_ok,
                f"tool calls {actual_tool_calls}, required <= {contract.max_tool_calls}",
                turn_id=turn_id,
            )
        )

    if contract.max_total_latency_ms is not None:
        latency_ok = observed.total_latency_ms <= contract.max_total_latency_ms
        scores.append(
            _score(
                "latency_ceiling",
                observed.total_latency_ms,
                latency_ok,
                f"latency {observed.total_latency_ms}ms, required <= {contract.max_total_latency_ms}ms",
                turn_id=turn_id,
            )
        )
    scores.append(
        _score(
            "latency_ms",
            observed.total_latency_ms,
            None,
            "informational latency measurement",
            turn_id=turn_id,
            hard=False,
        )
    )
    return scores


def score_case(spec: CaseSpec, case_run: CaseRun) -> CaseRun:
    if case_run.error or len(case_run.turns) != len(spec.turns):
        return case_run.model_copy(update={"verdict": Verdict.INVALID})
    scores: list[MetricScore] = []
    for index, turn in enumerate(case_run.turns):
        scores.extend(score_turn(spec, index, turn))
    if any(score.name == "execution_valid" and score.passed is False for score in scores):
        verdict = Verdict.INVALID
    elif any(score.hard and score.passed is False for score in scores):
        verdict = Verdict.FAIL
    else:
        verdict = Verdict.PASS
    return case_run.model_copy(update={"scores": scores, "verdict": verdict})
