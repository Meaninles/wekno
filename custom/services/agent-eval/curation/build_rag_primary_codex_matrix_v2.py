"""Build the second RAG-primary matrix with a post-change unseen regression.

Version 1 remains immutable for the prior full-flow result.  This version keeps
its seven conversations, moves them under a new frozen identity, and adds one
independent customer-support corpus composed around semantic binding rather
than copied answer shapes.  Semantic quality remains complete-conversation
Codex judgment; turn contracts contain only the read-only transport boundary.
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
        build_cases as build_v1_cases,
        neutral_contract,
        turn,
    )
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        build_cases as build_v1_cases,
        neutral_contract,
        turn,
    )


SUITE = "weknora-rag-primary-codex-matrix-v2"
CORPUS_VERSION = "rag-primary-corpora-v2"
SUPPORT_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_SUPPORT_ID}"


def _retarget_v1(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata["matrix_revision"] = 2
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-rag-primary-matrix-v2-retained-v1-capability",
                    "metadata": metadata,
                }
            ),
        }
    )


def customer_support_case() -> CaseSpec:
    profile = "general-agent"
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    turns = [
        turn(1, "从当前知识库查：进入标准换机资格判断需要哪两项证据？带引用。"),
        turn(2, "支持事项 ZX-7，设备序列号 NS-204；客户或申请人没有提供，已观察现象也未知。分诊协调人是 Noor。只整理聊天状态，不创建文件或服务单。"),
        turn(3, "按手册说明分诊协调人和技术复核人的职责，给引用；通用职责不要写成 ZX-7 已完成的动作。"),
        turn(4, "技术复核人指定为 Mei。这里只确定角色，没有说明 Mei 已经开始或完成复核；复核状态保持未知。"),
        turn(5, "本轮只输出 JSON：事项编号、序列号、客户、观察现象、协调人、复核人、复核状态。不检索。"),
        turn(6, "查手册解释为什么不能擅自远程擦除，带引用。‘不能擦除’是知识问题，不是让你擦除、执行命令或生成文件。"),
        turn(7, "更新 ZX-7：已观察现象为‘设备从睡眠恢复后屏幕闪烁’，原来的现象未知结束。"),
        turn(8, "假设另一个设备出现电池鼓包，手册建议怎样处理？查证并引用；这是独立假设，不要加到 ZX-7。"),
        turn(9, "ZX-7 的复核现已完成，结论是证据不足；购买凭证待补，诊断代码也待补。客户身份仍未知。"),
        turn(10, "Reply in English with only ZX-7's current serial, observed symptom, coordinator, reviewer, review result, and pending evidence. No tools this turn."),
        turn(11, "另问一条通用知识：设备寄回要用什么包装？检索并引用，但不要把包装规则记成 ZX-7 已经寄回。"),
        turn(12, "修改聊天里的 RMA 草稿，加入‘等待购买凭证和诊断代码’；不要创建 RMA、改服务系统、写文件、发邮件或操作设备。"),
        turn(13, "汇总 ZX-7：有效事实、已结束未知项、仍未知/待补、用户原文来源、适用手册规则和操作边界分开；手册规则本轮重新检索并逐项引用。"),
    ]
    return CaseSpec(
        case_id="rag-primary-v2-general-customer-device-support",
        family_id="rag-primary-customer-support-semantic-binding",
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
            knowledge_base_ids=[SUPPORT_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=turns,
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "new-post-semantic-binding-regression",
            "explicit-knowledge-base",
            "long-context",
            "counterexamples",
            "domain:customer-support",
            "profile:general-agent",
            "knowledge:device-support-handbook",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-semantic-binding-rag-regression-20260901",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "independent-role-field-object-hypothesis-action-capabilities",
                "domain": "customer-support",
                "created_after_proof_direction_prompt_change": True,
                "created_after_semantic_binding_prompt_change": True,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
                "matrix_revision": 2,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [*(_retarget_v1(case) for case in build_v1_cases()), customer_support_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix v2: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v2.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
