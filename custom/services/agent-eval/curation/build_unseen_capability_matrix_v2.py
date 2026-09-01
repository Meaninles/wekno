"""Version 2 of the unseen capability matrix with explicit KB boundaries.

Version 1 is intentionally left untouched for historical reproducibility.  The
questions and semantic capability design are unchanged; v2 corrects only test
orchestration so an empty request cannot silently fall back to an agent's
tenant-wide/default knowledge scope.
"""

from __future__ import annotations

from pathlib import Path

from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import CaseSpec, KnowledgeSelectionMode

try:
    from .build_unseen_capability_matrix_v1 import build_cases as build_v1_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_unseen_capability_matrix_v1 import build_cases as build_v1_cases


SUITE = "weknora-unseen-capability-matrix-v2"
NO_KB_AGENT_IDS = {
    "quick-answer": "${AGENT_EVAL_AGENT_QUICK_NO_KB_ID}",
    "rag-reasoning": "${AGENT_EVAL_AGENT_RAG_NO_KB_ID}",
    "general-agent": "${AGENT_EVAL_AGENT_GENERAL_NO_KB_ID}",
}


def _with_explicit_boundary(case: CaseSpec) -> CaseSpec:
    has_targets = bool(case.setup.knowledge_base_ids or case.setup.knowledge_ids)
    selection = (
        KnowledgeSelectionMode.EXPLICIT
        if has_targets
        else KnowledgeSelectionMode.NONE
    )
    agent = case.agent
    if not has_targets:
        agent = agent.model_copy(
            update={"agent_id": NO_KB_AGENT_IDS[case.agent_profile_id or ""]}
        )
    metadata = dict(case.provenance.metadata)
    metadata.update(
        {
            "knowledge_selection_contract": selection.value,
            "v1_question_content_unchanged": True,
            "sealed_holdout_used": False,
        }
    )
    return case.model_copy(
        update={
            "suite": SUITE,
            "agent": agent,
            "setup": case.setup.model_copy(
                update={"knowledge_selection_mode": selection}
            ),
            "tags": [
                *case.tags,
                f"knowledge-selection:{selection.value}",
            ],
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-capability-matrix-v2-explicit-kb-boundary",
                    "metadata": metadata,
                }
            ),
        }
    )


def build_cases() -> list[CaseSpec]:
    cases = [_with_explicit_boundary(case) for case in build_v1_cases()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid unseen capability matrix v2: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "unseen-capability-matrix.v2.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
