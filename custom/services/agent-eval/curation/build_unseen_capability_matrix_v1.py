"""Build a cross-domain, capability-shaped three-agent validation suite.

The suite is intentionally unrelated to the procurement/Skill examples used
during earlier optimization.  It contains only visible DEV/GATE cases.  The
sealed holdout is managed by its existing identity-only manifest and is never
loaded by this compiler.
"""

from __future__ import annotations

from pathlib import Path

from weknora_eval.dataset import dataset_sha256, validate_dataset, write_jsonl
from weknora_eval.models import (
    AgentSelector,
    Capability,
    CaseSetup,
    CaseSpec,
    ConversationStateContract,
    EvidenceAnchor,
    Provenance,
    ReviewMode,
    Split,
    TextRule,
    ToolPolicy,
    TurnContract,
    TurnSpec,
)


SUITE = "weknora-unseen-capability-matrix-v1"
MODEL_ID = "${AGENT_EVAL_SUMMARY_MODEL_ID}"
CORPUS_VERSION = "unseen-corpora-v1"
NO_RETRIEVAL = [
    "knowledge_search",
    "grep_chunks",
    "list_knowledge_chunks",
    "query_knowledge_graph",
    "web_search",
    "web_fetch",
]

PROFILES = {
    "quick-answer": ("knowledge-chat", "builtin-quick-answer", "rag-qa", 5, 4),
    "rag-reasoning": ("agent-chat", "builtin-smart-reasoning", "rag-qa", 10, 8),
    "general-agent": ("agent-chat", "builtin-general-agent", "general-agent", 10, 12),
}

KB = {
    "product": "${AGENT_EVAL_KB_UNSEEN_PRODUCT_MANUAL_ID}",
    "project": "${AGENT_EVAL_KB_UNSEEN_PROJECT_HANDBOOK_ID}",
    "it": "${AGENT_EVAL_KB_UNSEEN_IT_RUNBOOK_ID}",
    "policy": "${AGENT_EVAL_KB_UNSEEN_GOVERNANCE_POLICY_ID}",
}


def r(
    rule_id: str,
    *,
    any_of: list[str] | None = None,
    all_of: list[str] | None = None,
    description: str = "",
) -> TextRule:
    return TextRule(
        rule_id=rule_id,
        any_of=any_of or [],
        all_of=all_of or [],
        description=description,
    )


def _base_contract(
    profile: str,
    *,
    required: list[TextRule] | None = None,
    forbidden: list[TextRule] | None = None,
    active: list[TextRule] | None = None,
    retired: list[TextRule] | None = None,
    unknown: list[TextRule] | None = None,
    boundaries: list[TextRule] | None = None,
    forbidden_inferences: list[TextRule] | None = None,
    scoped: bool = False,
    no_retrieval: bool = False,
    evidence: list[EvidenceAnchor] | None = None,
    judge_rubric: str | None = None,
    max_chars: int = 900,
) -> TurnContract:
    evidence = evidence or []
    # Codex reviews the complete conversation semantically.  The contract
    # deliberately retains only objective execution/tool/citation signals;
    # lexical rules below are scenario annotations used by the builder call
    # sites, never release criteria and never repair triggers.
    return TurnContract(
        required_claims=[],
        forbidden_claims=[],
        conversation_state=ConversationStateContract(),
        evidence_anchors=[],
        min_evidence_anchors=0,
        citation_required=bool(evidence),
        min_citations=1 if evidence else 0,
        min_retrieved_sources={"documents": 1} if evidence else {},
        tool_policy=ToolPolicy(
            read_only=True,
            forbidden_tools=NO_RETRIEVAL if no_retrieval else [],
        ),
        max_response_chars=None,
        max_tool_calls=PROFILES[profile][4],
        forbid_stale_citations_without_retrieval=bool(evidence),
        judge_rubric=None,
    )


def state(
    profile: str,
    *,
    active: list[TextRule] | None = None,
    retired: list[TextRule] | None = None,
    unknown: list[TextRule] | None = None,
    boundaries: list[TextRule] | None = None,
    forbidden_inferences: list[TextRule] | None = None,
    scoped: bool = False,
    max_chars: int = 700,
) -> TurnContract:
    return _base_contract(
        profile,
        active=active,
        retired=retired,
        unknown=unknown,
        boundaries=boundaries,
        forbidden_inferences=forbidden_inferences,
        scoped=scoped,
        no_retrieval=True,
        max_chars=max_chars,
    )


