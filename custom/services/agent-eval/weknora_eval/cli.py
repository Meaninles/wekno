from __future__ import annotations

import argparse
import json
import os
import sys
from collections import Counter
from pathlib import Path

from .client import WeKnoraClient
from .calibration import load_calibration, run_judge_calibration
from .dataset import (
    DatasetError,
    build_quarantine_from_transcripts,
    freeze_dataset,
    load_jsonl,
    split_by_family,
    validate_dataset,
    write_jsonl,
)
from .discovery import (
    DiscoveryCollector,
    assert_discovery_reviewed,
    load_adaptive_turn_plan,
    load_discovery_scenario,
    review_discovery_turn,
    write_discovery_result,
)
from .gates import evaluate_gate, load_policy
from .judge import JudgeError, judge_case, judge_runtime_contract
from .langfuse_store import publish_dataset, run_langfuse_experiment
from .models import CaseRun, ExperimentRun, Split, Verdict
from .readiness import assert_ready, evaluate_readiness, file_sha256
from .report import load_gate, load_run, render_markdown, write_json
from .runner import EvalRunner
from .scoring import score_case


def _split(value: str) -> Split:
    try:
        return Split(value)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(str(exc)) from exc


def _client(args: argparse.Namespace) -> WeKnoraClient:
    env_name = args.api_key_env
    api_key = os.environ.get(env_name, "").strip()
    if not api_key:
        raise DatasetError(f"{env_name} is required")
    return WeKnoraClient(args.base_url, api_key, args.timeout)


def cmd_doctor(args: argparse.Namespace) -> int:
    fingerprint = EvalRunner(_client(args)).doctor()
    print(json.dumps(fingerprint.model_dump(mode="json"), ensure_ascii=False, indent=2))
    return 0


def cmd_preflight(args: argparse.Namespace) -> int:
    runner = EvalRunner(_client(args))
    report = evaluate_readiness(
        dataset_path=args.dataset,
        manifest_path=args.manifest,
        profile_path=args.profiles,
        policy_path=args.policy,
        calibration_path=args.calibration,
        split=args.split,
        sut=runner.doctor(),
    )
    if args.output:
        write_json(args.output, report)
    print(json.dumps(report, ensure_ascii=False, indent=2))
    assert_ready(report)
    return 0


def cmd_calibration_validate(args: argparse.Namespace) -> int:
    suite = load_calibration(args.input)
    counts = Counter(item.expected_label for item in suite.items)
    print(
        json.dumps(
            {
                "suite_id": suite.suite_id,
                "items": len(suite.items),
                "expected_labels": counts,
                "minimum_accuracy": suite.minimum_accuracy,
                "minimum_confidence": suite.minimum_confidence,
            },
            ensure_ascii=False,
        )
    )
    return 0


def cmd_calibration_run(args: argparse.Namespace) -> int:
    result = run_judge_calibration(load_calibration(args.input))
    result["suite_sha256"] = file_sha256(args.input)
    write_json(args.output, result)
    print(json.dumps(result, ensure_ascii=False, indent=2))
    return 0 if result["passed"] else 1


def cmd_discover(args: argparse.Namespace) -> int:
    scenario = load_discovery_scenario(args.scenario)
    prior = None
    adaptive_turns: dict[str, dict] = {}
    if args.resume_from:
        with Path(args.resume_from).open("r", encoding="utf-8") as handle:
            prior = json.load(handle)
        assert_discovery_reviewed(prior, set(args.profile or []))
        if len(args.profile or []) != 1:
            raise DatasetError("resumed discovery requires exactly one --profile")
        if not args.next_turn_file:
            raise DatasetError(
                "resumed discovery requires --next-turn-file authored after reviewing the latest answer"
            )
        profile_id = args.profile[0]
        adaptive_turns[profile_id] = load_adaptive_turn_plan(
            args.next_turn_file,
            prior,
            profile_id=profile_id,
        )
    elif args.next_turn_file:
        raise DatasetError("--next-turn-file is only valid together with --resume-from")

    def progress(profile_id: str, turn_id: str, absolute_turn: int, status: str, result: dict | None) -> None:
        suffix = ""
        if result is not None:
            suffix = f" latency_ms={result.get('total_latency_ms', 0)} error={bool(result.get('error'))}"
        print(
            f"DISCOVERY profile={profile_id} turn={turn_id} index={absolute_turn} status={status}{suffix}",
            flush=True,
        )

    collected = DiscoveryCollector(_client(args)).collect(
        scenario,
        prior=prior,
        selected_profiles=set(args.profile or []),
        adaptive_turns=adaptive_turns,
        progress=progress,
    )
    write_discovery_result(args.output, collected)
    failed = [item["profile_id"] for item in collected["sessions"] if item.get("error")]
    print(
        json.dumps(
            {
                "discovery_id": collected["discovery_id"],
                "output": args.output,
                "sessions": len(collected["sessions"]),
                "failed_profiles": failed,
                "formal_eval_executed": False,
            },
            ensure_ascii=False,
        )
    )
    return 2 if failed else 0


