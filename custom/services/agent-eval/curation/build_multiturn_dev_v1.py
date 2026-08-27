from __future__ import annotations

from pathlib import Path

from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseSetup,
    CaseSpec,
    ConversationStateContract,
    DecisionContract,
    DecisionMode,
    EvidenceAnchor,
    EvidenceClaimRule,
    Provenance,
    Split,
    TextRule,
    ToolPolicy,
    TurnContract,
    TurnSpec,
)


MODEL_ID = "${AGENT_EVAL_SUMMARY_MODEL_ID}"
KNOWLEDGE_ID = "${AGENT_EVAL_PROCUREMENT_KNOWLEDGE_ID}"
CORPUS_VERSION = "${AGENT_EVAL_CORPUS_VERSION}"
SUITE = "weknora-three-agent-multiturn-dev-v1"
SOURCE = "codex-curated-from-procurement-discovery-20260827"


PROFILES = {
    "quick-answer": {
        "agent": AgentSelector(
            endpoint="knowledge-chat",
            agent_id="builtin-quick-answer",
            agent_type="rag-qa",
        ),
        "history_turns": 5,
        "citation_tool_budget": 4,
        "reasoning_tool_budget": 4,
    },
    "rag-reasoning": {
        "agent": AgentSelector(
            endpoint="agent-chat",
            agent_id="builtin-smart-reasoning",
            agent_type="rag-qa",
        ),
        "history_turns": 10,
        "citation_tool_budget": 6,
        "reasoning_tool_budget": 8,
    },
    "general-agent": {
        "agent": AgentSelector(
            endpoint="agent-chat",
            agent_id="builtin-general-agent",
            agent_type="general-agent",
        ),
        "history_turns": 10,
        "citation_tool_budget": 10,
        "reasoning_tool_budget": 10,
    },
}

DISCOVERY_SOURCES = {
    "quick-answer": {
        "artifact": "artifacts/discovery-quick-adaptive-20260827.json",
        "sha256": "efe1823dc1e9d5357192890c5284cb34cb387320768982b351042b0e52e12d4d",
    },
    "rag-reasoning": {
        "artifact": "artifacts/discovery-rag-reasoning-adaptive-20260827.json",
        "sha256": "a1771f5a4116ef0beeeba4e00acc5bc8ac8811972c2a58e2ef33a0a9691bcd2a",
    },
    "general-agent": {
        "artifact": "artifacts/discovery-general-agent-adaptive-20260827.json",
        "sha256": "0a13fccfa2231ef989012beea54fb2146deb92b3a246d729ad53c38f4ff2a659",
    },
}


NO_RETRIEVAL_TOOLS = [
    "knowledge_search",
    "grep_chunks",
    "list_knowledge_chunks",
    "query_knowledge_graph",
    "web_search",
    "web_fetch",
]


def text_rule(
    rule_id: str,
    *,
    any_of: list[str] | None = None,
    all_of: list[str] | None = None,
    unless_any_of: list[str] | None = None,
    description: str = "",
) -> TextRule:
    return TextRule(
        rule_id=rule_id,
        any_of=any_of or [],
        all_of=all_of or [],
        unless_any_of=unless_any_of or [],
        description=description,
    )


def no_internal_planning_rule() -> TextRule:
    return text_rule(
        "no-internal-planning",
        any_of=["Now I have", "Now let me", "Let me organize", "Let me answer"],
        description="不得向用户暴露英文内部规划过渡语",
    )


def no_retrieval_policy() -> ToolPolicy:
    return ToolPolicy(read_only=True, forbidden_tools=NO_RETRIEVAL_TOOLS)