def plain(
    profile: str,
    rule_id: str,
    terms: list[str],
    *,
    forbidden: list[TextRule] | None = None,
    no_retrieval: bool = True,
    judge_rubric: str | None = None,
) -> TurnContract:
    return _base_contract(
        profile,
        required=[r(rule_id, any_of=terms)],
        forbidden=forbidden,
        no_retrieval=no_retrieval,
        judge_rubric=judge_rubric,
    )


def cited(
    profile: str,
    anchor_id: str,
    terms: list[str],
    *,
    required: list[str] | None = None,
) -> TurnContract:
    return _base_contract(
        profile,
        required=[r(anchor_id + "-answer", any_of=required or terms)],
        evidence=[EvidenceAnchor(anchor_id=anchor_id, any_of=terms)],
        judge_rubric=(
            "The answer must address the current question using only retrieved evidence, "
            "distinguish uncertainty, and place citations on the claims they support."
        ),
    )


def t(number: int, query: str, contract: TurnContract) -> TurnSpec:
    return TurnSpec(turn_id=f"turn-{number:03d}", query=query, contract=contract)


def _case(
    *,
    case_id: str,
    family_id: str,
    split: Split,
    profile: str,
    domain: str,
    kb: str | None,
    turns: list[TurnSpec],
) -> CaseSpec:
    endpoint, agent_id, agent_type, history, _ = PROFILES[profile]
    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite=SUITE,
        split=split,
        capabilities=[
            Capability.LONG_CONTEXT_DIALOGUE,
            Capability.TOOL_USE,
            *(
                [Capability.RAG_RETRIEVAL, Capability.CITATION]
                if kb
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
            knowledge_base_ids=[KB[kb]] if kb else [],
            knowledge_ids=[],
            summary_model_id=MODEL_ID,
            channel="agent-eval-unseen-capability",
        ),
        turns=turns,
        tags=[
            "unseen-distribution",
            "capability-shaped",
            f"domain:{domain}",
            f"profile:{profile}",
            f"knowledge:{kb or 'none'}",
            "twelve-turns",
            "counterexamples",
        ],
        corpus_version=CORPUS_VERSION if kb else None,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-designed-capability-matrix-20260831",
            needs_codex_review=False,
            reference_answers={},
            metadata={
                "design_basis": "capability-dimensions-not-lexical-substitution",
                "domain": domain,
                "configured_history_turns": history,
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "gold_answer_policy": "contracts-accept-multiple-valid-answers",
            },
        ),
    )


