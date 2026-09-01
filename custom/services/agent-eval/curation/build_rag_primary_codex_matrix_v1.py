"""Build the RAG-primary, Codex-reviewed production readiness matrix.

This suite intentionally leaves semantic quality to Codex reading each complete
conversation.  Machine contracts retain only artifact/read-only validity and
never encode expected phrases, required claims, reference answers, citation
counts, or scenario-specific answer shapes.
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

try:
    from .build_semantic_routing_regression_v1 import (
        build_cases as build_semantic_cases,
    )
    from .build_unseen_capability_matrix_v2 import (
        build_cases as build_unseen_cases,
    )
except ImportError:  # pragma: no cover - direct script execution
    from build_semantic_routing_regression_v1 import (
        build_cases as build_semantic_cases,
    )
    from build_unseen_capability_matrix_v2 import (
        build_cases as build_unseen_cases,
    )


SUITE = "weknora-rag-primary-codex-matrix-v1"
CORPUS_VERSION = "rag-primary-corpora-v1"
MODEL_ID = "${AGENT_EVAL_SUMMARY_MODEL_ID}"
LAB_KB_ID = "${AGENT_EVAL_KB_POST_CHANGE_LAB_ID}"
HR_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_HR_ID}"

PROFILE_RUNTIME = {
    "quick-answer": ("knowledge-chat", "builtin-quick-answer", "rag-qa", 5),
    "rag-reasoning": ("agent-chat", "builtin-smart-reasoning", "rag-qa", 10),
    "general-agent": ("agent-chat", "builtin-general-agent", "general-agent", 10),
}

SOURCE_SELECTION = {
    "unseen-gate-quick-it": (
        "rag-primary-v1-quick-it-runbook",
        "it-operations",
        "it-runbook",
    ),
    "unseen-dev-rag-product": (
        "rag-primary-v1-rag-product-manual",
        "product-manual",
        "product-manual",
    ),
    "unseen-gate-rag-policy": (
        "rag-primary-v1-rag-governance-policy",
        "policy-governance",
        "governance-policy",
    ),
    "unseen-dev-general-project": (
        "rag-primary-v1-general-project-handbook",
        "project-management",
        "project-handbook",
    ),
    "regression-v1-general-facilities": (
        "rag-primary-v1-general-facilities-playbook",
        "facilities-operations",
        "facilities-playbook",
    ),
}


def neutral_contract() -> TurnContract:
    """Keep only transport/read-only validity; Codex owns semantic judgment."""

    return TurnContract(tool_policy=ToolPolicy(read_only=True))


def turn(number: int, query: str) -> TurnSpec:
    return TurnSpec(
        turn_id=f"turn-{number:03d}",
        query=query,
        contract=neutral_contract(),
    )


def _metadata(
    *, domain: str, history_turns: int, design_basis: str, newly_created: bool
) -> dict[str, object]:
    return {
        "configured_history_turns": history_turns,
        "design_basis": design_basis,
        "domain": domain,
        "created_after_proof_direction_prompt_change": newly_created,
        "gold_answer_policy": "codex-complete-conversation-material-error-only",
        "knowledge_selection_contract": "explicit",
        "sealed_holdout_used": False,
        "sut_prompt_injection": False,
        "reference_answers_used": False,
        "judge_feedback_used": False,
    }


def _retarget_source_case(case: CaseSpec) -> CaseSpec:
    new_case_id, domain, knowledge_label = SOURCE_SELECTION[case.case_id]
    history_turns = PROFILE_RUNTIME[case.agent_profile_id or ""][3]
    return case.model_copy(
        update={
            "case_id": new_case_id,
            "family_id": f"rag-primary-{case.family_id}",
            "suite": SUITE,
            "split": Split.GATE,
            "capabilities": [
                Capability.RAG_RETRIEVAL,
                Capability.CITATION,
                Capability.LONG_CONTEXT_DIALOGUE,
                Capability.TOOL_USE,
            ],
            "setup": case.setup.model_copy(
                update={
                    "knowledge_selection_mode": KnowledgeSelectionMode.EXPLICIT,
                    "channel": "agent-eval-rag-primary-codex",
                }
            ),
            "turns": [
                current.model_copy(update={"contract": neutral_contract()})
                for current in case.turns
            ],
            "tags": [
                "rag-primary",
                "codex-complete-conversation-review",
                "unseen-distribution",
                "explicit-knowledge-base",
                "long-context",
                "counterexamples",
                f"domain:{domain}",
                f"profile:{case.agent_profile_id}",
                f"knowledge:{knowledge_label}",
            ],
            "corpus_version": CORPUS_VERSION,
            "repetitions": 3,
            "review_mode": ReviewMode.CODEX_CONVERSATION,
            "provenance": Provenance(
                source="codex-rag-primary-matrix-v1-recomposed-capabilities",
                needs_codex_review=False,
                reference_answers={},
                reference_evidence=[],
                metadata=_metadata(
                    domain=domain,
                    history_turns=history_turns,
                    design_basis=(
                        "recomposed-practical-rag-capabilities-without-lexical-golds"
                    ),
                    newly_created=False,
                ),
            ),
        }
    )


def _new_case(
    *,
    case_id: str,
    family_id: str,
    profile: str,
    domain: str,
    knowledge_label: str,
    kb_id: str,
    turns: list[TurnSpec],
) -> CaseSpec:
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite=SUITE,
        split=Split.GATE,
        capabilities=[
            Capability.RAG_RETRIEVAL,
            Capability.CITATION,
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
        ],
        agent_profile_id=profile,
        agent=AgentSelector(
            endpoint=endpoint,
            agent_id=agent_id,
            agent_type=agent_type,
        ),
        setup=CaseSetup(
            knowledge_base_ids=[kb_id],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=turns,
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "new-post-change-regression",
            "explicit-knowledge-base",
            "long-context",
            "counterexamples",
            f"domain:{domain}",
            f"profile:{profile}",
            f"knowledge:{knowledge_label}",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-rag-primary-regression-20260901",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata=_metadata(
                domain=domain,
                history_turns=history_turns,
                design_basis=(
                    "new-capability-composition-from-practical-rag-usage-shapes"
                ),
                newly_created=True,
            ),
        ),
    )


def lab_quick_case() -> CaseSpec:
    turns = [
        turn(1, "查实验室规程：一次样本交接至少记录哪些字段，未知字段怎么处理？请给出处。"),
        turn(2, "批次 Quill-27 当前在接收环节；实际保管位置未知，保管责任人是 Morgan，温度状态待确认。只在对话里整理，不要写 LIMS、打印标签或呼叫物流。"),
        turn(3, "把我提供的批次事实和规程要求分开说明；规程部分重新检索并引用。"),
        turn(4, "仓库备注没有显示运输已经完成；这只表示当前记录不能证明完成，不要登记成‘运输尚未完成’。用一句话说生命周期该怎么记。"),
        turn(5, "本回答只用 JSON 给出当前环节、实际位置、责任人、温度和运输生命周期；不要调用工具。"),
        turn(6, "现在回到知识问答：异常记录数为零能不能证明运输状态？请检索规程并引用。上一轮不调用工具只限上一轮。"),
        turn(7, "更新 Quill-27：实际位置确定为 Cold Room D，温度为 -18°C；原来的位置未知和温度待确认结束。"),
        turn(8, "规程里离心处理的边界是什么？请查证并引用，但不要执行任何处理。"),
        turn(9, "修改聊天中的交接方案，增加‘复核防拆封条编号’；不要修改文件、LIMS，也不要打印或发消息。"),
        turn(10, "Reply in English with the current stage, location, custodian, temperature, and unresolved transport lifecycle. No tools for this answer."),
        turn(11, "分别摘录我给出 Cold Room D、-18°C 和 Morgan 的原始片段；不要把规程文字说成我的陈述。"),
        turn(12, "No reliable evidence states that transport started or completed. Keep both propositions unknown; do not turn missing proof into their negation. 用中文确认。"),
        turn(13, "做 Quill-27 最终快照：当前事实、已结束的未知项、仍未知生命周期、用户来源、聊天方案和外部操作边界分开；当前规程结论请重新检索并引用。"),
    ]
    return _new_case(
        case_id="rag-primary-v1-quick-lab-handoff",
        family_id="rag-primary-lab-evidence-proof-direction",
        profile="quick-answer",
        domain="laboratory-handoff",
        knowledge_label="lab-handoff",
        kb_id=LAB_KB_ID,
        turns=turns,
    )


def hr_general_case() -> CaseSpec:
    turns = [
        turn(1, "先用一句话说明这个知识库能解决哪类问题，再列远程设备寄送前的两个前置条件，带引用。"),
        turn(2, "入职批次 Alder-5 有4人，协调人是苏檀；实际收货地址还没确认，签收时间窗也未知。暂时只整理，不操作 HRIS。"),
        turn(3, "按手册区分人事协调人、直属经理和安全负责人各自能做什么；不要把通用职责变成 Alder-5 已完成事项，给引用。"),
        turn(4, "现有备注没有显示账号已经启用。这只是缺少启用证据，不代表账号尚未启用。请把账号生命周期保持为未知。"),
        turn(5, "本轮只输出 YAML：批次、人数、协调人、地址、时间窗、账号状态；不检索，也不加说明。"),
        turn(6, "继续查手册：‘不得安装未受管的远程控制软件’限制什么？句子含‘不得安装’不等于禁止你检索，引用依据。"),
        turn(7, "地址更新为云杉路18号，原地址未知结束；签收时间窗仍待确认。"),
        turn(8, "如果标准安全会话无法完成身份核验，例外流程是什么？谁不能自行批准？查证并引用。"),
        turn(9, "修改聊天里的首日清单，加入‘核对替代核验方法’；但不要创建文件、改 HRIS、发邮件、启用账号或生成物流单。"),
        turn(10, "The receiving window is 14:00-16:00, so the old pending window is retired. Summarize current user-provided facts in English; no tools this turn."),
        turn(11, "逐条指出地址、签收时间窗和协调人来自我的哪段话；手册规则不要归到我的来源。"),
        turn(12, "安全负责人已书面批准例外方案；这不等于替代核验已经执行、账号已启用或设备已寄出。按手册解释，带引用。"),
        turn(13, "给出 Alder-5 首日交接摘要：有效事实、已结束未知项、仍未知状态、用户来源、适用手册规则和外部操作边界分开；规则逐项引用。"),
    ]
    return _new_case(
        case_id="rag-primary-v1-general-remote-onboarding",
        family_id="rag-primary-hr-role-proof-operation-boundary",
        profile="general-agent",
        domain="human-resources",
        knowledge_label="remote-onboarding",
        kb_id=HR_KB_ID,
        turns=turns,
    )


def build_cases() -> list[CaseSpec]:
    source_cases = {
        case.case_id: case
        for case in [*build_unseen_cases(), *build_semantic_cases()]
    }
    selected = [
        _retarget_source_case(source_cases[source_id])
        for source_id in SOURCE_SELECTION
    ]
    cases = [*selected, lab_quick_case(), hr_general_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
