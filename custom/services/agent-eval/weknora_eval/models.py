from __future__ import annotations

from datetime import datetime, timezone
from enum import Enum
from typing import Any, Literal

from pydantic import BaseModel, ConfigDict, Field, model_validator


def utc_now() -> str:
    return datetime.now(timezone.utc).isoformat()


class StrictModel(BaseModel):
    model_config = ConfigDict(extra="forbid")


class Capability(str, Enum):
    RAG_RETRIEVAL = "rag_retrieval"
    DOCUMENT_PROCESSING = "document_processing"
    LONG_CONTEXT_DIALOGUE = "long_context_dialogue"
    TOOL_USE = "tool_use"
    CITATION = "citation"


class Split(str, Enum):
    DEV = "dev"
    GATE = "gate"
    SEALED_HOLDOUT = "sealed_holdout"
    GRADER_CALIBRATION = "grader_calibration"
    QUARANTINE = "quarantine"


class Verdict(str, Enum):
    PASS = "PASS"
    FAIL = "FAIL"
    INVALID = "INVALID"


class AgentSelector(StrictModel):
    endpoint: Literal["knowledge-chat", "agent-chat"] = "agent-chat"
    agent_id: str
    agent_type: str | None = None


class CaseSetup(StrictModel):
    knowledge_base_ids: list[str] = Field(default_factory=list)
    knowledge_ids: list[str] = Field(default_factory=list)
    web_search_enabled: bool = False
    summary_model_id: str | None = None
    channel: str = "agent-eval"


class TextRule(StrictModel):
    rule_id: str
    description: str = ""
    any_of: list[str] = Field(default_factory=list)
    all_of: list[str] = Field(default_factory=list)
    unless_any_of: list[str] = Field(default_factory=list)
    case_sensitive: bool = False

    @model_validator(mode="after")
    def require_pattern(self) -> "TextRule":
        if not self.any_of and not self.all_of:
            raise ValueError("text rule requires any_of or all_of")
        return self


class ConversationStateContract(StrictModel):
    """Deterministic multi-turn state assertions without a single gold answer."""

    active_facts: list[TextRule] = Field(default_factory=list)
    retired_facts: list[TextRule] = Field(default_factory=list)
    unknown_facts: list[TextRule] = Field(default_factory=list)
    forbidden_inferences: list[TextRule] = Field(default_factory=list)
    action_boundaries: list[TextRule] = Field(default_factory=list)
    require_scoped_sections: bool = False

    @model_validator(mode="after")
    def require_unique_rule_ids(self) -> "ConversationStateContract":
        rules = [
            *self.active_facts,
            *self.retired_facts,
            *self.unknown_facts,
            *self.forbidden_inferences,
            *self.action_boundaries,
        ]
        rule_ids = [rule.rule_id for rule in rules]
        if len(rule_ids) != len(set(rule_ids)):
            raise ValueError("conversation state rule_id must be unique")
        return self


class DecisionMode(str, Enum):
    DEFER = "defer"
    COMPARE_ONLY = "compare_only"
    FINAL_ALLOWED = "final_allowed"


class DecisionContract(StrictModel):
    """Controls whether a turn may advance from analysis to a final decision."""

    mode: DecisionMode
    required_unknowns: list[TextRule] = Field(default_factory=list)
    required_defer_claims: list[TextRule] = Field(default_factory=list)
    forbidden_recommendations: list[TextRule] = Field(default_factory=list)

    @model_validator(mode="after")
    def require_defer_signal(self) -> "DecisionContract":
        if (
            self.mode in {DecisionMode.DEFER, DecisionMode.COMPARE_ONLY}
            and not self.required_defer_claims
            and not self.forbidden_recommendations
        ):
            raise ValueError(
                "defer/compare_only decision contract requires a deterministic defer or forbidden recommendation rule"
            )
        return self


class EvidenceAnchor(StrictModel):
    anchor_id: str
    description: str = ""
    any_of: list[str] = Field(default_factory=list)
    all_of: list[str] = Field(default_factory=list)
    source_ids: list[str] = Field(default_factory=list)
    min_matching_fragments: int = Field(default=1, ge=1)

    @model_validator(mode="after")
    def require_selector(self) -> "EvidenceAnchor":
        if not self.any_of and not self.all_of and not self.source_ids:
            raise ValueError("evidence anchor requires text or source selectors")
        return self


