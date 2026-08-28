"""Build the v2 frozen multi-turn suite without mutating the v1 baseline.

V2 keeps every scenario, prompt and repetition from v1. It revises only
deterministic contracts where human review found valid Chinese paraphrases
that v1 rejected. It does not add a reference answer or relax omissions,
wrong lifecycle placement, unsupported evidence, verbosity, tool use, or
premature decisions. Stable case IDs let immutable v1 observations be rescored
against this contract-only revision.
"""

from __future__ import annotations

from pathlib import Path

from curation.build_multiturn_dev_v1 import text_rule
from curation.build_multiturn_ready_v1 import build_cases as build_v1_cases
from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    CaseSpec,
    EvidenceAnchor,
    EvidenceClaimRule,
    TextRule,
    TurnSpec,
)


SUITE = "weknora-three-agent-multiturn-ready-v2"
WAREHOUSE_FAMILY = "gate-warehouse-state-supersession-and-detour"
ACTIVE_RETIRED_FAMILY = "active-retired-state-window-plus-two"
ALIGNMENT_FAMILY = "current-turn-alignment-topic-detour"
DECISION_FAMILY = "decision-under-unknowns-procurement-path"
RANKING_TERMS = [
    "最适配",
    "最为适配",
    "更适合",
    "更为适合",
    "更匹配",
    "明显适配",
    "恰属该区间",
    "较适配",
    "相对适配",
    "较匹配",
    "倾向",
    "适用性较高",
    "适用性反而较高",
    "适用性更高",
    "适用性反而更高",
    "匹配度较高",
    "匹配度更高",
    "风险最低",
    "风险较低",
    "风险更低",
    "最稳妥",
]
INTERNAL_PLANNING_TERMS = [
    "Now I have",
    "Now let me",
    "Let me organize",
    "Let me answer",
    "I have the retrieval results",
    "Looking at the returned evidence",
    "Looking at my earlier answer",
    "Now rewriting",
    "Now I'll write",
    "citation handle",
    "chunk_id",
]


def _replace_rule(
    rules: list[TextRule], rule_id: str, replacement: TextRule
) -> list[TextRule]:
    return [replacement if rule.rule_id == rule_id else rule for rule in rules]


def _replace_rule_many(
    rules: list[TextRule], rule_id: str, replacements: list[TextRule]
) -> list[TextRule]:
    out: list[TextRule] = []
    for rule in rules:
        out.extend(replacements if rule.rule_id == rule_id else [rule])
    return out


def _strengthen_common_turn(turn: TurnSpec) -> TurnSpec:
    """Make internal execution narration a hard failure on every scored turn."""

    planning_rule = text_rule(
        "no-internal-planning",
        any_of=INTERNAL_PLANNING_TERMS,
        description="不得向用户暴露检索、校验、引用修复或改写过程",
    )
    forbidden = _replace_rule(
        turn.contract.forbidden_claims,
        "no-internal-planning",
        planning_rule,
    )
    if not any(rule.rule_id == "no-internal-planning" for rule in forbidden):
        forbidden.append(planning_rule)
    return turn.model_copy(
        update={
            "contract": turn.contract.model_copy(
                update={"forbidden_claims": forbidden}
            )
        }
    )


def _revise_warehouse_turn(turn: TurnSpec) -> TurnSpec:
    state = turn.contract.conversation_state
    if turn.turn_id == "turn-008":
        state = state.model_copy(
            update={
                "unknown_facts": _replace_rule(
                    state.unknown_facts,
                    "d-claim",
                    text_rule(
                        "d-claim",
                        all_of=["D"],
                        any_of=[
                            "待核验",
                            "未核验",
                            "尚未核验",
                            "未经核验",
                            "待核实",
                            "尚未核实",
                        ],
                    ),
                ),
                # A quoted supplier claim is not itself a forbidden inference.
                # Only an answer that positively upgrades it to a verified fact
                # should fail this deterministic rule.
                "forbidden_inferences": _replace_rule(
                    state.forbidden_inferences,
                    "d-exclusive",
                    text_rule(
                        "d-exclusive",
                        any_of=[
                            "确认只能由D",
                            "确认只能由它",
                            "已核验只能由D",
                            "已证实只有D能",
                            "D不可替代已确认",
                            "技术组确认D不可替代",
                        ],
                    ),
                ),
            }
        )
    if turn.turn_id in {"turn-009", "turn-012"}:
        rule_id = "exclusive-retired" if turn.turn_id == "turn-009" else "exclusive-old-final"
        state = state.model_copy(
            update={
                "retired_facts": _replace_rule(
                    state.retired_facts,
                    rule_id,
                    text_rule(
                        rule_id,
                        all_of=["D"],
                        any_of=["废弃", "作废", "不再成立", "不成立", "推翻", "否定"],
                    ),
                )
            }
        )
    return turn.model_copy(
        update={"contract": turn.contract.model_copy(update={"conversation_state": state})}
    )


