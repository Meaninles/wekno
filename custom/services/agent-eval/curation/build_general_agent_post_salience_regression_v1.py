"""Build the post-salience general-agent component regression.

The six conversations are an exact question-preserving subset of the immutable
RAG-primary v4 matrix.  Only suite/provenance metadata changes so the new
general-agent image can be verified without rerunning unchanged agent profiles
or rewriting the historical full-flow manifest.
"""

from __future__ import annotations

from pathlib import Path

from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import CaseSpec

try:
    from .build_rag_primary_codex_matrix_v4 import build_cases as build_v4_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v4 import build_cases as build_v4_cases


SUITE = "weknora-general-agent-post-salience-regression-v1"
EXPECTED_CASE_COUNT = 6


def _retarget(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata.update(
        {
            "component_regression": "general-agent-post-salience-v1",
            "source_questions_unchanged": True,
            "full_flow_run_consumed": False,
        }
    )
    return case.model_copy(
        update={
            "suite": SUITE,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "rag-primary-v4-question-preserving-component-regression",
                    "metadata": metadata,
                }
            ),
        }
    )


def build_cases() -> list[CaseSpec]:
    source = build_v4_cases()
    selected = [_retarget(case) for case in source if case.agent_profile_id == "general-agent"]
    if len(selected) != EXPECTED_CASE_COUNT:
        raise ValueError(
            f"expected {EXPECTED_CASE_COUNT} general-agent cases, got {len(selected)}"
        )
    errors = validate_dataset(selected)
    if errors:
        raise ValueError("invalid general-agent component regression: " + "; ".join(errors))

    source_by_id = {case.case_id: case for case in source}
    for case in selected:
        original = source_by_id[case.case_id]
        if [turn.query for turn in case.turns] != [turn.query for turn in original.turns]:
            raise ValueError(f"question drift detected for {case.case_id}")
        if [turn.contract for turn in case.turns] != [turn.contract for turn in original.turns]:
            raise ValueError(f"turn contract drift detected for {case.case_id}")
    return selected


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "general-agent-post-salience-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
