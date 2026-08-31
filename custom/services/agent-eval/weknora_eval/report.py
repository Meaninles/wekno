from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from html import escape
from pathlib import Path

from .models import AnswerTrack, ExperimentRun, GateResult, Verdict


def _safe(value: object) -> str:
    return str(value or "").replace("|", "/").replace("\n", " ")


def _percentile(values: list[int], fraction: float) -> int:
    if not values:
        return 0
    ordered = sorted(values)
    return ordered[max(0, math.ceil(len(ordered) * fraction) - 1)]


def _failure_class(metric: str) -> str:
    if metric == "execution_valid":
        return "execution/infrastructure"
    if metric.startswith(("citation_", "evidence_", "retrieval_")):
        return "retrieval/grounding"
    if metric.startswith(("read_only_", "forbidden_tool", "state.action_boundary")):
        return "tool/action safety"
    if metric in {"response_max_length", "response_min_length"} or metric.startswith(
        "forbidden_claim.no-internal-planning"
    ):
        return "response hygiene"
    if metric.startswith("latency"):
        return "performance"
    return "semantic/state"


def _repair_only_pass(case: object) -> bool:
    production_review = getattr(case, "production_codex_review", None)
    assisted_review = getattr(case, "eval_assisted_codex_review", None)
    if production_review is not None and assisted_review is not None:
        return (
            production_review.verdict.value != "PASS"
            and assisted_review.verdict.value == "PASS"
        )
    return bool(getattr(case, "repair_only_pass", False))


def _selected_verdict(case: object, gate: GateResult | None) -> Verdict:
    track = gate.answer_track if gate is not None else AnswerTrack.PRODUCTION_CANDIDATE
    if track == AnswerTrack.PRODUCTION_CANDIDATE:
        review = getattr(case, "production_codex_review", None)
        if review is not None:
            return review.verdict
        return getattr(case, "production_verdict", None) or case.verdict
    if track == AnswerTrack.EVAL_ASSISTED_ANSWER:
        review = getattr(case, "eval_assisted_codex_review", None)
        if review is not None:
            return review.verdict
        return getattr(case, "eval_assisted_verdict", None) or Verdict.INVALID
    return case.verdict


def _score_payload(scores: list[object], turn_id: str) -> list[dict[str, object]]:
    return [
        {
            "name": score.name,
            "passed": score.passed,
            "value": score.value,
            "comment": score.comment,
        }
        for score in scores
        if score.turn_id == turn_id
    ]


def _preformatted(value: object) -> str:
    rendered = (
        value
        if isinstance(value, str)
        else json.dumps(value, ensure_ascii=False, indent=2, sort_keys=True)
    )
    return f"<pre>{escape(rendered)}</pre>"


def write_json(path: str | Path, value: object) -> None:
    target = Path(path)
    target.parent.mkdir(parents=True, exist_ok=True)
    payload = value.model_dump(mode="json") if hasattr(value, "model_dump") else value
    with target.open("w", encoding="utf-8", newline="\n") as handle:
        json.dump(payload, handle, ensure_ascii=False, indent=2, sort_keys=True)
        handle.write("\n")


def load_run(path: str | Path) -> ExperimentRun:
    with Path(path).open("r", encoding="utf-8") as handle:
        return ExperimentRun.model_validate(json.load(handle))


def load_gate(path: str | Path) -> GateResult:
    with Path(path).open("r", encoding="utf-8") as handle:
        return GateResult.model_validate(json.load(handle))


