"""Compile the production-derived, branch-safe three-agent eval suite.

The source conversations were read from production without mutation.  Their
assistant answers are deliberately excluded: Codex preserves the business
intent and correction pattern, then writes executable contracts that allow
many valid answers.  Each RAG case selects exactly one knowledge base and no
individual documents.
"""

from __future__ import annotations

import hashlib
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
    Split,
    TextRule,
    ToolPolicy,
    TurnContract,
    TurnSpec,
)


SUITE = "weknora-production-derived-multiturn-v1"
MODEL_ID = "${AGENT_EVAL_SUMMARY_MODEL_ID}"
CORPUS_VERSION = "${AGENT_EVAL_PRODUCTION_CORPUS_VERSION}"
SOURCE = "codex-curated-production-derived-20260830"

KB = {
    "digital-policies": "${AGENT_EVAL_KB_DIGITAL_POLICIES_ID}",
    "smart-moutai": "${AGENT_EVAL_KB_SMART_MOUTAI_ID}",
    "ip-evidence": "${AGENT_EVAL_KB_IP_EVIDENCE_ID}",
    "cloud-platform": "${AGENT_EVAL_KB_CLOUD_PLATFORM_ID}",
    "weknora-guide": "${AGENT_EVAL_KB_WEKNORA_GUIDE_ID}",
}

PROFILES = {
    "quick-answer": {
        "history": 5,
        "agent": AgentSelector(
            endpoint="knowledge-chat",
            agent_id="builtin-quick-answer",
            agent_type="rag-qa",
        ),
        "max_tools": 4,
    },
    "rag-reasoning": {
        "history": 10,
        "agent": AgentSelector(
            endpoint="agent-chat",
            agent_id="builtin-smart-reasoning",
            agent_type="rag-qa",
        ),
        "max_tools": 8,
    },
    "general-agent": {
        "history": 10,
        "agent": AgentSelector(
            endpoint="agent-chat",
            agent_id="builtin-general-agent",
            agent_type="general-agent",
        ),
        "max_tools": 12,
    },
}

NO_RETRIEVAL_TOOLS = [
    "knowledge_search",
    "grep_chunks",
    "list_knowledge_chunks",
    "query_knowledge_graph",
    "web_search",
    "web_fetch",
]

INTERNAL_PLANNING_TERMS = [
    "Now I have",
    "Now let me",
    "Let me organize",
    "Let me answer",
    "Let me carefully",
    "Let me verify",
    "I have the retrieval results",
    "Looking at the returned evidence",
    "citation handle",
    "evidence handle",
    "evidence map",
    "chunk_id",
    "stop hook",
    "validation error",
    "output contract",
    "contract says",
]


def rule(
    rule_id: str,
    *,
    any_of: list[str] | None = None,
    all_of: list[str] | None = None,
    unless_any_of: list[str] | None = None,
    description: str = "",
) -> TextRule:
    return TextRule(
        rule_id=rule_id,
        any_of=any_of or [],
        all_of=all_of or [],
        unless_any_of=unless_any_of or [],
        description=description,
    )


def internal_planning_rule() -> TextRule:
    return rule(
        "no-internal-planning",
        any_of=INTERNAL_PLANNING_TERMS,
        description="不得向用户暴露检索、契约校验或内部推理过程",
    )


def anchor(
    anchor_id: str,
    *,
    any_of: list[str] | None = None,
    all_of: list[str] | None = None,
    description: str = "",
) -> EvidenceAnchor:
    return EvidenceAnchor(
        anchor_id=anchor_id,
        any_of=any_of or [],
        all_of=all_of or [],
        description=description,
    )


def contract(
    profile_id: str,
    *,
    required: list[TextRule] | None = None,
    forbidden: list[TextRule] | None = None,
    active: list[TextRule] | None = None,
    retired: list[TextRule] | None = None,
    unknown: list[TextRule] | None = None,
    forbidden_inferences: list[TextRule] | None = None,
    boundaries: list[TextRule] | None = None,
    scoped: bool = False,
    evidence: list[EvidenceAnchor] | None = None,
    citation: bool = False,
    no_retrieval: bool = False,
    min_anchors: int | None = None,
    max_chars: int = 1200,
    judge_rubric: str | None = None,
) -> TurnContract:
    evidence = evidence or []
    return TurnContract(
        required_claims=required or [],
        forbidden_claims=[*(forbidden or []), internal_planning_rule()],
        conversation_state=ConversationStateContract(
            active_facts=active or [],
            retired_facts=retired or [],
            unknown_facts=unknown or [],
            forbidden_inferences=forbidden_inferences or [],
            action_boundaries=boundaries or [],
            require_scoped_sections=scoped,
        ),
        evidence_anchors=evidence,
        min_evidence_anchors=(len(evidence) if min_anchors is None else min_anchors),
        citation_required=citation,
        min_citations=1 if citation else 0,
        max_citations=5 if citation else None,
        min_retrieved_sources={"documents": 1} if citation else {},
        tool_policy=ToolPolicy(
            read_only=True,
            forbidden_tools=NO_RETRIEVAL_TOOLS if no_retrieval else [],
        ),
        min_response_chars=1,
        max_response_chars=max_chars,
        max_tool_calls=PROFILES[profile_id]["max_tools"],
        forbid_stale_citations_without_retrieval=citation,
        judge_rubric=judge_rubric,
    )


def turn(turn_number: int, query: str, turn_contract: TurnContract) -> TurnSpec:
    return TurnSpec(
        turn_id=f"turn-{turn_number:03d}",
        query=query,
        contract=turn_contract,
    )