def hr_quick_dev() -> CaseSpec:
    p = "quick-answer"
    turns = [
        t(1, "记一个入职协调事项：海棠批次有6位新人，本轮只做清单，不要替我发送邀请。", state(p, active=[r("batch", all_of=["海棠批次", "6位"])], boundaries=[r("no-invite", any_of=["不发送", "不会发送"], all_of=["邀请"])])),
        t(2, "联系人是孟夏；办公地点还没定。只更新这两个字段。", state(p, active=[r("contact", all_of=["孟夏"])], unknown=[r("office", all_of=["办公地点"], any_of=["没定", "待确认", "未知"])])),
        t(3, "分析一下为什么聊天里不能直接执行HR系统开户。这是在问原因，不是在登记一个新状态。", plain(p, "analysis-not-state", ["原因", "授权", "权限"], judge_rubric="Explain the distinction between analysis and an authorized action; do not invent a completed action or a new project state.")),
        t(4, "先把入职日写成9月14日，背景核验仍待确认。", state(p, active=[r("start-v1", all_of=["9月14日"])], unknown=[r("check", all_of=["背景核验"], any_of=["待确认", "未知"])])),
        t(5, "插一句：试用期这个词通常是什么意思？别把通用解释混进入职事项。", plain(p, "topic-switch", ["试用期"], judge_rubric="Give a general explanation and keep it separate from the tracked onboarding facts.")),
        t(6, "日期改口了：当前是9月21日，9月14日作废。核验状态没变化。请分栏。", state(p, active=[r("start-v2", all_of=["9月21日"])], retired=[r("start-retired", all_of=["9月14日"], any_of=["作废", "废弃"])], unknown=[r("check-still", all_of=["背景核验"], any_of=["待确认", "未知"])], scoped=True)),
        t(7, "不要检索。只告诉我现在的入职日和仍未知的项目。", state(p, active=[r("current-date", all_of=["9月21日"])], unknown=[r("current-unknown", all_of=["背景核验"], any_of=["待确认", "未知"])])),
        t(8, "修改协调方案：加一项伙伴复核；但不要修改任何文件，也不要发邮件。", state(p, active=[r("plan-content", all_of=["伙伴复核"])], boundaries=[r("no-file", any_of=["不修改", "不会修改"], all_of=["文件"]), r("no-email", any_of=["不发送", "不会发送"], all_of=["邮件"])])),
        t(9, "Status update, different word order: Meng Xia remains the contact; the office is pending; do not email anyone. Reply in English.", _base_contract(p, required=[r("english-contact", all_of=["Meng Xia"]), r("english-office", all_of=["office"], any_of=["pending", "unknown"]), r("english-boundary", all_of=["email"], any_of=["do not", "won't", "will not"])], no_retrieval=True)),
        t(10, "我刚才哪句话说明背景核验还没完成？引用我的原话片段，不要补原因。", plain(p, "source-fragment", ["背景核验仍待确认", "背景核验", "待确认"], forbidden=[r("no-invented-reason", any_of=["征信问题", "材料造假"])])),
        t(11, "办公地点确定为北楼3层；原来的未知状态结束。其他事实别动。", state(p, active=[r("office-resolved", all_of=["北楼3层"])], forbidden_inferences=[r("no-old-unknown", all_of=["办公地点"], any_of=["待确认", "未知"])])),
        t(12, "做最终状态摘要：当前事实、已废弃事实、未知/待办、来源归属和行动边界都要分清。", state(p, active=[r("final-date", all_of=["9月21日"]), r("final-office", all_of=["北楼3层"]), r("final-plan", all_of=["伙伴复核"])], retired=[r("final-old-date", all_of=["9月14日"], any_of=["作废", "废弃"])], unknown=[r("final-check", all_of=["背景核验"], any_of=["待确认", "未知"])], boundaries=[r("final-no-file", all_of=["文件"], any_of=["不修改", "不会修改"]), r("final-no-email", all_of=["邮件"], any_of=["不发送", "不会发送"])], scoped=True, max_chars=1100)),
    ]
    return _case(case_id="unseen-dev-quick-hr", family_id="unseen-hr-lifecycle-language", split=Split.DEV, profile=p, domain="human-resources", kb=None, turns=turns)


def product_rag_dev() -> CaseSpec:
    p = "rag-reasoning"
    turns = [
        t(1, "查手册说明Orion设备通电后如何开始校准，并给出处。", cited(p, "warmup", ["8 分钟", "8分钟", "CAL"])),
        t(2, "这台设备编号P-17，现在放在A室；维护人还不知道。", state(p, active=[r("device", all_of=["P-17", "A室"])], unknown=[r("maintainer", all_of=["维护人"], any_of=["不知道", "待确认", "未知"])])),
        t(3, "分析为什么现在不能直接执行恢复出厂。不要真的执行，也不要把分析当成状态登记。", cited(p, "reset-boundary", ["恢复出厂", "离线数据", "支持人员"], required=["恢复出厂", "数据", "支持"])),
        t(4, "网络断了的话能缓存多久？只按手册回答并引用。", cited(p, "offline", ["24 小时", "24小时", "补传"])),
        t(5, "现场看到琥珀色连续闪，但具体电量没测。记录观察和未知值。", state(p, active=[r("amber", all_of=["琥珀色", "闪"])], unknown=[r("battery", all_of=["电量"], any_of=["没测", "未知", "待确认"])])),
        t(6, "P-17搬到C室，A室这个位置失效；维护人仍未知。按生命周期整理。", state(p, active=[r("room-c", all_of=["P-17", "C室"])], retired=[r("room-a", all_of=["A室"], any_of=["失效", "作废", "废弃"])], unknown=[r("maintainer-still", all_of=["维护人"], any_of=["未知", "待确认"])], scoped=True)),
        t(7, "别查资料，只复述我现场观察到的灯色和没测到的量。", state(p, active=[r("observed-color", all_of=["琥珀色"])], unknown=[r("unmeasured", all_of=["电量"], any_of=["没测", "未知"])])),
        t(8, "修改排障方案，加入‘先导出缓存’；但不要修改文件，也不要重置设备。", state(p, active=[r("plan-edit", all_of=["先导出缓存"])], boundaries=[r("no-file-edit", all_of=["文件"], any_of=["不修改", "不会修改"]), r("no-reset", all_of=["重置"], any_of=["不要", "不会", "不执行"])])),
        t(9, "手册里‘不要安装第三方固件’到底限制什么？这是知识问答，仍要检索手册并给引用。", cited(p, "firmware", ["第三方固件", "连接问题"], required=["第三方固件"])),
        t(10, "In English: P-17 is now in Room C, the maintainer is still unknown, and no reset was performed.", _base_contract(p, required=[r("en-device", all_of=["P-17", "Room C"]), r("en-maintainer", all_of=["maintainer"], any_of=["unknown", "pending"]), r("en-reset", all_of=["reset"], any_of=["not", "no"])], no_retrieval=True)),
        t(11, "区分来源：哪些是我现场说的，哪些来自手册？涉及手册的结论要带引用。", cited(p, "source-separation", ["琥珀色", "15%", "4 小时", "4小时"], required=["现场", "手册"])),
        t(12, "综合给出当前设备状态、废弃位置、未知项、排障方案和维护边界；手册事实逐项引用。", cited(p, "product-summary", ["24 小时", "第三方固件", "恢复出厂"], required=["P-17", "C室", "维护人"])),
    ]
    return _case(case_id="unseen-dev-rag-product", family_id="unseen-product-evidence-state", split=Split.DEV, profile=p, domain="product-manual", kb="product", turns=turns)