def render_markdown(run: ExperimentRun, gate: GateResult | None = None) -> str:
    selected_verdicts = Counter(_selected_verdict(case, gate).value for case in run.cases)
    diagnostic_verdicts = Counter(case.verdict.value for case in run.cases)
    capability_metrics: dict[str, list[bool]] = defaultdict(list)
    for case in run.cases:
        for score in case.scores:
            if score.passed is not None:
                capability_metrics[score.name].append(score.passed)
    execution_contract = (
        run.metadata.get("execution_contract")
        if isinstance(run.metadata.get("execution_contract"), dict)
        else {}
    )
    lines = [
        f"# WeKnora Agent Eval — {run.run_id}",
        "",
        f"- Suite: `{run.suite}`",
        f"- Dataset: `{run.dataset_sha256}`",
        f"- SUT: mode=`{run.sut.mode}`, release=`{run.sut.release}`, commit=`{run.sut.commit}`",
        f"- Selected-track decisions: {len(run.cases)} "
        f"(PASS={selected_verdicts['PASS']}, FAIL={selected_verdicts['FAIL']}, "
        f"INVALID={selected_verdicts['INVALID']})",
        f"- Deterministic diagnostics: PASS={diagnostic_verdicts['PASS']}, "
        f"FAIL={diagnostic_verdicts['FAIL']}, INVALID={diagnostic_verdicts['INVALID']} "
        "(informational for Codex-review v2 quality gates)",
    ]
    if execution_contract:
        lines.extend(
            [
                "",
                "## Execution identity",
                "",
                "| Field | Value |",
                "|---|---|",
            ]
        )
        for field, value in sorted(execution_contract.items()):
            lines.append(f"| `{field}` | `{_safe(value)}` |")
    sut_provenance = {
        "source_commit": run.sut.commit,
        "worktree_dirty": run.sut.raw.get("worktree_dirty"),
        "runtime_image_id": run.sut.raw.get("runtime_image_id"),
        "general_agent_image_id": run.sut.raw.get("general_agent_image_id"),
        "reported_commit": run.sut.raw.get("reported_commit"),
    }
    if any(value not in (None, "") for value in sut_provenance.values()):
        lines.extend(
            [
                "",
                "## SUT provenance",
                "",
                "| Field | Value |",
                "|---|---|",
            ]
        )
        for field, value in sut_provenance.items():
            lines.append(f"| `{field}` | `{_safe(value)}` |")
    if gate is not None:
        lines.extend(
            [
                f"- Gate: **{gate.verdict.value}** (`{gate.policy_id}`)",
                f"- Gate kind: `{gate.gate_kind.value}`; answer track: `{gate.answer_track.value}`",
                "",
            ]
        )
        lines.extend(["## Gate checks", "", "| Check | Verdict | Detail |", "|---|---:|---|"])
        for check in gate.checks:
            lines.append(f"| `{check.name}` | **{check.verdict.value}** | {check.comment.replace('|', '/')} |")
    lines.extend(
        [
            "",
            "## Per-agent summary",
            "",
            "| Agent | Sessions | PASS | FAIL | INVALID | Turns | p50 | p95 | Max |",
            "|---|---:|---:|---:|---:|---:|---:|---:|---:|",
        ]
    )
    agents = sorted({case.agent_profile_id or "unassigned" for case in run.cases})
    for agent in agents:
        cases = [
            case for case in run.cases if (case.agent_profile_id or "unassigned") == agent
        ]
        agent_verdicts = Counter(_selected_verdict(case, gate).value for case in cases)
        latencies = [turn.total_latency_ms for case in cases for turn in case.turns]
        lines.append(
            f"| `{agent}` | {len(cases)} | {agent_verdicts['PASS']} | "
            f"{agent_verdicts['FAIL']} | {agent_verdicts['INVALID']} | {len(latencies)} | "
            f"{_percentile(latencies, 0.50)}ms | {_percentile(latencies, 0.95)}ms | "
            f"{max(latencies, default=0)}ms |"
        )

    lines.extend(
        [
            "",
            "## Cases",
            "",
            "| Case | Attempt | Agent | Split | Production diagnostics | Production Codex | Assisted diagnostics | Assisted Codex | Repair-only pass | Turns | Failed production metrics | Error |",
            "|---|---:|---|---|---:|---:|---:|---:|---:|---:|---|---|",
        ]
    )
    for case in run.cases:
        failed_metrics = sorted(
            {score.name for score in case.scores if score.passed is False}
        )
        lines.append(
            f"| `{case.case_id}` | {case.attempt_index} | "
            f"`{case.agent_profile_id or 'unassigned'}` | `{case.split.value}` | "
            f"**{(case.production_verdict or case.verdict).value}** | "
            f"**{case.production_codex_review.verdict.value if case.production_codex_review else '—'}** | "
            f"**{(case.eval_assisted_verdict or case.verdict).value}** | "
            f"**{case.eval_assisted_codex_review.verdict.value if case.eval_assisted_codex_review else '—'}** | "
            f"{'yes' if _repair_only_pass(case) else 'no'} | {len(case.turns)} | "
            f"{_safe(', '.join(failed_metrics))} | {_safe(case.error)} |"
        )

    all_turns = [turn for case in run.cases for turn in case.turns]
    triggered = [turn for turn in all_turns if turn.repair.triggered]
    successful = [turn for turn in triggered if turn.repair.succeeded]
    repair_only = [case for case in run.cases if _repair_only_pass(case)]
    lines.extend(
        [
            "",
            "## Repair dependency",
            "",
            f"- Repair trigger rate: {len(triggered)}/{len(all_turns)} "
            f"({len(triggered) / len(all_turns) if all_turns else 0:.1%})",
            f"- Repair success rate: {len(successful)}/{len(triggered)} "
            f"({len(successful) / len(triggered) if triggered else 0:.1%})",
            f"- Repair-only pass rate: {len(repair_only)}/{len(run.cases)} "
            f"({len(repair_only) / len(run.cases) if run.cases else 0:.1%})",
            f"- Average repair attempts: "
            f"{sum(turn.repair.attempts for turn in triggered) / len(triggered) if triggered else 0:.2f}",
            f"- Added model calls: {sum(turn.repair.added_model_calls for turn in all_turns)}",
            f"- Added tool calls: {sum(turn.repair.added_tool_calls for turn in all_turns)}",
            f"- Added latency: {sum(turn.repair.added_latency_ms for turn in all_turns)}ms",
        ]
    )

    lines.extend(
        [
            "",
            "## Dual-track answer ledger",
            "",
            "The JSON artifact stores the complete snapshots and all scores. "
            "This ledger makes surface equality and repair cost visible without "
            "treating deterministic phrase diagnostics as the quality verdict.",
            "",
            "| Case | Attempt | Turn | Production chars/refs | Assisted chars/refs | Same answer | SSE = history | Repair | Production diagnostic failures | Assisted diagnostic failures |",
            "|---|---:|---|---:|---:|---:|---:|---|---|---|",
        ]
    )
    divergent_turns: list[tuple[object, object, str, str, object, object]] = []
    for case in run.cases:
        production_scores = case.production_scores or case.scores
        assisted_scores = case.eval_assisted_scores
        for turn in case.turns:
            production = turn.production_candidate
            assisted = turn.eval_assisted_answer
            production_content = production.content if production is not None else turn.content
            production_refs = production.references if production is not None else turn.references
            assisted_content = assisted.content if assisted is not None else ""
            assisted_refs = assisted.references if assisted is not None else []
            same_answer = (
                assisted is not None
                and production_content == assisted_content
                and production_refs == assisted_refs
            )
            production_failed = sorted(
                score.name
                for score in production_scores
                if score.turn_id == turn.turn_id and score.passed is False
            )
            assisted_failed = sorted(
                score.name
                for score in assisted_scores
                if score.turn_id == turn.turn_id and score.passed is False
            )
            repair_label = "none"
            if turn.repair.triggered:
                repair_label = (
                    f"{','.join(turn.repair.repair_types) or 'unspecified'}; "
                    f"attempts={turn.repair.attempts}; success={turn.repair.succeeded}; "
                    f"model+={turn.repair.added_model_calls}; "
                    f"tool+={turn.repair.added_tool_calls}; "
                    f"latency+={turn.repair.added_latency_ms}ms"
                )
                if turn.repair.failure_reason:
                    repair_label += f"; failure={turn.repair.failure_reason}"
            surface = (
                "yes"
                if turn.production_surface_equivalent is True
                else "no"
                if turn.production_surface_equivalent is False
                else "unproven"
            )
            lines.append(
                f"| `{case.case_id}` | {case.attempt_index} | `{turn.turn_id}` | "
                f"{len(production_content)}/{len(production_refs)} | "
                f"{len(assisted_content)}/{len(assisted_refs)} | "
                f"{'yes' if same_answer else 'no'} | {surface} | {_safe(repair_label)} | "
                f"{_safe(', '.join(production_failed) or 'none')} | "
                f"{_safe(', '.join(assisted_failed) or 'none')} |"
            )
            if (assisted is not None and not same_answer) or turn.repair.triggered:
                divergent_turns.append(
                    (case, turn, production_content, assisted_content, production_refs, assisted_refs)
                )

    if divergent_turns:
        lines.extend(["", "### Repair/difference details", ""])
        for case, turn, production_content, assisted_content, production_refs, assisted_refs in divergent_turns:
            production_scores = case.production_scores or case.scores
            assisted_scores = case.eval_assisted_scores
            lines.extend(
                [
                    f"<details><summary><code>{escape(case.case_id)}#attempt-{case.attempt_index}/{escape(turn.turn_id)}</code></summary>",
                    "",
                    "Production candidate:",
                    _preformatted(production_content),
                    "Production references:",
                    _preformatted(production_refs),
                    "Production scores:",
                    _preformatted(_score_payload(production_scores, turn.turn_id)),
                    "Eval-assisted answer:",
                    _preformatted(assisted_content),
                    "Eval-assisted references:",
                    _preformatted(assisted_refs),
                    "Eval-assisted scores:",
                    _preformatted(_score_payload(assisted_scores, turn.turn_id)),
                    "Repair trace:",
                    _preformatted(turn.repair.model_dump(mode="json")),
                    "",
                    "</details>",
                    "",
                ]
            )

    codex_reviews = [
        (case, track, review)
        for case in run.cases
        for track, review in (
            ("production", case.production_codex_review),
            ("eval_assisted", case.eval_assisted_codex_review),
        )
        if review is not None
    ]
    if codex_reviews:
        review_verdicts = Counter(review.verdict.value for _, _, review in codex_reviews)
        minor_count = sum(
            item.rating.value == "minor_issue"
            for _, _, review in codex_reviews
            for item in review.dimensions.values()
        )
        major_count = sum(
            item.rating.value == "major_issue"
            for _, _, review in codex_reviews
            for item in review.dimensions.values()
        )
        lines.extend(
            [
                "",
                "## Codex whole-conversation review",
                "",
                "- Standard: materially correct and useful; minor issues are allowed, material errors are not.",
                f"- Reviews: {len(codex_reviews)} "
                f"(PASS={review_verdicts['PASS']}, FAIL={review_verdicts['FAIL']}, INVALID={review_verdicts['INVALID']})",
                f"- Dimension findings: minor={minor_count}, major={major_count}",
                "",
                "| Case | Attempt | Track | Verdict | Summary | Findings |",
                "|---|---:|---|---:|---|---|",
            ]
        )
        for case, track, review in codex_reviews:
            lines.append(
                f"| `{case.case_id}` | {case.attempt_index} | `{track}` | "
                f"**{review.verdict.value}** | {_safe(review.summary)} | "
                f"{_safe('; '.join([*review.findings, *review.critical_findings]))} |"
            )

    failed_rows: list[tuple[str, int, str, str, str, int]] = []
    failure_taxonomy: Counter[str] = Counter()
    for case in run.cases:
        by_turn: dict[str, list[str]] = defaultdict(list)
        for score in case.scores:
            if score.passed is False:
                by_turn[score.turn_id or "case"].append(score.name)
                failure_taxonomy[_failure_class(score.name)] += 1
        if case.error:
            failure_taxonomy["execution/infrastructure"] += 1
        observed = {turn.turn_id: turn for turn in case.turns}
        for turn_id, names in by_turn.items():
            turn = observed.get(turn_id)
            failed_rows.append(
                (
                    case.case_id,
                    case.attempt_index,
                    case.agent_profile_id or "unassigned",
                    turn_id,
                    ", ".join(sorted(set(names))),
                    turn.total_latency_ms if turn else 0,
                )
            )
    lines.extend(
        [
            "",
            "## Deterministic diagnostic failures",
            "",
            "| Case | Attempt | Agent | Turn | Failed metrics | Latency |",
            "|---|---:|---|---|---|---:|",
        ]
    )
    if failed_rows:
        for case_id, attempt, agent, turn_id, metrics, latency in failed_rows:
            lines.append(
                f"| `{case_id}` | {attempt} | `{agent}` | `{turn_id}` | "
                f"{_safe(metrics)} | {latency}ms |"
            )
    else:
        lines.append("| — | — | — | — | none | — |")

    lines.extend(
        [
            "",
            "## Failure taxonomy",
            "",
            "| Class | Failed signals |",
            "|---|---:|",
        ]
    )
    if failure_taxonomy:
        for name, count in sorted(failure_taxonomy.items()):
            lines.append(f"| {name} | {count} |")
    else:
        lines.append("| none | 0 |")

    judge_scores = [
        score
        for case in run.cases
        for score in case.scores
        if score.name == "judge.contract_satisfaction"
    ]
    if judge_scores:
        labels = Counter(str(score.value) for score in judge_scores)
        confidences = [float(score.metadata.get("confidence", 0)) for score in judge_scores]
        lines.extend(
            [
                "",
                "## Judge summary",
                "",
                f"- Coverage: {len(judge_scores)} turns",
                f"- Labels: PASS={labels['pass']}, FAIL={labels['fail']}, INVALID={labels['invalid']}",
                f"- Confidence: min={min(confidences):.3f}, average={sum(confidences) / len(confidences):.3f}",
            ]
        )
    lines.extend(["", "## Metric pass rates", "", "| Metric | Passed | Total | Rate |", "|---|---:|---:|---:|"])
    for name, values in sorted(capability_metrics.items()):
        passed = sum(values)
        lines.append(f"| `{name}` | {passed} | {len(values)} | {passed / len(values):.1%} |")
    lines.append("")
    return "\n".join(lines)
