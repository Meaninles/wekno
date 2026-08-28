from __future__ import annotations

import re
from collections import Counter
from typing import Any

from .models import (
    CaseRun,
    CaseSpec,
    MetricScore,
    ObservedTurn,
    TextRule,
    Verdict,
    is_measured_sut_execution_error,
)


CITATION_RE = re.compile(r'<src id="(S[1-9][0-9]*)"\s*/>')
MUTATION_HINTS = (
    "delete", "remove", "create", "update", "replace", "write", "upload", "insert",
    "删除", "新增", "创建", "修改", "替换", "写入", "上传",
)
READ_ONLY_INTERNAL_TOOLS = {"todo_write"}
STATE_SECTION_ALIASES = {
    "active": ("当前有效事实", "当前事实", "已确认事实", "已确认"),
    "retired": ("已废弃事实", "废弃事实", "失效事实"),
    "unknown": (
        "待确认事实", "待确认事项", "待确认项", "未知事实", "未确认事实", "待确认",
    ),
    "action_boundary": ("行动边界", "操作边界", "权限边界"),
}

# These groups intentionally cover only low-risk lexical equivalences that
# repeatedly caused reviewed false negatives. They are not a general synonym
# dictionary: the contract still owns the semantic claim, while the scorer
# avoids requiring one exact surface form for that claim.
TEXT_EQUIVALENCE_GROUPS = (
    ("可以公开", "可公开"),
    ("未提供", "没有提供", "尚未提供", "未说明", "未知"),
    ("废弃", "废止", "作废", "失效"),
    (
        "并非不可替代", "不是不可替代", "不再不可替代", "可替代", "不具排他性",
        "主张不成立", "核验为不成立", "已核验为不成立",
    ),
)
RETIRED_STATUS_TERMS = (
    "废弃", "废止", "作废", "失效", "不再成立", "不成立", "推翻", "否定", "取代", "替代",
)
UNKNOWN_STATUS_TERMS = (
    "待确认", "待核验", "未核验", "尚未核验", "未经核验", "待核实", "尚未核实",
    "未知", "未提供", "没有提供", "尚未提供", "未说明",
)
INTERNAL_PLANNING_PATTERNS = (
    re.compile(r"\blet me (?:carefully|re-?examine|verify|think|check|count|rewrite|organize|answer)\b", re.I),
    re.compile(r"\b(?:stop hook|validation error|output contract|contract says|evidence map|evidence handle|citation handle|current-turn)\b", re.I),
    re.compile(r"\b(?:final attempt|now the key issue|re-reading|let me read)\b", re.I),
)


def _normal(value: str, case_sensitive: bool) -> str:
    # Contracts describe semantic lexical anchors, not presentation syntax.
    # Markdown emphasis and line wrapping must not turn an otherwise exact
    # Chinese phrase (for example ``不少于 **3日**``) into a false negative.
    # Apply the same normalization to observations and contract fragments so
    # English rules with spaces remain symmetric as well.
    without_markdown = re.sub(r"[*_`~]", "", value)
    collapsed = re.sub(r"\s+", "", without_markdown)
    return collapsed if case_sensitive else collapsed.casefold()


def _equivalent_terms(item: str, case_sensitive: bool) -> tuple[str, ...]:
    probe = _normal(item, case_sensitive)
    for group in TEXT_EQUIVALENCE_GROUPS:
        if probe in {_normal(candidate, case_sensitive) for candidate in group}:
            return group
    return (item,)


def _contains_term(value: str, item: str, case_sensitive: bool) -> bool:
    return any(
        _normal(candidate, case_sensitive) in value
        for candidate in _equivalent_terms(item, case_sensitive)
    )


def _rule_matches(rule: TextRule, text: str) -> bool:
    value = _normal(text, rule.case_sensitive)
    any_ok = not rule.any_of or any(
        _contains_term(value, item, rule.case_sensitive) for item in rule.any_of
    )
    all_ok = all(_contains_term(value, item, rule.case_sensitive) for item in rule.all_of)
    excluded = any(
        _contains_term(value, item, rule.case_sensitive) for item in rule.unless_any_of
    )
    return any_ok and all_ok and not excluded


def _has_internal_planning_leak(text: str) -> bool:
    return any(pattern.search(text) for pattern in INTERNAL_PLANNING_PATTERNS)