def project_general_dev() -> CaseSpec:
    p = "general-agent"
    turns = [
        t(1, "依据项目手册说明一条风险记录至少应含哪些信息，给引用。", cited(p, "risk-fields", ["风险描述", "影响", "负责人", "复核日期"])),
        t(2, "登记一个风险：代号R-8，影响是接口延迟；负责人待定。", state(p, active=[r("risk", all_of=["R-8", "接口延迟"])], unknown=[r("owner", all_of=["负责人"], any_of=["待定", "待确认", "未知"])])),
        t(3, "主持人叫周禾。注意：这不代表周禾就是风险负责人。", state(p, active=[r("facilitator", all_of=["主持人", "周禾"])], unknown=[r("owner-still", all_of=["负责人"], any_of=["待定", "待确认", "未知"])], forbidden_inferences=[r("not-owner", all_of=["周禾", "负责人"], any_of=["就是", "确定", "已指定"])])),
        t(4, "为什么‘讨论范围变更’不等于‘已经批准变更’？分析即可，别改项目记录。", cited(p, "change-analysis", ["变更请求", "决策状态", "不等于"], required=["讨论", "批准"])),
        t(5, "当前范围是移动端适配；桌面端是否纳入还不知道。", state(p, active=[r("scope-mobile", all_of=["移动端适配"])], unknown=[r("desktop", all_of=["桌面端"], any_of=["不知道", "待确认", "未知"])])),
        t(6, "改方案内容：补一段桌面端影响分析；但不要修改Jira、文件或任何外部系统。", state(p, active=[r("analysis-plan", all_of=["桌面端影响分析"])], boundaries=[r("no-jira", all_of=["Jira"], any_of=["不修改", "不会修改"]), r("no-external", all_of=["外部系统"], any_of=["不修改", "不会修改"])])),
        t(7, "不要搜索，只列我已经明确的范围和未知范围。", state(p, active=[r("known-scope", all_of=["移动端适配"])], unknown=[r("unknown-scope", all_of=["桌面端"], any_of=["待确认", "未知"])])),
        t(8, "范围更新：桌面端确定不纳入；先前的‘是否纳入’未知状态结束。", state(p, active=[r("desktop-excluded", all_of=["桌面端", "不纳入"])], forbidden_inferences=[r("no-stale-unknown", all_of=["桌面端"], any_of=["待确认", "未知"])])),
        t(9, "换个话题：解释一下决策日志的价值，不要把通用解释写成R-8的新事实。", plain(p, "decision-log", ["决策", "记录", "追溯"], judge_rubric="Explain the general value of a decision log without inventing facts about R-8.")),
        t(10, "R-8负责人现在确定为叶宁，原‘待定’作废；下一复核日期仍未给。", state(p, active=[r("owner-resolved", all_of=["叶宁"])], retired=[r("owner-pending-retired", all_of=["负责人", "待定"], any_of=["作废", "结束", "失效"])], unknown=[r("review-date", all_of=["复核日期"], any_of=["未给", "待确认", "未知"])], scoped=True)),
        t(11, "指出‘负责人不能从主持人推断’这条边界分别来自哪条用户消息和哪条手册原则。手册部分引用。", cited(p, "owner-source", ["会议主持人", "风险负责人", "待确认"], required=["主持人", "负责人"])),
        t(12, "写一份R-8状态快照，清楚区分有效、废弃、未知、来源和不允许的外部操作。", state(p, active=[r("final-risk", all_of=["R-8", "接口延迟"]), r("final-owner", all_of=["叶宁"]), r("final-scope", all_of=["移动端适配", "桌面端", "不纳入"])], retired=[r("final-owner-old", all_of=["负责人", "待定"], any_of=["作废", "失效"])], unknown=[r("final-date", all_of=["复核日期"], any_of=["待确认", "未知"])], boundaries=[r("final-jira", all_of=["Jira"], any_of=["不修改", "不会修改"]), r("final-system", all_of=["外部系统"], any_of=["不修改", "不会修改"])], scoped=True, max_chars=1200)),
    ]
    return _case(case_id="unseen-dev-general-project", family_id="unseen-project-boundary-source", split=Split.DEV, profile=p, domain="project-management", kb="project", turns=turns)