def cmd_discover_export(args: argparse.Namespace) -> int:
    scenario = load_discovery_scenario(args.scenario)
    collected = DiscoveryCollector(_client(args)).export_existing_session(
        scenario,
        profile_id=args.profile,
        session_id=args.session_id,
    )
    write_discovery_result(args.output, collected)
    print(
        json.dumps(
            {
                "output": args.output,
                "profile": args.profile,
                "session_id": args.session_id,
                "turns": len(collected["sessions"][0]["turns"]),
                "formal_eval_executed": False,
            },
            ensure_ascii=False,
        )
    )
    return 0


def cmd_discover_review(args: argparse.Namespace) -> int:
    target = Path(args.artifact)
    with target.open("r", encoding="utf-8") as handle:
        payload = json.load(handle)
    review_discovery_turn(
        payload,
        profile_id=args.profile,
        turn_id=args.turn,
        disposition=args.disposition,
        findings=args.finding,
        next_action=args.next_action,
    )
    write_discovery_result(target, payload)
    print(
        json.dumps(
            {
                "artifact": str(target),
                "profile": args.profile,
                "turn": args.turn,
                "disposition": args.disposition,
                "formal_eval_executed": False,
            },
            ensure_ascii=False,
        )
    )
    return 0


def cmd_dataset_build(args: argparse.Namespace) -> int:
    cases = build_quarantine_from_transcripts(
        args.transcripts,
        suite=args.suite,
        agent_id=args.agent_id,
        endpoint=args.endpoint,
    )
    write_jsonl(args.output, cases)
    print(f"harvested {len(cases)} quarantined cases into {args.output}")
    return 0


def cmd_dataset_validate(args: argparse.Namespace) -> int:
    cases = load_jsonl(args.input)
    errors = validate_dataset(cases)
    if errors:
        for error in errors:
            print(error, file=sys.stderr)
        return 2
    counts = Counter(case.split.value for case in cases)
    print(json.dumps({"cases": len(cases), "families": len({c.family_id for c in cases}), "splits": counts}, ensure_ascii=False))
    return 0


def cmd_dataset_split(args: argparse.Namespace) -> int:
    cases = load_jsonl(args.input)
    split_cases = split_by_family(
        cases,
        salt=args.salt,
        dev_ratio=args.dev_ratio,
        gate_ratio=args.gate_ratio,
        holdout_ratio=args.holdout_ratio,
    )
    if errors := validate_dataset(split_cases):
        raise DatasetError("split output invalid: " + "; ".join(errors))
    output_dir = Path(args.output_dir)
    output_dir.mkdir(parents=True, exist_ok=True)
    write_jsonl(output_dir / "all.jsonl", split_cases)
    for split in Split:
        selected = [case for case in split_cases if case.split == split]
        if selected:
            write_jsonl(output_dir / f"{split.value}.jsonl", selected)
    return 0


def cmd_dataset_freeze(args: argparse.Namespace) -> int:
    cases = load_jsonl(args.input)
    if errors := validate_dataset(cases):
        raise DatasetError("dataset invalid: " + "; ".join(errors))
    dependencies: dict[str, str] = {}
    for value in args.dependency:
        name, separator, path = value.partition("=")
        if not separator or not name.strip() or not path.strip():
            raise DatasetError("--dependency must use name=path")
        dependencies[name.strip()] = file_sha256(path.strip())
    manifest = freeze_dataset(
        cases,
        [str(Path(args.input))],
        dependency_sha256=dependencies,
    )
    write_json(args.output, manifest)
    print(manifest.dataset_sha256)
    return 0


