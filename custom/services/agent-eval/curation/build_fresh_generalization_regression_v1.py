"""Build a post-prompt regression that was not available during optimization.

The suite adds a new corpus and three genuine no-KB profiles. Its contracts
contain only mechanical isolation, retrieval and citation observations; Codex
reviews each full conversation for semantic quality.
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
    TurnSpec,
)


SUITE = "weknora-fresh-generalization-regression-v1"
CORPUS_VERSION = "northbank-audio-release-v1"
MEDIA_KB_ID = "${AGENT_EVAL_KB_FRESH_GENERALIZATION_MEDIA_ID}"
NO_KB_AGENT_IDS = {
    "quick-answer": "${AGENT_EVAL_AGENT_QUICK_NO_KB_ID}",
    "rag-reasoning": "${AGENT_EVAL_AGENT_RAG_NO_KB_ID}",
    "general-agent": "${AGENT_EVAL_AGENT_GENERAL_NO_KB_ID}",
}


def no_kb_chat(profile: str):
    return _base_contract(profile, no_retrieval=True)


def selected_chat(profile: str):
    # Quick-answer is intentionally bound to one KB for the complete
    # conversation. Semantic source separation is reviewed by Codex; retrieval
    # and citations remain objective observations rather than lexical scoring.
    return _base_contract(profile)


def evidence(profile: str, anchor: str, terms: list[str]):
    return cited(profile, anchor, terms)


def make_case(
    *,
    case_id: str,
    family_id: str,
    profile: str,
    domain: str,
    selection: KnowledgeSelectionMode,
    turns: list[TurnSpec],
) -> CaseSpec:
    endpoint, default_agent_id, agent_type, history_turns, _ = PROFILES[profile]
    if selection == KnowledgeSelectionMode.NONE:
        agent_id = NO_KB_AGENT_IDS[profile]
        knowledge_base_ids: list[str] = []
    elif selection == KnowledgeSelectionMode.EXPLICIT:
        agent_id = default_agent_id
        knowledge_base_ids = [MEDIA_KB_ID]
    else:  # pragma: no cover - this builder intentionally forbids ambiguity
        raise ValueError("fresh regression requires explicit knowledge selection")

    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite=SUITE,
        split=Split.DEV,
        capabilities=[
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
            *(
                [Capability.RAG_RETRIEVAL, Capability.CITATION]
                if selection == KnowledgeSelectionMode.EXPLICIT
                else []
            ),
        ],
        agent_profile_id=profile,
        agent=AgentSelector(
            endpoint=endpoint,
            agent_id=agent_id,
            agent_type=agent_type,
        ),
        setup=CaseSetup(
            knowledge_base_ids=knowledge_base_ids,
            knowledge_ids=[],
            knowledge_selection_mode=selection,
            summary_model_id=MODEL_ID,
            channel="agent-eval-fresh-generalization-regression",
        ),
        turns=turns,
        tags=[
            "post-prompt-independent-regression",
            "capability-shaped",
            "codex-conversation-review",
            f"domain:{domain}",
            f"profile:{profile}",
            f"knowledge:{selection.value}",
            "thirteen-turns",
        ],
        corpus_version=(
            CORPUS_VERSION if selection == KnowledgeSelectionMode.EXPLICIT else None
        ),
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-post-prompt-fresh-regression-20260901",
            needs_codex_review=False,
            reference_answers={},
            metadata={
                "design_basis": "new-capability-compositions-not-noun-substitution",
                "domain": domain,
                "configured_history_turns": history_turns,
                "knowledge_selection_mode": selection.value,
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "gold_answer_policy": "codex-full-conversation-review",
                "created_after_terminal_projection_change": True,
            },
        ),
    )


def quick_selected_audio_case() -> CaseSpec:
    p = "quick-answer"
    turns = [
        t(1, "查 Northbank 手册：机器转写、人工核对和无障碍审核是什么关系？请引用依据。", evidence(p, "transcript-states", ["机器转写", "人工核对", "无障碍审核"])),
        t(2, "我这边的 Ripple-6 目前是审听稿，制作负责人还没定。先把这两个用户事实说清楚。", selected_chat(p)),
        t(3, "把我刚说的项目现状和手册里的通用规则分开；项目现状不要伪造引用。", selected_chat(p)),
        t(4, "负责人现在定为Sora。音频版本没有变化。", selected_chat(p)),
        t(5, "手册说原始人声轨为什么不能直接公开？这句话里的‘不能’不是叫你停止查手册，要给引用。", evidence(p, "rights-boundary", ["原始人声轨", "权利核验", "不得公开发布"])),
        t(6, "把公告草稿的措辞改成‘权利状态待确认’，只在聊天里给文字，先别上传音频、发公告或动任务板。", selected_chat(p)),
        t(7, "纠正一个信息：Ripple-6 实际仍是拼接稿，之前说审听稿作废；负责人Sora不变。", selected_chat(p)),
        t(8, "岔开一下：通俗解释校验和能解决什么问题。别把这个科普内容混成 Ripple-6 的状态。", selected_chat(p)),
        t(9, "按手册，较新的签署授权替代旧授权时，哪些事情仍不能据此推断？引用。", evidence(p, "authorization-supersession", ["较新的", "签署授权", "旧文件", "删除"])),
        t(10, "用户更新：受访者Mei的新授权已签署；其他受访者的权利状态仍未知。不要扩大这个结论。", selected_chat(p)),
        t(11, "Answer in English. Give only Ripple-6's current audio stage, producer, and the two rights-status scopes. Keep handbook claims distinct from my updates.", selected_chat(p)),
        t(12, "我第一次怎么描述音频版本的？只摘录我的原话，再说明后来哪条用户消息推翻了它。", selected_chat(p)),
        t(13, "做最终交接：当前项目事实、已作废说法、未知项、各自来源和未发生的外部操作要分得清；手册规则需要引用，用户事实不用硬加引用。", evidence(p, "final-source-separation", ["交付交接", "未知", "外部操作"])),
    ]
    return make_case(
        case_id="fresh-v1-quick-audio-release",
        family_id="fresh-audio-evidence-and-user-state",
        profile=p,
        domain="audio-publishing",
        selection=KnowledgeSelectionMode.EXPLICIT,
        turns=turns,
    )


def quick_no_kb_garden_case() -> CaseSpec:
    p = "quick-answer"
    c = no_kb_chat(p)
    turns = [
        t(1, "记一下社区菜园 Willow 区：今天只观察到表土偏干，深层湿度没测；先别替我预约水车。", c),
        t(2, "这里哪些是观察，哪些是未知，哪句只是行动边界？用自然语言说，不要套业务模板。", c),
        t(3, "补充：志愿者协调人是Inez，但下次巡查时间还没定。", c),
        t(4, "口语点说：现在能确定啥、还差啥？别去翻任何知识库。", c),
        t(5, "换个话题，解释中位数为什么不一定等于平均数；这段科普不要写进 Willow 区记录。", c),
        t(6, "修正：后来已测到深层湿度为31%，所以‘没测’这条结束；表土观察仍保留。", c),
        t(7, "把提醒语的内容改成‘周五前复测’，但不要真的创建提醒、更新日历或联系志愿者。", c),
        t(8, "‘讨论要不要浇水’是否等于已经浇水？只解释语义，不要脑补现场动作。", c),
        t(9, "Which exact user sentence first said the deeper reading was unknown? Quote only my words.", c),
        t(10, "再更新：下次巡查定在周五傍晚；复测由谁做仍没定。Inez只负责协调，不自动等于复测人。", c),
        t(11, "用英文列当前两项测量信息和两个角色状态，不要带上刚才的统计学话题。", c),
        t(12, "在聊天里写一句给志愿者看的提醒草稿，可以写内容，但别发送。", c),
        t(13, "最后用一小段话交代 Willow 区现状：保留有效更新，指出被替代的未知状态，并明确哪些外部动作没有发生。", c),
    ]
    return make_case(
        case_id="fresh-v1-quick-community-garden",
        family_id="fresh-colloquial-state-without-kb",
        profile=p,
        domain="community-garden",
        selection=KnowledgeSelectionMode.NONE,
        turns=turns,
    )


def rag_no_kb_research_case() -> CaseSpec:
    p = "rag-reasoning"
    c = no_kb_chat(p)
    turns = [
        t(1, "我们策划一轮代号Kite的访谈研究：目标招6人，目前一个也没招；入选条件还没定。不要替我联系候选人。", c),
        t(2, "先判断‘目标6人’和‘已招0人’分别是哪类信息，别把目标写成完成进度。", c),
        t(3, "把访谈提纲的内容改为先问近期经历，再问改进建议；只给我文字，不要改共享表格。", c),
        t(4, "如果我说‘分析为何现在不该启动招募’，这能证明招募已经启动吗？说明理由。", c),
        t(5, "临时换题：为什么诱导性问题会影响质性访谈？回答一般原理，不要并入Kite项目事实。", c),
        t(6, "项目更新：形式从线下面谈改成远程视频，线下方案不再有效；人数目标不变。", c),
        t(7, "引用我提出形式变更的原句，不要把你的解释当用户证据。", c),
        t(8, "参与补贴金额还是未知，主持人定为Omar。请只更新这两点。", c),
        t(9, "用很口语的中文复述当前计划，未知的就说未知，别为了完整去补数。", c),
        t(10, "In English, distinguish the recruitment target, actual recruitment, interview format, moderator, and unresolved incentive.", c),
        t(11, "把邀请信措辞写成两句聊天草稿；写内容是允许的，但不要发信、建日程或登记报名。", c),
        t(12, "再澄清：Omar是主持人，不是招募负责人；招募负责人仍未指定。", c),
        t(13, "总结Kite目前的有效计划、被撤回的方案、完成进度、未知项、来源和行动边界。不要展示思考过程或内部标签。", c),
    ]
    return make_case(
        case_id="fresh-v1-rag-interview-study",
        family_id="fresh-plan-progress-and-content-action",
        profile=p,
        domain="user-research",
        selection=KnowledgeSelectionMode.NONE,
        turns=turns,
    )


def general_no_kb_rehearsal_case() -> CaseSpec:
    p = "general-agent"
    c = no_kb_chat(p)
    turns = [
        t(1, "展览彩排代号Ember：场地暂定二号厅但未确认，现场导演是Jia。不要替我订场。", c),
        t(2, "直接告诉我哪些已经确定、哪些只是暂定；‘不要订场’不能写成场地状态。", c),
        t(3, "修改彩排清单的文字方案，加入‘入场前检查照明’，但别创建文件或改项目系统。", c),
        t(4, "先聊别的：cue sheet通常解决什么协作问题？不要把一般说明当成Ember已经采用的流程。", c),
        t(5, "场地现已确认改为四号厅，二号厅方案作废；导演还是Jia。", c),
        t(6, "无线讲解器要借多少台尚未决定，只记录这个未知项，不要查设备库。", c),
        t(7, "我最早关于场地说了什么？逐字摘录用户原话，并指出后来哪条更新替代了它。", c),
        t(8, "把借用申请的草稿数字写成12台供讨论，但这不是最终数量，也不要提交申请。", c),
        t(9, "新增角色：Milo负责灯光核验；Jia仍是现场导演，两者别合并。", c),
        t(10, "Write a concise English handoff with the confirmed hall, the two roles, and the unresolved device quantity. Do not mention the cue-sheet detour.", c),
        t(11, "‘说明为什么未检查前不能开灯’是分析请求，不代表灯已经开过，也不代表检查完成。你同意吗？", c),
        t(12, "可以在聊天里写一段场地确认邮件正文，但不要发送、保存草稿或调用外部工具。", c),
        t(13, "给Ember做最终交接，区分生效事实、废弃方案、未知/讨论值、用户来源、允许的文本产出和未执行的外部动作。只给最终回答。", c),
    ]
    return make_case(
        case_id="fresh-v1-general-exhibit-rehearsal",
        family_id="fresh-role-scope-and-tool-boundary",
        profile=p,
        domain="museum-rehearsal",
        selection=KnowledgeSelectionMode.NONE,
        turns=turns,
    )


def build_cases() -> list[CaseSpec]:
    cases = [
        quick_selected_audio_case(),
        quick_no_kb_garden_case(),
        rag_no_kb_research_case(),
        general_no_kb_rehearsal_case(),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid fresh generalization suite: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "fresh-generalization-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