def it_quick_gate() -> CaseSpec:
    p = "quick-answer"
    turns = [
        t(1, "从运行手册查Sev-2首次确认时限，带引用。", cited(p, "sev2-ack", ["Sev-2", "15 分钟", "15分钟"])),
        t(2, "事件编号INC-42，当前影响是部分用户登录慢，根因未知。", state(p, active=[r("incident", all_of=["INC-42", "登录慢"])], unknown=[r("cause", all_of=["根因"], any_of=["未知", "待确认"])])),
        t(3, "当班响应人是Kai，事件负责人还没定，别把两种角色合并。", state(p, active=[r("responder", all_of=["Kai", "当班响应人"])], unknown=[r("lead", all_of=["事件负责人"], any_of=["没定", "待确认", "未知"])], forbidden_inferences=[r("role-merge", all_of=["Kai", "事件负责人"], any_of=["就是", "确定", "已指定"])])),
        t(4, "分析为什么‘建议回滚’并不等于‘已经回滚’。不要执行任何命令。", cited(p, "rollback-auth", ["回滚", "共同确认", "不构成执行授权"], required=["建议", "执行", "授权"])),
        t(5, "错误率刚测到6%，只持续了3分钟；是否满足回滚条件仍未知。", state(p, active=[r("error-observation", all_of=["6%", "3分钟"])], unknown=[r("rollback-condition", all_of=["回滚条件"], any_of=["未知", "待确认", "未满足"])])),
        t(6, "修正：刚才6%读数无效，当前是4%；3分钟这段也废弃。", state(p, active=[r("error-current", all_of=["4%"])], retired=[r("error-old", all_of=["6%", "3分钟"], any_of=["无效", "废弃", "作废"])], scoped=True)),
        t(7, "别检索，只复述INC-42的当前影响和未知根因。", state(p, active=[r("impact-current", all_of=["INC-42", "登录慢"])], unknown=[r("cause-current", all_of=["根因"], any_of=["未知", "待确认"])])),
        t(8, "修改通报文案，补上‘下一次更新时间待定’，但不要修改生产配置或事件系统。", state(p, active=[r("update-copy", all_of=["下一次更新时间", "待定"])], boundaries=[r("no-prod-config", all_of=["生产配置"], any_of=["不修改", "不会修改"]), r("no-incident-system", all_of=["事件系统"], any_of=["不修改", "不会修改"])])),
        t(9, "手册说不能声称已经重启集群。请解释这条知识，但仍需检索并引用；句子里的‘不能’不是禁止你查资料。", cited(p, "operation-boundary", ["重启集群", "声称", "实际变更"], required=["重启", "声称"])),
        t(10, "Root cause remains pending; current error rate is 4%; do not change production. Reply in English.", _base_contract(p, required=[r("en-cause", all_of=["root cause"], any_of=["pending", "unknown"]), r("en-rate", all_of=["4%"]), r("en-boundary", all_of=["production"], any_of=["do not", "will not", "won't"])], no_retrieval=True)),
        t(11, "根据手册列首次通报必须包含的要素，并说明INC-42当前还缺哪一项，带引用。", cited(p, "first-update", ["用户影响", "缓解措施", "下一次更新时间"], required=["用户影响", "下一次更新时间"])),
        t(12, "汇总事件：当前事实、无效读数、未知项、角色归属、通报缺口和操作边界，手册结论引用。", cited(p, "it-summary", ["Sev-2", "根因", "实际变更"], required=["INC-42", "4%", "6%"])),
    ]
    return _case(case_id="unseen-gate-quick-it", family_id="unseen-it-observation-authorization", split=Split.GATE, profile=p, domain="it-operations", kb="it", turns=turns)