def cmd_dataset_publish(args: argparse.Namespace) -> int:
    cases = load_jsonl(args.input)
    if errors := validate_dataset(cases):
        raise DatasetError("dataset invalid: " + "; ".join(errors))
    name = publish_dataset(cases, args.name)
    print(name)
    return 0


def cmd_run(args: argparse.Namespace) -> int:
    unresolved = load_jsonl(args.dataset)
    if errors := validate_dataset(unresolved):
        raise DatasetError("dataset invalid: " + "; ".join(errors))
    cases = load_jsonl(args.dataset, resolve_variables=True)
    if args.case_id:
        requested = list(dict.fromkeys(args.case_id))
        known = {case.case_id for case in cases}
        unknown = sorted(set(requested) - known)
        if unknown:
            raise DatasetError("unknown --case-id value(s): " + ", ".join(unknown))
        selected_ids = set(requested)
        cases = [case for case in cases if case.case_id in selected_ids]
    splits = set(args.split or [Split.DEV])
    if Split.SEALED_HOLDOUT in splits and not args.allow_sealed:
        raise DatasetError("sealed_holdout requires explicit --allow-sealed")
    runner = EvalRunner(_client(args))
    execution_contract = {
        "summary_model_id": os.environ.get("AGENT_EVAL_SUMMARY_MODEL_ID", "").strip(),
        "corpus_version": os.environ.get("AGENT_EVAL_CORPUS_VERSION", "").strip(),
        "procurement_knowledge_id": os.environ.get(
            "AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID", ""
        ).strip(),
        "profile_set_sha256": file_sha256(args.profiles) if args.profiles else "",
        "framework_commit": os.environ.get("AGENT_EVAL_FRAMEWORK_COMMIT", "").strip(),
        "framework_worktree_dirty": os.environ.get(
            "AGENT_EVAL_WORKTREE_DIRTY", ""
        ).strip(),
        "scorer_sha256": file_sha256(Path(__file__).with_name("scoring.py")),
        "response_deadline_seconds": args.timeout,
    }
    metadata = {
        "execution_contract": execution_contract,
        **({"label": args.label} if args.label else {}),
    }
    if args.langfuse_experiment:
        run = run_langfuse_experiment(
            cases,
            runner,
            selected_splits=splits,
            name=args.langfuse_experiment,
            dataset_name=args.langfuse_dataset,
            max_concurrency=args.max_concurrency,
        )
        run = run.model_copy(update={"metadata": {**run.metadata, **metadata}})
    else:
        run = runner.run_suite(
            cases,
            selected_splits=splits,
            max_concurrency=args.max_concurrency,
            metadata=metadata,
        )
    # Dataset identity is based on the immutable, unresolved contracts; runtime
    # environment substitution must not silently create a new dataset version.
    from .dataset import dataset_sha256

    run = run.model_copy(update={"dataset_sha256": dataset_sha256(unresolved)})
    write_json(args.output, run)
    counts = Counter(case.verdict.value for case in run.cases)
    print(json.dumps({"run_id": run.run_id, "output": args.output, "verdicts": counts}, ensure_ascii=False))
    return 0 if not counts[Verdict.INVALID.value] else 2