def _retired_exclusivity_rule(rule_id: str, party: str) -> TextRule:
    return text_rule(
        rule_id,
        all_of=[party],
        any_of=["废弃", "作废", "不再成立", "不成立", "推翻", "否定"],
    )


def _revise_active_retired_turn(turn: TurnSpec) -> TurnSpec:
    state = turn.contract.conversation_state
    # Preserve the semantic prohibition while accepting ordinary Chinese
    # realizations such as “不得发起任何采购” and the ledger form
    # “发起采购：未经授权，不予执行”.  Positive action statements still fail
    # because each rule requires an explicit negative execution marker.
    state = state.model_copy(
        update={
            "action_boundaries": _replace_rule(
                _replace_rule(
                    _replace_rule(
                        state.action_boundaries,
                        "no-file-write",
                        text_rule(
                            "no-file-write",
                            all_of=["文件"],
                            any_of=[
                                "不得创建",
                                "不得修改",
                                "不创建",
                                "不修改",
                                "不会创建",
                                "不会修改",
                                "不予执行",
                            ],
                        ),
                    ),
                    "no-procurement-action",
                    text_rule(
                        "no-procurement-action",
                        all_of=["采购"],
                        any_of=[
                            "不得发起",
                            "不发起",
                            "不会发起",
                            "不启动",
                            "不会启动",
                            "不予执行",
                        ],
                    ),
                ),
                "chat-only",
                text_rule("chat-only", all_of=["对话"], any_of=["只", "仅"]),
            )
        }
    )
    if turn.turn_id == "turn-007":
        state = state.model_copy(
            update={
                "unknown_facts": _replace_rule(
                    state.unknown_facts,
                    "a-claim",
                    text_rule(
                        "a-claim",
                        all_of=["A"],
                        any_of=[
                            "待核验",
                            "未核验",
                            "尚未核验",
                            "未经核验",
                            "待核实",
                            "尚未核实",
                        ],
                    ),
                ),
                "forbidden_inferences": _replace_rule(
                    state.forbidden_inferences,
                    "a-exclusive",
                    text_rule(
                        "a-exclusive",
                        any_of=[
                            "确认只能由A",
                            "确认只能由它",
                            "已核验只能由A",
                            "已证实只有A能",
                            "A不可替代已确认",
                        ],
                    ),
                ),
            }
        )
    if turn.turn_id == "turn-006":
        state = state.model_copy(
            update={
                "unknown_facts": _replace_rule(
                    state.unknown_facts,
                    "requirements-complete",
                    text_rule(
                        "requirements-complete",
                        all_of=["需求", "完整"],
                        any_of=["待确认", "待核实", "尚未确认", "未确认"],
                    ),
                )
            }
        )
    if turn.turn_id in {"turn-008", "turn-012"}:
        rule_id = "retired-exclusive" if turn.turn_id == "turn-008" else "old-exclusive"
        state = state.model_copy(
            update={
                "retired_facts": _replace_rule(
                    state.retired_facts,
                    rule_id,
                    _retired_exclusivity_rule(rule_id, "A"),
                )
            }
        )
    if turn.turn_id == "turn-008":
        # “A供应商不可替代主张：已核验为否，B、C通过适配可满足”
        # explicitly negates exclusivity without using the exact phrase
        # “并非不可替代”. Preserve all named parties and require a negative
        # exclusivity realization.
        state = state.model_copy(
            update={
                "active_facts": _replace_rule(
                    state.active_facts,
                    "not-exclusive",
                    text_rule(
                        "not-exclusive",
                        all_of=["A", "B", "C"],
                        any_of=[
                            "并非不可替代",
                            "非不可替代",
                            "可替代",
                            "不可替代主张：已核验为否",
                            "不可替代主张已核验为否",
                        ],
                    ),
                )
            }
        )
    if turn.turn_id in {"turn-009", "turn-012"}:
        # “采购标的最终类别：待确认” is the same lifecycle fact as
        # “标的类别待确认”; requiring the two-character phrase to be directly
        # adjacent produced a deterministic false negative.
        state = state.model_copy(
            update={
                "unknown_facts": _replace_rule(
                    state.unknown_facts,
                    "category",
                    text_rule("category", all_of=["标的", "类别", "待确认"]),
                )
            }
        )
    if turn.turn_id == "turn-012":
        # “当前对话用户的身份：没有提供” is a complete, correctly scoped
        # realization. Keep the subject atoms strict while accepting ordinary
        # negative-status paraphrases.
        state = state.model_copy(
            update={
                "active_facts": [
                    *state.active_facts,
                    text_rule(
                        "supplier-route-result-relationship",
                        all_of=["技术路线"],
                        any_of=[
                            "同一结果目标",
                            "相同结果目标",
                            "同一目标",
                            "相同目标",
                            "实现同一结果",
                            "实现相同结果",
                        ],
                    ),
                ],
                "unknown_facts": _replace_rule(
                    state.unknown_facts,
                    "user",
                    text_rule(
                        "user",
                        all_of=["用户", "身份"],
                        any_of=["未提供", "没有提供", "未说明"],
                    ),
                ),
                "forbidden_unknown_facts": [
                    text_rule(
                        "resolved-a-not-unknown",
                        all_of=["A"],
                        any_of=["待核验", "未核验", "尚未核验", "未经核验"],
                    ),
                ],
                "forbidden_inferences": [
                    *state.forbidden_inferences,
                    text_rule(
                        "wrong-current-date-source",
                        all_of=["2027年1月31日"],
                        any_of=[
                            "业务团队调整",
                            "业务团队确认",
                            "由业务团队",
                            "业务调整",
                            "来源：业务",
                            "技术团队调整",
                            "技术团队确认",
                            "由技术团队",
                            "法务调整",
                            "法务确认",
                            "由法务",
                        ],
                    ),
                    text_rule(
                        "historical-response-scope-persisted",
                        any_of=[
                            "不推断设备、软件或施工范围",
                            "不推断设备/软件/施工范围",
                            "设备/软件/施工范围推断禁令",
                            "设备、软件或施工范围推断禁令",
                            "不讨论采购方式",
                            "不得推断设备、软件或施工范围",
                            "不得推断设备/软件/施工范围",
                            "不得讨论采购方式",
                            "不选择采购方式",
                            "不得选择采购方式",
                            "不要选择采购方式",
                        ],
                    ),
                ],
            }
        )
    if turn.turn_id in {"turn-010", "turn-012"}:
        # Score the three legal conclusions independently. This accepts both
        # sentence form ("不涉密") and ordinary ledger/table form
        # ("项目涉密：否") without allowing one conclusion to substitute for
        # another or requiring one exact surface form.
        rule_id = "legal-confirmation" if turn.turn_id == "turn-010" else "legal"
        state = state.model_copy(
            update={
                "active_facts": _replace_rule_many(
                    state.active_facts,
                    rule_id,
                    [
                        text_rule(
                            f"{rule_id}-confidentiality",
                            any_of=[
                                "不涉密",
                                "非涉密",
                                "涉密：否",
                                "涉密:否",
                                "涉密|否",
                            ],
                        ),
                        text_rule(
                            f"{rule_id}-urgency",
                            any_of=[
                                "不应急",
                                "非应急",
                                "不属应急",
                                "应急：否",
                                "应急:否",
                                "应急|否",
                            ],
                        ),
                        text_rule(
                            rule_id,
                            all_of=["不可替代专利"],
                            any_of=[
                                "不存在",
                                "不可替代专利：否",
                                "不可替代专利:否",
                                "不可替代专利|否",
                            ],
                        ),
                    ],
                )
            }
        )
    return turn.model_copy(
        update={"contract": turn.contract.model_copy(update={"conversation_state": state})}
    )


