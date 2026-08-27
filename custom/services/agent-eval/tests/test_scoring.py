from __future__ import annotations

import unittest

from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseRun,
    CaseSpec,
    EvidenceAnchor,
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


if __name__ == "__main__":
    unittest.main()
