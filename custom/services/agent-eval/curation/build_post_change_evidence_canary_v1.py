"""Build a one-time capability canary created after evidence-query isolation.

The case is structurally shorter than the formal 13-turn matrix and uses a new
laboratory corpus. Contracts retain only observable execution/citation checks;
Codex reviews semantic quality from the complete conversation.
"""

from __future__ import annotations

from pathlib import Path

from curation.build_unseen_capability_matrix_v1 import (
    MODEL_ID,
    PROFILES,
    _base_contract,
    cited,
    t,
)
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


SUITE = "weknora-post-change-evidence-canary-v1"
CORPUS_VERSION = "helix-lab-handoff-v1"
KB_ID = "${AGENT_EVAL_KB_POST_CHANGE_LAB_ID}"


def dialogue_only():
    return _base_contract("quick-answer", no_retrieval=True)


def source_grounded(anchor: str, terms: list[str]):
    return cited("quick-answer", anchor, terms)


def build_cases() -> list[CaseSpec]:
    endpoint, agent_id, agent_type, history_turns, _ = PROFILES["quick-answer"]
    state = dialogue_only()
    turns = [
        t(
            1,
            "查 Helix 规程：接收时防拆封条完好能证明什么，又不能据此证明什么？请引用。",
            source_grounded("seal-scope", ["防拆封条", "开启痕迹", "不能据此推断"]),
        ),
        t(
            2,
            "新批次 Tern-42 现在只确认处在接收环节；保管位置暂写冷库A但还没确认，保管责任人也未知。先别打印标签或更新LIMS。",
            state,
        ),
        t(
            3,
            "把我提供的当前事实和规程对‘位置未确认’的处理要求分开说明；规程部分要引用，用户事实不要冒充文档证据。",
            source_grounded("pending-location", ["实际保管位置", "待确认", "计划位置"]),
        ),
        t(
            4,
            "修改交接说明的内容：把温度字段写成‘待确认’。只在聊天里展示文字，不要创建文件，也不要解除之前的外部操作边界。",
            state,
        ),
        t(
            5,
            "更新：实际保管位置确认是冷库C，冷库A这个暂定值作废；保管责任人定为Priya。当前环节仍是接收。",
            state,
        ),
        t(
            6,
            "规程为什么说主管批准前不得离心？这里的‘不得’是知识问题，不是让我执行离心，也不是叫你停止检索。请引用。",
            source_grounded("centrifuge-boundary", ["值班主管", "不得执行离心", "不等于"]),
        ),
        t(
            7,
            "逐字摘录我第一次提到保管位置的原话，再指出哪条用户更新替代了它；不要引用你的旧回答。",
            state,
        ),
        t(
            8,
            "补充：当前异常记录数是0；运输是否启动、完成都仍未知。只记录这两个层次，别从0推导生命周期。",
            state,
        ),
        t(
            9,
            "Write exactly one English handoff sentence containing only batch ID, current stage, confirmed location, custodian, and pending temperature. Do not invent a recipient, contact route, duration, or next action; do not send or save it.",
            state,
        ),
        t(
            10,
            "最终交接请区分：生效事实、被替代值、待确认/未知、用户原始来源、仍有效的行动边界，以及规程依据；规程结论要有本轮引用，外部操作不要写成已完成。",
            source_grounded("final-protocol", ["交接", "待确认", "外部操作"]),
        ),
    ]
    case = CaseSpec(
        case_id="post-change-v1-quick-lab-handoff",
        family_id="post-change-evidence-query-and-boundary",
        suite=SUITE,
        split=Split.DEV,
        capabilities=[
            Capability.RAG_RETRIEVAL,
            Capability.CITATION,
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
        ],
        agent_profile_id="quick-answer",
        agent=AgentSelector(endpoint=endpoint, agent_id=agent_id, agent_type=agent_type),
        setup=CaseSetup(
            knowledge_base_ids=[KB_ID],
            knowledge_ids=[],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-post-change-canary",
        ),
        turns=turns,
        tags=[
            "post-change-one-time-canary",
            "capability-composition",
            "codex-conversation-review",
            "domain:laboratory-sample-custody",
            "profile:quick-answer",
            "knowledge:explicit",
            "ten-turns",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-created-after-evidence-query-isolation-20260901",
            needs_codex_review=False,
            reference_answers={},
            metadata={
                "design_basis": "new-capability-sequence-not-noun-substitution",
                "domain": "laboratory-sample-custody",
                "configured_history_turns": history_turns,
                "knowledge_selection_mode": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "gold_answer_policy": "codex-full-conversation-review",
                "created_after_evidence_query_change": True,
            },
        ),
    )
    cases = [case]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid post-change canary: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "post-change-evidence-canary.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} case to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
