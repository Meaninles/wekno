from __future__ import annotations

import json
import math
from collections import Counter, defaultdict
from pathlib import Path

from .models import ExperimentRun, GateResult


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
    verdicts = Counter(case.verdict.value for case in run.cases)
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
        f"- Cases: {len(run.cases)} (PASS={verdicts['PASS']}, FAIL={verdicts['FAIL']}, INVALID={verdicts['INVALID']})",
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
        lines.extend([f"- Gate: **{gate.verdict.value}** (`{gate.policy_id}`)", ""])
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
        agent_verdicts = Counter(case.verdict.value for case in cases)
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
            "| Case | Attempt | Agent | Split | Verdict | Turns | Failed metrics | Error |",
            "|---|---:|---|---|---:|---:|---|---|",
        ]
    )
    for case in run.cases:
        failed_metrics = sorted(
            {score.name for score in case.scores if score.passed is False}
        )
        lines.append(
            f"| `{case.case_id}` | {case.attempt_index} | "
            f"`{case.agent_profile_id or 'unassigned'}` | `{case.split.value}` | "
            f"**{case.verdict.value}** | {len(case.turns)} | "
            f"{_safe(', '.join(failed_metrics))} | {_safe(case.error)} |"
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
            "## Failed turns",
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