def no_final_method_rules(*, include_public_tender: bool = True) -> list[TextRule]:
    methods = [
        ("inquiry", "询比"),
        ("auction", "竞价"),
        ("competitive-negotiation", "竞争谈判"),
    ]
    if include_public_tender:
        methods.insert(0, ("public-tender", "公开招标"))
    return [
        text_rule(
            f"no-final-{rule_id}",
            any_of=[
                f"首选{method}",
                f"首选为{method}",
                f"建议采用{method}",
                f"推荐采用{method}",
                f"最终采用{method}",
                f"确定采用{method}",
            ],
            unless_any_of=[
                f"不首选{method}",
                f"不宜首选{method}",
                f"暂不首选{method}",
                f"不建议采用{method}",
                f"暂不建议采用{method}",
                f"不能建议采用{method}",
                f"不推荐采用{method}",
                f"暂不推荐采用{method}",
                f"不能确定采用{method}",
                f"尚不能确定采用{method}",
            ],
            description=f"关键条件未满足时不得把{method}写成最终推荐",
        )
        for rule_id, method in methods
    ]


def method_definition_anchors() -> list[EvidenceAnchor]:
    return [
        EvidenceAnchor(
            anchor_id="inquiry-definition",
            all_of=["第三十五条", "询比采购", "一次性报出不可更改价格"],
        ),
        EvidenceAnchor(
            anchor_id="auction-definition",
            all_of=["第三十六条", "竞价采购", "多次竞争报价"],
        ),
        EvidenceAnchor(
            anchor_id="competitive-negotiation-definition",
            all_of=["第三十七条", "竞争谈判", "二家以上符合资格条件"],
        ),
    ]


def method_condition_anchors() -> list[EvidenceAnchor]:
    return [
        EvidenceAnchor(
            anchor_id="inquiry-conditions",
            all_of=["询比采购", "采购需求确定", "收费标准统一"],
        ),
        EvidenceAnchor(
            anchor_id="auction-conditions",
            all_of=["竞价采购", "采购需求明确", "服务标准要求完整"],
        ),
        EvidenceAnchor(
            anchor_id="competitive-negotiation-conditions",
            all_of=["竞争谈判", "功能性指标", "不同路径和方案"],
        ),
    ]


def method_claim_bindings(*, condition_anchors: bool) -> list[EvidenceClaimRule]:
    suffix = "conditions" if condition_anchors else "definition"
    return [
        EvidenceClaimRule(
            rule_id=f"inquiry-{suffix}-binding",
            claim=text_rule("inquiry-claim", all_of=["询比"]),
            anchor_ids=[f"inquiry-{suffix}"],
        ),
        EvidenceClaimRule(
            rule_id=f"auction-{suffix}-binding",
            claim=text_rule("auction-claim", all_of=["竞价"]),
            anchor_ids=[f"auction-{suffix}"],
        ),
        EvidenceClaimRule(
            rule_id=f"competitive-negotiation-{suffix}-binding",
            claim=text_rule(
                "competitive-negotiation-claim", all_of=["竞争谈判"]
            ),
            anchor_ids=[f"competitive-negotiation-{suffix}"],
        ),
    ]


def provenance(family: str, profile_id: str, *, note: str = "") -> Provenance:
    discovery_source = DISCOVERY_SOURCES[profile_id]
    return Provenance(
        source=SOURCE,
        source_hash=discovery_source["sha256"],
        needs_codex_review=False,
        metadata={
            "branch_safe": True,
            "requires_branch_curation": False,
            "gold_answer_policy": "contract-only-no-single-reference-answer",
            "source_discovery_report": "procurement-multiturn-discovery-20260827.md",
            "source_discovery_artifact": discovery_source["artifact"],
            "source_problem_family": family,
            "profile_id": profile_id,
            "configured_history_turns": PROFILES[profile_id]["history_turns"],
            "note": note,
        },
    )


def setup(*, with_knowledge: bool) -> CaseSetup:
    return CaseSetup(
        knowledge_ids=[KNOWLEDGE_ID] if with_knowledge else [],
        summary_model_id=MODEL_ID,
        channel="agent-eval",
    )


