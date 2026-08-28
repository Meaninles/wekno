"""Freeze the complete pre-change optimization DEV matrix.

The suite copies every reviewed DEV scenario from multiturn-ready.v3 without
changing prompts or contracts, then raises independent session repetitions to
three. Gate and sealed prompts are deliberately excluded: tuning uses only the
already-reviewed DEV families while the release splits remain untouched.
"""

from __future__ import annotations

from pathlib import Path

from curation.build_multiturn_ready_v3 import build_cases as build_v3_cases
from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import CaseSpec, Split


SUITE = "weknora-three-agent-multiturn-optimization-dev-v1"
SOURCE_DATASET = "multiturn-ready.v3"


def build_cases() -> list[CaseSpec]:
    source = [case for case in build_v3_cases() if case.split == Split.DEV]
    source_digest = dataset_sha256(source)
    cases: list[CaseSpec] = []
    for case in source:
        metadata = {
            **case.provenance.metadata,
            "optimization_suite": "pre-agent-change-v1",
            "source_dataset": SOURCE_DATASET,
            "source_dev_sha256": source_digest,
            "frozen_before_agent_change": True,
            "gate_prompt_derived": False,
        }
        cases.append(
            case.model_copy(
                update={
                    "suite": SUITE,
                    "repetitions": 3,
                    "tags": [*case.tags, "optimization-dev", "stability:3"],
                    "provenance": case.provenance.model_copy(
                        update={"metadata": metadata}
                    ),
                }
            )
        )
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid optimization DEV dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "multiturn-optimization-dev.v1.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} contract turns / "
        f"{sum(case.repetitions for case in cases)} sessions to {target} "
        f"sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
