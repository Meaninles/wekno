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