class EvidenceClaimRule(StrictModel):
    """Binds one answer claim to one or more acceptable evidence anchors."""

    rule_id: str
    description: str = ""
    claim: TextRule
    anchor_ids: list[str]
    require_adjacent_citation: bool = True

    @model_validator(mode="after")
    def require_anchor(self) -> "EvidenceClaimRule":
        if not self.anchor_ids:
            raise ValueError("evidence claim requires at least one anchor_id")
        return self


class ToolPolicy(StrictModel):
    read_only: bool = True
    required_tools: list[str] = Field(default_factory=list)
    allowed_tools: list[str] = Field(default_factory=list)
    forbidden_tools: list[str] = Field(default_factory=list)


class TurnContract(StrictModel):
    required_claims: list[TextRule] = Field(default_factory=list)
    forbidden_claims: list[TextRule] = Field(default_factory=list)
    conversation_state: ConversationStateContract = Field(
        default_factory=ConversationStateContract
    )
    decision: DecisionContract | None = None
    evidence_anchors: list[EvidenceAnchor] = Field(default_factory=list)
    evidence_claims: list[EvidenceClaimRule] = Field(default_factory=list)
    min_evidence_anchors: int = Field(default=0, ge=0)
    citation_required: bool = False
    min_citations: int = Field(default=0, ge=0)
    max_citations: int | None = Field(default=None, ge=0)
    min_retrieved_sources: dict[str, int] = Field(default_factory=dict)
    tool_policy: ToolPolicy = Field(default_factory=ToolPolicy)
    min_response_chars: int = Field(default=1, ge=0)
    max_response_chars: int | None = Field(default=None, ge=1)
    max_tool_calls: int | None = Field(default=None, ge=0)
    max_total_latency_ms: int | None = Field(default=None, ge=1)
    forbid_stale_citations_without_retrieval: bool = False
    judge_rubric: str | None = None

    @model_validator(mode="after")
    def validate_anchor_floor(self) -> "TurnContract":
        if self.min_evidence_anchors > len(self.evidence_anchors):
            raise ValueError("min_evidence_anchors exceeds available anchors")
        anchor_ids = [anchor.anchor_id for anchor in self.evidence_anchors]
        if len(anchor_ids) != len(set(anchor_ids)):
            raise ValueError("evidence anchor_id must be unique inside a turn")
        known_anchor_ids = set(anchor_ids)
        evidence_claim_ids = [claim.rule_id for claim in self.evidence_claims]
        if len(evidence_claim_ids) != len(set(evidence_claim_ids)):
            raise ValueError("evidence claim rule_id must be unique inside a turn")
        for claim in self.evidence_claims:
            unknown = sorted(set(claim.anchor_ids) - known_anchor_ids)
            if unknown:
                raise ValueError(
                    f"evidence claim {claim.rule_id} references unknown anchors: {unknown}"
                )
        if self.citation_required and self.min_citations == 0:
            self.min_citations = 1
        return self


class TurnSpec(StrictModel):
    turn_id: str
    query: str
    setup_override: CaseSetup | None = None
    contract: TurnContract


class Provenance(StrictModel):
    source: str = "curated"
    source_hash: str | None = None
    collected_at: str | None = None
    needs_codex_review: bool = False
    reference_answers: dict[str, str] = Field(default_factory=dict)
    reference_evidence: list[dict[str, Any]] = Field(default_factory=list)
    metadata: dict[str, Any] = Field(default_factory=dict)