def provenance(
    profile_id: str,
    family_id: str,
    *,
    session_ids: list[str],
    corpus_group: str | None,
    reuse_mode: str,
) -> Provenance:
    source_hash = hashlib.sha256("\n".join(sorted(session_ids)).encode("utf-8")).hexdigest()
    return Provenance(
        source=SOURCE,
        source_hash=source_hash,
        collected_at="2026-08-30T13:50:07.099699+08:00",
        needs_codex_review=False,
        metadata={
            "branch_safe": True,
            "requires_branch_curation": False,
            "gold_answer_policy": "contract-only-no-single-reference-answer",
            "observed_assistant_answers_used_as_gold": False,
            "source_environment": "weknora-prod-read-only",
            "source_session_ids": session_ids,
            "source_problem_family": family_id,
            "source_corpus_group": corpus_group,
            "reuse_mode": reuse_mode,
            "profile_id": profile_id,
            "configured_history_turns": PROFILES[profile_id]["history"],
        },
    )


def make_case(
    *,
    case_id: str,
    family_id: str,
    split: Split,
    profile_id: str,
    capabilities: list[Capability],
    turns: list[TurnSpec],
    kb_group: str | None,
    source_sessions: list[str],
    reuse_mode: str,
    tags: list[str],
) -> CaseSpec:
    return CaseSpec(
        case_id=case_id,
        family_id=family_id,
        suite=SUITE,
        split=split,
        enabled=True,
        capabilities=capabilities,
        agent_profile_id=profile_id,
        agent=PROFILES[profile_id]["agent"],
        setup=CaseSetup(
            knowledge_base_ids=[KB[kb_group]] if kb_group else [],
            knowledge_ids=[],
            summary_model_id=MODEL_ID,
            channel="agent-eval-production-derived",
        ),
        turns=turns,
        tags=[
            *tags,
            f"profile:{profile_id}",
            f"source:{kb_group or 'no-kb'}",
            "production-derived",
            "branch-safe",
            "single-kb-selection",
            "no-gold-answer",
        ],
        corpus_version=CORPUS_VERSION if kb_group else None,
        repetitions=3,
        provenance=provenance(
            profile_id,
            family_id,
            session_ids=source_sessions,
            corpus_group=kb_group,
            reuse_mode=reuse_mode,
        ),
    )


def quick_dev_case() -> CaseSpec:
    profile = "quick-answer"
    no_write = rule(
        "no-write",
        any_of=["不创建", "不会创建", "不得创建", "不修改", "不会修改"],
        all_of=["文件"],
    )
    turns = [
        turn(
            1,
            "在对话里建立季度经营分析任务卡：项目代号‘晨星经营复盘’，目标是找出三项可落地的降本措施。未经明确授权不要创建或修改文件，也不要替我发送材料。只确认任务，不推断措施内容。",
            contract(
                profile,
                active=[
                    rule("project", all_of=["晨星经营复盘"]),
                    rule("goal", all_of=["三项", "降本措施"]),
                ],
                boundaries=[no_write, rule("no-send", any_of=["不发送", "不会发送", "不得发送"])],
                forbidden_inferences=[rule("no-invented-measure", any_of=["裁员", "降薪", "关停门店"])],
                no_retrieval=True,
                max_chars=450,
            ),
        ),
        turn(
            2,
            "业务牵头人记为林岚；审批人尚未确定。只更新人员字段。",
            contract(
                profile,
                active=[rule("owner", all_of=["业务牵头人", "林岚"])],
                unknown=[rule("approver", all_of=["审批人"], any_of=["尚未确定", "待确认", "未知"])],
                no_retrieval=True,
                max_chars=350,
            ),
        ),
        turn(
            3,
            "初始测算预算为80万元。这只是预算，不代表已批准，也不要推断投入方向。",
            contract(
                profile,
                active=[rule("budget-v1", all_of=["80万元"])],
                forbidden_inferences=[rule("not-approved", any_of=["已经批准", "已获批"]), rule("no-budget-scope", any_of=["设备采购", "咨询服务", "系统开发"])],
                no_retrieval=True,
                max_chars=350,
            ),
        ),
        turn(
            4,
            "已确认范围只做流程和费用分析，不包含硬件采购；是否需要对接统一认证仍待确认。",
            contract(
                profile,
                active=[rule("scope", all_of=["流程", "费用分析", "不包含硬件采购"])],
                unknown=[rule("sso", all_of=["统一认证"], any_of=["待确认", "尚未确认", "未知"])],
                no_retrieval=True,
                max_chars=420,
            ),
        ),
        turn(
            5,
            "目标日期定为2026年10月31日，但紧急程度没有确认。只记录日期和未知项。",
            contract(
                profile,
                active=[rule("date", all_of=["2026年10月31日"])],
                unknown=[rule("urgency", all_of=["紧急程度"], any_of=["未确认", "待确认", "未知"])],
                no_retrieval=True,
                max_chars=350,
            ),
        ),
        turn(
            6,
            "预算改为92万元，原80万元作废；批准状态依然未知。请明确当前值、废弃值和未知项。",
            contract(
                profile,
                active=[rule("budget-v2", all_of=["92万元"])],
                retired=[rule("budget-retired", all_of=["80万元"], any_of=["作废", "废弃", "失效"])],
                unknown=[rule("approval-still-unknown", all_of=["批准"], any_of=["未知", "待确认", "未确认"])],
                no_retrieval=True,
                max_chars=450,
            ),
        ),
        turn(
            7,
            "做最终任务卡审计，分成‘当前有效事实/已废弃事实/待确认事项/行动边界’四段。恢复最早给出的项目目标，不要新增任何措施，不要执行操作。",
            contract(
                profile,
                active=[
                    rule("project-final", all_of=["晨星经营复盘", "三项", "降本措施"]),
                    rule("owner-final", all_of=["业务牵头人", "林岚"]),
                    rule("budget-final", all_of=["92万元"]),
                    rule("scope-final", all_of=["流程", "费用分析", "不包含硬件采购"]),
                    rule("date-final", all_of=["2026年10月31日"]),
                ],
                retired=[rule("budget-old-final", all_of=["80万元"], any_of=["作废", "废弃", "失效"])],
                unknown=[
                    rule("approver-final", all_of=["审批人"], any_of=["待确认", "未知", "未确定"]),
                    rule("sso-final", all_of=["统一认证"], any_of=["待确认", "未知", "未确认"]),
                    rule("approval-final", all_of=["批准状态"], any_of=["待确认", "未知", "未确认"]),
                ],
                boundaries=[no_write, rule("no-send-final", any_of=["不发送", "不会发送", "不得发送"])],
                scoped=True,
                forbidden_inferences=[rule("no-new-measures", any_of=["裁员", "降薪", "关停门店"])],
                no_retrieval=True,
                max_chars=1100,
            ),
        ),
    ]
    return make_case(
        case_id="prod-dev-quick-business-ledger-v1",
        family_id="prod-dev-business-ledger-state-supersession",
        split=Split.DEV,
        profile_id=profile,
        capabilities=[Capability.LONG_CONTEXT_DIALOGUE, Capability.TOOL_USE],
        turns=turns,
        kb_group=None,
        source_sessions=["13bf3e24-32dc-4184-8c24-6471fe58ce49"],
        reuse_mode="production-pattern-reconstruction-no-kb",
        tags=["state-lifecycle", "history-window", "action-boundary"],
    )