def _revise_alignment_turn(turn: TurnSpec) -> TurnSpec:
    contract = turn.contract
    if turn.turn_id in {"turn-002", "turn-005"}:
        # “待确认 / 尚未确认 / 未确认” express the same unresolved lifecycle
        # state. Keep each subject atom mandatory, but do not turn one preferred
        # surface form into a hidden reference answer.
        state = contract.conversation_state.model_copy(
            update={
                "unknown_facts": [
                    text_rule(
                        "public",
                        all_of=["公开"],
                        any_of=["待确认", "尚未确认", "未确认"],
                    ),
                    text_rule(
                        "complete",
                        all_of=["需求", "完整"],
                        any_of=["待确认", "尚未确认", "未确认"],
                    ),
                    text_rule(
                        "schedule",
                        all_of=["时间", "可行"],
                        any_of=["待确认", "尚未确认", "未确认"],
                    ),
                ],
                "require_scoped_sections": turn.turn_id == "turn-002",
            }
        )
        contract = contract.model_copy(update={"conversation_state": state})
    if turn.turn_id == "turn-003" and contract.decision is not None:
        unknowns = [
            text_rule(
                "public",
                all_of=["公开"],
                any_of=["待确认", "尚未确认", "未确认"],
            ),
            text_rule(
                "complete",
                all_of=["需求", "完整"],
                any_of=["待确认", "尚未确认", "未确认"],
            ),
            text_rule(
                "schedule",
                all_of=["时间", "可行"],
                any_of=["待确认", "尚未确认", "未确认"],
            ),
        ]
        priority_rules = []
        for rule_id, method in [
            ("inquiry", "询比"),
            ("auction", "竞价"),
            ("competitive-negotiation", "竞争谈判"),
        ]:
            priority_rules.append(
                text_rule(
                    f"no-priority-{rule_id}",
                    any_of=[f"优先{method}", f"优先考虑{method}", f"{method}优先"],
                    unless_any_of=[
                        f"不应优先{method}",
                        f"不可优先{method}",
                        f"不能优先{method}",
                        f"不宜优先{method}",
                        f"暂不优先{method}",
                    ],
                )
            )
            priority_rules.append(
                text_rule(
                    f"no-ranking-{rule_id}",
                    all_of=[method],
                    any_of=RANKING_TERMS,
                    unless_any_of=[
                        f"{method}并非最适配",
                        f"不能认为{method}更适合",
                        f"不倾向{method}",
                        f"{method}并非风险最低",
                        f"{method}风险并非最低",
                    ],
                )
            )
        decision = contract.decision.model_copy(
            update={
                "required_unknowns": unknowns,
                "forbidden_recommendations": [
                    *contract.decision.forbidden_recommendations,
                    *priority_rules,
                ],
                "required_defer_claims": _replace_rule(
                    contract.decision.required_defer_claims,
                    "no-final",
                    text_rule(
                        "no-final",
                        any_of=[
                            "暂不定案",
                            "不能确定首选",
                            "不宜确定首选",
                            "不定首选",
                            "未定首选",
                            "不作首选",
                            "不推荐最终",
                            "不做最终",
                            "仅作比较",
                            "只作比较",
                        ],
                    ),
                )
            }
        )
        state = contract.conversation_state.model_copy(
            update={
                "active_facts": [
                    text_rule("known-project-type", all_of=["系统升级服务"]),
                    text_rule("known-budget", all_of=["220万元"]),
                    text_rule("known-suppliers", all_of=["3家"]),
                ],
                "unknown_facts": unknowns,
                "forbidden_inferences": [
                    text_rule(
                        "unsupported-goods-classification",
                        any_of=[
                            "220万元（货物类",
                            "220万元(货物类",
                            "本项目为货物类",
                            "项目属于货物类",
                        ],
                    ),
                    text_rule(
                        "unsupported-legal-scope",
                        any_of=[
                            "本项目不属于依法必须招标",
                            "项目属于依法招标范围以外",
                            "220万元（货物类，依法招标范围以外",
                            "220万元(货物类,依法招标范围以外",
                        ],
                    ),
                ],
            }
        )
        contract = contract.model_copy(
            update={"decision": decision, "conversation_state": state}
        )
    if turn.turn_id == "turn-004":
        required = _replace_rule(
            _replace_rule(
                contract.required_claims,
                "publication-days",
                text_rule(
                    "publication-days",
                    any_of=["不少于3日", "至少3日"],
                ),
            ),
            "review-approval",
            text_rule(
                "review-approval",
                all_of=["分管立项", "采购部门", "公司领导"],
            ),
        )
        anchors = [
            anchor.model_copy(
                update={
                    "all_of": ["分管立项", "采购部门", "公司领导", "复核"],
                    "any_of": ["审批", "批准"],
                }
            )
            if anchor.anchor_id == "review-approval"
            else anchor
            for anchor in contract.evidence_anchors
        ]
        claims = []
        for claim in contract.evidence_claims:
            if claim.rule_id == "publication-period-binding":
                claim = claim.model_copy(
                    update={
                        "claim": text_rule(
                            "publication-claim",
                            any_of=["不少于3日", "至少3日"],
                        )
                    }
                )
            elif claim.rule_id == "review-approval-binding":
                claim = claim.model_copy(
                    update={
                        "claim": text_rule(
                            "review-claim",
                            all_of=["分管立项", "采购部门", "公司领导"],
                        )
                    }
                )
            claims.append(claim)
        contract = contract.model_copy(
            update={
                "required_claims": required,
                "evidence_anchors": anchors,
                "evidence_claims": claims,
            }
        )
    return turn.model_copy(update={"contract": contract})


