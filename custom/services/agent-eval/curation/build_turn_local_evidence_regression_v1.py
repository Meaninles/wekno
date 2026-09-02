"""Build a focused post-change regression for turn-local RAG evidence use.

This case was authored after the turn-local evidence change. It alternates
document questions with dialogue-only updates so the model, rather than query
keywords or the Eval runner, must decide when fresh retrieval is required.
The turn contracts contain no answer key, required claims, or phrase scoring.
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
    from .build_rag_primary_codex_matrix_v1 import MODEL_ID, PROFILE_RUNTIME, neutral_contract, turn
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import MODEL_ID, PROFILE_RUNTIME, neutral_contract, turn


SUITE = "weknora-turn-local-evidence-regression-v1"
CORPUS_VERSION = "northbank-audio-release-v1"
MEDIA_KB_ID = "${AGENT_EVAL_KB_FRESH_GENERALIZATION_MEDIA_ID}"


def build_case() -> CaseSpec:
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME["rag-reasoning"]
    turns = [
        turn(1, "从当前知识库说明拼接稿、审听稿、可发布版本之间是什么关系，并给出处。"),
        turn(2, "记录栏目代号Harbor-9：现在是拼接稿，制作负责人是Lian；计划发布窗口和权利状态都未知。先只整理我给的事实。"),
        turn(3, "我准备把说明文字写成‘可发布’，但还没有改音频。查手册判断这会不会改变音频状态或更新发布系统，带引用。"),
        turn(4, "更正项目状态：负责人改为Mara，Lian作废；音频仍是拼接稿。按当前与废弃事实分开，不查资料。"),
        turn(5, "重新读取手册：一次交付交接至少应记录哪些信息？不要拿Harbor-9的未知字段补成已知。"),
        turn(6, "补充用户事实：发布窗口明确待排期；权利状态仍然未知。请区分明确待定和未知。"),
        turn(7, "What does the handbook require for machine transcripts, name checking, and accessibility review? Answer in English with real citations."),
        turn(8, "只从我的消息追溯当前音频状态、当前负责人、废弃负责人、发布窗口类别和权利状态类别；本轮不要检索。"),
        turn(9, "假设有人只是在聊天里起草公告，查手册解释这是否等于上传音频、发送公告、更新任务板或执行发布；这是规则问题，不是Harbor-9已发生的动作。"),
        turn(10, "把Harbor-9当前状态写成一小段英文，只含代号、版本、负责人、发布窗口类别和权利状态类别。"),
        turn(11, "再查手册：新签署授权替代旧授权时，能不能据此认定旧文件已删除？请引用，不要把假设写进项目状态。"),
        turn(12, "最终汇总Harbor-9的当前事实、废弃事实、未知项、明确待定项和用户来源；适用手册规则本轮重新检索并引用，外部动作只说明证据边界。"),
    ]
    return CaseSpec(
        case_id="turn-local-v1-rag-audio-delivery",
        family_id="turn-local-evidence-alternating-audio-delivery",
        suite=SUITE,
        split=Split.GATE,
        capabilities=[
            Capability.RAG_RETRIEVAL,
            Capability.CITATION,
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
        ],
        agent_profile_id="rag-reasoning",
        agent=AgentSelector(endpoint=endpoint, agent_id=agent_id, agent_type=agent_type),
        setup=CaseSetup(
            knowledge_base_ids=[MEDIA_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-turn-local-evidence-regression",
        ),
        turns=turns,
        tags=[
            "post-change-new-case",
            "explicit-knowledge-base",
            "alternating-retrieval-and-dialogue",
            "long-context",
            "domain:audio-publishing",
            "profile:rag-reasoning",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-turn-local-evidence-regression-20260903",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "alternate evidence questions and user-state turns without lexical routing",
                "created_after_turn_local_evidence_change": True,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-whole-conversation-material-error-only",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [build_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid turn-local evidence regression: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "turn-local-evidence-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