def cmd_score(args: argparse.Namespace) -> int:
    cases = load_jsonl(args.dataset)
    if errors := validate_dataset(cases):
        raise DatasetError("dataset invalid: " + "; ".join(errors))
    specs = {case.case_id: case for case in cases}
    run = load_run(args.run)
    rescored: list[CaseRun] = []
    for case_run in run.cases:
        spec = specs.get(case_run.case_id)
        if spec is None:
            rescored.append(case_run.model_copy(update={"verdict": Verdict.INVALID, "error": "case missing from dataset"}))
        else:
            deterministic = score_case(spec, case_run)
            judge_scores = [
                score
                for score in case_run.scores
                if score.name == "judge.contract_satisfaction"
            ]
            rescored.append(
                deterministic.model_copy(
                    update={"scores": [*deterministic.scores, *judge_scores]}
                )
            )
    # Rescoring is also the supported path for a contract-only dataset
    # revision: observations remain immutable, while the output is explicitly
    # rebound to the supplied dataset identity and suite.
    from .dataset import dataset_sha256

    execution_contract = (
        dict(run.metadata.get("execution_contract"))
        if isinstance(run.metadata.get("execution_contract"), dict)
        else {}
    )
    execution_contract.update(
        {
            "framework_commit": os.environ.get(
                "AGENT_EVAL_FRAMEWORK_COMMIT",
                str(execution_contract.get("framework_commit") or ""),
            ).strip(),
            "framework_worktree_dirty": os.environ.get(
                "AGENT_EVAL_WORKTREE_DIRTY",
                str(execution_contract.get("framework_worktree_dirty") or ""),
            ).strip(),
            "scorer_sha256": file_sha256(Path(__file__).with_name("scoring.py")),
        }
    )
    output = run.model_copy(
        update={
            "cases": rescored,
            "dataset_sha256": dataset_sha256(cases),
            "suite": cases[0].suite if cases else run.suite,
            "metadata": {**run.metadata, "execution_contract": execution_contract},
        }
    )
    write_json(args.output, output)
    return 0


def cmd_judge(args: argparse.Namespace) -> int:
    specs = {case.case_id: case for case in load_jsonl(args.dataset)}
    run = load_run(args.run)
    baseline = load_run(args.baseline) if args.baseline else None
    baseline_by_key = (
        {(case.case_id, case.attempt_index): case for case in baseline.cases}
        if baseline
        else {}
    )
    calibration: dict[str, object] = {}
    if args.calibration_result:
        with Path(args.calibration_result).open("r", encoding="utf-8") as handle:
            calibration = json.load(handle)
        if calibration.get("passed") is not True:
            raise DatasetError("judge calibration did not pass")
        configured_model = os.environ.get("AGENT_EVAL_JUDGE_MODEL", "").strip()
        if calibration.get("judge_model") != configured_model:
            raise DatasetError("judge model differs from the calibrated model")
    judged: list[CaseRun] = []
    judge_errors: list[dict[str, object]] = []
    total_cases = len(run.cases)
    for completed, case in enumerate(run.cases, start=1):
        spec = specs.get(case.case_id)
        if spec is None:
            error = "case missing from dataset"
            judged.append(case.model_copy(update={"verdict": Verdict.INVALID, "error": error}))
            judge_errors.append(
                {
                    "case_id": case.case_id,
                    "attempt_index": case.attempt_index,
                    "error": error,
                }
            )
            print(
                f"JUDGE_PROGRESS completed={completed}/{total_cases} case={case.case_id} "
                f"attempt={case.attempt_index} verdict=INVALID",
                flush=True,
            )
            continue
        deterministic_scores = [
            score for score in case.scores if score.name != "judge.contract_satisfaction"
        ]
        try:
            judge_scores = judge_case(
                spec,
                case,
                baseline_by_key.get((case.case_id, case.attempt_index)),
            )
        except JudgeError as exc:
            error = f"judge_error:{exc}"
            judged.append(
                case.model_copy(
                    update={
                        "verdict": Verdict.INVALID,
                        "error": error,
                        "scores": deterministic_scores,
                    }
                )
            )
            judge_errors.append(
                {
                    "case_id": case.case_id,
                    "attempt_index": case.attempt_index,
                    "error": error,
                }
            )
            verdict = Verdict.INVALID
        else:
            judged.append(
                case.model_copy(update={"scores": [*deterministic_scores, *judge_scores]})
            )
            verdict = case.verdict
        print(
            f"JUDGE_PROGRESS completed={completed}/{total_cases} case={case.case_id} "
            f"attempt={case.attempt_index} verdict={verdict.value}",
            flush=True,
        )
    execution_contract = (
        dict(run.metadata.get("execution_contract"))
        if isinstance(run.metadata.get("execution_contract"), dict)
        else {}
    )
    execution_contract.update(
        {
            "judge_model": os.environ.get("AGENT_EVAL_JUDGE_MODEL", "").strip(),
            "judge_calibration_suite_sha256": str(
                calibration.get("suite_sha256") or ""
            ),
            "judge_prompt_sha256": file_sha256(Path(__file__).with_name("judge.py")),
            **judge_runtime_contract(),
        }
    )
    judge_metadata = {
        "calibration_result_sha256": file_sha256(args.calibration_result)
        if args.calibration_result
        else "",
        "accuracy": calibration.get("accuracy"),
        "minimum_accuracy": calibration.get("minimum_accuracy"),
        "error_count": len(judge_errors),
        "errors": judge_errors,
    }
    output = run.model_copy(
        update={
            "cases": judged,
            "metadata": {
                **run.metadata,
                "execution_contract": execution_contract,
                "judge": judge_metadata,
            },
        }
    )
    write_json(args.output, output)
    return 2 if judge_errors else 0