def policy_rag_gate() -> CaseSpec:
    p = "rag-reasoning"
    turns = [
        t(1, "按制度查住宿费用2,600元需要哪些审批，逐项引用。", cited(p, "approval-threshold", ["2,000", "直属经理", "财务复核"], required=["经理", "财务"])),
        t(2, "申请代号TR-9，金额2,600元，目的地是L国；税务规则还不知道。", state(p, active=[r("request", all_of=["TR-9", "2,600元", "L国"])], unknown=[r("tax", all_of=["税务规则"], any_of=["不知道", "待确认", "未知"])])),
        t(3, "直属经理是方屿；财务复核人未指定。", state(p, active=[r("manager", all_of=["方屿", "直属经理"])], unknown=[r("finance", all_of=["财务复核人"], any_of=["未指定", "待确认", "未知"])])),
        t(4, "分析‘可以先提交行程’为什么不等于‘已经批准费用’，不要替我提交。", cited(p, "emergency-boundary", ["紧急出差", "最终审批", "不能跳过"], required=["提交", "批准"])),
        t(5, "先记录业务理由为客户现场支持；补充凭证仍未收到。", state(p, active=[r("reason", all_of=["客户现场支持"])], unknown=[r("receipt", all_of=["补充凭证"], any_of=["未收到", "待确认", "未知"])])),
        t(6, "金额改成1,850元，2,600元这个旧值作废；税务规则还是未知。", state(p, active=[r("amount-new", all_of=["1,850元"])], retired=[r("amount-old", all_of=["2,600元"], any_of=["作废", "废弃"])], unknown=[r("tax-still", all_of=["税务规则"], any_of=["未知", "待确认"])], scoped=True)),
        t(7, "不要查制度，只按我刚说的复述当前金额和旧金额。", state(p, active=[r("amount-current", all_of=["1,850元"])], retired=[r("amount-retired", all_of=["2,600元"], any_of=["作废", "废弃"])], scoped=True)),
        t(8, "修改报销说明文字，加入客户现场支持；但不要修改或提交报销单。", state(p, active=[r("description-edit", all_of=["客户现场支持"])], boundaries=[r("no-form-edit", all_of=["报销单"], any_of=["不修改", "不会修改"]), r("no-submit", all_of=["报销单"], any_of=["不提交", "不会提交"])])),
        t(9, "制度知识问答：‘审阅人员不能批准’具体是什么意思？请检索并引用。", cited(p, "reviewer-boundary", ["指定审批人", "批准决定", "审阅人员"], required=["审阅", "审批人"])),
        t(10, "凭证已收到，原未知状态结束；L国税务仍待确认。", state(p, active=[r("receipt-resolved", all_of=["凭证", "已收到"])], unknown=[r("tax-pending", all_of=["L国", "税务"], any_of=["待确认", "未知"])])),
        t(11, "解释当前1,850元对应的审批路径，同时明确税务缺口不能用其他地区惯例补，带引用。", cited(p, "tax-no-inference", ["不超过 2,000", "直属经理", "不得", "其他地区"], required=["1,850元", "直属经理", "税务"])),
        t(12, "输出TR-9最终审阅摘要，区分当前/废弃/未知/来源/行动边界，不作批准结论。", state(p, active=[r("final-request", all_of=["TR-9", "1,850元", "客户现场支持"]), r("final-receipt", all_of=["凭证", "已收到"])], retired=[r("final-old-amount", all_of=["2,600元"], any_of=["作废", "废弃"])], unknown=[r("final-tax", all_of=["L国", "税务"], any_of=["待确认", "未知"])], boundaries=[r("final-no-submit", all_of=["报销单"], any_of=["不提交", "不会提交"]), r("final-no-approve", all_of=["批准"], any_of=["不作", "不会", "不能"])], scoped=True, max_chars=1200)),
    ]
    return _case(case_id="unseen-gate-rag-policy", family_id="unseen-policy-threshold-uncertainty", split=Split.GATE, profile=p, domain="policy-governance", kb="policy", turns=turns)