def rag_dev_case() -> CaseSpec:
    profile = "rag-reasoning"
    cite = lambda *args, **kwargs: contract(profile, citation=True, *args, **kwargs)
    no_levels = rule(
        "no-invented-change-level",
        any_of=["一级变更", "二级变更", "三级变更", "重大变更", "一般变更"],
        description="制度未提供变更等级时不得自行分级",
    )
    turns = [
        turn(
            1,
            "只依据当前知识库回答：信息化系统在建设阶段发生需求变更时，应先做什么、由哪些环节确认？不要套用投产后的运维变更制度，并给出引用。",
            cite(
                required=[rule("impact-first", any_of=["变更影响评估", "影响评估"])],
                forbidden=[no_levels],
                evidence=[anchor("impact-evidence", any_of=["变更影响评估", "影响评估"], description="建设阶段变更影响评估依据")],
                max_chars=900,
            ),
        ),
        turn(
            2,
            "范围再确认：这里是项目建设阶段，不是日常运维；知识库没有变更等级划分。请按制度重写流程，必须说明联合审批以及需要同步更新的项目材料，并给出引用。",
            cite(
                required=[
                    rule("joint-approval", any_of=["联合审批", "共同审批", "会签"]),
                    rule("update-docs", any_of=["需求规格说明书", "项目计划"]),
                ],
                forbidden=[no_levels],
                evidence=[
                    anchor("approval-evidence", any_of=["联合审批", "审批"]),
                    anchor("document-update-evidence", any_of=["需求规格说明书", "项目计划"]),
                ],
                max_chars=1100,
            ),
        ),
        turn(
            3,
            "建立案例台账：项目名‘启航门户升级’，本次变更是新增供应商协同接口，变更申请尚未批准。只区分已确认事实和待确认状态。",
            contract(
                profile,
                active=[rule("project-change", all_of=["启航门户升级", "供应商协同接口"])],
                unknown=[rule("change-approval", all_of=["变更申请"], any_of=["尚未批准", "待审批", "待确认"])],
                max_chars=500,
            ),
        ),
        turn(
            4,
            "预算从120万元调整为126万元；120万元从现在起废弃。审批状态仍然未知。",
            contract(
                profile,
                active=[rule("budget-current", all_of=["126万元"])],
                retired=[rule("budget-old", all_of=["120万元"], any_of=["废弃", "作废", "失效"])],
                unknown=[rule("approval-unknown", all_of=["审批"], any_of=["未知", "待确认", "尚未批准"])],
                max_chars=500,
            ),
        ),
        turn(
            5,
            "计划上线日期是2026年12月15日；是否延期尚未判断，不要推断工期风险。",
            contract(
                profile,
                active=[rule("go-live", all_of=["2026年12月15日"])],
                unknown=[rule("delay", all_of=["延期"], any_of=["尚未判断", "待确认", "未知"])],
                forbidden_inferences=[rule("no-delay-claim", any_of=["确定延期", "必然延期", "工期严重不足"])],
                max_chars=450,
            ),
        ),
        turn(
            6,
            "接口会处理供应商联系人信息，因此数据安全影响已确认需要评估；是否涉及敏感个人信息仍待法务确认。",
            contract(
                profile,
                active=[rule("security-impact", all_of=["数据安全", "影响评估"])],
                unknown=[rule("sensitive-pii", all_of=["敏感个人信息", "法务"], any_of=["待确认", "未确认", "未知"])],
                max_chars=550,
            ),
        ),
        turn(
            7,
            "结合制度说明这类建设阶段变更应由哪些相关方参与审批或确认。只写知识库能支持的角色，不要自造固定审批层级，并给出引用。",
            cite(
                required=[rule("participants", any_of=["项目", "立项部门", "用户部门", "承建"] )],
                forbidden=[no_levels],
                evidence=[anchor("participant-evidence", any_of=["立项部门", "用户部门", "承建单位", "相关部门"])],
                max_chars=900,
            ),
        ),
        turn(
            8,
            "如果变更获批并实施，测试与确认环节应留下什么结果材料？请重点核对变更测试报告和确认要求，给出引用。",
            cite(
                required=[rule("test-report", all_of=["变更", "测试报告"])],
                evidence=[anchor("test-report-evidence", all_of=["变更", "测试报告"])],
                max_chars=800,
            ),
        ),
        turn(
            9,
            "系统交付前，把与本次变更直接相关、需要同步更新或归档的材料整理成核对表。无法从知识库确认的材料要标成待确认，给出引用。",
            cite(
                required=[rule("delivery-materials", any_of=["需求规格说明书", "项目计划", "测试报告", "验收"] )],
                evidence=[anchor("material-evidence", any_of=["需求规格说明书", "项目计划", "测试报告", "验收材料"])],
                max_chars=1200,
            ),
        ),
        turn(
            10,
            "再做一次来源校验：不要引用运维变更等级，也不要把知识库没有规定的时限或审批级别写成制度要求。请分别列出‘制度明确’和‘仍待项目确认’。",
            contract(
                profile,
                required=[rule("scope-separation", all_of=["制度明确"], any_of=["待项目确认", "仍待确认"])],
                forbidden=[no_levels, rule("no-fabricated-deadline", any_of=["必须在24小时", "必须在3个工作日", "必须在5个工作日"])],
                max_chars=1000,
            ),
        ),
        turn(
            11,
            "当前业务补充：项目经理已确认技术方案评审完成；变更申请审批和敏感个人信息判断仍未完成。只更新当前状态，不把评审完成等同于变更获批。",
            contract(
                profile,
                active=[rule("design-reviewed", all_of=["技术方案评审", "完成"])],
                unknown=[
                    rule("change-not-approved", all_of=["变更申请"], any_of=["未完成", "待审批", "未获批"]),
                    rule("pii-not-decided", all_of=["敏感个人信息"], any_of=["未完成", "待确认", "未知"]),
                ],
                forbidden_inferences=[rule("review-not-approval", any_of=["因此变更已获批", "等同于变更获批"])],
                max_chars=700,
            ),
        ),
        turn(
            12,
            "输出‘启航门户升级’本次变更的最终执行清单：恢复最早的变更内容，列出当前预算、上线日期、已完成、待确认、制度步骤和交付证据；明确没有变更等级依据。制度结论给出引用，不要替项目作审批决定。",
            cite(
                required=[
                    rule("final-project", all_of=["启航门户升级", "供应商协同接口"]),
                    rule("final-current", all_of=["126万元", "2026年12月15日"]),
                    rule("final-process", any_of=["影响评估", "联合审批", "测试报告"]),
                ],
                forbidden=[no_levels, rule("no-approval-decision", any_of=["我已批准", "变更已批准", "可以直接实施"])],
                unknown=[
                    rule("final-approval", all_of=["变更申请"], any_of=["待审批", "未批准", "待确认"]),
                    rule("final-pii", all_of=["敏感个人信息"], any_of=["待确认", "未知", "未判断"]),
                ],
                evidence=[
                    anchor("final-process-evidence", any_of=["变更影响评估", "联合审批"]),
                    anchor("final-artifact-evidence", any_of=["需求规格说明书", "项目计划", "变更测试报告"]),
                ],
                max_chars=1800,
            ),
        ),
    ]
    return make_case(
        case_id="prod-dev-rag-construction-change-v1",
        family_id="prod-dev-construction-change-source-correction",
        split=Split.DEV,
        profile_id=profile,
        capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION, Capability.LONG_CONTEXT_DIALOGUE],
        turns=turns,
        kb_group="digital-policies",
        source_sessions=["efaac104-49a7-4eac-b1ee-7666ce2d07e2"],
        reuse_mode="production-derived-extend-branch-safe",
        tags=["source-correction", "document-scope", "history-window", "evidence-calibration"],
    )