def cmd_gate(args: argparse.Namespace) -> int:
    dataset = load_jsonl(args.dataset)
    if errors := validate_dataset(dataset):
        raise DatasetError("dataset invalid: " + "; ".join(errors))
    candidate = load_run(args.candidate)
    baseline = load_run(args.baseline) if args.baseline else None
    result = evaluate_gate(dataset, candidate, load_policy(args.policy), baseline)
    write_json(args.output, result)
    print(result.verdict.value)
    return {Verdict.PASS: 0, Verdict.FAIL: 1, Verdict.INVALID: 2}[result.verdict]


def cmd_report(args: argparse.Namespace) -> int:
    run = load_run(args.run)
    gate = load_gate(args.gate) if args.gate else None
    target = Path(args.output)
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_text(render_markdown(run, gate), encoding="utf-8", newline="\n")
    print(str(target))
    return 0


def _api_args(parser: argparse.ArgumentParser) -> None:
    parser.add_argument("--base-url", default=os.environ.get("WEKNORA_BASE_URL", "http://localhost:18080/api/v1"))
    parser.add_argument("--api-key-env", default="WEKNORA_E2E_TENANT_API_KEY")
    parser.add_argument("--timeout", type=float, default=600.0)


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(prog="weknora-agent-eval")
    sub = parser.add_subparsers(dest="command", required=True)

    doctor = sub.add_parser("doctor", help="verify eval-mode SUT capabilities")
    _api_args(doctor)
    doctor.set_defaults(func=cmd_doctor)

    preflight = sub.add_parser(
        "preflight",
        help="prove eval readiness without creating sessions or sending chat requests",
    )
    _api_args(preflight)
    preflight.add_argument("--dataset", required=True)
    preflight.add_argument("--manifest", required=True)
    preflight.add_argument("--profiles", required=True)
    preflight.add_argument("--policy", required=True)
    preflight.add_argument("--calibration", required=True)
    preflight.add_argument("--split", type=_split, default=Split.GATE)
    preflight.add_argument("--output")
    preflight.set_defaults(func=cmd_preflight)

    calibration = sub.add_parser(
        "calibration", help="validate or explicitly run the frozen judge calibration suite"
    )
    calibration_sub = calibration.add_subparsers(dest="calibration_command", required=True)
    calibration_validate = calibration_sub.add_parser("validate")
    calibration_validate.add_argument("--input", required=True)
    calibration_validate.set_defaults(func=cmd_calibration_validate)
    calibration_run = calibration_sub.add_parser("run")
    calibration_run.add_argument("--input", required=True)
    calibration_run.add_argument("--output", required=True)
    calibration_run.set_defaults(func=cmd_calibration_run)

    discover = sub.add_parser(
        "discover",
        help="collect unscored, real multi-turn conversations in the isolated eval environment",
    )
    _api_args(discover)
    discover.add_argument("--scenario", required=True)
    discover.add_argument("--output", required=True)
    discover.add_argument("--resume-from")
    discover.add_argument("--profile", action="append")
    discover.add_argument(
        "--next-turn-file",
        help="single Codex-authored adaptive turn linked to the latest reviewed answer; required when resuming",
    )
    discover.set_defaults(func=cmd_discover)

    discover_export = sub.add_parser(
        "discover-export",
        help="recover an unscored discovery checkpoint from an existing WeKnora session",
    )
    _api_args(discover_export)
    discover_export.add_argument("--scenario", required=True)
    discover_export.add_argument("--profile", required=True)
    discover_export.add_argument("--session-id", required=True)
    discover_export.add_argument("--output", required=True)
    discover_export.set_defaults(func=cmd_discover_export)

    discover_review = sub.add_parser(
        "discover-review",
        help="record the required Codex quality checkpoint for one completed discovery turn",
    )
    discover_review.add_argument("--artifact", required=True)
    discover_review.add_argument("--profile", required=True)
    discover_review.add_argument("--turn", required=True)
    discover_review.add_argument(
        "--disposition",
        required=True,
        choices=["accepted_observation", "accepted_with_findings", "rejected_answer"],
    )
    discover_review.add_argument("--finding", action="append", default=[])
    discover_review.add_argument("--next-action", default="")
    discover_review.set_defaults(func=cmd_discover_review)

    dataset = sub.add_parser("dataset", help="build, validate, split, freeze or publish datasets")
    dataset_sub = dataset.add_subparsers(dest="dataset_command", required=True)
    build = dataset_sub.add_parser("build")
    build.add_argument("--transcripts", action="append", required=True)
    build.add_argument("--suite", required=True)
    build.add_argument("--agent-id", required=True)
    build.add_argument("--endpoint", choices=["knowledge-chat", "agent-chat"], default="agent-chat")
    build.add_argument("--output", required=True)
    build.set_defaults(func=cmd_dataset_build)
    validate = dataset_sub.add_parser("validate")
    validate.add_argument("--input", required=True)
    validate.set_defaults(func=cmd_dataset_validate)
    split = dataset_sub.add_parser("split")
    split.add_argument("--input", required=True)
    split.add_argument("--output-dir", required=True)
    split.add_argument("--salt", required=True)
    split.add_argument("--dev-ratio", type=float, default=0.50)
    split.add_argument("--gate-ratio", type=float, default=0.25)
    split.add_argument("--holdout-ratio", type=float, default=0.15)
    split.set_defaults(func=cmd_dataset_split)
    freeze = dataset_sub.add_parser("freeze")
    freeze.add_argument("--input", required=True)
    freeze.add_argument("--output", required=True)
    freeze.add_argument("--dependency", action="append", default=[])
    freeze.set_defaults(func=cmd_dataset_freeze)
    publish = dataset_sub.add_parser("publish")
    publish.add_argument("--input", required=True)
    publish.add_argument("--name")
    publish.set_defaults(func=cmd_dataset_publish)

    run = sub.add_parser("run", help="run live WeKnora cases; production mode is refused")
    _api_args(run)
    run.add_argument("--dataset", required=True)
    run.add_argument("--output", required=True)
    run.add_argument("--split", type=_split, action="append")
    run.add_argument(
        "--case-id",
        action="append",
        default=[],
        help="run only the selected case id; repeat for focused eval-loop iterations",
    )
    run.add_argument("--allow-sealed", action="store_true")
    run.add_argument("--max-concurrency", type=int, default=1)
    run.add_argument("--label")
    run.add_argument("--profiles")
    run.add_argument("--langfuse-experiment")
    run.add_argument("--langfuse-dataset")
    run.set_defaults(func=cmd_run)

    score = sub.add_parser("score")
    score.add_argument("--dataset", required=True)
    score.add_argument("--run", required=True)
    score.add_argument("--output", required=True)
    score.set_defaults(func=cmd_score)

    judge = sub.add_parser("judge")
    judge.add_argument("--dataset", required=True)
    judge.add_argument("--run", required=True)
    judge.add_argument("--baseline")
    judge.add_argument("--calibration-result")
    judge.add_argument("--output", required=True)
    judge.set_defaults(func=cmd_judge)

    gate = sub.add_parser("gate")
    gate.add_argument("--dataset", required=True)
    gate.add_argument("--candidate", required=True)
    gate.add_argument("--baseline")
    gate.add_argument("--policy", required=True)
    gate.add_argument("--output", required=True)
    gate.set_defaults(func=cmd_gate)

    report = sub.add_parser("report")
    report.add_argument("--run", required=True)
    report.add_argument("--gate")
    report.add_argument("--output", required=True)
    report.set_defaults(func=cmd_report)
    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)
    try:
        return int(args.func(args))
    except Exception as exc:
        print(f"ERROR: {exc}", file=sys.stderr)
        return 2