def case_for_profile(
    *,
    profile_id: str,
    case_prefix: str,
    family_id: str,
    capabilities: list[Capability],
    turns: list[TurnSpec],
    with_knowledge: bool,
    tags: list[str],
    note: str = "",
) -> CaseSpec:
    profile = PROFILES[profile_id]
    return CaseSpec(
        case_id=f"{case_prefix}-{profile_id}-v1",
        family_id=family_id,
        suite=SUITE,
        split=Split.DEV,
        enabled=True,
        capabilities=capabilities,
        agent_profile_id=profile_id,
        agent=profile["agent"],
        setup=setup(with_knowledge=with_knowledge),
        turns=turns,
        tags=[*tags, f"profile:{profile_id}", "branch-safe", "no-gold-answer"],
        corpus_version=CORPUS_VERSION if with_knowledge else None,
        provenance=provenance(family_id, profile_id, note=note),
    )


def citation_claim_turn(profile_id: str) -> TurnSpec:
    return TurnSpec(
        turn_id="turn-001",
        query=(
            "只根据已选《采购管理办法》检索第三十六条，只说明竞价采购的定义。答案必须同时包含"
            "‘征集3家以上供应商’和‘多次竞争报价’，并把支持这一定义的系统有效引用紧邻该句。"
            "不要根据我的问题补写原文；如果没有检索到支持片段，就明确说缺少证据。"
        ),
        contract=TurnContract(
            required_claims=[
                text_rule(
                    "auction-definition",
                    all_of=["征集3家以上供应商", "多次竞争报价"],
                    description="定义必须保留制度原文中的两个核心要件",
                )
            ],
            forbidden_claims=[
                text_rule(
                    "no-invented-one-shot-quote",
                    any_of=["一次性报价", "服务标准统一"],
                    description="不得复用 discovery 中无证据的伪原文",
                ),
                no_internal_planning_rule(),
            ],
            evidence_anchors=[
                EvidenceAnchor(
                    anchor_id="article-36-auction-definition",
                    all_of=["第三十六条", "征集3家以上供应商", "多次竞争报价"],
                    description="第三十六条竞价采购定义的精确片段",
                )
            ],
            evidence_claims=[
                EvidenceClaimRule(
                    rule_id="auction-definition-binding",
                    claim=text_rule(
                        "auction-definition-claim",
                        all_of=["征集3家以上供应商", "多次竞争报价"],
                    ),
                    anchor_ids=["article-36-auction-definition"],
                    description="定义句必须邻接引用真正包含两个要件的证据片段",
                )
            ],
            min_evidence_anchors=1,
            citation_required=True,
            min_citations=1,
            max_citations=2,
            min_retrieved_sources={"documents": 1},
            tool_policy=ToolPolicy(read_only=True),
            min_response_chars=20,
            max_response_chars=500,
            max_tool_calls=PROFILES[profile_id]["citation_tool_budget"],
            forbid_stale_citations_without_retrieval=True,
        ),
    )


def citation_cases() -> list[CaseSpec]:
    return [
        case_for_profile(
            profile_id=profile_id,
            case_prefix="citation-claim-binding-auction",
            family_id="citation-claim-binding-auction-definition",
            capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION],
            turns=[citation_claim_turn(profile_id)],
            with_knowledge=True,
            tags=["citation-claim-binding", "exact-clause", "article-36"],
            note="同一条款、同一问法覆盖三个智能体，直接比较召回与引用绑定。",
        )
        for profile_id in PROFILES
    ]