def general_dev_case() -> CaseSpec:
    profile = "general-agent"
    cite = lambda *args, **kwargs: contract(profile, citation=True, *args, **kwargs)
    no_mutation = rule(
        "no-skill-mutation",
        any_of=["不会安装", "不安装", "不得安装", "不会执行", "不执行", "不会修改", "不修改"],
    )
    turns = [
        turn(1, "我准备给业务团队做WeKnora入门说明。先概括它能解决哪些知识问答和智能体场景，并给出知识库引用；不要执行任何操作。", cite(required=[rule("weknora-scope", any_of=["知识库", "RAG", "智能体"])], evidence=[anchor("guide-overview", any_of=["知识库", "RAG", "智能体"] )], boundaries=[rule("no-action", any_of=["不执行", "不会执行", "仅说明"])])),
        turn(2, "只依据当前知识库说明WeKnora的三类Skill分别是什么，名称要完整，并给出引用。", cite(required=[rule("three-skill-types", all_of=["轻量", "预加载", "专业"] )], evidence=[anchor("skill-types", all_of=["轻量", "预加载", "专业"] )], max_chars=800)),
        turn(3, "团队想找一个用于制作PPT的Skill。请给出查找和判断是否适用的步骤，不要假装已经找到了具体Skill，也不要安装。", contract(profile, required=[rule("search-evaluate", any_of=["搜索", "查找"], all_of=["PPT"])], forbidden_inferences=[rule("no-fake-result", any_of=["已经找到", "已安装成功", "安装完成"])], boundaries=[no_mutation], max_chars=850)),
        turn(4, "如果搜索结果有多个，不允许使用‘第一个’这类依赖上轮排序的说法。请说明用户应该怎样用明确名称或ID确认目标，仍然不要执行安装。", contract(profile, required=[rule("explicit-selection", any_of=["名称", "ID", "标识"] )], forbidden=[rule("no-ordinal", any_of=["下载第一个", "安装第一个", "选择第一个"])], boundaries=[no_mutation], max_chars=700)),
        turn(5, "比较轻量Skill、预加载运行时Skill和专业Skill的适用场景；不确定的实现细节要明确标注，不要扩展成产品承诺。", cite(required=[rule("skill-comparison", all_of=["轻量", "预加载", "专业"] )], evidence=[anchor("skill-comparison-evidence", all_of=["轻量", "预加载", "专业"] )], max_chars=1200)),
        turn(6, "解释Skill的渐进式披露机制，重点说明为什么不用一次把所有内容塞入上下文，并给出引用。", cite(required=[rule("progressive-disclosure", any_of=["渐进式披露", "逐层", "按需加载"] )], evidence=[anchor("progressive-evidence", any_of=["渐进式披露", "按需加载", "三层"] )], max_chars=900)),
        turn(7, "`read_skill`在这个机制里做什么？只解释读取边界，不执行工具，并给出引用。", cite(required=[rule("read-skill", all_of=["read_skill"], any_of=["读取", "加载"] )], evidence=[anchor("read-skill-evidence", all_of=["read_skill"] )], boundaries=[rule("no-read-execution", any_of=["不执行", "不会执行", "仅解释"])])),
        turn(8, "`execute_skill_script`和`read_skill`有什么区别？说明何时才应执行脚本；当前没有执行授权。", cite(required=[rule("execute-vs-read", all_of=["execute_skill_script", "read_skill"] )], evidence=[anchor("execute-evidence", all_of=["execute_skill_script"] )], boundaries=[rule("no-script-execution", any_of=["不执行", "不会执行", "没有执行授权"])])),
        turn(9, "专业Skill的管理入口或接口范围是什么？只列知识库明确支持的管理能力，不进行新增、修改或删除。", cite(required=[rule("professional-management", all_of=["专业", "Skill"], any_of=["管理", "接口", "API"] )], evidence=[anchor("professional-route-evidence", any_of=["专业 Skill", "professional", "/api/v1"] )], boundaries=[no_mutation], max_chars=1000)),
        turn(10, "团队还要区分快速问答、RAG推理和通用智能体。请按适用任务解释三者，不把推测写成固定产品限制，并给出引用。", cite(required=[rule("three-agents", all_of=["快速问答", "RAG推理", "通用智能体"] )], evidence=[anchor("agent-evidence", any_of=["快速问答", "智能推理", "通用智能体", "General Agent"] )], max_chars=1100)),
        turn(11, "行动边界更新：本次只产出培训说明，不安装Skill、不执行脚本、不创建或修改任何文件。请复述边界和仍需用户确认的目标Skill名称。", contract(profile, required=[rule("training-only", all_of=["培训说明"] )], unknown=[rule("skill-name-unknown", all_of=["Skill", "名称"], any_of=["待确认", "未确认", "未知"] )], boundaries=[no_mutation, rule("no-file", any_of=["不创建", "不会创建", "不修改", "不会修改"], all_of=["文件"])], max_chars=650)),
        turn(12, "生成最终培训提纲：恢复最早的WeKnora入门目标，包含三类Skill、渐进式披露、`read_skill`与`execute_skill_script`区别、三种内置智能体的选用原则，以及当前行动边界。需要知识依据的段落给出引用，不执行任何操作。", cite(required=[rule("final-goal", all_of=["WeKnora", "入门"] ), rule("final-skill-types", all_of=["轻量", "预加载", "专业"] ), rule("final-tools", all_of=["read_skill", "execute_skill_script"] ), rule("final-agents", all_of=["快速问答", "RAG推理", "通用智能体"] )], boundaries=[no_mutation, rule("final-no-file", any_of=["不创建", "不会创建", "不修改", "不会修改"], all_of=["文件"])], evidence=[anchor("final-skill-evidence", any_of=["渐进式披露", "read_skill", "execute_skill_script"] ), anchor("final-agent-evidence", any_of=["快速问答", "智能推理", "通用智能体", "General Agent"] )], max_chars=1900)),
    ]
    return make_case(
        case_id="prod-dev-general-weknora-skills-v1",
        family_id="prod-dev-weknora-skill-selection-action-boundary",
        split=Split.DEV,
        profile_id=profile,
        capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION, Capability.LONG_CONTEXT_DIALOGUE, Capability.TOOL_USE],
        turns=turns,
        kb_group="weknora-guide",
        source_sessions=["caa0d3c8-c5e1-426e-88bf-194b68d89389"],
        reuse_mode="production-derived-extend-branch-safe",
        tags=["skill-selection", "ordinal-safety", "action-boundary", "history-window"],
    )