def customer_general_gate() -> CaseSpec:
    p = "general-agent"
    turns = [
        t(1, "建立客户回访事项：代号Cedar，目标是整理反馈，不要替我联系客户。", state(p, active=[r("customer-task", all_of=["Cedar", "整理反馈"])], boundaries=[r("no-contact", all_of=["客户"], any_of=["不联系", "不会联系"])])),
        t(2, "当前负责人宋澄；回访日期未定。", state(p, active=[r("owner", all_of=["宋澄"])], unknown=[r("date", all_of=["回访日期"], any_of=["未定", "待确认", "未知"])])),
        t(3, "客户提到加载慢，但影响范围没有确认。别自行判断严重级别。", state(p, active=[r("symptom", all_of=["加载慢"])], unknown=[r("impact", all_of=["影响范围"], any_of=["没有确认", "待确认", "未知"])], forbidden_inferences=[r("severity", any_of=["严重级别P1", "严重级别P2", "全量故障"])])),
        t(4, "分析为什么‘建议发补偿券’不是‘已经发券’。只解释，不执行。", plain(p, "analysis-action", ["建议", "执行", "授权"], judge_rubric="Explain proposed content versus an executed external action; do not record a new customer state.")),
        t(5, "回访渠道先记电话；联系人号码没有提供。", state(p, active=[r("channel", all_of=["电话"])], unknown=[r("phone", all_of=["号码"], any_of=["没有提供", "待确认", "未知"])])),
        t(6, "改成视频会议，电话渠道作废；日期依然未定。", state(p, active=[r("channel-new", all_of=["视频会议"])], retired=[r("channel-old", all_of=["电话"], any_of=["作废", "废弃"])], unknown=[r("date-still", all_of=["日期"], any_of=["未定", "待确认", "未知"])], scoped=True)),
        t(7, "不要搜索，也不要调用任何工具。只说当前渠道与旧渠道。", state(p, active=[r("channel-current", all_of=["视频会议"])], retired=[r("channel-retired", all_of=["电话"], any_of=["作废", "废弃"])], scoped=True)),
        t(8, "修改沟通方案，增加‘先确认影响范围’；但不要修改CRM记录。", state(p, active=[r("plan", all_of=["先确认影响范围"])], boundaries=[r("no-crm", all_of=["CRM"], any_of=["不修改", "不会修改"])])),
        t(9, "What is still pending, and what action must not be taken? Answer in English.", _base_contract(p, required=[r("en-pending", any_of=["date", "phone", "impact"], all_of=["pending"]), r("en-boundary", all_of=["CRM"], any_of=["must not", "do not", "will not"])], no_retrieval=True)),
        t(10, "回访日期定为10月8日，号码仍未提供；别把日期的旧未知状态继续保留。", state(p, active=[r("date-resolved", all_of=["10月8日"])], unknown=[r("phone-still", all_of=["号码"], any_of=["未提供", "待确认", "未知"])], forbidden_inferences=[r("date-not-unknown", all_of=["回访日期"], any_of=["待确认", "未知", "未定"])])),
        t(11, "逐条指出负责人、渠道和日期分别来自我的哪次更新。只能引用我说过的片段。", plain(p, "user-source", ["宋澄", "视频会议", "10月8日"], forbidden=[r("no-source-invention", any_of=["CRM显示", "系统记录显示", "后台显示"])])),
        t(12, "最后做跨话题总结：Cedar有效事实、废弃渠道、未知信息、来源和行动边界。别新增客户事实。", state(p, active=[r("final-task", all_of=["Cedar", "整理反馈"]), r("final-owner", all_of=["宋澄"]), r("final-channel", all_of=["视频会议"]), r("final-date", all_of=["10月8日"]), r("final-plan", all_of=["先确认影响范围"])], retired=[r("final-phone-channel", all_of=["电话"], any_of=["作废", "废弃"])], unknown=[r("final-phone", all_of=["号码"], any_of=["未提供", "待确认", "未知"]), r("final-impact", all_of=["影响范围"], any_of=["待确认", "未知"])] , boundaries=[r("final-no-contact", all_of=["客户"], any_of=["不联系", "不会联系"]), r("final-no-crm", all_of=["CRM"], any_of=["不修改", "不会修改"])], scoped=True, max_chars=1200)),
    ]
    return _case(case_id="unseen-gate-general-customer", family_id="unseen-customer-topic-action", split=Split.GATE, profile=p, domain="customer-operations", kb=None, turns=turns)


