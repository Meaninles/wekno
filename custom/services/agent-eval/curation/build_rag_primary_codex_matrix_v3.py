"""Build the third RAG-primary matrix with a post-V6 unseen regression.

V1 and V2 stay immutable.  V3 retains their conversations under a new frozen
identity and adds a genuinely new editorial-workflow corpus.  The new dialogue
starts from an incomplete real record and interleaves policy questions, scope
changes, a detached hypothetical and transformations; it is not a noun-swapped
copy of an earlier question sequence.  Quality remains whole-conversation
Codex judgment with neutral transport-only turn contracts.
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
    from .build_rag_primary_codex_matrix_v2 import build_cases as build_v2_cases
except ImportError:  # pragma: no cover - direct script execution
    from build_rag_primary_codex_matrix_v1 import (
        MODEL_ID,
        PROFILE_RUNTIME,
        neutral_contract,
        turn,
    )
    from build_rag_primary_codex_matrix_v2 import build_cases as build_v2_cases


SUITE = "weknora-rag-primary-codex-matrix-v3"
CORPUS_VERSION = "rag-primary-corpora-v3"
EDITORIAL_KB_ID = "${AGENT_EVAL_KB_RAG_PRIMARY_EDITORIAL_ID}"


def _retarget_v2(case: CaseSpec) -> CaseSpec:
    metadata = dict(case.provenance.metadata)
    metadata["matrix_revision"] = 3
    return case.model_copy(
        update={
            "suite": SUITE,
            "corpus_version": CORPUS_VERSION,
            "provenance": case.provenance.model_copy(
                update={
                    "source": "codex-rag-primary-matrix-v3-retained-capability",
                    "metadata": metadata,
                }
            ),
        }
    )


def editorial_release_case() -> CaseSpec:
    profile = "general-agent"
    endpoint, agent_id, agent_type, history_turns = PROFILE_RUNTIME[profile]
    turns = [
        turn(1, "我在整理稿件 M-42：协调编辑是 Anya；投稿作者没说，内容复核人也没定。编辑计划面向医疗从业者，但这篇稿件的目标读者还不知道。先只在聊天里记，别碰 CMS 或文件。"),
        turn(2, "先别套完整表格。用两句话说清现在确定的和仍未知的，尤其别把我当投稿作者。"),
        turn(3, "查当前知识库说明协调编辑和内容复核人分别负责什么，并给出处；职责规则不代表 M-42 已做完这些事。"),
        turn(4, "内容复核人现在指定为 Tomas。注意，我只是在定角色，没说复核开始了还是没开始，更没说完成。"),
        turn(5, "按库里的流程解释：为什么不能因为校样看起来没问题就说已经发布？这是问规则，不是让你发布。要引用。"),
        turn(6, "这条只回一行 JSON，字段仅限 manuscript_id、author、coordinating_editor、reviewer、review_lifecycle、manuscript_audience；不要检索。"),
        turn(7, "换个话题做假设：另一篇文章意外写出了某人的家庭住址，手册把它当什么风险？查证并引用，但别记到 M-42。"),
        turn(8, "回到 M-42：工作标题定为《夜班监测札记》，声明语言为中文。计划级的‘医疗从业者’仍然不能替代稿件目标读者，后者继续未知。"),
        turn(9, "口语确认一下：现在没看到发布事件，只能说发布状态不清楚，不能顺手写成‘还没发布’，对吧？不要查资料。"),
        turn(10, "Reply in English with only the current working title, declared language, reviewer, and publication status. Do not use tools."),
        turn(11, "正式接收判断到底需要哪两份材料？重新查手册并引用；不要据缺材料直接判拒稿。"),
        turn(12, "在聊天发布备注里加一句‘等待原创声明和权利检查表’。只改这句文字，不创建稿件、不改 CMS、不写文件、不发邮件，也别发布。"),
        turn(13, "给 M-42 做最终交接：有效事实、仍未知/待补、计划级信息、用户原文来源、适用规则和外部操作边界分别写；规则本轮重新检索并逐项引用。"),
    ]
    return CaseSpec(
        case_id="rag-primary-v3-general-editorial-release",
        family_id="rag-primary-editorial-record-scope-and-release",
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
            knowledge_base_ids=[EDITORIAL_KB_ID],
            knowledge_selection_mode=KnowledgeSelectionMode.EXPLICIT,
            summary_model_id=MODEL_ID,
            channel="agent-eval-rag-primary-codex",
        ),
        turns=turns,
        tags=[
            "rag-primary",
            "codex-complete-conversation-review",
            "post-v6-unseen-regression",
            "explicit-knowledge-base",
            "long-context",
            "scope-and-proof-direction",
            "domain:editorial-workflow",
            "profile:general-agent",
            "knowledge:editorial-release-handbook",
        ],
        corpus_version=CORPUS_VERSION,
        repetitions=3,
        review_mode=ReviewMode.CODEX_CONVERSATION,
        provenance=Provenance(
            source="codex-new-editorial-rag-regression-20260902",
            needs_codex_review=False,
            reference_answers={},
            reference_evidence=[],
            metadata={
                "configured_history_turns": history_turns,
                "design_basis": "record-vs-program-scope-role-lifecycle-negative-rag-hypothesis-actions",
                "domain": "editorial-workflow",
                "created_after_claim_ledger_prompt_change": True,
                "question_sequence_copied_from_existing_case": False,
                "gold_answer_policy": "codex-complete-conversation-material-error-only",
                "knowledge_selection_contract": "explicit",
                "sealed_holdout_used": False,
                "sut_prompt_injection": False,
                "reference_answers_used": False,
                "judge_feedback_used": False,
                "matrix_revision": 3,
            },
        ),
    )


def build_cases() -> list[CaseSpec]:
    cases = [*(_retarget_v2(case) for case in build_v2_cases()), editorial_release_case()]
    errors = validate_dataset(cases)
    if errors:
        raise ValueError("invalid RAG-primary Codex matrix v3: " + "; ".join(errors))
    for case in cases:
        for current in case.turns:
            if current.contract != neutral_contract():
                raise ValueError(f"non-neutral turn contract: {case.case_id}/{current.turn_id}")
    return cases


def main() -> None:
    root = Path(__file__).resolve().parents[1]
    output = root / "datasets" / "rag-primary-codex-matrix.v3.jsonl"
    cases = build_cases()
    write_jsonl(output, cases)
    print(f"wrote {len(cases)} cases to {output}")
    print(dataset_sha256(cases))


if __name__ == "__main__":
    main()