def quick_gate_case() -> CaseSpec:
    profile = "quick-answer"
    cite = lambda *args, **kwargs: contract(profile, citation=True, *args, **kwargs)
    turns = [
        turn(1, "根据当前知识库，为‘智慧茅台2.0阶段复盘’搭建报告骨架。已完成工作范围是2024年、2025年和2026年上半年，重点看弱电工程、综合安防、交通通行；暂不写预测，给出引用。", cite(required=[rule("years-domains", all_of=["2024", "2025", "2026年上半年", "弱电", "综合安防", "交通通行"] )], evidence=[anchor("period-domain-evidence", any_of=["弱电", "综合安防", "交通通行", "2026年上半年"] )], max_chars=1300)),
        turn(2, "把已完成部分改成正式报告体，每个领域一个段落；只写知识库能证明已经完成的事项，不要输出检索说明。", cite(required=[rule("three-domain-paragraphs", all_of=["弱电", "综合安防", "交通通行"] )], evidence=[anchor("completed-domain-evidence", any_of=["弱电", "安防", "交通", "通行"] )], max_chars=1600)),
        turn(3, "再收紧口径：已完成部分删除‘预计、计划、拟开展、将要’等未来内容；无法确认完成状态的事项不纳入。只输出修订后的正文。", contract(profile, required=[rule("completed-only", any_of=["完成", "建成", "上线", "投用"] )], forbidden=[rule("no-future", any_of=["预计", "拟开展", "将要", "计划实施"] )], max_chars=1600)),
        turn(4, "单独核对2024年的三领域工作，事实后给出引用；如果某领域证据不足，明确写证据不足，不用其他年份补齐。", cite(required=[rule("year-2024", all_of=["2024"] )], evidence=[anchor("evidence-2024", any_of=["2024年", "2024"] )], max_chars=1400)),
        turn(5, "单独核对2025年的三领域工作，仍只写已完成事实并给出引用。", cite(required=[rule("year-2025", all_of=["2025"] )], evidence=[anchor("evidence-2025", any_of=["2025年", "2025"] )], max_chars=1400)),
        turn(6, "单独核对2026年上半年的三领域工作，不能把下半年打算当成已完成，给出引用。", cite(required=[rule("first-half-2026", all_of=["2026年上半年"] )], forbidden=[rule("no-h2-as-completed", any_of=["2026年下半年已完成", "下半年已经完成"] )], evidence=[anchor("evidence-2026-h1", any_of=["2026年上半年", "上半年工作总结"] )], max_chars=1400)),
        turn(7, "另起‘2027规划目标’一节，只依据《智慧茅台2.0顶层规划（2024-2027年）》说明目标。用户给出的‘1+3+N/一个生态、三大能力、多层项目’只是待核对说法：若2.0没有同样表述，不得强行对应，应摘出知识库实际采用的产业链数字生态圈和规划方向，给出引用。", cite(required=[rule("planning-2027", all_of=["2027"] ), rule("actual-top-plan", any_of=["产业链数字生态圈", "管控数字化", "产业数字化", "数字化底座", "数字化治理"] )], forbidden=[rule("no-forced-one-three-n", any_of=["2.0明确采用1+3+N", "2.0原文是1+3+N", "完全对应一个生态、三大能力、多层项目"] )], evidence=[anchor("top-plan-evidence", any_of=["产业链数字生态圈", "管控数字化", "产业数字化", "数字化底座", "数字化治理"] )], max_chars=1400)),
        turn(8, "输出最终报告：第一部分按弱电工程、综合安防、交通通行三个段落总结2024、2025、2026年上半年已完成工作；第二部分单列2027规划目标，并使用2.0知识库实际表述，不强行套入‘1+3+N’。必须区分完成事实与规划目标，关键事实给出引用，不编造金额或成效。", cite(required=[rule("final-periods", all_of=["2024", "2025", "2026年上半年", "2027"] ), rule("final-domains", all_of=["弱电", "综合安防", "交通通行"] ), rule("final-plan-language", any_of=["产业链数字生态圈", "管控数字化", "产业数字化", "数字化底座", "数字化治理"] )], forbidden=[rule("final-no-forced-framework", any_of=["2.0明确采用1+3+N", "2.0原文是1+3+N"] )], forbidden_inferences=[rule("no-invented-money", any_of=["共投资1亿元", "共投资2亿元", "节约1亿元"] )], evidence=[anchor("final-completed-evidence", any_of=["2024年", "2025年", "2026年上半年"] ), anchor("final-plan-evidence", any_of=["产业链数字生态圈", "管控数字化", "产业数字化", "数字化底座", "数字化治理"] )], max_chars=2300)),
    ]
    return make_case(
        case_id="prod-gate-quick-smart-moutai-report-v1",
        family_id="prod-gate-smart-moutai-temporal-report-revision",
        split=Split.GATE,
        profile_id=profile,
        capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION, Capability.LONG_CONTEXT_DIALOGUE],
        turns=turns,
        kb_group="smart-moutai",
        source_sessions=["13bf3e24-32dc-4184-8c24-6471fe58ce49"],
        reuse_mode="production-derived-reconstruct-single-kb",
        tags=["report-revision", "temporal-grounding", "completed-vs-planned", "history-window"],
    )


