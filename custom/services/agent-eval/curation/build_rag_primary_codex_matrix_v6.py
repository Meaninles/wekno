"""Build the post-prompt V6 RAG matrix without semantic answer keys.

V6 retains the frozen V5 capability cases and adds one new, independently
authored observatory conversation for each production agent. The new corpus and
conversation were created after the generic prompt/retrieval changes and are
used as a one-shot regression, not as a source of query-text routing rules.
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
    from .build_rag_primary_codex_matrix_v5 import build_cases as build_v5_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import MODEL_ID, PROFILE_RUNTIME, neutral_contract, turn
    from build_rag_primary_codex_matrix_v5 import build_cases as build_v5_cases


SUITE = "weknora-rag-primary-codex-matrix-v6"
CORPUS_VERSION = "rag-primary-corpora-v6"
OBSERVATORY_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_OBSERVATORY_ID}"


def _retarget_v5(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata["matrix_revision"] = 6
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-rag-primary-matrix-v6-retained-capability",
                    "metadata": metadata,
                }
            ),
        }
    )


def observatory_turns():
    return [
        turn(1, "查当前知识库：原始科学帧和快速预览图分别保留多久？请引用依据。"),
        turn(2, "记录观测会话Q-731：目标是Nebula-X，仪器操作员是Rowan；复核人未知，穹顶状态也未知。先在聊天里整理。"),
        turn(3, "方案里只是考虑使用L9滤镜，这还不是已选定的配置，也不是执行记录。保持这个模态。"),
        turn(4, "重新查指南：什么组合构成关键穹顶天气条件，满足时要通知哪些角色？逐项引用。"),
        turn(5, "Q-731当前湿度85%，持续风速没有提供。只更新会话状态，不要把临界条件、关穹顶或未关穹顶推出来。"),
        turn(6, "目标改为Nebula-Y，Nebula-X作废；其他字段不变。按当前与废弃事实分开。"),
        turn(7, "把当前会话表示成JSON直接回复，字段仅session_id、target、operator、reviewer、dome_status；这只是聊天内容，不创建下载文件。"),
        turn(8, "查指南说明L9滤镜的温度限制并给真实引用；不要把方案里的‘考虑使用’改成已选择。"),
        turn(9, "Reviewer is now explicitly pending assignment, while dome status remains unknown. Reply in English and keep those two classes distinct."),
        turn(10, "假设某仪器被标记隔离，分析后续电源操作边界，查指南引用；这是规则讨论，不是Q-731已发生的事件，也不要执行命令。"),
        turn(11, "只从我的消息追溯：最初目标、当前目标、操作员和复核人当前类别。不要查知识库。"),
        turn(12, "补充Q-731：门联锁明确显示CLOSED，并记录到匹配的控制器事件EV-7。根据指南判断关穹顶是否已有确认依据，带引用。"),
        turn(13, "最终汇总Q-731：当前事实、废弃事实、未知项、明确待定项、用户消息来源、适用指南规则和外部操作边界分开写；指南事实本轮重新检索并引用，只在聊天中回答。"),
    ]


def observatory_case(profile: str) -> CaseSpec:
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    return CaseSpec(
        case_id=f"rag-primary-v6-{profile}-observatory-regression",
        family_id=f"rag-primary-post-prompt-observatory-{profile}",
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
            knowledge_base_ids=[OBSERVATORY_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=observatory_turns(),
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "post-prompt-one-shot-regression",
            "explicit-knowledge-base",
            "long-context",
            "cross-profile-comparison",
            "domain:observatory-operations",
            f"profile:{profile}",
            "knowledge:observatory-night-operations-guide",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=1,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-post-prompt-observatory-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "one-shot-cross-profile-rag-state-source-and-operation-regression",
                "domain": "observatory-operations",
                "created_after_generic_prompt_revision": True,
                "used_for_subsequent_tuning": False,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
                "matrix_revision": 6,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [
        *(_retarget_v5(case) for case in build_v5_cases()),
        *(observatory_case(profile) for profile in PROFILE_RUNTIME),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix v6: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v6.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