def state_turns() -> list[TurnSpec]:
    short = 450
    common = {
        "tool_policy": no_retrieval_policy(),
        "max_citations": 0,
        "max_tool_calls": 4,
    }
    return [
        TurnSpec(
            turn_id="turn-001",
            query=(
                "建立项目台账：项目代号‘启明星视觉升级’，业务目标是提升缺陷识别率。未经我明确授权，"
                "不得创建或修改文件，也不得发起采购；只在对话里维护。只确认这些事实，不推断设备、软件或施工范围。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("project", all_of=["启明星视觉升级"]),
                        text_rule("goal", all_of=["提升缺陷识别率"]),
                    ],
                    forbidden_inferences=[
                        text_rule(
                            "scope",
                            any_of=["摄像头", "工控机", "软件开发", "安装施工"],
                        )
                    ],
                    action_boundaries=[
                        text_rule(
                            "no-file-write",
                            any_of=["不得创建", "不创建", "不会创建", "不得修改", "不修改", "不会修改"],
                            all_of=["文件"],
                        ),
                        text_rule(
                            "no-procurement-action",
                            any_of=["不得发起采购", "不发起采购", "不会发起采购", "不启动采购", "不会启动采购"],
                        ),
                        text_rule(
                            "chat-only",
                            any_of=["只在对话", "仅在对话", "只在会话", "仅在会话"],
                        ),
                    ],
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-002",
            query="初始获批总预算为360万元，其中设备280万元、实施服务80万元。只更新台账，不讨论采购方式。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule(
                            "initial-budget",
                            all_of=["360万元", "280万元", "80万元"],
                        )
                    ]
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-003",
            query=(
                "财务把预算调整为390万元，其中设备300万元、实施服务90万元。360万元及280/80万元构成从现在起废弃。"
                "只记录当前值和废弃值。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule(
                            "current-budget",
                            all_of=["390万元", "300万元", "90万元"],
                        )
                    ],
                    retired_facts=[
                        text_rule(
                            "retired-budget",
                            all_of=["360万元", "280万元", "80万元", "废弃"],
                        )
                    ],
                ),
                max_response_chars=500,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-004",
            query="初始目标日期是2026年11月30日。只记录日期，不推断是否紧急。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("initial-date", all_of=["2026年11月30日"])
                    ],
                    forbidden_inferences=[
                        text_rule("urgency", any_of=["非常紧急", "时间紧迫"])
                    ],
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-005",
            query="目标日期调整为2027年1月31日，2026年11月30日从现在起废弃。只更新日期状态。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("current-date", all_of=["2027年1月31日"])
                    ],
                    retired_facts=[
                        text_rule(
                            "retired-date",
                            all_of=["2026年11月30日", "废弃"],
                        )
                    ],
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-006",
            query="业务团队确认至少4家供应商可能满足，技术路线不同但都可能实现同一结果目标；需求是否完整仍待核实。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("supplier-count", all_of=["4家", "技术路线"])
                    ],
                    unknown_facts=[
                        text_rule(
                            "requirements-complete",
                            all_of=["需求", "完整", "待核实"],
                        )
                    ],
                    forbidden_inferences=[
                        text_rule(
                            "requirements-incomplete",
                            any_of=["需求不完整已确认", "规格不统一已确认"],
                        )
                    ],
                ),
                max_response_chars=500,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-007",
            query="A供应商声称接口只能由它安全改造。该说法目前只是供应商主张，尚未核验，不得写成事实。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    unknown_facts=[
                        text_rule("a-claim", all_of=["A", "待核验"])
                    ],
                    forbidden_inferences=[
                        text_rule(
                            "a-exclusive",
                            any_of=["只能由A实施", "只有A能做", "已确认A不可替代"],
                        )
                    ],
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-008",
            query="法务和技术核验后确认A并非不可替代，B、C通过适配也能满足。废弃‘只能A做’这一前提。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule(
                            "not-exclusive",
                            all_of=["A", "并非不可替代", "B", "C"],
                        )
                    ],
                    retired_facts=[
                        text_rule("retired-exclusive", all_of=["只能A", "废弃"])
                    ],
                ),
                max_response_chars=500,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-009",
            query="采购标的最终类别仍未确认；采购信息是否可以公开也仍未确认。只把两项都列为待确认。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    unknown_facts=[
                        text_rule("category", all_of=["标的类别", "待确认"]),
                        text_rule("publicity", all_of=["采购信息", "公开", "待确认"]),
                    ]
                ),
                max_response_chars=short,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-010",
            query="法务确认项目不涉密、不应急，也不存在不可替代专利。只记录确认结果，不选择采购方式。",
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule(
                            "legal-confirmation",
                            all_of=["不涉密", "不应急", "不存在不可替代专利"],
                        )
                    ]
                ),
                forbidden_claims=[
                    *no_final_method_rules()
                ],
                max_response_chars=500,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-011",
            query=(
                "补充来源：项目负责人是林梅；不涉密、不应急由法务确认，4家方案可行由业务和技术团队确认。"
                "当前对话用户身份没有提供，不得把用户等同于林梅。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("owner", all_of=["项目负责人", "林梅"]),
                        text_rule("legal-source", all_of=["法务", "不涉密", "不应急"]),
                        text_rule("business-source", all_of=["业务", "技术", "4家"]),
                    ],
                    unknown_facts=[
                        text_rule("user-identity", all_of=["用户身份", "未提供"])
                    ],
                    forbidden_inferences=[
                        text_rule(
                            "owner-is-user",
                            any_of=["当前用户是林梅", "你就是林梅", "用户林梅"],
                        )
                    ],
                ),
                max_response_chars=600,
                **common,
            ),
        ),
        TurnSpec(
            turn_id="turn-012",
            query=(
                "现在做一次完整状态审计，不要重新检索制度，也不要选择采购方式。分为‘当前有效事实’、‘已废弃事实’、"
                "‘待确认事实’和‘行动边界’四栏；必须依据整个会话自行恢复，不要要求我重述。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("project", all_of=["启明星视觉升级"]),
                        text_rule("goal", all_of=["提升缺陷识别率"]),
                        text_rule(
                            "budget",
                            all_of=["390万元", "300万元", "90万元"],
                        ),
                        text_rule("date", all_of=["2027年1月31日"]),
                        text_rule("suppliers", all_of=["4家", "B", "C"]),
                        text_rule("owner", all_of=["项目负责人", "林梅"]),
                        text_rule("legal", all_of=["不涉密", "不应急", "不存在不可替代专利"]),
                    ],
                    retired_facts=[
                        text_rule(
                            "old-budget",
                            all_of=["360万元", "280万元", "80万元", "废弃"],
                        ),
                        text_rule(
                            "old-date",
                            all_of=["2026年11月30日", "废弃"],
                        ),
                        text_rule("old-exclusive", all_of=["只能A", "废弃"]),
                    ],
                    unknown_facts=[
                        text_rule("category", all_of=["标的类别", "待确认"]),
                        text_rule("publicity", all_of=["采购信息", "公开", "待确认"]),
                        text_rule("requirements", all_of=["需求", "完整", "待确认"]),
                        text_rule("user", all_of=["用户身份", "未提供"]),
                    ],
                    forbidden_inferences=[
                        text_rule(
                            "wrong-current-state",
                            any_of=[
                                "当前预算360万元",
                                "当前日期2026年11月30日",
                                "只能由A实施",
                                "当前用户是林梅",
                            ],
                        )
                    ],
                    action_boundaries=[
                        text_rule(
                            "no-file-write",
                            any_of=["不得创建", "不创建", "不会创建", "不得修改", "不修改", "不会修改"],
                            all_of=["文件"],
                        ),
                        text_rule(
                            "no-procurement-action",
                            any_of=["不得发起采购", "不发起采购", "不会发起采购", "不启动采购", "不会启动采购"],
                        ),
                        text_rule(
                            "chat-only",
                            any_of=["只在对话", "仅在对话", "只在会话", "仅在会话"],
                        ),
                    ],
                    require_scoped_sections=True,
                ),
                forbidden_claims=[
                    *no_final_method_rules(),
                    no_internal_planning_rule(),
                ],
                max_response_chars=1600,
                judge_rubric=(
                    "只判断四栏是否将用户事实按当前、废弃、待确认和行动边界正确归类；"
                    "不得因为措辞不同而失败，也不得把讨论话题或助手推断当作项目事实。"
                ),
                **common,
            ),
        ),
    ]