def rag_gate_case() -> CaseSpec:
    profile = "rag-reasoning"
    cite = lambda *args, **kwargs: contract(profile, citation=True, *args, **kwargs)
    no_overclaim = rule(
        "no-unsupported-exact-match",
        any_of=["技术逻辑完全一致", "能够证明专利来源于技术文档", "已经证明技术来源"],
        unless_any_of=["不能判定", "不足以", "无法证明", "不支持"],
    )
    turns = [
        turn(1, "对当前知识库中的专利与项目技术文档建立相关性分析方法。核心要求：比较主要技术逻辑，不允许只凭共同关键词或基础技术下结论；先给方法，不作批量结论。", contract(profile, required=[rule("method-core", all_of=["核心", "技术逻辑"], any_of=["关键词", "基础技术"] )], forbidden=[no_overclaim], max_chars=1100)),
        turn(2, "定义‘完全一致、高度一致、一般相关、证据不足’四档的判定边界，完全一致必须要求关键步骤或结构形成可对应的技术链。", contract(profile, required=[rule("four-levels", all_of=["完全一致", "高度一致", "一般相关", "证据不足", "关键"] )], max_chars=1200)),
        turn(3, "先分析CN202011593952.0的核心技术问题、关键步骤和必要技术特征。只引用专利本身能支持的内容。", cite(required=[rule("patent-2020", all_of=["CN202011593952.0"] )], evidence=[anchor("patent-2020-evidence", any_of=["202011593952", "电子标签及其信息传输方法"] )], max_chars=1500)),
        turn(4, "再查看项目技术文档中的防伪溯源接口，核对keyIndex、random、token、reverseUid、rawData、rawDataLockFlag分别处于什么数据或校验流程。不要因为参数名出现就判为同一技术，给出引用。", cite(required=[rule("interface-params", all_of=["keyIndex", "random", "token", "reverseUid", "rawData", "rawDataLockFlag"] )], evidence=[anchor("interface-parameter-evidence", any_of=["keyIndex", "reverseUid", "rawDataLockFlag"] )], max_chars=1800)),
        turn(5, "现在比较CN202011593952.0与上述接口：共同使用身份或安全数据是否足以证明技术来源？给出支持点、差异点和当前等级；证据不足时必须降级。", cite(required=[rule("support-difference-grade", all_of=["支持点", "差异", "证据"] )], forbidden=[no_overclaim], evidence=[anchor("comparison-2020-evidence", any_of=["电子标签", "信息传输", "keyIndex", "rawData"] )], max_chars=1800)),
        turn(6, "假设业务方坚持要说明两者有关联，也只能提出可核验的关联角度。请列出还必须补到哪些流程、字段关系或结构证据，不能把假设写成事实。", contract(profile, required=[rule("verification-needed", any_of=["还需", "需要补充", "待核验"], all_of=["证据"] )], forbidden=[no_overclaim], max_chars=1300)),
        turn(7, "‘配置字’可能只是术语差异。请说明术语映射在什么证据条件下才成立，以及什么情况下必须判为不可映射。", contract(profile, required=[rule("term-mapping", all_of=["术语", "映射", "证据"] )], forbidden=[no_overclaim], max_chars=1200)),
        turn(8, "转到CN201010184037.6：提取其数据传输加解密的关键流程，再与项目接口的安全参数进行技术链比较，给出引用。", cite(required=[rule("patent-2010", all_of=["CN201010184037.6", "加解密"] )], evidence=[anchor("patent-2010-evidence", any_of=["201010184037", "数据传输加解密方法"] ), anchor("project-security-evidence", any_of=["keyIndex", "random", "token", "reverseUid"] )], max_chars=1900)),
        turn(9, "针对keyIndex、random、token、reverseUid、rawData、rawDataLockFlag，分别说明项目文档语境下的作用，并与CN201010184037.6里的对应概念区分；没有一一对应证据就写没有。", cite(required=[rule("parameter-differences", all_of=["keyIndex", "random", "token", "reverseUid", "rawData", "rawDataLockFlag"] )], forbidden=[no_overclaim], evidence=[anchor("parameter-context-evidence", any_of=["keyIndex", "rawDataLockFlag", "reverseUid"] )], max_chars=2100)),
        turn(10, "把当前证据按‘专利核心特征证据/项目实现证据/二者映射证据/仍缺证据’四栏整理，不能用同词共现替代映射证据。", contract(profile, required=[rule("four-evidence-columns", all_of=["专利核心特征", "项目实现", "映射证据", "仍缺证据"] )], forbidden=[no_overclaim], max_chars=1700)),
        turn(11, "再分析CN201110259552.0的防转移标签技术：先提取核心结构和作用机制，再判断项目文档是否存在同层级技术链，给出引用；不得沿用前两个专利的结论。", cite(required=[rule("patent-2011", all_of=["CN201110259552.0", "防转移"] )], forbidden=[no_overclaim], evidence=[anchor("patent-2011-evidence", any_of=["201110259552", "具有防转移功能"] )], max_chars=1900)),
        turn(12, "形成最终审查矩阵，分别覆盖CN202011593952.0、CN201010184037.6、CN201110259552.0。每项写核心技术链、项目对应证据、关键差异、等级和置信边界；恢复最早的‘不能只看关键词’原则。除非有逐步技术链证据，否则不得判完全一致或证明来源。关键结论给出引用。", cite(required=[rule("all-patents", all_of=["CN202011593952.0", "CN201010184037.6", "CN201110259552.0"] ), rule("final-method", all_of=["核心技术链", "关键差异", "等级", "证据"] )], forbidden=[no_overclaim], evidence=[anchor("final-patent-evidence", any_of=["电子标签及其信息传输方法", "数据传输加解密方法", "具有防转移功能"] ), anchor("final-project-evidence", any_of=["keyIndex", "random", "token", "reverseUid", "rawDataLockFlag"] )], max_chars=2800)),
    ]
    return make_case(
        case_id="prod-gate-rag-patent-evidence-v1",
        family_id="prod-gate-patent-core-logic-evidence-calibration",
        split=Split.GATE,
        profile_id=profile,
        capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION, Capability.LONG_CONTEXT_DIALOGUE],
        turns=turns,
        kb_group="ip-evidence",
        source_sessions=["5bbabfcd-91ec-41cc-ad09-e9445a8acd13"],
        reuse_mode="production-derived-merge-sources-into-single-kb",
        tags=["evidence-calibration", "anti-keyword-overfit", "confidence-boundary", "history-window"],
    )


