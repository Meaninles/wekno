"""Build an independent post-change regression suite for semantic tool routing.

The cases are capability-shaped and use a dedicated corpus. They are not
derived from existing DEV/GATE questions and never load the sealed holdout.
"""

from __future__ import annotations

from pathlib import Path

from curation.build_unseen_capability_matrix_v1 import MODEL_ID, PROFILES, _base_contract, cited, t
from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseSetup,
    CaseSpec,
    Provenance,
    ReviewMode,
    Split,
    TurnSpec,
)


SUITE = "weknora-semantic-routing-regression-v1"
CORPUS_VERSION = "semantic-routing-facilities-v1"
KB_ID = "${AGENT_EVAL_KB_SEMANTIC_ROUTING_FACILITIES_ID}"


def chat(profile: str):
    return _base_contract(profile, no_retrieval=True)


def evidence(profile: str, anchor: str, terms: list[str]):
    return cited(profile, anchor, terms)


def make_case(
    *, case_id: str, family_id: str, profile: str, domain: str, kb: bool, turns: list[TurnSpec]
) -> CaseSpec:
    endpoint, agent_id, agent_type, history_turns, _ = PROFILES[profile]
    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite=SUITE,
        split=Split.DEV,
        capabilities=[
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
            *([Capability.RAG_RETRIEVAL, Capability.CITATION] if kb else []),
        ],
        agent_profile_id=profile,
        agent=AgentSelector(endpoint=endpoint, agent_id=agent_id, agent_type=agent_type),
        setup=CaseSetup(
            knowledge_base_ids=[KB_ID] if kb else [],
            knowledge_ids=[],
            summary_model_id=MODEL_ID,
            channel="agent-eval-semantic-routing-regression",
        ),
        turns=turns,
        tags=[
            "post-change-regression",
            "capability-shaped",
            "semantic-tool-routing",
            "negative-language-counterexample",
            f"domain:{domain}",
            f"profile:{profile}",
            f"knowledge:{'facilities' if kb else 'none'}",
            "twelve-turns",
        ],
        corpus_version=CORPUS_VERSION if kb else None,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-post-change-semantic-regression-20260901",
            needs_codex_review=False,
            reference_answers={},
            metadata={
                "design_basis": "semantic-capability-counterfactuals",
                "domain": domain,
                "configured_history_turns": history_turns,
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "gold_answer_policy": "codex-full-conversation-review",
                "independent_from_existing_dev_gate_wording": True,
            },
        ),
    )


def quick_case() -> CaseSpec:
    p = "quick-answer"
    turns = [
        t(1, "只记一个交接约束：蓝线窗口只接收纸质原件；经办人还没确定。不要替我打电话。", chat(p)),
        t(2, "说明‘不要替我打电话’为什么只是行动边界，不是一个已经发生的业务状态。", chat(p)),
        t(3, "现在经办人确定为Amina，先前的未知状态结束。只更新经办人。", chat(p)),
        t(4, "把交接说明的内容补成‘签收时间待确认’，但不要创建文件，也不要发送消息。", chat(p)),
        t(5, "换个话题：通俗解释‘单一事实来源’，不要把解释写进蓝线窗口事项。", chat(p)),
        t(6, "修正：蓝线窗口也接受带签章的电子副本；‘只接收纸质原件’作废。", chat(p)),
        t(7, "不要检索。只列当前可接受的材料形式和仍未知的签收时间。", chat(p)),
        t(8, "In English: Amina is the handler; a signed electronic copy is accepted; the receipt time remains pending. Do not send anything.", chat(p)),
        t(9, "我哪句话第一次说经办人未知？只摘录我的原话，不要引用你的回答。", chat(p)),
        t(10, "新增观察：窗口今天排队约12人；平均等待时长没有测。只记观察与未知。", chat(p)),
        t(11, "‘分析为什么不能发送’这句话本身有没有证明消息已经发送？只回答语义关系。", chat(p)),
        t(12, "给出最终交接快照，分清当前事实、作废事实、未知项、用户来源和行动边界；不要添加模板字段。", chat(p)),
    ]
    return make_case(
        case_id="regression-v1-quick-handoff",
        family_id="regression-semantic-handoff-no-tools",
        profile=p,
        domain="service-handoff",
        kb=False,
        turns=turns,
    )