def state_cases() -> list[CaseSpec]:
    notes = {
        "quick-answer": "第12轮远超5轮窗口；最早事实不在原始历史中。",
        "rag-reasoning": "第12轮严格只保留前10个完整问答对；turn-001不在原始历史中。",
        "general-agent": (
            "当前实现第12轮实际可带入11个历史问答对，turn-001仍在；"
            "该 case 同时暴露配置轮数不一致和长提示注意力污染。"
        ),
    }
    return [
        case_for_profile(
            profile_id=profile_id,
            case_prefix="active-retired-state-window",
            family_id="active-retired-state-window-plus-two",
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE, Capability.TOOL_USE],
            turns=state_turns(),
            with_knowledge=True,
            tags=["state-lifecycle", "history-window", "source-attribution", "action-boundary"],
            note=notes[profile_id],
        )
        for profile_id in PROFILES
    ]


def alignment_turns(profile_id: str) -> list[TurnSpec]:
    tool_budget = PROFILES[profile_id]["reasoning_tool_budget"]
    return [
        TurnSpec(
            turn_id="turn-001",
            query="请依据已选制度比较询比、竞价和竞争谈判的定义与适用重点，每种方式一段并就近引用。暂不结合具体项目。",
            contract=TurnContract(
                required_claims=[
                    text_rule("three-methods", all_of=["询比", "竞价", "竞争谈判"])
                ],
                forbidden_claims=[no_internal_planning_rule()],
                evidence_anchors=method_definition_anchors(),
                evidence_claims=method_claim_bindings(condition_anchors=False),
                min_evidence_anchors=3,
                citation_required=True,
                min_citations=3,
                max_citations=6,
                min_retrieved_sources={"documents": 1},
                max_response_chars=1200,
                max_tool_calls=tool_budget,
            ),
        ),
        TurnSpec(
            turn_id="turn-002",
            query=(
                "建立项目事实：系统升级服务预算220万元，至少3家供应商可参与，是否可以公开采购、需求是否完整、"
                "全流程时间是否可行都尚未确认。只列已确认和待确认，不比较方式，本轮不要检索。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    active_facts=[
                        text_rule("known", all_of=["系统升级服务", "220万元", "3家"])
                    ],
                    unknown_facts=[
                        text_rule("public", all_of=["公开", "待确认"]),
                        text_rule("complete", all_of=["需求", "完整", "待确认"]),
                        text_rule("schedule", all_of=["时间", "待确认"]),
                    ],
                ),
                forbidden_claims=[
                    text_rule("no-comparison", any_of=["询比", "竞价", "竞争谈判"]),
                    no_internal_planning_rule(),
                ],
                max_citations=0,
                tool_policy=no_retrieval_policy(),
                max_response_chars=600,
                max_tool_calls=4,
            ),
        ),
        TurnSpec(
            turn_id="turn-003",
            query=(
                "仅基于刚才明确的项目事实和制度，比较询比、竞价、竞争谈判的适配点与风险，不定首选。"
                "每种方式一行，制度判断就近引用。"
            ),
            contract=TurnContract(
                required_claims=[
                    text_rule("three-methods", all_of=["询比", "竞价", "竞争谈判"])
                ],
                decision=DecisionContract(
                    mode=DecisionMode.COMPARE_ONLY,
                    required_defer_claims=[
                        text_rule(
                            "no-final",
                            any_of=["暂不定案", "不能确定首选", "不宜确定首选"],
                        )
                    ],
                    forbidden_recommendations=no_final_method_rules(
                        include_public_tender=False
                    ),
                ),
                evidence_anchors=method_condition_anchors(),
                evidence_claims=method_claim_bindings(condition_anchors=True),
                min_evidence_anchors=3,
                citation_required=True,
                min_citations=3,
                max_citations=6,
                min_retrieved_sources={"documents": 1},
                max_response_chars=900,
                max_tool_calls=tool_budget,
            ),
        ),
        TurnSpec(
            turn_id="turn-004",
            query=(
                "先停止采购方式比较，临时只回答两个制度问题：中标候选人公示至少多少日？"
                "如果异议涉及实质内容并影响候选人排名，由哪些分管公司领导批准复核？每个结论就近引用。"
            ),
            contract=TurnContract(
                required_claims=[
                    text_rule("publication-days", all_of=["不少于3日"]),
                    text_rule("review-approval", all_of=["分管立项部门", "采购部门", "公司领导"]),
                ],
                forbidden_claims=[
                    text_rule("no-stale-comparison", any_of=["询比", "竞价", "竞争谈判"]),
                    no_internal_planning_rule(),
                ],
                evidence_anchors=[
                    EvidenceAnchor(
                        anchor_id="publication-period",
                        all_of=["中标候选人", "公示期", "不少于3日"],
                    ),
                    EvidenceAnchor(
                        anchor_id="review-approval",
                        all_of=["分管立项部门", "采购部门", "公司领导", "批准", "复核"],
                    ),
                ],
                evidence_claims=[
                    EvidenceClaimRule(
                        rule_id="publication-period-binding",
                        claim=text_rule("publication-claim", all_of=["不少于3日"]),
                        anchor_ids=["publication-period"],
                    ),
                    EvidenceClaimRule(
                        rule_id="review-approval-binding",
                        claim=text_rule(
                            "review-claim",
                            all_of=["分管立项部门", "采购部门", "公司领导"],
                        ),
                        anchor_ids=["review-approval"],
                    ),
                ],
                min_evidence_anchors=2,
                citation_required=True,
                min_citations=2,
                max_citations=4,
                min_retrieved_sources={"documents": 1},
                max_response_chars=600,
                max_tool_calls=tool_budget,
            ),
        ),
        TurnSpec(
            turn_id="turn-005",
            query=(
                "回到系统升级项目并停止讨论公示和复核。本轮只列仍待确认的三项：采购信息是否可公开、需求是否完整、"
                "全流程时间是否可行；不要再比较采购方式，也不要检索。"
            ),
            contract=TurnContract(
                conversation_state=ConversationStateContract(
                    unknown_facts=[
                        text_rule("public", all_of=["采购信息", "公开", "待确认"]),
                        text_rule("complete", all_of=["需求", "完整", "待确认"]),
                        text_rule("schedule", all_of=["时间", "可行", "待确认"]),
                    ]
                ),
                forbidden_claims=[
                    text_rule("no-methods", any_of=["询比", "竞价", "竞争谈判"]),
                    text_rule("no-side-topic", any_of=["公示", "复核"]),
                    no_internal_planning_rule(),
                ],
                max_citations=0,
                tool_policy=no_retrieval_policy(),
                max_response_chars=350,
                max_tool_calls=4,
            ),
        ),
    ]


