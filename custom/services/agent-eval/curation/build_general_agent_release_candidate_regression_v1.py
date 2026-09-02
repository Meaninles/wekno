"""Build the final general-agent release-candidate RAG regression.

Six frozen conversations preserve the exact questions used by the prior
component run.  A seventh case was authored only after capability isolation
and terminal-integrity changes, against a new knowledge base and a differently
ordered dialogue.  Quality remains complete-conversation Codex judgment only;
no answer key, required claim, Judge rubric, or repair feedback reaches the SUT.
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
    from .build_general_agent_post_salience_regression_v1 import (
        build_cases as build_frozen_component_cases,
    )
    from .build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )
except ImportError:  # pragma: no cover - direct script execution
    from build_general_agent_post_salience_regression_v1 import (
        build_cases as build_frozen_component_cases,
    )
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )


SUITE = "weknora-general-agent-release-candidate-regression-v1"
CORPUS_VERSION = "general-agent-release-candidate-rag-corpora-v1"
KB_ID = "${AGENT_EVAL_KB_RELEASE_CANDIDATE_ARCHIVE_ID}"
EXPECTED_CASE_COUNT = 7


def _retarget_frozen_case(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata.update(
        {
            "component_regression": "general-agent-release-candidate-v1",
            "source_questions_unchanged": True,
            "full_flow_run_consumed": False,
        }
    )
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "post-salience-question-preserving-release-candidate-regression",
                    "metadata": metadata,
                }
            ),
        }
    )


def _new_archive_case() -> CaseSpec:
    profile = "general-agent"
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    turns = [
        turn(
            1,
            "先在聊天里记一个数字档案包 AR-19：内容概述是‘海岸声学观测原始数据’，元数据管理员为 Amina；提交单位和包级保留级别都未知。保存项目总体按长期保留管理，但不要把项目级信息填进 AR-19。不要写数字仓储，也不要创建或修改文件。",
        ),
        turn(
            2,
            "查当前指南：元数据管理员和保存复核人分别负责什么？说明角色职责即可，不能据此说 AR-19 已整理材料、已复核或没开始复核；给真实引用。",
        ),
        turn(
            3,
            "保存复核人现在定为 Benoit。这里只确定谁承担角色；复核是开始、未开始、完成还是失败都没有信息，保持未知。请用一句自然语言确认，不检索。",
        ),
        turn(
            4,
            "只输出一行 JSON，键限 package_id、submitting_unit、summary、metadata_steward、preservation_reviewer、review_lifecycle、package_retention_class；不要用任何工具。",
        ),
        turn(
            5,
            "从指南查进入保存入库资格判断需要哪两项材料，以及材料齐备和实际入库为什么是两回事；逐项引用。",
        ),
        turn(
            6,
            "更新 AR-19：校验和清单已收到；提交单位签署的权利声明还没提供。材料不齐只表示权利声明待补，入库资格和实际入库状态都保持未知，不能写成拒绝。",
        ),
        turn(
            7,
            "Reply in English with exactly four bullets: package ID, received eligibility material, pending material, and actual ingest status. No tools.",
        ),
        turn(
            8,
            "提交单位更正为‘北湾生态站’，原来的未知结束；包级保留级别仍未知。只说这两个字段变化，不搜索。",
        ),
        turn(
            9,
            "指南里‘不得运行未经批准的格式迁移插件’限制的是哪种行为？这句话是在问制度，不是让你运行插件、命令或脚本，也不是禁止检索。查证并引用。",
        ),
        turn(
            10,
            "另一个档案包如果检测到可执行恶意载荷，指南要求如何处理、升级给谁？这是独立假设，请检索引用，但别把恶意载荷写进 AR-19。",
        ),
        turn(
            11,
            "把聊天里的交接备注改成‘等待提交单位签署的权利声明’。直接给出更新后的备注文字；不要建档案包、写仓储、生成文件、发邮件或运行任何插件。",
        ),
        turn(
            12,
            "权利声明现已由北湾生态站签署，旧的待补状态结束。两项材料现在齐备，但我没有提供数字仓储成功事件，所以实际入库状态仍未知；不要把它写成已入库或未入库。",
        ),
        turn(
            13,
            "用中文完成 AR-19 交接：分开写当前事实、已废弃值、仍未知项、项目级信息、独立假设、聊天备注、每条用户事实的原文来源和持续生效的外部操作边界；本轮重新检索指南并逐项引用角色与生命周期、材料资格、实际入库事件、迁移插件和独立风险规则。不要把规则当成这个包已发生的动作。",
        ),
    ]
    return CaseSpec(
        case_id="release-candidate-v1-general-digital-archive-ingest",
        family_id="release-candidate-unseen-digital-archive-capabilities",
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
            channel="agent-eval-general-release-candidate",
        ),
        turns=turns,
        tags=[
            "release-candidate-regression",
            "new-after-capability-isolation",
            "codex-complete-conversation-review",
            "explicit-independent-knowledge-base",
            "long-context",
            "domain:digital-archives",
            "profile:general-agent",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-release-candidate-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "capability-isolation-source-polarity-and-terminal-integrity",
                "domain": "digital-archives",
                "created_after_capability_isolation_change": True,
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


def build_cases() -> list[CaseSpec]:
    source = build_frozen_component_cases()
    cases = [_retarget_frozen_case(case) for case in source]
    cases.append(_new_archive_case())
    if len(cases) != EXPECTED_CASE_COUNT:
        raise ValueError(f"expected {EXPECTED_CASE_COUNT} cases, got {len(cases)}")
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid general-agent release-candidate regression: " + "; ".join(errors))
    for current in cases[-1].turns:
        if current.contract != neutral_contract():
            raise ValueError(f"non-neutral turn contract: {cases[-1].case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "general-agent-release-candidate-regression.v1.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
