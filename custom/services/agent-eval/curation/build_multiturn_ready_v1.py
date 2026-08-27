from __future__ import annotations

from pathlib import Path

from curation.build_multiturn_dev_v1 import (
    CORPUS_VERSION,
    KNOWLEDGE_ID,
    MODEL_ID,
    PROFILES,
    build_cases as build_dev_cases,
    no_internal_planning_rule,
    no_retrieval_policy,
    text_rule,
)
from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    Capability,
    CaseSetup,
    CaseSpec,
    ConversationStateContract,
    EvidenceAnchor,
    EvidenceClaimRule,
    Provenance,
    Split,
    ToolPolicy,
    TurnContract,
    TurnSpec,
)


SUITE = "weknora-three-agent-multiturn-ready-v1"
SOURCE = "codex-curated-business-contracts-20260827"


def gate_provenance(family_id: str, profile_id: str) -> Provenance:
    return Provenance(
        source=SOURCE,
        needs_codex_review=False,
        metadata={
            "branch_safe": True,
            "requires_branch_curation": False,
            "gold_answer_policy": "contract-only-no-single-reference-answer",
            "family_isolation": "new-business-facts-and-question-family-not-used-by-dev",
            "profile_id": profile_id,
            "configured_history_turns": PROFILES[profile_id]["history_turns"],
            "owner": "codex",
        },
    )


def setup() -> CaseSetup:
    return CaseSetup(
        knowledge_ids=[KNOWLEDGE_ID],
        summary_model_id=MODEL_ID,
        channel="agent-eval",
    )


def gate_case(
    *,
    profile_id: str,
    prefix: str,
    family_id: str,
    capabilities: list[Capability],
    turns: list[TurnSpec],
    tags: list[str],
) -> CaseSpec:
    return CaseSpec(
        case_id=f"{prefix}-{profile_id}-v1",
        family_id=family_id,
        suite=SUITE,
        split=Split.GATE,
        capabilities=capabilities,
        agent_profile_id=profile_id,
        agent=PROFILES[profile_id]["agent"],
        setup=setup(),
        turns=turns,
        tags=[*tags, f"profile:{profile_id}", "branch-safe", "no-gold-answer"],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        provenance=gate_provenance(family_id, profile_id),
    )


def public_invited_definition_turn(profile_id: str) -> TurnSpec:
    return TurnSpec(
        turn_id="turn-001",
        query=(
            "只根据已选《采购管理办法》的第三十三条，分别说明公开采购和邀请采购如何邀请供应商。"
            "公开采购的句子必须写出‘采购公告’与‘不特定’，邀请采购的句子必须写出‘采购邀请书’"
            "以及‘特定的供应商或3家以上的潜在供应商’，并让每个结论紧邻系统有效引用。"
            "不要扩展到采购方式推荐；没检索到原文就明确缺少证据。"
        ),
        contract=TurnContract(
            required_claims=[
                text_rule("public-definition", all_of=["采购公告", "不特定"]),
                text_rule(
                    "invited-definition",
                    all_of=["采购邀请书", "特定", "3家以上"],
                ),
            ],
            forbidden_claims=[
                text_rule(
                    "no-method-recommendation",
                    any_of=["建议采用", "推荐采用", "最终选择", "首选"],
                ),
                no_internal_planning_rule(),
            ],
            evidence_anchors=[
                EvidenceAnchor(
                    anchor_id="article-33-public-invited",
                    all_of=[
                        "第三十三条",
                        "采购公告",
                        "不特定",
                        "采购邀请书",
                        "3家以上",
                    ],
                    description="第三十三条公开采购与邀请采购定义原文",
                )
            ],
            evidence_claims=[
                EvidenceClaimRule(
                    rule_id="public-definition-binding",
                    claim=text_rule("public-claim", all_of=["采购公告", "不特定"]),
                    anchor_ids=["article-33-public-invited"],
                ),
                EvidenceClaimRule(
                    rule_id="invited-definition-binding",
                    claim=text_rule(
                        "invited-claim", all_of=["采购邀请书", "特定", "3家以上"]
                    ),
                    anchor_ids=["article-33-public-invited"],
                ),
            ],
            min_evidence_anchors=1,
            citation_required=True,
            min_citations=1,
            max_citations=2,
            min_retrieved_sources={"documents": 1},
            tool_policy=ToolPolicy(read_only=True),
            min_response_chars=35,
            max_response_chars=650,
            max_tool_calls=PROFILES[profile_id]["citation_tool_budget"],
            forbid_stale_citations_without_retrieval=True,
        ),
    )