def _lifecycle_contradiction(rule: TextRule, segment: str, lifecycle: str) -> bool:
    """Detect an actually active stale value, not an explicitly retired/unknown mention."""

    if lifecycle == "action_boundary":
        return _rule_matches(rule, segment)
    status_terms = RETIRED_STATUS_TERMS if lifecycle == "retired" else UNKNOWN_STATUS_TERMS
    normalized_status = {
        _normal(candidate, rule.case_sensitive)
        for term in status_terms
        for candidate in _equivalent_terms(term, rule.case_sensitive)
    }
    identity_terms = [
        term
        for term in [*rule.all_of, *rule.any_of]
        if _normal(term, rule.case_sensitive) not in normalized_status
    ]
    if not identity_terms:
        return False
    value = _normal(segment, rule.case_sensitive)
    identity_present = all(
        _contains_term(value, term, rule.case_sensitive) for term in identity_terms
    )
    explicitly_scoped = any(term in value for term in normalized_status)
    return identity_present and not explicitly_scoped


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

        # A Markdown heading owns every following line (including table rows)
        # until another lifecycle heading appears. Without this precedence, a
        # cell such as “废弃原因” was mistaken for a new section header and the
        # very word proving retirement was discarded from the scored text.
        if "|" not in line:
            table_columns = None
            key = _state_section_key(line)
            if key is not None:
                found.add(key)
                current = key
                sections[key].append(line)
                continue
            if current is not None:
                sections[current].append(line)
            continue

        if current is not None:
            sections[current].append(line)
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
    scope_all_of_to_segments: bool = False,
    all_of_must_be_near_segment_start: bool = True,
    include_heading_sections: bool = True,
) -> None:
    for rule in rules:
        matched = _rule_matches(rule, text)
        if scope_all_of_to_segments and rule.all_of:
            matched = any(
                _rule_matches(rule, segment)
                and (
                    not all_of_must_be_near_segment_start
                    or all(
                        0 <= _normal(segment, rule.case_sensitive).find(
                            _normal(item, rule.case_sensitive)
                        ) <= 48
                        for item in rule.all_of
                    )
                )
                for segment in (
                    _answer_claim_segments(text)
                    if include_heading_sections
                    else [line.strip() for line in text.splitlines() if line.strip()]
                )
            )
        absent = not matched
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


def _answer_claim_segments(text: str) -> list[str]:
    """Return atomic lines plus semantic Markdown sections for citation binding.

    A standalone heading such as ``**询比采购**`` names the claim in the
    paragraph immediately below it. Treating only physical lines as segments
    makes a correctly adjacent citation look detached. Plain paragraphs are
    intentionally not merged, so an unrelated citation on the next line still
    cannot support a claim.
    """

    lines = [line.strip() for line in text.splitlines() if line.strip()]
    segments = list(lines)
    heading_indexes: list[int] = []
    for index, line in enumerate(lines):
        if re.match(r"^#{1,6}\s+\S", line) or re.match(r"^\*\*[^*]+\*\*\s*$", line):
            heading_indexes.append(index)
    for position, start in enumerate(heading_indexes):
        end = heading_indexes[position + 1] if position + 1 < len(heading_indexes) else len(lines)
        if end > start + 1:
            segments.append("\n".join(lines[start:end]))
    return segments


def _first_following_citation(segment: str, rule: TextRule) -> str | None:
    """Return the first citation after the matched claim text in one segment."""

    normalized_chars: list[str] = []
    original_ends: list[int] = []
    for index, char in enumerate(segment):
        if char.isspace() or char in "*_`~":
            continue
        folded = char if rule.case_sensitive else char.casefold()
        for folded_char in folded:
            normalized_chars.append(folded_char)
            original_ends.append(index + 1)
    normalized = "".join(normalized_chars)
    term_ends: list[int] = []
    for term in rule.all_of:
        probe = _normal(term, rule.case_sensitive)
        position = normalized.rfind(probe)
        if position >= 0:
            term_ends.append(position + len(probe))
    for term in rule.any_of:
        probe = _normal(term, rule.case_sensitive)
        position = normalized.rfind(probe)
        if position >= 0:
            term_ends.append(position + len(probe))
    if not term_ends:
        return None
    normalized_end = max(term_ends)
    if normalized_end <= 0 or normalized_end > len(original_ends):
        return None
    match = CITATION_RE.search(segment, original_ends[normalized_end - 1])
    return match.group(1) if match else None