class CaseSpec(StrictModel):
    schema_version: Literal[1] = 1
    case_id: str
    family_id: str
    suite: str
    split: Split
    enabled: bool = True
    capabilities: list[Capability]
    agent_profile_id: str | None = None
    agent: AgentSelector
    setup: CaseSetup = Field(default_factory=CaseSetup)
    turns: list[TurnSpec]
    tags: list[str] = Field(default_factory=list)
    corpus_version: str | None = None
    repetitions: int = Field(default=1, ge=1)
    provenance: Provenance = Field(default_factory=Provenance)

    @model_validator(mode="after")
    def validate_case(self) -> "CaseSpec":
        if not self.turns:
            raise ValueError("case requires at least one turn")
        turn_ids = [turn.turn_id for turn in self.turns]
        if len(turn_ids) != len(set(turn_ids)):
            raise ValueError("turn_id must be unique inside a case")
        if self.split != Split.QUARANTINE and self.provenance.needs_codex_review:
            raise ValueError("unreviewed cases must remain in quarantine")
        return self


class ObservedTurn(StrictModel):
    turn_id: str
    session_id: str
    message_id: str = ""
    content: str = ""
    references: list[dict[str, Any]] = Field(default_factory=list)
    retrieval_stats: dict[str, Any] = Field(default_factory=dict)
    tools: list[str] = Field(default_factory=list)
    agent_steps: list[dict[str, Any]] = Field(default_factory=list)
    agent_mode: bool = False
    agent_tool_count: int = 0
    is_completed: bool = False
    ttfb_ms: int = 0
    total_latency_ms: int = 0
    event_count: int = 0
    error: str | None = None


class MetricScore(StrictModel):
    name: str
    value: float | bool | str
    passed: bool | None = None
    hard: bool = True
    comment: str = ""
    turn_id: str | None = None
    metadata: dict[str, Any] = Field(default_factory=dict)


class CaseRun(StrictModel):
    case_id: str
    family_id: str
    split: Split
    agent_profile_id: str | None = None
    attempt_index: int = Field(default=1, ge=1)
    verdict: Verdict
    turns: list[ObservedTurn] = Field(default_factory=list)
    scores: list[MetricScore] = Field(default_factory=list)
    error: str | None = None


class SUTFingerprint(StrictModel):
    mode: str
    release: str = ""
    commit: str = ""
    environment: str = ""
    capabilities: list[str] = Field(default_factory=list)
    raw: dict[str, Any] = Field(default_factory=dict)


class ExperimentRun(StrictModel):
    schema_version: Literal[1] = 1
    run_id: str
    suite: str
    dataset_sha256: str
    created_at: str = Field(default_factory=utc_now)
    splits: list[Split]
    sut: SUTFingerprint
    cases: list[CaseRun]
    metadata: dict[str, Any] = Field(default_factory=dict)


class GatePolicy(StrictModel):
    schema_version: Literal[1] = 1
    policy_id: str
    required_splits: list[Split] = Field(default_factory=lambda: [Split.GATE])
    required_capabilities: list[Capability] = Field(default_factory=list)
    required_agent_profiles: list[str] = Field(default_factory=list)
    min_repetitions_per_case: int = Field(default=1, ge=1)
    min_case_coverage: float = Field(default=1.0, ge=0, le=1)
    max_invalid_rate: float = Field(default=0.0, ge=0, le=1)
    forbid_hard_failures: bool = True
    forbid_pass_to_fail_regressions: bool = True
    max_metric_rate_regression: dict[str, float] = Field(default_factory=dict)
    required_execution_identity_fields: list[str] = Field(default_factory=list)
    max_p95_latency_regression_ratio: float = Field(default=0.15, ge=0)
    max_p95_latency_regression_ms: int = Field(default=500, ge=0)
    require_baseline: bool = True


class GateCheck(StrictModel):
    name: str
    verdict: Verdict
    comment: str
    details: dict[str, Any] = Field(default_factory=dict)


class GateResult(StrictModel):
    schema_version: Literal[1] = 1
    policy_id: str
    candidate_run_id: str
    baseline_run_id: str | None = None
    verdict: Verdict
    checks: list[GateCheck]
    created_at: str = Field(default_factory=utc_now)


class FrozenDatasetManifest(StrictModel):
    schema_version: Literal[1] = 1
    suite: str
    dataset_sha256: str
    case_count: int
    family_count: int
    split_counts: dict[str, int]
    source_files: list[str]
    dependency_sha256: dict[str, str] = Field(default_factory=dict)
    created_at: str = Field(default_factory=utc_now)
