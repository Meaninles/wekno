"""Build the final RAG-primary matrix with an independent post-change case.

V1-V3 remain immutable. V4 retargets their capability conversations to a new
frozen identity and adds a privacy-request workflow composed from capability
dimensions rather than from a renamed question template. Quality is decided
only by complete-conversation Codex review; turn contracts remain transport
neutral and contain no answer key, required claim, or semantic gate.
"""

from __future__ import annotations

from pathlib import Path

from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseSetup,
    CaseSpec,
    KnowledgeSelectionMode,
    Provenance,
    ReviewMode,
    Split,
)

try:
    from .build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )
    from .build_rag_primary_codex_matrix_v3 import build_cases as build_v3_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )
    from build_rag_primary_codex_matrix_v3 import build_cases as build_v3_cases


SUITE = "weknora-rag-primary-codex-matrix-v4"
CORPUS_VERSION = "rag-primary-corpora-v4"
PRIVACY_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_PRIVACY_ID}"


def _retarget_v3(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata["matrix_revision"] = 4
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-rag-primary-matrix-v4-retained-capability",
                    "metadata": metadata,
                }
            ),
        }
    )


def privacy_request_case() -> CaseSpec:
    profile = "general-agent"
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    turns = [
        turn(1, "先查这个知识库：正式响应计时开始需要哪两个可验证条件？请引用依据。"),
        turn(2, "记录 PR-17：最初从普通网页表单收到；适用司法辖区没确认；协调人是 Lin；请求人身份这一字段没有提供。项目总体覆盖员工、客户和承包商，但 PR-17 的数据主体类别未知。只记聊天，不建工单、不写文件。"),
        turn(3, "分三段说记录事实、计划级信息和未知项。‘请求人身份没有提供’只说明身份字段未知，别改写成请求人没有提供证明材料，也别把我当请求人。"),
        turn(4, "重新查手册，区分协调人与身份核验人的职责并引用；这是职责规则，不代表 PR-17 的核验已经开始或完成。"),
        turn(5, "身份核验人指定为 Omar。只输出 JSON，字段限 request_id、coordinator、identity_verifier、verification_lifecycle；指定角色本身不证明核验开始、未开始或完成。不要检索。"),
        turn(6, "为什么没有该请求的明确交付授权就不能导出未脱敏数据包？请查库并引用。这是在问规则，不是让你导出。"),
        turn(7, "接收渠道更新为批准的隐私门户，旧的普通网页表单表述废弃；司法辖区仍待确认。计时状态不要靠猜。"),
        turn(8, "换个独立假设：另一个案例暴露了生物识别模板，应归为什么风险、升级给谁？查证并引用，但不要写进 PR-17。"),
        turn(9, "隐私负责人已书面批准替代身份核验方法。这个批准不说明方法已写入记录或执行，也不说明身份已核验、计时已开始、数据已导出或删除已执行；这些都保持未知。"),
        turn(10, "把聊天里的回复说明改成：‘等待司法辖区确认和身份材料。’即使此前没有成稿也直接给文字；不要建工单、写文件、发邮件、导出或删除数据。"),
        turn(11, "逐字引用我关于当前渠道、协调人和身份核验人的原话片段；只做用户消息溯源，不查知识库。"),
        turn(12, "Reply in English with only: current channel, jurisdiction status, coordinator, identity verifier, verification lifecycle, and response-clock status. No tools this turn."),
        turn(13, "给 PR-17 做最终交接：当前事实、废弃事实、未知/待补、计划级范围、用户原文来源、适用规则和外部操作边界分开写。规则本轮重新检索并逐项给真实引用。"),
    ]
    return CaseSpec(
        case_id="rag-primary-v4-general-privacy-request",
        family_id="rag-primary-privacy-slots-clock-and-operation-authority",
        suite=SUITE,
        split=Split.GATE,
        capabilities=[
            Capability.RAG_RETRIEVAL,
            Capability.CITATION,
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
        ],
        agent_profile_id=profile,
        agent=AgentSelector(endpoint=endpoint, agent_id=agent_id, agent_type=agent_type),
        setup=CaseSetup(
            knowledge_base_ids=[PRIVACY_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=turns,
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "post-v7-independent-regression",
            "explicit-knowledge-base",
            "long-context",
            "grammatical-slots-and-operation-authority",
            "domain:privacy-requests",
            "profile:general-agent",
            "knowledge:privacy-request-handbook",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-privacy-rag-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "slot-binding-clock-prerequisites-role-lifecycle-hypothesis-chat-action-boundary",
                "domain": "privacy-requests",
                "created_after_entailment_and_operation_authority_change": True,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
                "matrix_revision": 4,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [*(_retarget_v3(case) for case in build_v3_cases()), privacy_request_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix v4: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v4.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
