"""Build the v3 frozen suite for evaluator reliability improvements.

V3 keeps the v2 prompts, case IDs, splits and repetitions.  It adds contract
coverage for reviewed scorer false negatives and for a real stale-state
resurrection that v2 failed to detect.  Observation compatibility lets the
same immutable model transcripts be rescored before another costly live run.
"""

from __future__ import annotations

from pathlib import Path

from curation.build_multiturn_dev_v1 import text_rule
from curation.build_multiturn_ready_v2 import build_cases as build_v2_cases
from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import CaseSpec, TextRule, TurnSpec


SUITE = "weknora-three-agent-multiturn-ready-v3"
WAREHOUSE_FAMILY = "gate-warehouse-state-supersession-and-detour"
INTERNAL_PLANNING_TERMS = [
    "Now I have",
    "Now let me",
    "Let me organize",
    "Let me answer",
    "Let me carefully",
    "Let me re-examine",
    "Let me verify",
    "I have the retrieval results",
    "Looking at the returned evidence",
    "Looking at my earlier answer",
    "Now rewriting",
    "Now I'll write",
    "citation handle",
    "evidence handle",
    "evidence map",
    "chunk_id",
    "stop hook",
    "validation error",
    "output contract",
    "contract says",
    "final attempt",
]


def _replace_rule(
    rules: list[TextRule], rule_id: str, replacement: TextRule
) -> list[TextRule]:
    return [replacement if rule.rule_id == rule_id else rule for rule in rules]


def _revise_turn(case: CaseSpec, turn: TurnSpec) -> TurnSpec:
    planning_rule = text_rule(
        "no-internal-planning",
        any_of=INTERNAL_PLANNING_TERMS,
        description="不得向用户暴露检索、校验、引用修复、契约验证或改写过程",
    )
    forbidden_claims = _replace_rule(
        turn.contract.forbidden_claims,
        "no-internal-planning",
        planning_rule,
    )
    if not any(rule.rule_id == "no-internal-planning" for rule in forbidden_claims):
        forbidden_claims.append(planning_rule)

    state = turn.contract.conversation_state
    if case.family_id == WAREHOUSE_FAMILY and turn.turn_id == "turn-012":
        stale_supplier = text_rule(
            "resolved-d-not-unknown-final",
            all_of=["D"],
            any_of=["待核验", "未核验", "尚未核验", "未经核验", "待核实", "尚未核实"],
            description="技术组已经推翻的D排他主张不得恢复成待确认状态",
        )
        state = state.model_copy(
            update={
                "forbidden_unknown_facts": [
                    *state.forbidden_unknown_facts,
                    stale_supplier,
                ]
            }
        )

    contract = turn.contract.model_copy(
        update={
            "forbidden_claims": forbidden_claims,
            "conversation_state": state,
        }
    )
    return turn.model_copy(update={"contract": contract})


def build_cases() -> list[CaseSpec]:
    cases: list[CaseSpec] = []
    for case in build_v2_cases():
        metadata = {
            **case.provenance.metadata,
            "contract_revision": "v3-evaluator-reliability",
            "observation_compatible_with": "multiturn-ready.v2",
        }
        cases.append(
            case.model_copy(
                update={
                    "suite": SUITE,
                    "turns": [_revise_turn(case, turn) for turn in case.turns],
                    "tags": [tag for tag in case.tags if tag != "contract:v2"]
                    + ["contract:v3"],
                    "provenance": case.provenance.model_copy(
                        update={"metadata": metadata}
                    ),
                }
            )
        )
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid v3 ready dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "multiturn-ready.v3.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} turns "
        f"to {target} sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