def public_invited_cases() -> list[CaseSpec]:
    return [
        gate_case(
            profile_id=profile_id,
            prefix="gate-public-invited-definition",
            family_id="gate-article-33-public-invited-definition",
            capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION],
            turns=[public_invited_definition_turn(profile_id)],
            tags=["gate", "article-33", "citation-claim-binding"],
        )
        for profile_id in PROFILES
    ]


def _state_contract(
    *,
    active: list | None = None,
    retired: list | None = None,
    unknown: list | None = None,
    forbidden: list | None = None,
    boundaries: list | None = None,
    scoped: bool = False,
    max_chars: int = 550,
) -> TurnContract:
    return TurnContract(
        conversation_state=ConversationStateContract(
            active_facts=active or [],
            retired_facts=retired or [],
            unknown_facts=unknown or [],
            forbidden_inferences=forbidden or [],
            action_boundaries=boundaries or [],
            require_scoped_sections=scoped,
        ),
        tool_policy=no_retrieval_policy(),
        max_citations=0,
        max_tool_calls=4,
        max_response_chars=max_chars,
    )


def warehouse_state_turns(profile_id: str) -> list[TurnSpec]:
    no_write = text_rule(
        "no-write",
        any_of=["不创建", "不会创建", "不得创建", "不修改", "不会修改", "不得修改"],
        all_of=["文件"],
    )
    no_start = text_rule(
        "no-start",
        any_of=["不发起采购", "不会发起采购", "不得发起采购", "不启动采购"],
    )
    return [
        TurnSpec(
            turn_id="turn-001",
            query="建立业务台账：项目代号‘寒星冷链温控改造’，目标是降低仓储温差。未经授权不得创建或修改文件，也不得发起采购，只在对话内维护。",
            contract=_state_contract(
                active=[
                    text_rule("project", all_of=["寒星冷链温控改造"]),
                    text_rule("goal", all_of=["降低仓储温差"]),
                ],
                boundaries=[no_write, no_start],
            ),
        ),
        TurnSpec(
            turn_id="turn-002",
            query="初始预算是210万元，其中设备160万元、平台服务50万元。只记录，不选择采购方式。",
            contract=_state_contract(
                active=[text_rule("budget-v1", all_of=["210万元", "160万元", "50万元"])]
            ),
        ),
        TurnSpec(
            turn_id="turn-003",
            query="财务批复把预算改为235万元，其中设备175万元、平台服务60万元；210万元和160/50构成废弃。",
            contract=_state_contract(
                active=[text_rule("budget-v2", all_of=["235万元", "175万元", "60万元"])],
                retired=[text_rule("budget-retired", all_of=["210万元", "废弃"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-004",
            query="项目负责人是周岚。当前对话用户身份仍未提供，不得把用户等同于周岚。",
            contract=_state_contract(
                active=[text_rule("owner", all_of=["项目负责人", "周岚"])],
                unknown=[text_rule("user-identity", all_of=["用户身份", "未提供"])],
                forbidden=[text_rule("owner-is-user", any_of=["你就是周岚", "用户是周岚", "当前用户周岚"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-005",
            query="初始验收日期记为2027年3月31日，只记录日期，不推断工期是否紧张。",
            contract=_state_contract(
                active=[text_rule("date-v1", all_of=["2027年3月31日"])],
                forbidden=[text_rule("urgency", any_of=["工期紧张", "非常紧急", "时间不足已确认"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-006",
            query="验收日期调整为2027年5月15日，2027年3月31日从现在起废弃。",
            contract=_state_contract(
                active=[text_rule("date-v2", all_of=["2027年5月15日"])],
                retired=[text_rule("date-retired", all_of=["2027年3月31日", "废弃"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-007",
            query="已确认范围包含温度传感器和监控平台；是否包含仓库布线施工仍待确认。只区分已确认和待确认。",
            contract=_state_contract(
                active=[text_rule("scope-known", all_of=["温度传感器", "监控平台"])],
                unknown=[text_rule("wiring-unknown", all_of=["布线施工", "待确认"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-008",
            query="D供应商声称现有网关只能由它兼容。该说法只是供应商主张，尚未核验，不得写成排他事实。",
            contract=_state_contract(
                unknown=[text_rule("d-claim", all_of=["D", "待核验"])],
                forbidden=[text_rule("d-exclusive", any_of=["只能由D", "只有D能", "D不可替代已确认"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-009",
            query="技术组完成核验：D并非不可替代，E、F经适配也能兼容；废弃‘只能D’的前提。",
            contract=_state_contract(
                active=[text_rule("non-exclusive", all_of=["D", "并非不可替代", "E", "F"])],
                retired=[text_rule("exclusive-retired", all_of=["只能D", "废弃"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-010",
            query=(
                "先暂停台账，只根据已选《采购管理办法》第三十四条回答一个旁支问题：依法必须招标的"
                "重要设备、材料等货物，达到什么单项合同估算价必须公开招标？答案写明金额并紧邻系统有效引用。"
            ),
            contract=TurnContract(
                required_claims=[text_rule("goods-threshold", all_of=["200万元", "公开招标"])],
                evidence_anchors=[
                    EvidenceAnchor(
                        anchor_id="article-34-goods-threshold",
                        all_of=["第三十四条", "重要设备", "材料", "200万元", "公开招标"],
                    )
                ],
                evidence_claims=[
                    EvidenceClaimRule(
                        rule_id="goods-threshold-binding",
                        claim=text_rule("threshold-claim", all_of=["200万元", "公开招标"]),
                        anchor_ids=["article-34-goods-threshold"],
                    )
                ],
                min_evidence_anchors=1,
                citation_required=True,
                min_citations=1,
                max_citations=2,
                min_retrieved_sources={"documents": 1},
                tool_policy=ToolPolicy(read_only=True),
                max_response_chars=450,
                max_tool_calls=PROFILES[profile_id]["citation_tool_budget"],
                forbid_stale_citations_without_retrieval=True,
            ),
        ),
        TurnSpec(
            turn_id="turn-011",
            query="回到寒星项目：法务确认采购信息可以公开；立项审批状态仍待确认。只更新台账，不因为刚才的金额门槛直接选采购方式。",
            contract=_state_contract(
                active=[text_rule("publicity", all_of=["采购信息", "可以公开"])],
                unknown=[text_rule("approval", all_of=["立项审批", "待确认"])],
                forbidden=[text_rule("no-threshold-decision", any_of=["因此必须公开招标", "所以必须公开招标", "确定采用公开招标"])],
            ),
        ),
        TurnSpec(
            turn_id="turn-012",
            query=(
                "现在做最终台账审计，分成‘当前有效事实/已废弃事实/待确认事项/行动边界’四段。"
                "必须保留来源主体，不要复述旁支的门槛答案，也不要选择采购方式或执行任何操作。"
            ),
            contract=_state_contract(
                active=[
                    text_rule("project-final", all_of=["寒星冷链温控改造", "降低仓储温差"]),
                    text_rule("budget-final", all_of=["235万元", "175万元", "60万元"]),
                    text_rule("owner-final", all_of=["项目负责人", "周岚"]),
                    text_rule("date-final", all_of=["2027年5月15日"]),
                    text_rule("scope-final", all_of=["温度传感器", "监控平台"]),
                    text_rule("supplier-final", all_of=["技术组", "D", "并非不可替代", "E", "F"]),
                    text_rule("publicity-final", all_of=["法务", "采购信息", "可以公开"]),
                ],
                retired=[
                    text_rule("budget-old-final", all_of=["210万元", "废弃"]),
                    text_rule("date-old-final", all_of=["2027年3月31日", "废弃"]),
                    text_rule("exclusive-old-final", all_of=["只能D", "废弃"]),
                ],
                unknown=[
                    text_rule("wiring-final", all_of=["布线施工", "待确认"]),
                    text_rule("approval-final", all_of=["立项审批", "待确认"]),
                    text_rule("identity-final", all_of=["用户身份", "未提供"]),
                ],
                forbidden=[
                    text_rule("owner-user-final", any_of=["你就是周岚", "用户是周岚", "当前用户周岚"]),
                    text_rule("no-final-method", any_of=["建议采用", "推荐采用", "最终选择", "确定采用"]),
                    text_rule("no-side-answer", all_of=["200万元", "必须公开招标"]),
                ],
                boundaries=[no_write, no_start],
                scoped=True,
                max_chars=1200,
            ),
        ),
    ]


def warehouse_state_cases() -> list[CaseSpec]:
    return [
        gate_case(
            profile_id=profile_id,
            prefix="gate-warehouse-state-window",
            family_id="gate-warehouse-state-supersession-and-detour",
            capabilities=[
                Capability.LONG_CONTEXT_DIALOGUE,
                Capability.RAG_RETRIEVAL,
                Capability.CITATION,
                Capability.TOOL_USE,
            ],
            turns=warehouse_state_turns(profile_id),
            tags=["gate", "history-window", "state-supersession", "topic-detour"],
        )
        for profile_id in PROFILES
    ]


def build_cases() -> list[CaseSpec]:
    dev_cases = [
        case.model_copy(
            update={
                "suite": SUITE,
                "provenance": case.provenance.model_copy(
                    update={
                        "metadata": {
                            **case.provenance.metadata,
                            "promoted_into_suite": SUITE,
                        }
                    }
                ),
            }
        )
        for case in build_dev_cases()
    ]
    cases = [*dev_cases, *public_invited_cases(), *warehouse_state_cases()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid ready dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "multiturn-ready.v1.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} turns "
        f"to {target} sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