def portfolio_rag_no_kb_gate() -> CaseSpec:
    """Reason over user-supplied planning data without any retrieval surface."""

    p = "rag-reasoning"
    turns = [
        t(1, "这轮只用我提供的数据做试点组合分析，不查外部资料，也不要替我创建任务。预算上限先记为18万元。", state(p, active=[r("budget", all_of=["18万元"])], boundaries=[r("no-task", all_of=["创建任务"], any_of=["不要", "不会"]) ])),
        t(2, "候选A成本8万、预期覆盖120人；候选B成本11万、覆盖190人；候选C成本6万、覆盖80人。风险等级都还没评。", state(p, active=[r("options", all_of=["候选A", "候选B", "候选C"])], unknown=[r("risk", all_of=["风险等级"], any_of=["没评", "未知", "待确认"])])),
        t(3, "先算哪些两项组合不超过预算，并展示计算过程。不要把覆盖人数当成收益承诺。", plain(p, "bounded-combinations", ["A+C", "14", "B+C", "17"], no_retrieval=True)),
        t(4, "更正一处：B成本其实是13万，11万作废；A和C的数据不变。请更新刚才的组合判断。", state(p, active=[r("b-cost-new", all_of=["13万"])], retired=[r("b-cost-old", all_of=["11万"], any_of=["作废", "旧值"])], scoped=True)),
        t(5, "‘不要替我创建任务’为什么不等于‘不能分析任务怎么拆’？解释这个边界，不要登记新的试点事实。", plain(p, "analysis-boundary", ["分析", "执行", "授权"], no_retrieval=True)),
        t(6, "换个话题：用一句通俗话解释机会成本。不要把这个定义写进候选A/B/C的状态。", plain(p, "topic-switch", ["机会成本"], no_retrieval=True)),
        t(7, "新增约束：A和B不能同时选；C是否需要法务复核仍待确认。当前不要求推荐最终组合。", state(p, active=[r("mutual-exclusion", all_of=["A", "B", "不能同时选"])], unknown=[r("legal", all_of=["C", "法务复核"], any_of=["待确认", "未知"])])),
        t(8, "修改分析方案的内容：增加敏感性分析；但不要修改任何表格文件，也不要在项目系统里落任务。", state(p, active=[r("sensitivity", all_of=["敏感性分析"])], boundaries=[r("no-sheet", all_of=["表格文件"], any_of=["不要修改", "不会修改"]), r("no-system-task", all_of=["项目系统", "任务"], any_of=["不要", "不会"]) ])),
        t(9, "In English, summarize the current budget, the corrected cost of B, and the unresolved review for C. Keep proposal editing separate from changing a file.", _base_contract(p, required=[r("english-state", all_of=["18", "13", "C"])], no_retrieval=True)),
        t(10, "逐条指出18万元、B的13万元和A/B互斥分别来自我的哪次原话；只能摘我说过的片段。", plain(p, "source-trace", ["18万元", "13万", "A和B不能同时选"], no_retrieval=True)),
        t(11, "法务回复了：C不需要额外复核，原待确认状态结束；预算和其他约束不变。", state(p, active=[r("legal-resolved", all_of=["C", "不需要额外复核"])], forbidden_inferences=[r("no-stale-legal", all_of=["法务复核"], any_of=["待确认", "未知"])])),
        t(12, "给我一份决策前快照：有效数据、已作废数据、仍未知事项、来源、分析范围和行动边界分清；可以列可行组合，但不要替我做最终选择。", state(p, active=[r("final-data", all_of=["18万元", "13万", "A", "B", "C"])], retired=[r("final-retired", all_of=["11万"], any_of=["作废", "旧值"])], boundaries=[r("final-no-choice", all_of=["最终选择"], any_of=["不要", "不会"]), r("final-no-file", all_of=["文件"], any_of=["不修改", "不会修改"])], scoped=True, max_chars=1200)),
    ]
    return _case(
        case_id="unseen-gate-rag-portfolio-no-kb",
        family_id="unseen-portfolio-reasoning-no-retrieval",
        split=Split.GATE,
        profile=p,
        domain="portfolio-planning",
        kb=None,
        turns=turns,
    )


def build_cases() -> list[CaseSpec]:
    cases = [
        hr_quick_dev(),
        product_rag_dev(),
        project_general_dev(),
        it_quick_gate(),
        policy_rag_gate(),
        portfolio_rag_no_kb_gate(),
        customer_general_gate(),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid unseen capability matrix: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "unseen-capability-matrix.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