def alignment_cases() -> list[CaseSpec]:
    return [
        case_for_profile(
            profile_id=profile_id,
            case_prefix="current-turn-alignment",
            family_id="current-turn-alignment-topic-detour",
            capabilities=[
                Capability.LONG_CONTEXT_DIALOGUE,
                Capability.RAG_RETRIEVAL,
                Capability.CITATION,
            ],
            turns=alignment_turns(profile_id),
            with_knowledge=True,
            tags=["current-turn-alignment", "topic-detour", "resume", "citation"],
            note="后续问题只依赖用户已声明事实，不假设智能体上一轮回答正确。",
        )
        for profile_id in PROFILES
    ]


def decision_turn(profile_id: str) -> TurnSpec:
    return TurnSpec(
        turn_id="turn-001",
        query=(
            "项目是系统升级服务，预算220万元，法务确认不属于依法必须招标范围；至少3家供应商可能参与，"
            "且不涉密、不应急。尚未确认采购信息能否公开、需求是否完整、采购全流程时间是否可行。"
            "请依据已选制度比较公开采购、询比、竞价和竞争谈判会受哪些条件影响，但不要推荐最终方式。"
            "区分已确认与待确认，每个制度判断就近引用。"
        ),
        contract=TurnContract(
            required_claims=[
                text_rule("known-facts", all_of=["220万元", "不属于依法必须招标", "3家", "不应急"]),
                text_rule(
                    "method-coverage",
                    all_of=["公开采购", "询比", "竞价", "竞争谈判"],
                ),
            ],
            forbidden_claims=[no_internal_planning_rule()],
            conversation_state=ConversationStateContract(
                unknown_facts=[
                    text_rule("public", all_of=["采购信息", "公开", "待确认"]),
                    text_rule("complete", all_of=["需求", "完整", "待确认"]),
                    text_rule("schedule", all_of=["时间", "可行", "待确认"]),
                ],
                forbidden_inferences=[
                    text_rule(
                        "nonmandatory-does-not-set-publicity",
                        any_of=["非依法必招所以可以公开", "非依法必招因此无需确认公开"],
                    )
                ],
            ),
            decision=DecisionContract(
                mode=DecisionMode.COMPARE_ONLY,
                required_unknowns=[
                    text_rule("public", all_of=["采购信息", "公开", "待确认"]),
                    text_rule("complete", all_of=["需求", "完整", "待确认"]),
                    text_rule("schedule", all_of=["时间", "可行", "待确认"]),
                ],
                required_defer_claims=[
                    text_rule(
                        "defer",
                        any_of=["暂不能确定", "暂不推荐", "不能据此定案", "不宜确定首选"],
                    )
                ],
                forbidden_recommendations=no_final_method_rules(),
            ),
            evidence_anchors=[
                EvidenceAnchor(
                    anchor_id="public-procurement-conditions",
                    all_of=["采购信息可以公开", "采购标的具有竞争条件", "采购时间允许", "采购成本合理"],
                ),
                *method_condition_anchors(),
            ],
            evidence_claims=[
                EvidenceClaimRule(
                    rule_id="publicity-condition-binding",
                    claim=text_rule(
                        "publicity-condition-claim",
                        all_of=["采购信息", "公开"],
                    ),
                    anchor_ids=["public-procurement-conditions"],
                ),
                EvidenceClaimRule(
                    rule_id="public-time-condition-binding",
                    claim=text_rule(
                        "public-time-condition-claim",
                        all_of=["时间", "可行"],
                    ),
                    anchor_ids=["public-procurement-conditions"],
                ),
                *method_claim_bindings(condition_anchors=True),
            ],
            min_evidence_anchors=4,
            citation_required=True,
            min_citations=4,
            max_citations=8,
            min_retrieved_sources={"documents": 1},
            max_response_chars=1000,
            max_tool_calls=PROFILES[profile_id]["reasoning_tool_budget"],
            judge_rubric=(
                "关键条件未确认时只能比较和列待确认项，不得以非依法必招、供应商数量或非应急单独推出最终采购方式。"
            ),
        ),
    )


def decision_cases() -> list[CaseSpec]:
    return [
        case_for_profile(
            profile_id=profile_id,
            case_prefix="decision-under-unknowns",
            family_id="decision-under-unknowns-procurement-path",
            capabilities=[
                Capability.RAG_RETRIEVAL,
                Capability.LONG_CONTEXT_DIALOGUE,
                Capability.CITATION,
            ],
            turns=[decision_turn(profile_id)],
            with_knowledge=True,
            tags=["decision-sufficiency", "unknowns", "no-premature-conclusion"],
            note="不要求唯一推荐答案；只门禁未知条件和禁止过早定案。",
        )
        for profile_id in PROFILES
    ]


def build_cases() -> list[CaseSpec]:
    cases = [
        *citation_cases(),
        *state_cases(),
        *alignment_cases(),
        *decision_cases(),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid curated dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "multiturn-dev.v1.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} turns "
        f"to {target} sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
