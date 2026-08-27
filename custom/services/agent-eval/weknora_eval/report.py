from __future__ import annotations

import json
from collections import Counter, defaultdict
from pathlib import Path

from .models import ExperimentRun, GateResult


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
    lines = [
        f"# WeKnora Agent Eval — {run.run_id}",
        "",
        f"- Suite: `{run.suite}`",
        f"- Dataset: `{run.dataset_sha256}`",
        f"- SUT: mode=`{run.sut.mode}`, release=`{run.sut.release}`, commit=`{run.sut.commit}`",
        f"- Cases: {len(run.cases)} (PASS={verdicts['PASS']}, FAIL={verdicts['FAIL']}, INVALID={verdicts['INVALID']})",
    ]
    if gate is not None:
        lines.extend([f"- Gate: **{gate.verdict.value}** (`{gate.policy_id}`)", ""])
        lines.extend(["## Gate checks", "", "| Check | Verdict | Detail |", "|---|---:|---|"])
        for check in gate.checks:
            lines.append(f"| `{check.name}` | **{check.verdict.value}** | {check.comment.replace('|', '/')} |")
    lines.extend(["", "## Cases", "", "| Case | Split | Verdict | Error |", "|---|---|---:|---|"])
    for case in run.cases:
        lines.append(
            f"| `{case.case_id}` | `{case.split.value}` | **{case.verdict.value}** | {(case.error or '').replace('|', '/')} |"
        )
    lines.extend(["", "## Metric pass rates", "", "| Metric | Passed | Total | Rate |", "|---|---:|---:|---:|"])
    for name, values in sorted(capability_metrics.items()):
        passed = sum(values)
        lines.append(f"| `{name}` | {passed} | {len(values)} | {passed / len(values):.1%} |")
    lines.append("")
    return "\n".join(lines)
