"""Build a balanced post-routing RAG regression on top of frozen V4 cases.

V1-V4 remain immutable. V5 retains every earlier capability conversation and
adds one genuinely new water-quality incident workflow executed independently
by all three production agent profiles. The shared conversation permits a
direct profile comparison without encoding an answer key, lexical trigger, or
semantic gate. Complete-conversation Codex review remains the sole quality
decision.
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
    from .build_rag_primary_codex_matrix_v4 import build_cases as build_v4_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )
    from build_rag_primary_codex_matrix_v4 import build_cases as build_v4_cases


SUITE = "weknora-rag-primary-codex-matrix-v5"
CORPUS_VERSION = "rag-primary-corpora-v5"
WATER_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_WATER_ID}"
WATER_FAMILY_ID = "rag-primary-water-incident-evidence-state-and-action"


def _retarget_v4(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata["matrix_revision"] = 5
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-rag-primary-matrix-v5-retained-capability",
                    "metadata": metadata,
                }
            ),
        }
    )


def water_incident_turns():
    return [
        turn(1, "先查当前知识库：仪器异常要算完成现场复核，需要哪两类可验证材料？请给依据。"),
        turn(2, "记录 WV-204：站点是 River Gate 7；首次从巡检员的移动端报告收到；事件协调人是 Mei；现场读数没有提供；校准结果也没有提供。全网计划监测浊度、余氯和电导率，但 WV-204 涉及哪些参数目前未知。先只在聊天里整理。"),
        turn(3, "把站点事实、全网计划信息和未知项分开。‘没有提供现场读数’只说明读数字段未知，不等于巡检员没去现场，也不等于读数为零。"),
        turn(4, "重新查指南，说明事件协调人、现场采样保管人和校准负责人各自做什么并引用；通用职责不能写成 WV-204 已完成事项。"),
        turn(5, "现场采样保管人定为 Ivo。只输出 JSON，字段限 incident_id、site、coordinator、sample_custodian、sampling_lifecycle；指定角色本身不说明采样开始、没开始或完成。本轮不要检索。"),
        turn(6, "指南为什么说没有新告警不能证明事件已经关闭？查证并引用。这是在问规则，不是让你操作监测系统。"),
        turn(7, "更新当前记录：批准渠道的原始仪器告警已经收到，来源是 historian event H-883；旧的移动端报告仍是首次报告渠道，但不再是告警证据来源。独立现场读数仍未知。"),
        turn(8, "现场采样安排在明天 09:00，所以采样是明确待执行，不是未知，也不是已开始。校准结果依然只是未知，别顺手改成待执行。"),
        turn(9, "切到另一个独立假设：Station Delta 已确认高浊度且同时失去余氯，风险怎么分级、升给谁？查库引用，但不要把它写进 WV-204。"),
        turn(10, "回到 WV-204，把聊天里的交接说明补一句：‘等待独立现场读数和校准结果。’只给文字，不建工单、不写文件、不发消息、不校准仪器、不采样，也不要运行命令。"),
        turn(11, "逐字摘录我给出的站点、协调人、采样保管人、告警证据来源和采样安排片段；只做用户消息溯源，不查知识库。"),
        turn(12, "Reply in English with only the current site, coordinator, sample custodian, sampling lifecycle, field-reading status, calibration-result status, and closure status. Do not use tools this turn."),
        turn(13, "给 WV-204 做最终交接：当前事实、废弃或被替代的表述、明确待执行、仍未知、项目级信息、用户原文来源、适用指南规则和外部操作边界分开写。规则本轮重新检索并逐项给真实引用。"),
    ]


def water_incident_case(profile: str) -> CaseSpec:
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    return CaseSpec(
        case_id=f"rag-primary-v5-{profile}-water-incident",
        family_id=f"{WATER_FAMILY_ID}-{profile}",
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
            knowledge_base_ids=[WATER_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=water_incident_turns(),
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "post-model-owned-tool-selection-regression",
            "explicit-knowledge-base",
            "long-context",
            "cross-profile-comparison",
            "domain:water-quality-operations",
            f"profile:{profile}",
            "knowledge:water-quality-incident-guide",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-water-rag-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "cross-profile-evidence-lifecycle-source-scope-and-action-boundary",
                "domain": "water-quality-operations",
                "created_after_model_owned_tool_selection": True,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
                "matrix_revision": 5,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [
        *(_retarget_v4(case) for case in build_v4_cases()),
        *(water_incident_case(profile) for profile in PROFILE_RUNTIME),
    ]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix v5: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v5.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