def rag_case() -> CaseSpec:
    p = "rag-reasoning"
    turns = [
        t(1, "手册说访客证不能带出园区。请检索解释这条规则并引用；句子里的‘不能’不是禁止检索。", evidence(p, "badge-rule", ["访客证", "带出园区"])),
        t(2, "巡检点代号M-4，当前值守人未知。只记录这两项。", chat(p)),
        t(3, "按手册分析为什么‘建议关闭入口’不等于‘入口已经关闭’，给引用，但不要执行处置。", evidence(p, "closure-auth", ["关闭入口", "授权", "设施日志"])),
        t(4, "现场只看到控制器紫色双闪；自检结果没有核实。记录观察和未知，不要查资料。", chat(p)),
        t(5, "逐字摘录我刚才描述现场现象的片段，不要补手册解释。", chat(p)),
        t(6, "修改通报草案的内容，加入‘下一次更新时间待定’；不要创建文件、发送通报或更新设施系统。", chat(p)),
        t(7, "手册里的‘不得安装未经批准的无线扩展模块’限制什么？这是知识问答，仍要检索并引用。", evidence(p, "module-boundary", ["无线扩展模块", "未经批准"])),
        t(8, "M-4值守人改为Noor，原未知状态结束；其他现场事实不变。", chat(p)),
        t(9, "In English, state only the checkpoint, assigned person, observed signal, and unresolved test result. Do not retrieve.", chat(p)),
        t(10, "手册规定首次巡检通报包含哪些字段？重新检索本轮证据并引用。", evidence(p, "bulletin-fields", ["观察到的现象", "已核实的影响", "下一次更新时间"])),
        t(11, "把我的现场陈述与手册解释分栏；只给手册一栏加引用。", evidence(p, "source-separation", ["紫色双闪", "自检"])),
        t(12, "总结M-4当前事实、未知项、草案内容和行动边界。只依据用户消息，不要检索，也不要把手册规则变成已执行状态。", chat(p)),
    ]
    return make_case(
        case_id="regression-v1-rag-facilities",
        family_id="regression-semantic-evidence-routing",
        profile=p,
        domain="facilities-operations",
        kb=True,
        turns=turns,
    )


def general_case() -> CaseSpec:
    p = "general-agent"
    turns = [
        t(1, "从场地手册查首次巡检通报必须包含什么，给引用。", evidence(p, "notice-fields", ["观察到的现象", "负责人", "下一次更新时间"])),
        t(2, "登记演练事项D-4：已核实影响是北入口排队；负责人未知。直接在对话里整理，不要生成下载文件。", chat(p)),
        t(3, "当班协调人是Lin，但Lin不是D-4负责人。保持负责人未知。", chat(p)),
        t(4, "依据手册解释为什么审阅入口关闭方案并不等于批准或执行关闭，引用依据。", evidence(p, "review-not-execution", ["关闭入口", "明确授权", "执行记录"])),
        t(5, "改通报内容，补一句‘下一次更新时间待确认’；不要写文件、发消息或改设施系统。", chat(p)),
        t(6, "不要搜索，也不要用思考工具。只列D-4已明确和仍未知的信息。", chat(p)),
        t(7, "把负责人更新为Priya，旧的未知状态作废；协调人仍是Lin。", chat(p)),
        t(8, "In English, distinguish the coordinator from the owner and confirm that no external operation was performed.", chat(p)),
        t(9, "换个话题解释‘职责分离’的一般价值，不要把一般解释变成D-4事实。", chat(p)),
        t(10, "引用我的原话，说明北入口排队和负责人Priya分别来自哪次用户更新。不要引用助手输出。", chat(p)),
        t(11, "手册对未核实原因怎样处理？检索当前证据并引用；不要把规则冒充D-4已有原因。", evidence(p, "unknown-cause", ["尚未核实", "待确认"])),
        t(12, "写D-4最终状态快照：有效、废弃、未知、来源、通报草案和外部操作边界分开。只输出聊天文本。", chat(p)),
    ]
    return make_case(
        case_id="regression-v1-general-facilities",
        family_id="regression-semantic-artifact-authority",
        profile=p,
        domain="facilities-drill",
        kb=True,
        turns=turns,
    )


def build_cases() -> list[CaseSpec]:
    cases = [quick_case(), rag_case(), general_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid semantic-routing regression suite: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "semantic-routing-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