def _revise_decision_turn(turn: TurnSpec) -> TurnSpec:
    if turn.turn_id != "turn-001" or turn.contract.decision is None:
        return turn
    uncertainty_markers = ["待确认", "待核实", "尚未确认", "未确认"]
    unknown_rules = [
        text_rule(
            "public",
            all_of=["采购信息", "公开"],
            any_of=uncertainty_markers,
        ),
        text_rule(
            "complete",
            all_of=["需求", "完整"],
            any_of=uncertainty_markers,
        ),
        text_rule(
            "schedule",
            all_of=["时间", "可行"],
            any_of=uncertainty_markers,
        ),
    ]
    known_rules = [
        text_rule("known-budget", all_of=["220万元"]),
        text_rule(
            "known-legal-scope",
            any_of=[
                "不属于依法必须招标",
                "非依法必须招标",
                "不属依法必须招标",
                "依法不属于必须招标范围",
                "不在依法必须招标范围",
                "非依法必招",
                "依法可不招标",
                "无需依法招标",
            ],
        ),
        text_rule("known-suppliers", all_of=["3家"]),
        text_rule("known-nonurgent", any_of=["不应急", "非应急"]),
    ]
    ranking_rules = []
    for rule_id, method in [
        ("public-procurement", "公开采购"),
        ("inquiry", "询比"),
        ("auction", "竞价"),
        ("competitive-negotiation", "竞争谈判"),
    ]:
        ranking_rules.append(
            text_rule(
                f"no-ranking-{rule_id}",
                all_of=[method],
                any_of=RANKING_TERMS,
                unless_any_of=[
                    f"{method}并非最适配",
                    f"不能认为{method}更适合",
                    f"不倾向{method}",
                    f"{method}并非风险最低",
                    f"{method}风险并非最低",
                ],
            )
        )
    decision = turn.contract.decision.model_copy(
        update={
            "forbidden_recommendations": [
                *turn.contract.decision.forbidden_recommendations,
                *ranking_rules,
            ],
            "required_defer_claims": _replace_rule(
                turn.contract.decision.required_defer_claims,
                "defer",
                text_rule(
                    "defer",
                    any_of=[
                        "暂不能确定",
                        "暂不推荐",
                        "不能据此定案",
                        "不宜确定首选",
                        "不推荐最终方式",
                        "不作最终推荐",
                        "不做最终推荐",
                        "最终方式需待",
                        "最终采购方式的选择，需",
                        "待确认后再确定",
                        "确认后再确定",
                        "待条件明确后",
                    ],
                ),
            ),
            "required_unknowns": unknown_rules,
        }
    )
    evidence_claims = []
    for claim in turn.contract.evidence_claims:
        if claim.rule_id == "publicity-condition-binding":
            claim = claim.model_copy(
                update={
                    "claim": text_rule(
                        "publicity-condition-claim",
                        all_of=["信息", "公开"],
                    )
                }
            )
        elif claim.rule_id == "public-time-condition-binding":
            claim = claim.model_copy(
                update={
                    "claim": text_rule(
                        "public-time-condition-claim",
                        all_of=["时间"],
                        any_of=["可行", "允许"],
                    )
                }
            )
        evidence_claims.append(claim)
    # This tail condition lives in the next overlapping fragment. It is not a
    # required answer point, but if a model chooses to assert it, the citation
    # immediately following the clause must be the fragment that actually
    # contains it. This catches crossed adjacent-chunk handles without forcing
    # one reference answer or an exhaustive condition list.
    negotiation_tail_anchor = EvidenceAnchor(
        anchor_id="competitive-negotiation-tail-condition",
        all_of=["适用于谈判方式的其他采购"],
        required=False,
    )
    evidence_anchors = [
        *turn.contract.evidence_anchors,
        negotiation_tail_anchor,
    ]
    evidence_claims.append(
        EvidenceClaimRule(
            rule_id="competitive-negotiation-tail-binding",
            claim=text_rule(
                "competitive-negotiation-tail-claim",
                all_of=["适用于谈判方式的其他采购"],
            ),
            anchor_ids=[negotiation_tail_anchor.anchor_id],
            require_following_citation=True,
            required=False,
        )
    )
    contract = turn.contract.model_copy(
        update={
            "required_claims": _replace_rule_many(
                turn.contract.required_claims, "known-facts", known_rules
            ),
            "decision": decision,
            "evidence_anchors": evidence_anchors,
            "evidence_claims": evidence_claims,
            "judge_rubric": (
                "Fail unsupported relationship invention even when all individual words appear in cited evidence. "
                "In particular, an unknown must remain unknown and must not be said to affect, determine, imply, "
                "equal, or favor a different policy condition unless the adjacent evidence explicitly states that "
                "relationship. Fee-standard uniformity, demand completeness, competitive conditions, detailed-spec "
                "indeterminacy, publicity, and timing are distinct concepts. Neutral separate unknowns pass."
            ),
            "conversation_state": turn.contract.conversation_state.model_copy(
                update={
                    "active_facts": known_rules,
                    "unknown_facts": unknown_rules,
                    "require_scoped_sections": True,
                    "forbidden_inferences": [
                        *turn.contract.conversation_state.forbidden_inferences,
                        text_rule(
                            "unsupported-standardization",
                            all_of=["系统升级服务"],
                            any_of=[
                                "通常方案非标准化",
                                "通常非标准化",
                                "一般非标准化",
                                "往往非标准化",
                            ],
                        ),
                        text_rule(
                            "unsupported-service-price-competition",
                            all_of=["服务类", "价格竞争"],
                            any_of=["通常不以", "一般不以", "往往不以"],
                        ),
                        text_rule(
                            "unsupported-project-heuristic",
                            all_of=["系统升级服务"],
                            any_of=["通常", "一般", "往往"],
                        ),
                        text_rule(
                            "unsupported-goods-classification-generic",
                            all_of=["系统升级服务"],
                            any_of=[
                                "标准化货物",
                                "货物类项目",
                                "属于货物",
                                "为货物",
                                "按货物",
                            ],
                        ),
                        text_rule(
                            "unsupported-public-demand-condition",
                            all_of=["公开", "需求", "完整"],
                            any_of=[
                                "任一不满足",
                                "不能走",
                                "无法走",
                                "不满足",
                                "无法满足",
                                "不具备",
                                "不适用",
                                "不宜",
                            ],
                            unless_any_of=[
                                "不是公开采购条件",
                                "不属于公开采购条件",
                                "不能据此判断",
                                "不意味着不能公开",
                            ],
                        ),
                        text_rule(
                            "unknown-demand-implies-negotiation",
                            all_of=["需求", "竞争谈判"],
                            any_of=[
                                "恰符合",
                                "正好符合",
                                "因此符合",
                                "便符合",
                                "可据此采用",
                            ],
                            unless_any_of=["不能据此", "不足以"],
                        ),
                        text_rule(
                            "unsupported-forced-invitation-path",
                            all_of=["邀请"],
                            any_of=["只能走", "只能采用", "唯一可行"],
                            unless_any_of=["不能认为", "并非"],
                        ),
                        text_rule(
                            "unsupported-demand-public-competition-bridge",
                            all_of=["公开采购", "需求", "竞争条件"],
                            any_of=["影响", "取决于", "决定", "意味着", "关联"],
                            unless_any_of=["不能据此", "并不意味着"],
                        ),
                        text_rule(
                            "unsupported-demand-fee-standard-bridge",
                            all_of=["询比", "需求", "收费标准"],
                            any_of=["影响", "取决于", "决定", "意味着", "等同"],
                            unless_any_of=["不能据此", "分别待确认", "是不同条件"],
                        ),
                        text_rule(
                            "unsupported-demand-detailed-spec-bridge",
                            all_of=["竞争谈判", "需求", "详细规格"],
                            any_of=[
                                "影响",
                                "取决于",
                                "决定",
                                "意味着",
                                "等同",
                                "恰符合",
                                "正好符合",
                            ],
                            unless_any_of=["不能据此", "分别待确认", "是不同条件"],
                        ),
                        text_rule(
                            "unknown-demand-parenthetical-redefinition",
                            any_of=[
                                "需求是否完整（",
                                "需求是否完整(",
                                "需求是否完整，即",
                                "需求是否完整即",
                                "该直接条件中需求是否完整",
                            ],
                        ),
                        text_rule(
                            "unknown-schedule-parenthetical-redefinition",
                            any_of=[
                                "采购全流程时间是否可行（",
                                "采购全流程时间是否可行(",
                                "采购全流程时间是否可行，即",
                                "采购全流程时间是否可行即",
                            ],
                        ),
                        text_rule(
                            "no-unsupported-causal-bridge-language",
                            any_of=[
                                "直接影响",
                                "取决于",
                                "意味着",
                                "等同于",
                                "恰符合",
                                "正好符合",
                                "因此符合",
                                "影响竞争条件",
                                "影响收费标准",
                                "影响是否属于",
                            ],
                            unless_any_of=["不能据此", "并不意味着"],
                        ),
                    ]
                }
            ),
        }
    )
    return turn.model_copy(update={"contract": contract})


def build_cases() -> list[CaseSpec]:
    cases: list[CaseSpec] = []
    for case in build_v1_cases():
        turns = case.turns
        if case.family_id == WAREHOUSE_FAMILY:
            turns = [_revise_warehouse_turn(turn) for turn in turns]
        elif case.family_id == ACTIVE_RETIRED_FAMILY:
            turns = [_revise_active_retired_turn(turn) for turn in turns]
        elif case.family_id == ALIGNMENT_FAMILY:
            turns = [_revise_alignment_turn(turn) for turn in turns]
        elif case.family_id == DECISION_FAMILY:
            turns = [_revise_decision_turn(turn) for turn in turns]
        turns = [_strengthen_common_turn(turn) for turn in turns]
        metadata = {
            **case.provenance.metadata,
            "contract_revision": "v2-reviewed-paraphrase-acceptance",
            "observation_compatible_with": "multiturn-ready.v1",
        }
        cases.append(
            case.model_copy(
                update={
                    "suite": SUITE,
                    "turns": turns,
                    "tags": [*case.tags, "contract:v2"],
                    "provenance": case.provenance.model_copy(update={"metadata": metadata}),
                }
            )
        )
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid v2 ready dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "multiturn-ready.v2.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} turns "
        f"to {target} sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
