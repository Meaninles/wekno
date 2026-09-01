"""Build a newly designed post-prompt adversarial regression.

The conversation is capability-shaped rather than a noun-substituted copy of
an existing case. It exercises semantic state equivalence, lifecycle modality,
chat-versus-external persistence, source scope, topic isolation, attribution,
and tool abstention over fourteen turns with a genuine no-KB runtime profile.
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
    ToolPolicy,
    TurnContract,
    TurnSpec,
)


SUITE = "weknora-post-prompt-lab-handover-v1"
MODEL_ID = "${AGENT_EVAL_SUMMARY_MODEL_ID}"
NO_KB_RAG_AGENT_ID = "${AGENT_EVAL_AGENT_RAG_NO_KB_ID}"
RETRIEVAL_TOOLS = [
    "knowledge_search",
    "grep_chunks",
    "list_knowledge_chunks",
    "get_document_info",
    "query_knowledge_graph",
    "wiki_search",
    "wiki_read_page",
    "web_search",
    "web_fetch",
]
ALL_VISIBLE_TOOLS = [
    *RETRIEVAL_TOOLS,
    "thinking",
    "sequentialthinking",
    "todo_write",
    "create_artifact",
    "Bash",
]


def contract(*, no_tools: bool = False) -> TurnContract:
    return TurnContract(
        citation_required=False,
        tool_policy=ToolPolicy(
            read_only=True,
            forbidden_tools=ALL_VISIBLE_TOOLS if no_tools else RETRIEVAL_TOOLS,
        ),
        max_tool_calls=0 if no_tools else 8,
    )


def turn(number: int, query: str, *, no_tools: bool = False) -> TurnSpec:
    return TurnSpec(
        turn_id=f"turn-{number:03d}",
        query=query,
        contract=contract(no_tools=no_tools),
    )


def build_cases() -> list[CaseSpec]:
    turns = [
        turn(1, "把这当作纯聊天里的交接草稿，不查任何外部资料。样本代号 Atlas，拟交接日先记周四；保管负责人还没决定。不要写 LIMS、不要发邮件，也不要创建文件。"),
        turn(2, "我换个说法：保管人尚未敲定。判断这和‘还没决定’是不是同一个未决状态，用一句话回答；不要复制成两条未知项。"),
        turn(3, "我只是在问一个语义问题：如果以后有人建议叫快递，为什么不等于已经预订了快递？别把‘快递’登记成 Atlas 的事实或行动边界。"),
        turn(4, "观察员确定为 Mira。注意，她是观察员，不是保管负责人；这句话也没有说明交接已经开始。"),
        turn(5, "把聊天里的交接说明内容加一行‘温度记录：待补’，但仍然不要创建或修改任何文件。"),
        turn(6, "只用 YAML 输出当前两个未决事项，并在每项后摘录我的原话；不要加第三个字段，也不要调用任何工具。", no_tools=True),
        turn(7, "Answer in English: are ‘pending’ and ‘not settled yet’ two separate custodian states, and has any external record been updated?"),
        turn(8, "临时换题：用一句日常话解释 checksum。不要检索，也不要把这个解释写进 Atlas 交接状态。", no_tools=True),
        turn(9, "回到 Atlas：拟交接日改为周五，周四作废；温度记录已经给出，为交接时 -20°C。保管负责人仍是 TBD。"),
        turn(10, "我告诉你一条实验室惯例：保管负责人通常需要签字。这是规则信息，不是 Atlas 已签字或已交接的证据。"),
        turn(11, "现在只回答 Atlas 的交接生命周期处于什么状态。不要从日期、观察员或签字惯例推断已开始、未开始或已完成，也不要用工具。", no_tools=True),
        turn(12, "修改聊天方案：交接前增加‘核对样本封签编号’；但不要编辑 LIMS、文件，也不要发送消息。"),
        turn(13, "分别摘录周五、Mira、-20°C 的原始用户片段。若没有可靠 source_id，就只引原文，别猜第几轮。", no_tools=True),
        turn(14, "做最终快照：分开列有效事实、作废事实、仍未知事项、用户来源、聊天方案和外部行动边界；不要把快递建议、checksum 解释或签字惯例写成 Atlas 已发生的事实。", no_tools=True),
    ]
    case = CaseSpec(
        case_id="post-prompt-lab-handover-semantic-isolation-v1",
        family_id="post-prompt-semantic-state-source-tool-isolation",
        suite=SUITE,
        split=Split.DEV,
        capabilities=[Capability.LONG_CONTEXT_DIALOGUE, Capability.TOOL_USE],
        agent_profile_id="rag-reasoning",
        agent=AgentSelector(
            endpoint="agent-chat",
            agent_id=NO_KB_RAG_AGENT_ID,
            agent_type="rag-qa",
        ),
        setup=CaseSetup(
            knowledge_selection_mode=KnowledgeSelectionMode.NONE,
            summary_model_id=MODEL_ID,
            channel="agent-eval-post-prompt-regression",
        ),
        turns=turns,
        repetitions=2,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        tags=[
            "post-prompt-regression",
            "new-capability-composition",
            "fourteen-turns",
            "no-knowledge-base",
            "semantic-equivalence",
            "counterexamples",
            "domain:lab-handover",
        ],
        provenance=Provenance(
            source="codex-new-post-change-capability-regression-20260901",
            needs_codex_review=False,
            reference_answers={},
            metadata={
                "configured_history_turns": 10,
                "created_after_prompt_change": True,
                "design_basis": "cross-capability-composition-not-noun-substitution",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "release_decision": "codex_whole_conversation_review",
            },
        ),
    )
    cases = [case]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid post-prompt regression: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "post-prompt-lab-handover.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} case to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
