"""Build the unseen post-change RAG synthesis regression.

This case was authored only after the terminal-task salience change. It uses a
new corpus and a capability-driven conversation sequence rather than copying a
prior case and replacing domain nouns. Semantic quality is intentionally left
to complete-conversation Codex review; the SUT receives no answer key, required
claim, reference answer, Judge feedback, or Eval repair instruction.
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
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )


SUITE = "weknora-post-terminal-synthesis-regression-v1"
CORPUS_VERSION = "orion-quality-batch-release-v1"
KB_ID = "${AGENT_EVAL_KB_POST_TERMINAL_QUALITY_ID}"


def build_cases() -> list[CaseSpec]:
    profile = "general-agent"
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    turns = [
        turn(
            1,
            "我们先在聊天里整理批次 Q-88：产品代码 N7，包装线最初记作 Line-B；目标市场还没确认。质量项目总体覆盖欧盟和加拿大，但这不是 Q-88 的目标市场。不要写 MES、不要建质量工单，也不要创建文件。",
        ),
        turn(
            2,
            "用两段简短说明：第一段只写 Q-88 已知信息，第二段写未知项。别把我当操作员，也别把项目市场范围塞进批次字段；这轮不要查资料。",
        ),
        turn(
            3,
            "查当前手册，解释取样协调人和放行批准人的职责差别并引用。这里只问职责，不能顺手推断 Q-88 的取样或实验室复核进度。",
        ),
        turn(
            4,
            "Q-88 的取样协调人定为 Vera，放行批准人还未指定。这个角色更新没有说明取样开始、没开始或完成，生命周期继续未知。",
        ),
        turn(
            5,
            "分析包装校样被接受为什么仍不能直接说批次已经放行；按手册给依据。这是解释规则，不是让你执行放行。",
        ),
        turn(
            6,
            "包装线更正为 Line-D，Line-B 作废；目标市场确认是欧盟。请只复述这次字段变化，不检索，也不要把项目里的加拿大带进来。",
        ),
        turn(
            7,
            "现在最终分析证书已经收到，但指定放行批准人的决定没有提供。保留放行状态未知，不要从缺一项推成已放行、未放行、拒绝或报废。",
        ),
        turn(
            8,
            "临时讨论另一个批次：如果冷链温度超出允许范围，手册要求如何定性、升级给谁？查证并引用，但不要把这个假设记进 Q-88。",
        ),
        turn(
            9,
            "质量负责人书面批准了 Q-88 的替代取样方案。批准只说明方案获批；是否写入记录、是否执行、是否采样、实验室复核是否开始、是否放行，全都没有新证据。",
        ),
        turn(
            10,
            "把聊天里的交接备注改成‘等待放行批准人决定’。直接给我这句文字；仍然不要碰 MES、工单、文件或邮件。",
        ),
        turn(
            11,
            "口语回答：我说‘没见到放行事件’，这只能让实际放行状态保持不确定，不能自动写成‘肯定还没放’，对吧？不要检索。",
        ),
        turn(
            12,
            "Reply in English with exactly five bullets: batch ID, current packaging line, destination market, sampling coordinator, and release status. No tools this turn.",
        ),
        turn(
            13,
            "回到中文，给 Q-88 做完整交接。请分别写当前批次事实、已废弃值、仍未知或待补事项、项目级信息、另一个批次的假设、聊天内备注、用户原文来源和外部操作边界；再重新检索手册，说明当前证据下的放行判断、角色与生命周期边界以及校样与放行的区别，并把真实引用放在对应规则后。不要只回答某一条制度。",
        ),
    ]
    case = CaseSpec(
        case_id="post-terminal-v1-general-quality-batch-release",
        family_id="post-terminal-current-task-rag-synthesis",
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
            knowledge_base_ids=[KB_ID],
            knowledge_ids=[],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-post-terminal-synthesis",
        ),
        turns=turns,
        tags=[
            "post-terminal-synthesis-regression",
            "codex-complete-conversation-review",
            "explicit-independent-knowledge-base",
            "long-context",
            "current-task-salience",
            "mixed-dialogue-and-rag-synthesis",
            "domain:manufacturing-quality",
            "profile:general-agent",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-post-terminal-synthesis-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "current-task-salience-mixed-rag-state-operation-authority",
                "domain": "manufacturing-quality",
                "created_after_terminal_task_salience_change": True,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit-independent",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
            },
        ),
    )
    cases = [case]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid post-terminal synthesis regression: " + "; ".join(errors))
    for current in case.turns:
        if current.contract != neutral_contract():
            raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "post-terminal-synthesis-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} case to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