def score_turn(spec: CaseSpec, turn_index: int, observed: ObservedTurn) -> list[MetricScore]:
    turn_spec = spec.turns[turn_index]
    contract = turn_spec.contract
    turn_id = turn_spec.turn_id
    scores: list[MetricScore] = []

    execution_ok = observed.error is None and observed.is_completed and bool(observed.content.strip())
    measured_sut_failure = is_measured_sut_execution_error(observed.error)
    scores.append(
        _score(
            "execution_valid",
            execution_ok,
            execution_ok,
            "completed persisted assistant response" if execution_ok else (observed.error or "response incomplete or empty"),
            turn_id=turn_id,
            metadata={"failure_origin": "sut"} if measured_sut_failure else {},
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
        matched = _rule_matches(rule, observed.content)
        if rule.rule_id == "no-internal-planning":
            matched = matched or _has_internal_planning_leak(observed.content)
        absent = not matched
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
    if state.require_scoped_sections:
        _score_forbidden_rule_group(
            scores,
            prefix="state.forbidden_unknown",
            rules=state.forbidden_unknown_facts,
            text=state_texts["unknown"],
            turn_id=turn_id,
        )
    _score_forbidden_rule_group(
        scores,
        prefix="state.forbidden_inference",
        rules=state.forbidden_inferences,
        text=observed.content,
        turn_id=turn_id,
        scope_all_of_to_segments=True,
        all_of_must_be_near_segment_start=False,
        include_heading_sections=False,
    )
    if state.require_scoped_sections:
        # A fact appearing in its correct section is not enough when the same
        # retired/unknown value is also presented as currently valid. These are
        # lifecycle contradictions, so keep them as hard independent gates.
        for lifecycle, rules in (
            ("retired", state.retired_facts),
            ("unknown", state.unknown_facts),
            ("action_boundary", state.action_boundaries),
        ):
            for rule in rules:
                # Lifecycle contradictions must coexist in one row/line. A
                # section-level conjunction could combine an unrelated "A"
                # fact with a different line's "已废弃" marker.
                active_segments = [
                    line.strip()
                    for line in state_texts["active"].splitlines()
                    if line.strip()
                ]
                clean = not any(
                    _lifecycle_contradiction(rule, segment, lifecycle)
                    for segment in active_segments
                )
                scores.append(
                    _score(
                        f"state.lifecycle.{lifecycle}-not-active.{rule.rule_id}",
                        clean,
                        clean,
                        f"{lifecycle} item must not also appear in the active section",
                        turn_id=turn_id,
                    )
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
            scope_all_of_to_segments=True,
        )

    used_citations = citation_ids(observed)
    reference_ids = [_reference_citation_id(reference) for reference in observed.references]
    reference_ids = [item for item in reference_ids if item]
    citation_integrity = (
        len(reference_ids) == len(set(reference_ids))
        and set(used_citations) == set(reference_ids)
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
        matched = matches >= anchor.min_matching_fragments
        passed = matched or not anchor.required
        matched_anchors += int(matched)
        scores.append(
            _score(
                f"evidence_anchor.{anchor.anchor_id}",
                matches,
                passed,
                anchor.description
                or (
                    f"matching evidence fragments >= {anchor.min_matching_fragments}"
                    if anchor.required
                    else "optional evidence anchor; enforced when its claim is emitted"
                ),
                turn_id=turn_id,
            )
        )

    if contract.evidence_claims:
        answer_segments = _answer_claim_segments(observed.content)
        for claim_rule in contract.evidence_claims:
            claim_segments = [
                segment
                for segment in answer_segments
                if _rule_matches(claim_rule.claim, segment)
            ]
            acceptable_citations: set[str] = set()
            for anchor_id in claim_rule.anchor_ids:
                acceptable_citations.update(anchor_citation_ids.get(anchor_id, set()))
            if claim_rule.require_following_citation:
                adjacent_citations = {
                    citation_id
                    for segment in claim_segments
                    if (citation_id := _first_following_citation(segment, claim_rule.claim))
                }
            else:
                adjacent_citations = {
                    citation_id
                    for segment in claim_segments
                    for citation_id in CITATION_RE.findall(segment)
                }
            if not claim_segments and not claim_rule.required:
                passed = True
            elif claim_rule.require_adjacent_citation:
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
                        "required": claim_rule.required,
                        "require_following_citation": claim_rule.require_following_citation,
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
        mutation_tools = sorted(
            {
                name
                for name in tools
                if name not in READ_ONLY_INTERNAL_TOOLS
                and any(hint in name for hint in MUTATION_HINTS)
            }
        )
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
    execution_failures = [
        score
        for score in scores
        if score.name == "execution_valid" and score.passed is False
    ]
    if execution_failures:
        verdict = (
            Verdict.FAIL
            if all(score.metadata.get("failure_origin") == "sut" for score in execution_failures)
            else Verdict.INVALID
        )
    elif any(score.hard and score.passed is False for score in scores):
        verdict = Verdict.FAIL
    else:
        verdict = Verdict.PASS
    return case_run.model_copy(update={"scores": scores, "verdict": verdict})