def general_gate_case() -> CaseSpec:
    profile = "general-agent"
    cite = lambda *args, **kwargs: contract(profile, citation=True, *args, **kwargs)
    no_fake = rule("no-invented-investment", any_of=["总投资10亿元", "每年投资1亿元", "投资额约5亿元"] )
    turns = [
        turn(1, "依据当前知识库起草茅台云私有云平台情况说明的材料框架，包含定位、技术架构、解决方案与产品、信创、资源使用、投资和下一步规划。先列证据框架，不编数字，给出引用。", cite(required=[rule("seven-sections", all_of=["定位", "技术架构", "解决方案", "信创", "资源", "投资", "下一步"] )], evidence=[anchor("cloud-overview-evidence", any_of=["私有云", "OpenStack", "茅台云"] )], max_chars=1600)),
        turn(2, "核对平台部署形态、OpenStack架构、生产中心与车间灾备中心，以及承载系统规模。只写有来源的数字并给出引用。", cite(required=[rule("architecture-centers", all_of=["OpenStack", "生产中心", "车间", "130"] )], evidence=[anchor("architecture-evidence", any_of=["OpenStack", "17个制酒车间", "130余个", "130+"] )], max_chars=1400)),
        turn(3, "整理知识库能够证明的安全防护与合规能力；WAF、VPN、堡垒机等只有检索到才写，不要把用户最初设想当作文档事实。", cite(required=[rule("security-sourced", any_of=["防火墙", "WAF", "VPN", "堡垒机", "等保"] )], evidence=[anchor("security-evidence", any_of=["防火墙", "WAF", "VPN", "堡垒机", "等级保护"] )], max_chars=1300)),
        turn(4, "说明国产化资源池采用的服务器、操作系统、数据库和中间件等能力；品牌或比例没有证据就不写，给出引用。", cite(required=[rule("domestic-stack", all_of=["服务器", "操作系统", "数据库", "中间件"] )], evidence=[anchor("domestic-evidence", any_of=["国产化资源池", "自主可控", "国产服务器"] )], max_chars=1300)),
        turn(5, "根据2026年6月运维月报核对计算资源：CPU剩余量、分配率和6月使用率分别是多少？数字后给出引用。", cite(required=[rule("cpu-metrics", all_of=["10424", "66.93%", "37.81%"] )], evidence=[anchor("cpu-evidence", all_of=["10424", "66.93%"] )], max_chars=750)),
        turn(6, "继续核对内存：剩余量、分配率和6月使用率分别是多少？不要把分配率写成使用率，给出引用。", cite(required=[rule("memory-metrics", all_of=["50641", "58.11%", "63.79%"] )], evidence=[anchor("memory-evidence", all_of=["50641", "58.11%"] )], max_chars=750)),
        turn(7, "汇总分布式块、对象和文件存储的剩余容量与6月使用率，单位和存储类型不能串位，给出引用。", cite(required=[rule("storage-capacity", all_of=["755.12", "561.02", "532.87"] )], evidence=[anchor("storage-evidence", any_of=["755.12", "561.02", "532.87"] )], max_chars=1200)),
        turn(8, "比较5月到6月的资源变化，只给能从月报核对出的趋势和数字；不要把容量剩余量变化误写成使用率变化。", cite(required=[rule("month-comparison", all_of=["5月", "6月"] )], evidence=[anchor("monthly-evidence", any_of=["34.25%", "38.06%", "30.22%", "43.38%", "40.34%", "44.16%"] )], max_chars=1300)),
        turn(9, "投资部分明确排除公有云。请只汇总知识库可归属于私有云的合同或项目投资；无法形成总体投资或年度投资时明确证据缺口，不得估算。", cite(required=[rule("private-only", all_of=["不含公有云"], any_of=["证据不足", "无法", "待核实"] )], forbidden=[no_fake], evidence=[anchor("private-investment-evidence", any_of=["茅台云扩容", "平台服务采购", "合同"] )], max_chars=1500)),
        turn(10, "下一步规划也只写知识库明确提出的扩容、信创或运营安排；把已完成事实与规划分开，未找到时间表就标注待确认。", cite(required=[rule("plan-separated", all_of=["已完成", "规划"], any_of=["待确认", "未明确"] )], evidence=[anchor("planning-evidence", any_of=["扩容", "下一步", "规划", "信创"] )], max_chars=1400)),
        turn(11, "把材料改成正式报告体，但资源数字只保留CPU、内存和主要存储的汇总指标；投资仍不含公有云，未知数据不得补齐。", contract(profile, required=[rule("report-scope", all_of=["CPU", "内存", "存储", "不含公有云"] )], forbidden=[no_fake], max_chars=1800)),
        turn(12, "输出最终茅台云私有云情况说明：恢复最早要求的七个板块，保留OpenStack、生产与灾备布局、信创栈、2026年6月CPU/内存/主要存储指标；投资排除公有云，规划与现状分开。每个可核验板块给出引用，任何缺口明确说明，不得猜测。", cite(required=[rule("final-seven", all_of=["定位", "技术架构", "解决方案", "信创", "资源", "投资", "下一步"] ), rule("final-architecture", all_of=["OpenStack", "生产中心", "灾备"] ), rule("final-resource", all_of=["10424", "50641", "755.12"] ), rule("final-public-exclusion", all_of=["不含公有云"] )], forbidden=[no_fake], evidence=[anchor("final-cloud-overview", any_of=["OpenStack", "生产中心", "灾备中心"] ), anchor("final-cloud-resource", any_of=["10424", "50641", "755.12"] )], max_chars=2800)),
    ]
    return make_case(
        case_id="prod-gate-general-cloud-report-v1",
        family_id="prod-gate-cloud-multisource-synthesis",
        split=Split.GATE,
        profile_id=profile,
        capabilities=[Capability.RAG_RETRIEVAL, Capability.CITATION, Capability.LONG_CONTEXT_DIALOGUE, Capability.TOOL_USE],
        turns=turns,
        kb_group="cloud-platform",
        source_sessions=["190a38fd-3d63-4b99-8ebd-82b13752ee5b"],
        reuse_mode="production-derived-extend-branch-safe",
        tags=["multi-document-kb", "numeric-grounding", "fact-vs-plan", "history-window"],
    )


def build_cases() -> list[CaseSpec]:
    cases = [
        quick_dev_case(),
        rag_dev_case(),
        general_dev_case(),
        quick_gate_case(),
        rag_gate_case(),
        general_gate_case(),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid production-derived dataset: " + "; ".join(errors))
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    target = root / "datasets" / "production-multiturn-ready.v1.jsonl"
    cases = build_cases()
    write_jsonl(target, cases)
    print(
        f"wrote {len(cases)} cases / {sum(len(case.turns) for case in cases)} turns "
        f"to {target} sha256={dataset_sha256(cases)}"
    )


if __name__ == "__main__":
    main()
