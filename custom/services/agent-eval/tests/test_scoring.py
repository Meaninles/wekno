from __future__ import annotations

import unittest

from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    ConversationStateContract,
    DecisionContract,
    DecisionMode,
    EvidenceAnchor,
    EvidenceClaimRule,
    ObservedTurn,
    Split,
    TextRule,
    ToolPolicy,
    TurnContract,
    TurnSpec,
    Verdict,
)
from weknora_eval.scoring import score_case


class ScoringTests(unittest.TestCase):
    def setUp(self) -> None:
        self.spec = CaseSpec(
            case_id="case",
            family_id="family",
            suite="suite",
            split=Split.GATE,
            capabilities=[Capability.RAG_RETRIEVAL],
            agent=AgentSelector(agent_id="agent"),
            turns=[
                TurnSpec(
                    turn_id="turn-1",
                    query="question",
                    contract=TurnContract(
                        required_claims=[
                            TextRule(rule_id="methods", all_of=["招标采购", "询比采购"])
                        ],
                        evidence_anchors=[
                            EvidenceAnchor(anchor_id="source", all_of=["招标采购", "询比采购"])
                        ],
                        min_evidence_anchors=1,
                        citation_required=True,
                        min_retrieved_sources={"documents": 1},
                    ),
                )
            ],
        )

    def test_contract_accepts_non_reference_wording(self) -> None:
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            message_id="message",
            content="制度允许询比采购，也允许招标采购。<src id=\"S1\" />",
            references=[
                {
                    "id": "chunk",
                    "evidence_content": "采购方式包括招标采购和询比采购。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk"},
                }
            ],
            retrieval_stats={"documents": 1, "total": 1},
            is_completed=True,
            total_latency_ms=100,
        )
        result = score_case(
            self.spec,
            CaseRun(
                case_id="case",
                family_id="family",
                split=Split.GATE,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_reviewed_low_risk_equivalent_wording_is_accepted(self) -> None:
        contract = TurnContract(
            required_claims=[
                TextRule(rule_id="public", any_of=["可以公开"]),
                TextRule(rule_id="identity", any_of=["未提供"]),
            ]
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="该信息可公开；用户身份未知。",
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_action_state_and_product_alias_equivalences_are_semantic(self) -> None:
        contract = TurnContract(
            required_claims=[
                TextRule(
                    rule_id="agents",
                    all_of=["快速问答", "RAG推理", "通用智能体"],
                )
            ],
            conversation_state=ConversationStateContract(
                active_facts=[
                    TextRule(rule_id="security", all_of=["数据安全", "影响评估"]),
                ],
                unknown_facts=[
                    TextRule(rule_id="approver", all_of=["审批人"], any_of=["待确认"]),
                ],
                action_boundaries=[
                    TextRule(rule_id="send", any_of=["不发送"]),
                ],
            ),
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content=(
                "快速问答、智能推理和通用智能体各有适用任务；"
                "数据安全影响已确认需要评估；审批人尚未确定；不会替您发送材料。"
            ),
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_state_scope_accepts_presentation_colon_and_containment_paraphrase(self) -> None:
        contract = TurnContract(
            conversation_state=ConversationStateContract(
                active_facts=[
                    TextRule(
                        rule_id="scope",
                        all_of=["流程", "费用分析", "不包含硬件采购"],
                    )
                ]
            )
        )
        spec = self.spec.model_copy(
            update={"turns": [self.spec.turns[0].model_copy(update={"contract": contract})]}
        )
        for answer in (
            "已确认范围：流程、费用分析；不包含：硬件采购。",
            "分析范围为流程和费用分析（不含硬件采购）。",
        ):
            with self.subTest(answer=answer):
                observed = ObservedTurn(
                    turn_id="turn-1",
                    session_id="session",
                    content=answer,
                    is_completed=True,
                )
                result = score_case(
                    spec,
                    CaseRun(
                        case_id=spec.case_id,
                        family_id=spec.family_id,
                        split=spec.split,
                        verdict=Verdict.INVALID,
                        turns=[observed],
                    ),
                )
                self.assertEqual(result.verdict, Verdict.PASS)

    def test_forbidden_examples_inside_explicit_absence_are_not_assertions(self) -> None:
        contract = TurnContract(
            forbidden_claims=[
                TextRule(
                    rule_id="no-invented-level",
                    any_of=["一级变更", "重大变更", "一般变更"],
                )
            ]
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )

        denied = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="知识库无变更等级划分（如重大变更、一般变更），运维一级变更分类也不适用。",
            is_completed=True,
        )
        denied_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[denied],
            ),
        )
        self.assertEqual(denied_result.verdict, Verdict.PASS)

        asserted = denied.model_copy(
            update={"content": "制度未规定其他级别；本项目按重大变更处理。"}
        )
        asserted_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[asserted],
            ),
        )
        self.assertEqual(asserted_result.verdict, Verdict.FAIL)

    def test_internal_planning_heuristic_catches_unlisted_leak(self) -> None:
        contract = TurnContract(
            forbidden_claims=[
                TextRule(rule_id="no-internal-planning", any_of=["Now let me"])
            ]
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="Let me carefully re-examine the output contract before answering.",
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "forbidden_claim.no-internal-planning"
                and score.passed is False
                for score in result.scores
            )
        )

    def test_citation_integrity_is_identifier_based_not_reference_order_based(self) -> None:
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content=(
                '询比采购有制度依据。<src id="S2" />'
                '招标采购亦有制度依据。<src id="S1" />'
            ),
            references=[
                {
                    "id": "chunk-one",
                    "evidence_content": "采购方式包括招标采购和询比采购。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk-one"},
                },
                {
                    "id": "chunk-two",
                    "evidence_content": "采购方式包括询比采购。",
                    "metadata": {"citation_id": "S2", "chunk_id": "chunk-two"},
                },
            ],
            retrieval_stats={"documents": 1, "total": 2},
            is_completed=True,
        )
        result = score_case(
            self.spec,
            CaseRun(
                case_id="case",
                family_id="family",
                split=Split.GATE,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_text_rules_ignore_markdown_emphasis_and_line_wrapping(self) -> None:
        contract = self.spec.turns[0].contract.model_copy(
            update={
                "required_claims": [
                    TextRule(rule_id="period", all_of=["不少于3日"])
                ],
                "evidence_anchors": [
                    EvidenceAnchor(
                        anchor_id="period-source",
                        all_of=["中标候选人", "不少于3日"],
                    )
                ],
            }
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            message_id="message",
            content='中标候选人公示期不少于 **3日**。<src id="S1" />',
            references=[
                {
                    "id": "chunk",
                    "evidence_content": "中标候选人公示期应不少于\n3日。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk"},
                }
            ],
            retrieval_stats={"documents": 1, "total": 1},
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_missing_evidence_is_hard_failure(self) -> None:
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="招标采购和询比采购。",
            retrieval_stats={"documents": 0, "total": 0},
            is_completed=True,
        )
        result = score_case(
            self.spec,
            CaseRun(
                case_id="case",
                family_id="family",
                split=Split.GATE,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.FAIL)

    def test_read_only_policy_allows_internal_todo_memory_but_blocks_writes(self) -> None:
        contract = TurnContract(tool_policy=ToolPolicy(read_only=True))
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        internal_todo = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="完成。",
            tools=["todo_write"],
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[internal_todo],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

        file_write = internal_todo.model_copy(update={"tools": ["file_write"]})
        write_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[file_write],
            ),
        )
        self.assertEqual(write_result.verdict, Verdict.FAIL)

    def test_method_ranking_is_scoped_to_its_own_answer_segment(self) -> None:
        contract = TurnContract(
            decision=DecisionContract(
                mode=DecisionMode.COMPARE_ONLY,
                forbidden_recommendations=[
                    TextRule(
                        rule_id="no-ranking-inquiry",
                        all_of=["询比"],
                        any_of=["较适配"],
                    ),
                    TextRule(
                        rule_id="no-ranking-auction",
                        all_of=["竞价"],
                        any_of=["较适配"],
                    ),
                ],
            )
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="询比：仅说明条件。\n竞价：较适配。",
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        scores = {score.name: score for score in result.scores}
        self.assertTrue(
            scores["decision.forbidden_recommendation.no-ranking-inquiry"].passed
        )
        self.assertFalse(
            scores["decision.forbidden_recommendation.no-ranking-auction"].passed
        )

    def test_lower_risk_language_is_a_method_ranking(self) -> None:
        contract = TurnContract(
            decision=DecisionContract(
                mode=DecisionMode.COMPARE_ONLY,
                forbidden_recommendations=[
                    TextRule(
                        rule_id="no-ranking-negotiation",
                        all_of=["竞争谈判"],
                        any_of=["风险最低", "风险较低"],
                    )
                ],
            )
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="竞争谈判：在当前条件下风险最低。\n\n待确认后再确定。",
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        score = next(
            item
            for item in result.scores
            if item.name == "decision.forbidden_recommendation.no-ranking-negotiation"
        )
        self.assertFalse(score.passed)

    def test_forbidden_inference_conjunction_is_scoped_to_one_claim_segment(self) -> None:
        contract = TurnContract(
            conversation_state=ConversationStateContract(
                forbidden_inferences=[
                    TextRule(
                        rule_id="no-project-heuristic",
                        all_of=["系统升级服务"],
                        any_of=["通常"],
                    )
                ]
            )
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )

        def scored(content: str) -> bool:
            result = score_case(
                spec,
                CaseRun(
                    case_id=spec.case_id,
                    family_id=spec.family_id,
                    split=spec.split,
                    verdict=Verdict.INVALID,
                    turns=[
                        ObservedTurn(
                            turn_id="turn-1",
                            session_id="session",
                            content=content,
                            is_completed=True,
                        )
                    ],
                ),
            )
            return next(
                item.passed
                for item in result.scores
                if item.name == "state.forbidden_inference.no-project-heuristic"
            )

        self.assertTrue(scored("项目是系统升级服务。\n\n制度通常要求三家供应商。"))
        self.assertFalse(
            scored(
                "竞价条件：这里先放一段超过四十八个字符的制度条件说明，"
                "用于证明禁止推断只要求同段而不要求项目名靠近段首。"
                "系统升级服务通常属于标准化货物。"
            )
        )

    def test_unknown_condition_cannot_be_transferred_or_used_as_support(self) -> None:
        contract = TurnContract(
            conversation_state=ConversationStateContract(
                forbidden_inferences=[
                    TextRule(
                        rule_id="no-public-demand-transfer",
                        all_of=["公开", "需求", "完整"],
                        any_of=["任一不满足", "不能走"],
                        unless_any_of=["不是公开采购条件", "不能据此判断"],
                    ),
                    TextRule(
                        rule_id="no-unknown-demand-support",
                        all_of=["需求", "竞争谈判"],
                        any_of=["恰符合", "因此符合"],
                        unless_any_of=["不能据此", "不足以"],
                    ),
                ]
            )
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )

        def failures(content: str) -> set[str]:
            result = score_case(
                spec,
                CaseRun(
                    case_id=spec.case_id,
                    family_id=spec.family_id,
                    split=spec.split,
                    verdict=Verdict.INVALID,
                    turns=[
                        ObservedTurn(
                            turn_id="turn-1",
                            session_id="session",
                            content=content,
                            is_completed=True,
                        )
                    ],
                ),
            )
            return {item.name for item in result.scores if item.passed is False}

        self.assertIn(
            "state.forbidden_inference.no-public-demand-transfer",
            failures(
                "公开采购：信息能否公开、需求是否完整、时间是否可行，"
                "任一不满足就不能走公开路径。"
            ),
        )
        self.assertIn(
            "state.forbidden_inference.no-unknown-demand-support",
            failures("竞争谈判：若需求尚不完整，恰符合需要讨论的情形。"),
        )
        self.assertEqual(
            failures(
                "公开采购：需求是否完整不是公开采购条件，不能据此判断。\n\n"
                "竞争谈判：需求是否完整待确认，不足以据此判断。"
            ),
            set(),
        )

    def test_forbidden_state_relationship_does_not_cross_table_rows(self) -> None:
        contract = TurnContract(
            conversation_state=ConversationStateContract(
                forbidden_inferences=[
                    TextRule(
                        rule_id="wrong-date-source",
                        all_of=["2027年1月31日"],
                        any_of=["业务团队确认"],
                    )
                ]
            )
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )

        def passed(content: str) -> bool:
            result = score_case(
                spec,
                CaseRun(
                    case_id=spec.case_id,
                    family_id=spec.family_id,
                    split=spec.split,
                    verdict=Verdict.INVALID,
                    turns=[
                        ObservedTurn(
                            turn_id="turn-1",
                            session_id="session",
                            content=content,
                            is_completed=True,
                        )
                    ],
                ),
            )
            return next(
                item.passed
                for item in result.scores
                if item.name == "state.forbidden_inference.wrong-date-source"
            )

        self.assertTrue(
            passed(
                "### 当前有效事实\n"
                "| 当前目标日期 | 2027年1月31日 |\n"
                "| 供应商数量 | 业务团队确认至少4家 |"
            )
        )
        self.assertFalse(
            passed("| 当前目标日期 | 2027年1月31日（业务团队确认） |")
        )

    def test_multi_turn_state_decision_and_claim_binding_are_hard_contracts(self) -> None:
        spec = CaseSpec(
            case_id="multi-turn",
            family_id="multi-turn-family",
            suite="suite",
            split=Split.GATE,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE, Capability.CITATION],
            agent=AgentSelector(agent_id="agent"),
            turns=[
                TurnSpec(
                    turn_id="turn-12",
                    query="audit current state",
                    contract=TurnContract(
                        conversation_state=ConversationStateContract(
                            active_facts=[
                                TextRule(
                                    rule_id="current-budget",
                                    all_of=["390万元", "300万元", "90万元"],
                                )
                            ],
                            retired_facts=[
                                TextRule(
                                    rule_id="old-budget",
                                    all_of=["360万元", "废弃"],
                                )
                            ],
                            unknown_facts=[
                                TextRule(
                                    rule_id="publicity",
                                    all_of=["采购信息", "待确认"],
                                )
                            ],
                            forbidden_inferences=[
                                TextRule(
                                    rule_id="identity",
                                    all_of=["林梅", "当前用户"],
                                )
                            ],
                            action_boundaries=[
                                TextRule(
                                    rule_id="read-only",
                                    all_of=["不得创建", "不得发起采购"],
                                )
                            ],
                        ),
                        decision=DecisionContract(
                            mode=DecisionMode.DEFER,
                            required_unknowns=[
                                TextRule(
                                    rule_id="publicity",
                                    all_of=["采购信息", "待确认"],
                                )
                            ],
                            required_defer_claims=[
                                TextRule(
                                    rule_id="defer",
                                    any_of=["暂不定案", "暂不能确定"],
                                )
                            ],
                            forbidden_recommendations=[
                                TextRule(
                                    rule_id="no-premature-negotiation",
                                    any_of=["首选竞争谈判", "建议采用竞争谈判"],
                                )
                            ],
                        ),
                        evidence_anchors=[
                            EvidenceAnchor(
                                anchor_id="auction-definition",
                                all_of=["征集3家以上供应商", "多次竞争报价"],
                            )
                        ],
                        evidence_claims=[
                            EvidenceClaimRule(
                                rule_id="auction-definition",
                                claim=TextRule(
                                    rule_id="auction-claim",
                                    all_of=["征集3家以上供应商", "多次竞争报价"],
                                ),
                                anchor_ids=["auction-definition"],
                            )
                        ],
                        citation_required=True,
                        min_evidence_anchors=1,
                        max_tool_calls=2,
                    ),
                )
            ],
        )
        observed = ObservedTurn(
            turn_id="turn-12",
            session_id="session",
            content=(
                "当前预算390万元，其中设备300万元、服务90万元。\n"
                "360万元旧预算已经废弃。\n"
                "采购信息是否公开仍待确认，因此暂不定案。\n"
                "行动边界：不得创建文件，也不得发起采购。\n"
                "竞价采购要求征集3家以上供应商并进行多次竞争报价。<src id=\"S1\" />"
            ),
            references=[
                {
                    "id": "chunk-auction",
                    "evidence_content": "征集3家以上供应商，对供应商多次竞争报价进行比较。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk-auction"},
                }
            ],
            retrieval_stats={"documents": 1, "total": 1},
            tools=["knowledge_search"],
            agent_tool_count=1,
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)
        self.assertTrue(
            any(
                score.name == "evidence_claim.auction-definition" and score.passed
                for score in result.scores
            )
        )

    def test_evidence_claim_requires_the_matching_citation_on_the_claim_segment(self) -> None:
        contract = self.spec.turns[0].contract.model_copy(
            update={
                "evidence_claims": [
                    EvidenceClaimRule(
                        rule_id="methods",
                        claim=TextRule(
                            rule_id="methods-claim",
                            all_of=["招标采购", "询比采购"],
                        ),
                        anchor_ids=["source"],
                    )
                ]
            }
        )
        spec = self.spec.model_copy(
            update={
                "turns": [
                    self.spec.turns[0].model_copy(update={"contract": contract})
                ]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="制度允许招标采购和询比采购。\n补充说明。<src id=\"S1\" />",
            references=[
                {
                    "id": "chunk",
                    "evidence_content": "采购方式包括招标采购和询比采购。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk"},
                }
            ],
            retrieval_stats={"documents": 1, "total": 1},
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "evidence_claim.methods" and score.passed is False
                for score in result.scores
            )
        )

    def test_evidence_claim_accepts_citation_inside_the_same_markdown_section(self) -> None:
        contract = self.spec.turns[0].contract.model_copy(
            update={
                "evidence_claims": [
                    EvidenceClaimRule(
                        rule_id="methods",
                        claim=TextRule(
                            rule_id="methods-claim",
                            all_of=["招标采购", "询比采购"],
                        ),
                        anchor_ids=["source"],
                    )
                ]
            }
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content=(
                "**招标采购和询比采购**\n"
                "制度允许使用这两种方式。<src id=\"S1\" />"
            ),
            references=[
                {
                    "id": "chunk",
                    "evidence_content": "采购方式包括招标采购和询比采购。",
                    "metadata": {"citation_id": "S1", "chunk_id": "chunk"},
                }
            ],
            retrieval_stats={"documents": 1, "total": 1},
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.PASS)

    def test_optional_following_evidence_claim_is_conditional_and_positional(self) -> None:
        anchor = EvidenceAnchor(
            anchor_id="tail",
            all_of=["适用于谈判方式的其他采购"],
            required=False,
        )
        claim = EvidenceClaimRule(
            rule_id="tail-binding",
            claim=TextRule(
                rule_id="tail-claim",
                all_of=["适用于谈判方式的其他采购"],
            ),
            anchor_ids=["tail"],
            require_following_citation=True,
            required=False,
        )
        contract = TurnContract(
            evidence_anchors=[anchor],
            evidence_claims=[claim],
        )
        spec = self.spec.model_copy(
            update={
                "turns": [self.spec.turns[0].model_copy(update={"contract": contract})]
            }
        )

        absent = ObservedTurn(
            turn_id="turn-1",
            session_id="session-absent",
            content="竞争谈判的其他直接条件见制度片段。",
            references=[],
            retrieval_stats={"documents": 0, "total": 0},
            is_completed=True,
        )
        absent_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[absent],
            ),
        )
        self.assertEqual(absent_result.verdict, Verdict.PASS)

        references = [
            {
                "id": "tail",
                "evidence_content": "7.适用于谈判方式的其他采购。",
                "metadata": {"citation_id": "S1", "chunk_id": "tail"},
            },
            {
                "id": "head",
                "evidence_content": "竞争谈判适用于技术复杂项目。",
                "metadata": {"citation_id": "S2", "chunk_id": "head"},
            },
        ]
        wrong = ObservedTurn(
            turn_id="turn-1",
            session_id="session-wrong",
            content=(
                "前段条件。<src id=\"S1\" />；"
                "适用于谈判方式的其他采购。<src id=\"S2\" />"
            ),
            references=references,
            retrieval_stats={"documents": 2, "total": 2},
            is_completed=True,
        )
        wrong_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[wrong],
            ),
        )
        self.assertEqual(wrong_result.verdict, Verdict.FAIL)

        correct = wrong.model_copy(
            update={
                "session_id": "session-correct",
                "content": "适用于谈判方式的其他采购。<src id=\"S1\" />",
                "references": references[:1],
            }
        )
        correct_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[correct],
            ),
        )
        self.assertEqual(correct_result.verdict, Verdict.PASS)

    def test_scoped_state_sections_reject_a_fact_in_the_wrong_lifecycle_column(self) -> None:
        spec = CaseSpec(
            case_id="scoped-state",
            family_id="scoped-state-family",
            suite="suite",
            split=Split.GATE,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            turns=[
                TurnSpec(
                    turn_id="turn-1",
                    query="audit",
                    contract=TurnContract(
                        conversation_state=ConversationStateContract(
                            active_facts=[
                                TextRule(rule_id="current", all_of=["390万元"])
                            ],
                            retired_facts=[
                                TextRule(rule_id="retired", all_of=["360万元"])
                            ],
                            unknown_facts=[
                                TextRule(rule_id="unknown", all_of=["采购信息"])
                            ],
                            forbidden_unknown_facts=[
                                TextRule(
                                    rule_id="resolved-not-unknown",
                                    all_of=["A主张", "未经核验"],
                                )
                            ],
                            action_boundaries=[
                                TextRule(rule_id="boundary", all_of=["不得创建"])
                            ],
                            require_scoped_sections=True,
                        )
                    ),
                )
            ],
        )
        observed = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content=(
                "### 当前有效事实\n360万元\n"
                "### 已废弃事实\n390万元\n"
                "### 待确认事实\n采购信息\n"
                "### 行动边界\n不得创建"
            ),
            is_completed=True,
        )
        result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[observed],
            ),
        )
        self.assertEqual(result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "state.active.current" and score.passed is False
                for score in result.scores
            )
        )

        stale_unknown = observed.model_copy(
            update={
                "content": (
                    "### 当前有效事实\n390万元\n"
                    "### 已废弃事实\n360万元；A主张已被推翻\n"
                    "### 待确认事实\n采购信息；A主张未经核验\n"
                    "### 行动边界\n不得创建"
                )
            }
        )
        stale_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[stale_unknown],
            ),
        )
        self.assertEqual(stale_result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "state.forbidden_unknown.resolved-not-unknown"
                and score.passed is False
                for score in stale_result.scores
            )
        )

        correct_table = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content=(
                "| 当前有效事实 | 已废弃事实 | 待确认事实 | 行动边界 |\n"
                "| --- | --- | --- | --- |\n"
                "| 390万元 | 360万元 | 采购信息 | 不得创建 |"
            ),
            is_completed=True,
        )
        correct_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[correct_table],
            ),
        )
        self.assertEqual(correct_result.verdict, Verdict.PASS)

        compact_headings = correct_table.model_copy(
            update={
                "content": (
                    "### 已确认\n390万元\n"
                    "### 已废弃事实\n360万元\n"
                    "### 待确认\n采购信息\n"
                    "### 行动边界\n不得创建"
                )
            }
        )
        compact_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[compact_headings],
            ),
        )
        self.assertEqual(compact_result.verdict, Verdict.PASS)

        headed_tables = correct_table.model_copy(
            update={
                "content": (
                    "### 当前有效事实\n| 事项 | 内容 |\n| --- | --- |\n| 预算 | 390万元 |\n"
                    "### 已废弃事实\n| 事项 | 废弃原因 | 原内容 |\n| --- | --- | --- |\n"
                    "| 初始预算 | 财务调整后废弃 | 360万元 |\n"
                    "### 待确认事实\n| 事项 | 状态 |\n| --- | --- |\n| 采购信息 | 待确认 |\n"
                    "### 行动边界\n| 边界 | 内容 |\n| --- | --- |\n| 文件 | 不得创建 |"
                )
            }
        )
        headed_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[headed_tables],
            ),
        )
        self.assertEqual(headed_result.verdict, Verdict.PASS)

        duplicated_retired = headed_tables.model_copy(
            update={
                "content": (
                    "### 当前有效事实\n390万元；360万元（已废弃）\n"
                    "### 已废弃事实\n360万元（已废弃）\n"
                    "### 待确认事实\n采购信息\n"
                    "### 行动边界\n不得创建"
                )
            }
        )
        duplicated_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[duplicated_retired],
            ),
        )
        self.assertEqual(duplicated_result.verdict, Verdict.PASS)

        resurrected_retired = headed_tables.model_copy(
            update={
                "content": (
                    "### 当前有效事实\n390万元；360万元\n"
                    "### 已废弃事实\n360万元（已废弃）\n"
                    "### 待确认事实\n采购信息\n"
                    "### 行动边界\n不得创建"
                )
            }
        )
        resurrected_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[resurrected_retired],
            ),
        )
        self.assertEqual(resurrected_result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "state.lifecycle.retired-not-active.retired"
                and score.passed is False
                for score in resurrected_result.scores
            )
        )

        duplicated_boundary = headed_tables.model_copy(
            update={
                "content": (
                    "### 当前有效事实\n390万元；维护方式为不得创建文件\n"
                    "### 已废弃事实\n360万元\n"
                    "### 待确认事实\n采购信息\n"
                    "### 行动边界\n不得创建文件"
                )
            }
        )
        boundary_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[duplicated_boundary],
            ),
        )
        self.assertEqual(boundary_result.verdict, Verdict.FAIL)
        self.assertTrue(
            any(
                score.name == "state.lifecycle.action_boundary-not-active.boundary"
                and score.passed is False
                for score in boundary_result.scores
            )
        )

    def test_forbidden_recommendation_rule_ignores_an_explicit_deferral(self) -> None:
        spec = CaseSpec(
            case_id="recommendation-negation",
            family_id="recommendation-negation-family",
            suite="suite",
            split=Split.GATE,
            capabilities=[Capability.LONG_CONTEXT_DIALOGUE],
            agent=AgentSelector(agent_id="agent"),
            turns=[
                TurnSpec(
                    turn_id="turn-1",
                    query="do not decide",
                    contract=TurnContract(
                        forbidden_claims=[
                            TextRule(
                                rule_id="no-auction",
                                any_of=["建议采用竞价"],
                                unless_any_of=["暂不建议采用竞价", "不建议采用竞价"],
                            )
                        ]
                    ),
                )
            ],
        )

        deferral = ObservedTurn(
            turn_id="turn-1",
            session_id="session",
            content="关键条件未知，暂不建议采用竞价。",
            is_completed=True,
        )
        deferral_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[deferral],
            ),
        )
        self.assertEqual(deferral_result.verdict, Verdict.PASS)

        recommendation = deferral.model_copy(
            update={"content": "关键条件已经满足，建议采用竞价。"}
        )
        recommendation_result = score_case(
            spec,
            CaseRun(
                case_id=spec.case_id,
                family_id=spec.family_id,
                split=spec.split,
                verdict=Verdict.INVALID,
                turns=[recommendation],
            ),
        )
        self.assertEqual(recommendation_result.verdict, Verdict.FAIL)


if __name__ == "__main__":
    unittest.main()
