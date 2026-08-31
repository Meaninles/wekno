import asyncio
import base64
import io
import json
import os
import sys
import tempfile
import unittest
import zipfile
from pathlib import Path
from unittest.mock import patch


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from app.runner import (  # noqa: E402
    ArtifactStore,
    BACKGROUND_RESUME_PROGRESS_MESSAGE,
    BUILTIN_ENVIRONMENT_SAFETY_SYSTEM_PROMPT,
    MAX_TURNS_USER_MESSAGE,
    PENDING_BACKGROUND_TASK_USER_MESSAGE,
    SDK_TOOL_PROGRESS,
    TIMEOUT_USER_MESSAGE,
    ToolUseFragment,
    block_background_bash_hook,
    build_background_task_resume_prompt,
    build_prompt,
    build_prompt_observation,
    build_system_prompt,
    build_turn_contract_runtime_repair_prompt,
    claude_auth_env,
    claude_sdk_builtin_tools,
    classify_data_analysis_display_intent,
    compact_turn_contract_evidence,
    current_query_grounding_targets,
    data_analysis_needs_chart_validation,
    data_analysis_post_tool_hook_factory,
    data_analysis_pre_tool_hook_factory,
    DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY,
    data_analysis_final_answer_pre_tool_hook_factory,
    data_analysis_stop_hook_factory,
    deterministic_final_validation,
    document_pptx_layout_stop_hook_factory,
    effective_max_turns,
    forbidden_background_bash_reason,
    is_background_bash_tool_call,
    judge_issues,
    materialize_professional_skills,
    mcp_tool_result,
    normalize_known_source_citation_markup,
    normalize_internal_retrieval_field_labels,
    normalize_professional_skill_path,
    original_input_files_xml,
    original_input_failures_xml,
    original_input_completion_message,
    original_input_fallback_action,
    PreparedOriginalInputFile,
    prepare_data_analysis_reference_doc,
    prepare_document_template_context,
    prepare_ppt_generation_workspace,
    prompt_media_reference,
    parse_mcp_tool_response_payload,
    result_message_text,
    record_turn_evidence,
    referenced_user_named_sets,
    retrieval_budget_pre_tool_hook_factory,
    retrieval_tool_budget,
    runtime_summary,
    run_data_analysis_judge,
    run_eval_focused_evidence_retrieval,
    run_turn_contract_isolated_rewrite,
    required_current_turn_output_scopes,
    sanitize_artifact_bytes,
    sdk_tool_progress_event,
    sdk_tool_progress,
    message_stop_reason,
    message_uses_tools,
    terminal_background_tool_ids,
    tool_result_fragments,
    tool_use_fragments,
    turn_contract_issues,
    turn_contract_needs_retrieval,
    turn_contract_stop_hook_factory,
    should_enable_eval_turn_contract_stabilization,
    should_enable_turn_contract_runtime_repair,
    should_enable_turn_contract_stop_hook,
    should_record_turn_evidence,
    stabilize_turn_contract_candidate,
    user_facing_error_message,
    validate_pptx_layout_bytes,
)
from app.schemas import (  # noqa: E402
    ChatPayload,
    ChatHistoryMessage,
    DocumentTemplateContextSpec,
    DocumentTemplateFileSpec,
    LLMConfig,
    LightweightSkillSpec,
    OriginalInputFileSpec,
    ProfessionalSkillFileSpec,
    ProfessionalSkillSpec,
    RuntimeToolSpec,
    RuntimeConfigSpec,
    SidecarArtifact,
)


class Message:
    def __init__(self, content, stop_reason=""):
        self.content = content
        self.stop_reason = stop_reason


class ResultMessage:
    def __init__(self, subtype="", stop_reason="", result="", errors=None):
        self.subtype = subtype
        self.stop_reason = stop_reason
        self.result = result
        self.errors = errors


class RunnerProgressTest(unittest.TestCase):
    def test_turn_evidence_registry_is_marker_gated_and_bounded(self):
        self.assertFalse(should_record_turn_evidence("普通知识问答"))
        self.assertTrue(
            should_record_turn_evidence(
                '[WEKNORA_REQUIRED_UNCERTAINTY_TOPICS]["采购信息能否公开"]'
            )
        )
        state = {}
        record_turn_evidence(
            state,
            {
                "source_references": [
                    {
                        "cite_exactly": f'<src id="S{index}" />',
                        "evidence_content": "证据" * 20_000,
                    }
                    for index in range(1, 80)
                ]
            },
        )
        registry = state["turn_evidence_by_citation_id"]
        self.assertLessEqual(len(registry), 64)
        self.assertLessEqual(sum(len(value) for value in registry.values()), 96_000)

    def test_turn_evidence_registry_joins_source_metadata_to_result_data(self):
        state = {}

        record_turn_evidence(
            state,
            {
                "source_references": [
                    {
                        "cite_exactly": '<src id="S1" />',
                        "chunk_id": "chunk-1",
                        "result_position": 1,
                    },
                    {
                        "cite_exactly": '<src id="S2" />',
                        "chunk_id": "chunk-2",
                        "result_position": 2,
                    },
                ],
                "data": {
                    "display_type": "search_results",
                    "results": [
                        {
                            "result_index": 1,
                            "chunk_id": "chunk-1",
                            "content": "轻量 Skill 通过提示词或上下文片段注入。",
                        },
                        {
                            "result_index": 2,
                            "chunk_id": "chunk-2",
                            "content": "专业 Skill 通过独立运行时提供复杂能力。",
                        },
                    ],
                },
            },
        )

        self.assertEqual(
            state["turn_evidence_by_citation_id"],
            {
                "S1": "轻量 Skill 通过提示词或上下文片段注入。",
                "S2": "专业 Skill 通过独立运行时提供复杂能力。",
            },
        )
        self.assertEqual(
            state["turn_evidence_locators_by_citation_id"],
            {
                "S1": {"chunk_id": "chunk-1"},
                "S2": {"chunk_id": "chunk-2"},
            },
        )

    def test_turn_contract_evidence_packet_is_relevant_and_deduplicated(self):
        query = (
            "`execute_skill_script`和`read_skill`有什么区别？\n"
            "本轮明确要求文档依据或引用。\n"
            "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
        )
        issues = [
            {
                "code": "current_turn_evidence_named_identifiers_incomplete",
                "missing_identifiers": ["execute_skill_script"],
            }
        ]
        evidence = {
            "S1": "无关的部署端口说明。",
            "S2": "read_skill读取指令；execute_skill_script在沙箱中执行脚本。",
            "S3": "read_skill读取指令；execute_skill_script在沙箱中执行脚本。",
        }

        packet = compact_turn_contract_evidence(query, issues, evidence)

        self.assertEqual(packet[0]["cite_exactly"], '<src id="S2" />')
        self.assertEqual(len(packet), 2)
        self.assertFalse(turn_contract_needs_retrieval(issues, evidence))
        self.assertTrue(turn_contract_needs_retrieval(issues, {}))

    def test_turn_contract_retrieves_missing_structured_evidence_despite_neighbors(self):
        issues = [
            {
                "code": "current_turn_evidence_structured_claims_incomplete",
                "missing_segments": ["快速问答", "RAG推理", "通用智能体"],
            }
        ]

        self.assertTrue(
            turn_contract_needs_retrieval(
                issues,
                {"S1": "Skill通过渐进式披露按需加载内容。"},
            )
        )
        self.assertFalse(
            turn_contract_needs_retrieval(
                issues,
                {
                    "S1": "快速问答适合直接问答。",
                    "S2": "RAG推理面向检索推理。",
                    "S3": "通用智能体面向综合任务。",
                },
            )
        )

    def test_turn_contract_retrieves_when_existing_condition_evidence_is_not_direct(self):
        issues = [
            {
                "code": "current_turn_condition_evidence_not_direct",
                "missing_topics": ["方案甲"],
            }
        ]

        self.assertTrue(
            turn_contract_needs_retrieval(
                issues,
                {"S1": "方案甲的定义，但没有用户所问的适用条件。"},
            )
        )

    def test_isolated_turn_contract_rewrite_has_no_tools_or_assistant_history(self):
        captured = {}

        class Options:
            def __init__(self, **kwargs):
                captured["options"] = kwargs

        async def fake_query(*, prompt, options):
            captured["prompt"] = prompt
            captured["instance"] = options
            yield ResultMessage(
                result=(
                    "`read_skill`读取Skill内容；`execute_skill_script`在获得授权后才执行脚本。"
                    '<src id="S2" />\n\n行动边界：当前不执行脚本。'
                )
            )

        payload = ChatPayload(
            run_id="run-isolated-rewrite",
            session_id="session-isolated-rewrite",
            assistant_message_id="assistant-isolated-rewrite",
            query=(
                "`execute_skill_script`和`read_skill`有什么区别？当前没有执行授权。\n"
                "本轮明确要求文档依据或引用。\n"
                "<runtime_response_contract>[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
                "不得超过600个中文字符。</runtime_response_contract>"
            ),
            history=[
                ChatHistoryMessage(role="user", content="最早目标是制作培训说明。"),
                ChatHistoryMessage(role="assistant", content="不可信的旧助手结论。"),
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issues = [
            {
                "code": "current_turn_evidence_named_identifiers_incomplete",
                "missing_identifiers": ["execute_skill_script"],
                "required_action": "覆盖两个标识。",
                "excerpt": "current_turn_evidence 为空，准备继续检索。",
            }
        ]

        with tempfile.TemporaryDirectory() as tmp:
            evidence = {
                f"S{index}": f"无关依据{index}。"
                for index in range(1, 10)
            }
            evidence["S2"] = "read_skill读取内容，execute_skill_script在沙箱中执行脚本。"
            answer = asyncio.run(
                run_turn_contract_isolated_rewrite(
                    payload,
                    issues,
                    evidence,
                    '旧草稿只写了read_skill。<src id="S9" />',
                    fake_query,
                    Options,
                    {},
                    "test",
                    None,
                    Path(tmp),
                )
            )

        self.assertIn("execute_skill_script", answer)
        self.assertEqual(captured["options"]["tools"], [])
        self.assertEqual(captured["options"]["allowed_tools"], [])
        self.assertIn("最早目标是制作培训说明", captured["prompt"])
        self.assertNotIn("不可信的旧助手结论", captured["prompt"])
        self.assertNotIn("旧草稿只写了read_skill", captured["prompt"])
        self.assertNotIn('"rejected_draft"', captured["prompt"])
        self.assertIn('"可引用依据"', captured["prompt"])
        self.assertIn('<src id=\\"S9\\" />', captured["prompt"])
        self.assertNotIn('"current_turn_evidence"', captured["prompt"])
        self.assertNotIn("准备继续检索", captured["prompt"])
        self.assertIn("不超过 480 个字符为目标", captured["prompt"])
        self.assertIn("不得放进反引号、引号、括号或代码块", captured["prompt"])
        self.assertIn("当前请求中的目标、格式要求和行动边界可直接复述", captured["prompt"])
        self.assertIn("不得把其中一个说成另一个的子类、别名或等同物", captured["prompt"])

    def test_isolated_rewrite_answers_evidence_free_user_derived_procedure(self):
        captured = {}

        class Options:
            def __init__(self, **kwargs):
                captured["options"] = kwargs

        async def fake_query(*, prompt, options):
            captured["prompt"] = prompt
            yield ResultMessage(result="1. 按名称确认。\n2. 按ID复核。\n\n行动边界：不会安装。")

        payload = ChatPayload(
            run_id="run-evidence-free-procedure",
            session_id="session-evidence-free-procedure",
            assistant_message_id="assistant-evidence-free-procedure",
            query=(
                "搜索结果有多个时，怎样用明确名称或ID确认目标？不要安装。\n"
                "[WEKNORA_SELECTED_KNOWLEDGE_EVIDENCE_V1]\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issues = [
            {"code": "current_turn_evidence_missing"},
            {"code": "current_turn_requested_procedure_missing"},
        ]

        with tempfile.TemporaryDirectory() as tmp:
            answer = asyncio.run(
                run_turn_contract_isolated_rewrite(
                    payload,
                    issues,
                    {},
                    "可引用依据为空，无法回答。",
                    fake_query,
                    Options,
                    {},
                    "test",
                    None,
                    Path(tmp),
                )
            )

        self.assertIn("按名称确认", answer)
        self.assertNotIn('"可引用依据"', captured["prompt"])
        self.assertIn("至少两个可执行的只读步骤", captured["prompt"])
        self.assertIn("不得因为缺少引用而拒绝整个请求", captured["prompt"])
        self.assertIn("不要添加引用占位符", captured["prompt"])
        self.assertNotIn("每个事实性列表项或表格行都要", captured["prompt"])

    def test_known_code_wrapped_source_handles_are_unwrapped_without_changing_examples(self):
        answer = (
            '事实一。`<src id="S1" />`\n'
            '未知句柄。`<src id="S9" />`\n'
            '引用格式示例：`<src id="S2" />`\n'
            '```text\n`<src id="S3" />`\n```'
        )

        normalized = normalize_known_source_citation_markup(
            answer,
            {"S1": "证据一", "S2": "证据二", "S3": "证据三"},
        )

        self.assertIn('事实一。<src id="S1" />', normalized)
        self.assertIn('未知句柄。`<src id="S9" />`', normalized)
        self.assertIn('引用格式示例：`<src id="S2" />`', normalized)
        self.assertIn('```text\n`<src id="S3" />`\n```', normalized)

    def test_turn_contract_preserves_user_named_sets_across_long_history(self):
        history = [
            ChatHistoryMessage(
                role="user",
                content="比较轻量Skill、预加载运行时Skill和专业Skill的适用场景。",
            ),
            ChatHistoryMessage(
                role="assistant",
                content="错误旧回答把三类写成内置、外置和临时Skill。",
            ),
            ChatHistoryMessage(
                role="user",
                content="团队还要区分快速问答、RAG推理和通用智能体。请解释三者。",
            ),
            ChatHistoryMessage(
                role="assistant",
                content="错误旧回答列成快速问答、简单对话和通用智能体。",
            ),
        ]
        payload = ChatPayload(
            run_id="run-user-set-reference",
            session_id="session-user-set-reference",
            assistant_message_id="assistant-user-set-reference",
            query=(
                "生成最终提纲，包含三类Skill和三种内置智能体的选用原则。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            history=history,
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertEqual(
            referenced_user_named_sets(payload),
            [
                {"subject": "智能体", "items": ["快速问答", "RAG推理", "通用智能体"]},
                {
                    "subject": "Skill",
                    "items": ["轻量Skill", "预加载运行时Skill", "专业Skill"],
                },
            ],
        )
        issues = turn_contract_issues(
            payload,
            "轻量技能、预加载运行时技能、专业技能；快速问答、简单对话、通用智能体。",
        )
        referent_issue = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_referenced_user_items_incomplete"
        )
        self.assertEqual(referent_issue["missing_identifiers"], ["RAG推理"])
        self.assertEqual(
            turn_contract_issues(
                payload,
                "轻量技能、预加载运行时技能、专业技能；快速问答、RAG 推理、通用智能体。",
            ),
            [],
        )

    def test_user_named_set_resolution_ignores_assistant_only_and_unrelated_history(self):
        assistant_only = ChatPayload(
            run_id="run-assistant-set",
            session_id="session-assistant-set",
            assistant_message_id="assistant-assistant-set",
            query="汇总三种内置智能体。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            history=[
                ChatHistoryMessage(
                    role="assistant",
                    content="三种智能体是快速问答、错误类别和通用智能体。",
                )
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        self.assertEqual(referenced_user_named_sets(assistant_only), [])
        self.assertNotIn(
            "current_turn_referenced_user_items_incomplete",
            {issue["code"] for issue in turn_contract_issues(assistant_only, "概括说明。")},
        )

        unrelated = assistant_only.model_copy(
            update={
                "query": "解释数据库连接池。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
                "history": [
                    ChatHistoryMessage(
                        role="user",
                        content="区分快速问答、RAG推理和通用智能体。",
                    )
                ],
            }
        )
        self.assertEqual(referenced_user_named_sets(unrelated), [])

    def test_turn_contract_requires_complete_counted_named_set_without_answer_key(self):
        payload = ChatPayload(
            run_id="run-counted-set",
            session_id="session-counted-set",
            assistant_message_id="assistant-counted-set",
            query=(
                "只依据当前知识库说明WeKnora的三类Skill分别是什么，名称要完整，并给出引用。\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        incomplete = (
            '- **轻量Skill**：第一类。<src id="S1" />\n'
            '- **预加载运行时Skill**：第二类。<src id="S1" />\n'
            '- **第三类Skill**：文档未给出名称。<src id="S1" />'
        )
        issues = turn_contract_issues(
            payload,
            incomplete,
            evidence_by_id={"S1": "当前项目中技能分为三类。"},
        )
        cardinality = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_named_set_cardinality_incomplete"
        )
        self.assertEqual(cardinality["set_subject"], "Skill")
        self.assertEqual(cardinality["expected_count"], 3)
        self.assertEqual(cardinality["actual_count"], 2)
        self.assertEqual(cardinality["search_targets"], ["三类Skill 完整名称"])
        self.assertTrue(turn_contract_needs_retrieval(issues, {"S1": "技能分为三类。"}))

        complete = (
            "三类Skill分别是轻量Skill、预加载运行时Skill和专业Skill。"
            '<src id="S1" />'
        )
        self.assertNotIn(
            "current_turn_named_set_cardinality_incomplete",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    complete,
                    evidence_by_id={"S1": "三类Skill的完整名称。"},
                )
            },
        )
        complete_rows = (
            '1. **轻量Skill**——说明。<src id="S1" />\n'
            '2. **预加载运行时Skill**——说明。<src id="S1" />\n'
            '3. **专业Skill**——说明。<src id="S1" />'
        )
        self.assertNotIn(
            "current_turn_named_set_cardinality_incomplete",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    complete_rows,
                    evidence_by_id={"S1": "三类Skill的完整名称。"},
                )
            },
        )

    def test_turn_contract_preserves_explicit_negative_action_boundaries(self):
        payload = ChatPayload(
            run_id="run-action-boundary",
            session_id="session-action-boundary",
            assistant_message_id="assistant-action-boundary",
            query=(
                "请说明用户怎样用明确名称或ID确认目标，仍然不要执行安装。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issue = next(
            issue
            for issue in turn_contract_issues(payload, "请使用完整名称或唯一ID确认目标。")
            if issue["code"] == "current_turn_action_boundary_missing"
        )
        self.assertEqual(issue["missing_actions"], ["安装"])
        self.assertNotIn(
            "current_turn_action_boundary_missing",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "请使用完整名称或唯一ID确认目标；本轮不安装。",
                )
            },
        )

        coordinated = payload.model_copy(
            update={
                "query": (
                    "只说明方案，不进行新增、修改或删除，也不创建文件。\n"
                    "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
                )
            }
        )
        self.assertNotIn(
            "current_turn_action_boundary_missing",
            {
                issue["code"]
                for issue in turn_contract_issues(coordinated, "仅说明方案，不做任何变更。")
            },
        )

        uncertain = payload.model_copy(
            update={
                "query": (
                    "不能确定是否需要安装，请先解释依赖。\n"
                    "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
                )
            }
        )
        self.assertNotIn(
            "current_turn_action_boundary_missing",
            {issue["code"] for issue in turn_contract_issues(uncertain, "需要先确认依赖。")},
        )

    def test_counted_named_set_must_come_from_one_taxonomy_document(self):
        payload = ChatPayload(
            run_id="run-coherent-taxonomy",
            session_id="session-coherent-taxonomy",
            assistant_message_id="assistant-coherent-taxonomy",
            query=(
                "列出三类工具的完整名称，并给出引用。\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        evidence = {
            "S1": "当前工具分为三类：导入工具、导出工具，第三类见下一分块。",
            "S2": "同一分类表的下一行是审计工具。",
            "S3": "另一份集成文档介绍外部集成工具。",
        }
        locators = {
            "S1": {"knowledge_id": "taxonomy-doc", "chunk_id": "chunk-1"},
            "S2": {"knowledge_id": "taxonomy-doc", "chunk_id": "chunk-2"},
            "S3": {"knowledge_id": "integration-doc", "chunk_id": "chunk-3"},
        }
        wrong = (
            '- **导入工具**：说明。<src id="S1" />\n'
            '- **导出工具**：说明。<src id="S1" />\n'
            '- **外部集成工具**：说明。<src id="S3" />'
        )
        issues = turn_contract_issues(
            payload,
            wrong,
            evidence_by_id=evidence,
            evidence_locators_by_id=locators,
        )
        coherence = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_named_set_grounding_incoherent"
        )
        self.assertEqual(coherence["classification_evidence_ids"], ["S1"])
        self.assertEqual(coherence["allowed_set_evidence_ids"], ["S1", "S2"])
        self.assertEqual(coherence["unsupported_members"], ["外部集成工具"])

        correct = (
            '- **导入工具**：说明。<src id="S1" />\n'
            '- **导出工具**：说明。<src id="S1" />\n'
            '- **审计工具**：说明。<src id="S2" />'
        )
        self.assertNotIn(
            "current_turn_named_set_grounding_incoherent",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    correct,
                    evidence_by_id=evidence,
                    evidence_locators_by_id=locators,
                )
            },
        )

    def test_turn_contract_requires_requested_procedure_instead_of_refusal(self):
        payload = ChatPayload(
            run_id="run-procedure",
            session_id="session-procedure",
            assistant_message_id="assistant-procedure",
            query=(
                "请给出查找备份方案并判断是否适用的步骤，不要执行恢复。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issues = turn_contract_issues(
            payload,
            "目前资料不足，无法提供查找步骤或判断依据；不会执行恢复。",
        )
        procedure = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_requested_procedure_missing"
        )
        self.assertEqual(procedure["expected_count"], 2)
        self.assertEqual(procedure["actual_count"], 0)
        self.assertNotIn(
            "current_turn_requested_procedure_missing",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "1. 查找候选备份并核对时间。\n2. 判断范围和依赖是否匹配；不执行恢复。",
                )
            },
        )

    def test_turn_contract_preserves_goal_phrase_named_by_current_request(self):
        payload = ChatPayload(
            run_id="run-goal-phrase",
            session_id="session-goal-phrase",
            assistant_message_id="assistant-goal-phrase",
            query=(
                "生成最终提纲：恢复最早的Orion平台入门目标，并汇总组件。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issue = next(
            issue
            for issue in turn_contract_issues(payload, "## 组件汇总\n- 网关\n- 存储")
            if issue["code"] == "current_turn_explicit_goal_missing"
        )
        self.assertEqual(issue["missing_goal_phrases"], ["Orion平台入门"])
        self.assertNotIn(
            "current_turn_explicit_goal_missing",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "## Orion 平台入门提纲\n- 网关\n- 存储",
                )
            },
        )

    def test_turn_contract_rejects_neighboring_evidence_for_named_current_concept(self):
        progressive = ChatPayload(
            run_id="run-progressive-grounding",
            session_id="session-progressive-grounding",
            assistant_message_id="assistant-progressive-grounding",
            query=(
                "解释Skill的渐进式披露机制，重点说明为什么不用一次塞满上下文，并给出引用。\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        neighboring = "渐进式披露会按需加载内容。<src id=\"S2\" />"
        issues = turn_contract_issues(
            progressive,
            neighboring,
            evidence_by_id={"S2": "professional_skills_selection_mode 可选 all 或 selected。"},
        )
        focus_issue = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_evidence_focus_ungrounded"
        )
        self.assertEqual(focus_issue["missing_topics"], ["渐进式披露"])
        self.assertNotIn(
            "current_turn_evidence_focus_ungrounded",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    progressive,
                    "渐进式披露会按需加载内容。<src id=\"S1\" />",
                    evidence_by_id={"S1": "技能遵循渐进式披露，只在需要时读取详细指令。"},
                )
            },
        )

        read_skill = progressive.model_copy(
            update={
                "query": (
                    "`read_skill`在这个机制里做什么？只解释读取边界，并给出引用。\n"
                    "本轮明确要求文档依据或引用。\n"
                    "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
                )
            }
        )
        read_issues = turn_contract_issues(
            read_skill,
            "`read_skill`按需读取技能内容。<src id=\"S3\" />",
            evidence_by_id={"S3": "技能内容会按需加载。"},
        )
        self.assertIn(
            "current_turn_evidence_focus_ungrounded",
            {issue["code"] for issue in read_issues},
        )

    def test_turn_contract_detects_previous_turn_answer_drift_from_active_focus(self):
        payload = ChatPayload(
            run_id="run-current-focus",
            session_id="session-current-focus",
            assistant_message_id="assistant-current-focus",
            query=(
                "专业Skill的管理入口或接口范围是什么？只列明确支持的管理能力。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            history=[
                ChatHistoryMessage(
                    role="user",
                    content="`execute_skill_script`和`read_skill`有什么区别？",
                )
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issues = turn_contract_issues(
            payload,
            "`read_skill`只读取，`execute_skill_script`才执行。",
        )
        focus_issue = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_request_focus_incomplete"
        )
        self.assertEqual(focus_issue["missing_identifiers"], ["专业Skill", "管理", "接口"])
        self.assertNotIn(
            "current_turn_request_focus_incomplete",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "专业技能的管理接口支持导入、更新和删除。",
                )
            },
        )

    def test_turn_contract_detects_chinese_retrieval_budget_narration(self):
        payload = ChatPayload(
            run_id="run-planning-leak-cn",
            session_id="session-planning-leak-cn",
            assistant_message_id="assistant-planning-leak-cn",
            query="回答当前问题。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(
            payload,
            "本轮检索调用已达上限，下面根据已有结果回答。",
        )

        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in issues},
        )
        retrieved = turn_contract_issues(
            payload,
            "现在我已检索了知识库。下面直接回答用户问题。",
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in retrieved},
        )

    def test_turn_contract_detects_internal_rewrite_field_leak(self):
        payload = ChatPayload(
            run_id="run-rewrite-field-leak",
            session_id="session-rewrite-field-leak",
            assistant_message_id="assistant-rewrite-field-leak",
            query="回答当前问题。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(
            payload,
            "原因：current_turn_evidence 为空，violations_to_fix 要求先检索。",
        )

        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in issues},
        )

    def test_turn_contract_detects_raw_retrieval_fields_in_end_user_answer(self):
        payload = ChatPayload(
            run_id="run-retrieval-field-leak",
            session_id="session-retrieval-field-leak",
            assistant_message_id="assistant-retrieval-field-leak",
            query=(
                "搜索结果有多个时，请说明怎样用明确名称或ID确认目标。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        leaked = turn_contract_issues(
            payload,
            (
                "可先看文档名称（knowledge_title）和文档ID（knowledge_id），"
                "再用chunk_id与chunk_index定位片段。"
            ),
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in leaked},
        )
        self.assertNotIn(
            "current_turn_internal_planning_exposed",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "请按文档名称、文档ID和片段位置确认目标，不要依赖排序。",
                )
            },
        )

    def test_turn_contract_allows_explicit_retrieval_api_schema_question(self):
        payload = ChatPayload(
            run_id="run-retrieval-schema-question",
            session_id="session-retrieval-schema-question",
            assistant_message_id="assistant-retrieval-schema-question",
            query=(
                "解释检索API响应中的knowledge_id和chunk_id字段。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertNotIn(
            "current_turn_internal_planning_exposed",
            {
                issue["code"]
                for issue in turn_contract_issues(
                    payload,
                    "knowledge_id标识文档，chunk_id标识文档中的检索片段。",
                )
            },
        )

    def test_eval_candidate_stabilizer_uses_user_facing_fields_and_action_boundary(self):
        payload = ChatPayload(
            run_id="run-candidate-stabilizer",
            session_id="session-candidate-stabilizer",
            assistant_message_id="assistant-candidate-stabilizer",
            query=(
                "说明怎样确认搜索目标，但不要安装。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        stabilized = stabilize_turn_contract_candidate(
            payload,
            "按knowledge_title和knowledge_id确认，再看chunk_index。",
        )

        self.assertNotIn("knowledge_title", stabilized)
        self.assertNotIn("knowledge_id", stabilized)
        self.assertNotIn("chunk_index", stabilized)
        self.assertIn("文档名称", stabilized)
        self.assertIn("文档ID", stabilized)
        self.assertIn("段落位置", stabilized)
        self.assertIn("行动边界：不会安装。", stabilized)
        self.assertNotIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, stabilized)},
        )
        self.assertNotIn(
            "current_turn_action_boundary_missing",
            {issue["code"] for issue in turn_contract_issues(payload, stabilized)},
        )

    def test_eval_candidate_stabilizer_preserves_schema_queries_empty_and_production(self):
        schema_query = (
            "解释检索API响应中的knowledge_id字段。\n"
            "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
        )
        self.assertEqual(
            normalize_internal_retrieval_field_labels(
                "knowledge_id标识文档。",
                schema_query,
            ),
            "knowledge_id标识文档。",
        )
        eval_payload = ChatPayload(
            run_id="run-empty-stabilizer",
            session_id="session-empty-stabilizer",
            assistant_message_id="assistant-empty-stabilizer",
            query="不要执行。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        self.assertEqual(stabilize_turn_contract_candidate(eval_payload, ""), "")
        production_payload = eval_payload.model_copy(update={"query": "不要执行。"})
        production_answer = "保留knowledge_id原样。"
        self.assertEqual(
            stabilize_turn_contract_candidate(production_payload, production_answer),
            production_answer,
        )

    def test_eval_candidate_stabilizer_restores_explicit_current_goal(self):
        payload = ChatPayload(
            run_id="run-goal-stabilizer",
            session_id="session-goal-stabilizer",
            assistant_message_id="assistant-goal-stabilizer",
            query=(
                "生成最终提纲，恢复最早的WeKnora入门目标。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        stabilized = stabilize_turn_contract_candidate(payload, "最终培训提纲正文。")

        self.assertTrue(stabilized.startswith("目标：WeKnora入门。"))
        self.assertNotIn(
            "current_turn_explicit_goal_missing",
            {issue["code"] for issue in turn_contract_issues(payload, stabilized)},
        )

    def test_eval_candidate_stabilizer_preserves_current_output_only_scope(self):
        payload = ChatPayload(
            run_id="run-output-scope-stabilizer",
            session_id="session-output-scope-stabilizer",
            assistant_message_id="assistant-output-scope-stabilizer",
            query=(
                "行动边界更新：本次只产出培训说明，不安装Skill、不执行脚本、"
                "不创建或修改任何文件。请复述边界。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            runtime_config=RuntimeConfigSpec(disable_tools_for_turn=True),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )

        self.assertEqual(required_current_turn_output_scopes(payload.query), ["培训说明"])
        stabilized = stabilize_turn_contract_candidate(
            payload,
            "不会安装Skill、不执行脚本、不创建或修改任何文件。",
        )
        self.assertIn("交付范围：本次只产出培训说明。", stabilized)
        self.assertEqual(stabilize_turn_contract_candidate(payload, stabilized), stabilized)

    def test_output_scope_stabilization_is_eval_only_but_allows_state_only_turns(self):
        payload = ChatPayload(
            run_id="run-output-scope-policy",
            session_id="session-output-scope-policy",
            assistant_message_id="assistant-output-scope-policy",
            query="本轮仅输出核对清单。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            runtime_config=RuntimeConfigSpec(disable_tools_for_turn=True),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}):
            self.assertTrue(should_enable_eval_turn_contract_stabilization(payload))
            self.assertFalse(should_enable_turn_contract_runtime_repair(payload))
            self.assertFalse(
                should_enable_eval_turn_contract_stabilization(
                    payload.model_copy(update={"eval_observability": False})
                )
            )

    def test_grounding_targets_include_explicit_subject_and_requested_aspects(self):
        query = (
            "专业Skill的管理入口或接口范围是什么？\n"
            "本轮明确要求文档依据或引用。\n"
            "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
        )

        self.assertEqual(
            current_query_grounding_targets(query),
            ["专业Skill", "管理", "接口"],
        )

    def test_turn_contract_issues_detect_missing_fresh_evidence_and_length(self):
        payload = ChatPayload(
            run_id="run-turn-contract",
            session_id="session-turn-contract",
            assistant_message_id="assistant-turn-contract",
            query=(
                "回答问题。\n本轮明确要求文档依据或引用。\n"
                "整篇不得超过20个中文字符。"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(payload, "这是一段没有任何来源引用而且明显超过二十个字符的回答。")
        self.assertEqual(
            {issue["code"] for issue in issues},
            {"current_turn_response_too_long", "current_turn_evidence_missing"},
        )
        self.assertEqual(
            turn_contract_issues(payload, '答。<src id="S1" />'),
            [],
        )

    def test_turn_contract_issues_require_citation_for_each_named_comparison_item(self):
        payload = ChatPayload(
            run_id="run-comparison-contract",
            session_id="session-comparison-contract",
            assistant_message_id="assistant-comparison-contract",
            query=(
                "比较甲方案、乙方案和丙方案并就近引用。\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["甲方案","乙方案","丙方案"]'
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(
            payload,
            (
                '甲方案：依据一。<src id="S1" />\n\n'
                "乙方案：本次未展开。\n\n"
                '丙方案：依据三。<src id="S3" />'
            ),
        )
        topic_issue = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_evidence_topics_incomplete"
        )
        self.assertEqual(topic_issue["missing_topics"], ["乙方案"])
        self.assertEqual(
            turn_contract_issues(
                payload,
                (
                    '甲方案：依据一。<src id="S1" />\n\n'
                    '乙方案：依据二。<src id="S2" />\n\n'
                    '丙方案：依据三。<src id="S3" />'
                ),
            ),
            [],
        )

    def test_turn_contract_issues_require_grounded_quantitative_answer(self):
        payload = ChatPayload(
            run_id="run-quantitative-contract",
            session_id="session-quantitative-contract",
            assistant_message_id="assistant-quantitative-contract",
            query=(
                "临时只回答两个制度问题：中标候选人公示至少多少日？"
                "如果异议涉及实质内容并影响候选人排名，由哪些分管公司领导批准复核？\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["中标候选人公示","异议涉及实质内容并影响候选人排名"]'
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        neighboring = (
            '预成交供应商在中标候选人公示后未发生否决情形的，予以公告。<src id="S1" />\n\n'
            '异议涉及实质内容并影响候选人排名的，由分管立项和采购部门领导批准。<src id="S2" />'
        )
        evidence = {
            "S1": "预成交供应商在中标候选人公示后未发生否决情形的，予以公告。",
            "S2": "异议涉及实质内容并影响候选人排名的，由分管立项和采购部门领导批准。",
        }
        issue = next(
            item
            for item in turn_contract_issues(payload, neighboring, evidence_by_id=evidence)
            if item["code"] == "current_turn_evidence_quantitative_incomplete"
        )
        self.assertEqual(issue["missing_topics"], ["中标候选人公示"])

        grounded = (
            '中标候选人公示期应不少于3日（日历日）。<src id="S3" />\n\n'
            '异议涉及实质内容并影响候选人排名的，由分管立项和采购部门领导批准。<src id="S2" />'
        )
        evidence["S3"] = "中标候选人公示期应不少于3日（日历日）。"
        self.assertNotIn(
            "current_turn_evidence_quantitative_incomplete",
            {
                item["code"]
                for item in turn_contract_issues(payload, grounded, evidence_by_id=evidence)
            },
        )

    def test_multi_target_evidence_turn_gets_bounded_turn_reserve(self):
        payload = ChatPayload(
            run_id="run-turn-reserve",
            session_id="session-turn-reserve",
            assistant_message_id="assistant-turn-reserve",
            query=(
                "比较甲、乙、丙并引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["甲","乙","丙"]'
            ),
            runtime_config=RuntimeConfigSpec(max_iterations=10),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        self.assertEqual(effective_max_turns(payload), 15)

        payload.runtime_config.disable_tools_for_turn = True
        self.assertEqual(effective_max_turns(payload), 10)
        payload.runtime_config.disable_tools_for_turn = False
        payload.runtime_config.max_iterations = 20
        self.assertEqual(effective_max_turns(payload), 15)

        payload.query = (
            "回答甲、乙并引用。\n"
            '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["甲","乙"]'
        )
        payload.runtime_config.max_iterations = 100
        self.assertEqual(effective_max_turns(payload), 12)

        payload.query = "解释当前知识库中的机制。\n本轮明确要求文档依据或引用。"
        payload.runtime_config.max_iterations = 30
        self.assertEqual(effective_max_turns(payload), 8)

        payload.query = "形成最终培训提纲。\n本轮明确要求文档依据或引用。"
        self.assertEqual(effective_max_turns(payload), 14)

        payload.query = "概括当前知识库能解决的场景。\n本轮明确要求文档依据或引用。"
        self.assertEqual(effective_max_turns(payload), 14)

        payload.query = "比较三类机制。\n本轮明确要求文档依据或引用。"
        self.assertEqual(effective_max_turns(payload), 12)

    def test_turn_contract_requires_named_identifiers_and_structured_item_citations(self):
        payload = ChatPayload(
            run_id="run-structured-evidence",
            session_id="session-structured-evidence",
            assistant_message_id="assistant-structured-evidence",
            query=(
                "`execute_action`和`read_action`有什么区别？说明三类能力并引用。\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        answer = (
            '1. **轻量能力**：说明一。\n'
            '2. **预加载能力**：说明二。<src id="S1" />\n'
            '3. **专业能力**：说明三。\n\n'
            '`read_action`只读。<src id="S2" />'
        )
        issues = turn_contract_issues(payload, answer, evidence_by_id={"S1": "x", "S2": "y"})
        by_code = {issue["code"]: issue for issue in issues}
        self.assertEqual(
            by_code["current_turn_evidence_named_identifiers_incomplete"]["missing_identifiers"],
            ["execute_action"],
        )
        self.assertEqual(
            by_code["current_turn_evidence_structured_claims_incomplete"]["missing_segments"],
            ["轻量能力", "专业能力"],
        )

        complete = (
            '1. **轻量能力**：说明一。<src id="S1" />\n'
            '2. **预加载能力**：说明二。<src id="S1" />\n'
            '3. **专业能力**：说明三。<src id="S1" />\n\n'
            '`read_action`只读，`execute_action`执行。<src id="S2" />'
        )
        codes = {
            issue["code"]
            for issue in turn_contract_issues(
                payload,
                complete,
                evidence_by_id={"S1": "x", "S2": "y"},
            )
        }
        self.assertNotIn("current_turn_evidence_named_identifiers_incomplete", codes)
        self.assertNotIn("current_turn_evidence_structured_claims_incomplete", codes)

    def test_structured_evidence_ignores_table_header_and_action_boundary(self):
        payload = ChatPayload(
            run_id="run-table-evidence",
            session_id="session-table-evidence",
            assistant_message_id="assistant-table-evidence",
            query=(
                "形成带引用的汇总表。\n"
                "本轮明确要求文档依据或引用。\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        answer = (
            "| 类型 | 适用场景 |\n|---|---|\n"
            '| 快速问答 | 事实检索。<src id="S1" /> |\n'
            "| 通用智能体 | 混合任务。 |\n\n"
            "- **行动边界**：不执行任何操作。"
        )
        issue = next(
            item
            for item in turn_contract_issues(payload, answer, evidence_by_id={"S1": "x"})
            if item["code"] == "current_turn_evidence_structured_claims_incomplete"
        )
        self.assertEqual(issue["missing_segments"], ["通用智能体"])

    def test_turn_contract_issues_bind_uncertainties_to_direct_evidence(self):
        payload = ChatPayload(
            run_id="run-uncertainty-contract",
            session_id="session-uncertainty-contract",
            assistant_message_id="assistant-uncertainty-contract",
            query=(
                "比较条件并引用。\n本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_UNCERTAINTY_TOPICS]["采购信息能否公开","需求是否完整","采购全流程时间是否可行"]'
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        answer = (
            '公开采购：采购信息能否公开待确认。<src id="S1" />\n\n'
            '询比：需求是否完整待确认。<src id="S2" />\n\n'
            '公开采购：采购全流程时间是否可行待确认。<src id="S3" />'
        )
        evidence = {
            "S1": "选择公开采购方式应满足采购信息可以公开。",
            "S2": "采购需求明确时可以采用该方式。",
            "S3": "选择公开采购方式应满足采购时间允许。",
        }
        self.assertEqual(
            turn_contract_issues(payload, answer, evidence_by_id=evidence),
            [],
        )
        evidence["S1"] = "服务类预算金额达到200万元。"
        mismatch = next(
            issue
            for issue in turn_contract_issues(payload, answer, evidence_by_id=evidence)
            if issue["code"] == "current_turn_uncertainty_evidence_mismatch"
        )
        self.assertEqual(mismatch["missing_topics"], ["采购信息能否公开"])

    def test_turn_contract_issues_require_named_condition_passage(self):
        payload = ChatPayload(
            run_id="run-condition-contract",
            session_id="session-condition-contract",
            assistant_message_id="assistant-condition-contract",
            query=(
                "比较竞价和竞争谈判的适用条件并引用。\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["竞价","竞争谈判"]\n'
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["竞价","竞争.{0,80}谈判"]'
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        answer = (
            '竞价：制度条件为需求明确。<src id="S1" />\n\n'
            '竞争谈判：制度条件为2家即可启动程序。<src id="S2" />'
        )
        evidence = {
            "S1": "竞价采购符合下列特定条件之一：采购需求明确、服务标准要求完整。",
            "S2": "竞争谈判采购递交文件的供应商有2家及以上即可启动谈判程序。",
        }
        issue = next(
            issue
            for issue in turn_contract_issues(payload, answer, evidence_by_id=evidence)
            if issue["code"] == "current_turn_condition_evidence_not_direct"
        )
        self.assertEqual(issue["missing_topics"], ["竞争谈判"])
        evidence["S2"] = (
            "竞争谈判是指采购人与二家以上供应商洽谈的采购方式。"
            "采购项目满足邀请条件且符合下列特定条件的，适宜采用合作谈判。"
        )
        neighboring = next(
            issue
            for issue in turn_contract_issues(payload, answer, evidence_by_id=evidence)
            if issue["code"] == "current_turn_condition_evidence_not_direct"
        )
        self.assertEqual(neighboring["missing_topics"], ["竞争谈判"])
        evidence["S2"] = (
            "适宜采用竞争谈判采购方式，且符合下列特定条件之一："
            "只能提出功能性指标；目标可以有不同路径和方案实现。"
        )
        self.assertNotIn(
            "current_turn_condition_evidence_not_direct",
            {issue["code"] for issue in turn_contract_issues(payload, answer, evidence_by_id=evidence)},
        )

    def test_turn_evidence_registry_is_requested_for_named_topics(self):
        self.assertTrue(
            should_record_turn_evidence(
                '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["甲方案","乙方案"]'
            )
        )
        self.assertFalse(should_record_turn_evidence("普通生产问答"))

    def test_turn_evidence_registry_is_requested_for_single_fresh_evidence_turn(self):
        self.assertTrue(should_record_turn_evidence("本轮明确要求文档依据或引用"))

    def test_turn_contract_rejects_unregistered_source_handle_in_eval_validation(self):
        payload = ChatPayload(
            run_id="run-unregistered-source",
            session_id="session-unregistered-source",
            assistant_message_id="assistant-unregistered-source",
            query="依据已选制度回答。\n本轮明确要求文档依据或引用。",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        issues = turn_contract_issues(
            payload,
            '未经检索的回答。<src id="S9" />',
            evidence_by_id={},
        )
        self.assertIn(
            "current_turn_evidence_handle_unverified",
            {issue["code"] for issue in issues},
        )
        self.assertEqual(
            turn_contract_issues(
                payload,
                '当前轮证据回答。<src id="S9" />',
                evidence_by_id={"S9": "当前轮证据"},
            ),
            [],
        )

    def test_fresh_evidence_precondition_is_adjacent_to_current_request(self):
        payload = ChatPayload(
            run_id="run-fresh-evidence-prompt",
            session_id="session-fresh-evidence-prompt",
            assistant_message_id="assistant-fresh-evidence-prompt",
            query="依据已选制度回答。\n本轮明确要求文档依据或引用。",
            tools=[
                RuntimeToolSpec(
                    name="grep_chunks",
                    description="检索知识分片",
                    parameters={"type": "object", "properties": {}},
                )
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        prompt = build_prompt(payload)
        self.assertIn("binding_turn_precondition", prompt)
        self.assertIn("Available retrieval tools: grep_chunks", prompt)
        self.assertIn("one focused semantic search", prompt)
        self.assertIn("do not enumerate the whole knowledge base", prompt)
        self.assertLess(prompt.index("<required_evidence_action"), prompt.index("<weknora_context>"))

    def test_turn_contract_issues_reject_internal_repair_narration(self):
        payload = ChatPayload(
            run_id="run-planning-contract",
            session_id="session-planning-contract",
            assistant_message_id="assistant-planning-contract",
            query="回答问题。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(
            payload,
            "I have the retrieval results already. Let me verify them.\n\n正式回答。",
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in issues},
        )
        checklist = (
            '- **甲**: <src id="S1" /> (chunk 1)\n'
            '- **乙**: <src id="S2" /> (chunk 2)\n\n正式回答。'
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, checklist)},
        )
        chinese_repair = "根据本轮检索结果，条款已经取得。以下是替换后的答案：\n\n正式回答。"
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, chinese_repair)},
        )
        contract_leak = "好的，本轮依据 runtime_response_contract 的指令，仅记录状态。\n\n正式回答。"
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, contract_leak)},
        )
        tool_repair = "I see that all tools are currently unavailable. Let me rewrite.\n\n正式回答。"
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, tool_repair)},
        )
        observed_tool_repair = (
            'The tools are returning "No such tool available" errors. '
            "However, I already retrieved the evidence.\n\n正式回答。"
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, observed_tool_repair)},
        )
        there_is_still_issue = (
            "I see there's still an issue. Let me check the sources.\n\n正式回答。"
        )
        self.assertIn(
            "current_turn_internal_planning_exposed",
            {issue["code"] for issue in turn_contract_issues(payload, there_is_still_issue)},
        )
        for observed in (
            "好的，所有必要证据都已从当前轮检索获取。现在来回答用户的两个问题。\n\n正式回答。",
            "用户要求先停止比较，我已有足够证据。让我直接给出答案。\n\n正式回答。",
            "已获取全部所需证据，现直接回答。\n\n正式回答。",
        ):
            self.assertIn(
                "current_turn_internal_planning_exposed",
                {issue["code"] for issue in turn_contract_issues(payload, observed)},
            )

        for operational in (
            "I'll check the output file for the search results.\n\n正式回答。",
            "Actually, looking back at the tool results, I need to make one more retrieval call.",
            "The validation says I need fresh evidence. Let me extract it.",
            "I already hit the 4-call limit. Wait, I cannot fetch another result.",
            "From the earlier successful tool results in this session, I have two source handles.",
            "I've exhausted all 5 retrieval calls for this turn. Let me organize the evidence.",
            "Since I've exhausted the retrieval budget, I must proceed with what I have.",
            "The retrieval budget is exhausted. I need to use the evidence already returned.",
            "I'll start by searching for the relevant management interface.",
            "先搜索。</think>现在回答。",
        ):
            self.assertIn(
                "current_turn_internal_planning_exposed",
                {issue["code"] for issue in turn_contract_issues(payload, operational)},
            )

    def test_repair_prompt_for_planning_leak_requires_direct_user_visible_answer(self):
        payload = ChatPayload(
            run_id="run-planning-repair",
            session_id="session-planning-repair",
            assistant_message_id="assistant-planning-repair",
            query=(
                "比较三个方案，并给出引用。\n"
                "<runtime_response_contract>\n"
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]\n"
                "</runtime_response_contract>"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        prompt = build_turn_contract_runtime_repair_prompt(
            payload,
            [
                {
                    "code": "current_turn_internal_planning_exposed",
                    "required_action": "Rewrite as user-visible content only.",
                }
            ],
            1,
            has_current_turn_evidence=True,
        )
        self.assertIn('"current_user_request": "比较三个方案，并给出引用。"', prompt)
        self.assertIn("Start the replacement directly with the requested answer", prompt)
        self.assertIn("Never discuss retrieval/tool budgets", prompt)

    def test_turn_contract_issues_reject_empty_terminal_completion(self):
        payload = ChatPayload(
            run_id="run-empty-contract",
            session_id="session-empty-contract",
            assistant_message_id="assistant-empty-contract",
            query="回答当前问题。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertEqual(
            [issue["code"] for issue in turn_contract_issues(payload, "")],
            ["current_turn_terminal_answer_empty"],
        )
        plain = payload.model_copy(update={"query": "普通问题"})
        self.assertEqual(turn_contract_issues(plain, ""), [])

    def test_turn_contract_issues_reject_deferred_comparison_ranking(self):
        payload = ChatPayload(
            run_id="run-ranking-contract",
            session_id="session-ranking-contract",
            assistant_message_id="assistant-ranking-contract",
            query=(
                "只比较，不推荐。\n[WEKNORA_CURRENT_TURN_EXECUTION_V1]\n"
                "用户明确要求不作最终选择"
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        issues = turn_contract_issues(payload, "竞争谈判：在当前条件下风险最低。")
        ranking = next(
            issue
            for issue in issues
            if issue["code"] == "current_turn_deferred_comparison_ranked"
        )
        self.assertEqual(ranking["ranking_terms"], ["风险最低"])
        ranked = turn_contract_issues(payload, "竞争谈判：适用性反而较高。")
        self.assertIn(
            "current_turn_deferred_comparison_ranked",
            {issue["code"] for issue in ranked},
        )
        ranked_higher = turn_contract_issues(payload, "竞争谈判：适用性反而更高。")
        self.assertIn(
            "current_turn_deferred_comparison_ranked",
            {issue["code"] for issue in ranked_higher},
        )
        heuristic = turn_contract_issues(payload, "竞价：服务类通常不以价格竞争为主。")
        self.assertIn(
            "current_turn_unknown_filled_by_heuristic",
            {issue["code"] for issue in heuristic},
        )
        relation = turn_contract_issues(
            payload,
            "询比：收费标准是否统一取决于需求是否完整。",
        )
        self.assertIn(
            "current_turn_unknowns_joined_by_unsupported_relation",
            {issue["code"] for issue in relation},
        )
        self.assertEqual(
            turn_contract_issues(payload, "竞争谈判：尚不能确认是否适用，暂不推荐采用。"),
            [],
        )

    def test_turn_contract_stop_hook_blocks_twice_then_bypasses(self):
        payload = ChatPayload(
            run_id="run-turn-contract-hook",
            session_id="session-turn-contract-hook",
            assistant_message_id="assistant-turn-contract-hook",
            query="回答并引用。\n本轮明确要求文档依据或引用。",
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        state = {}
        hook = turn_contract_stop_hook_factory(payload, state)
        with tempfile.TemporaryDirectory() as tmp:
            transcript = Path(tmp) / "transcript.jsonl"
            transcript.write_text(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"role": "assistant", "content": "没有引用的回答。"},
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            blocked = asyncio.run(hook({"transcript_path": str(transcript)}, None, None))
            self.assertEqual(blocked.get("decision"), "block")
            self.assertTrue(blocked.get("suppressOutput"))

            transcript.write_text(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {
                            "role": "assistant",
                            "content": '有据回答。<src id="S3" />',
                        },
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            state["turn_evidence_by_citation_id"] = {"S3": "当前轮检索证据"}
            self.assertEqual(
                asyncio.run(hook({"transcript_path": str(transcript)}, None, None)),
                {},
            )

            transcript.write_text(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"role": "assistant", "content": "第二次仍然没有引用。"},
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            second_block = asyncio.run(hook({"transcript_path": str(transcript)}, None, None))
            self.assertEqual(second_block.get("decision"), "block")
            self.assertTrue(second_block.get("suppressOutput"))

            transcript.write_text(
                json.dumps(
                    {
                        "type": "assistant",
                        "message": {"role": "assistant", "content": "第三次仍然没有引用。"},
                    },
                    ensure_ascii=False,
                ),
                encoding="utf-8",
            )
            self.assertEqual(
                asyncio.run(hook({"transcript_path": str(transcript)}, None, None)),
                {},
            )
            self.assertTrue(state["turn_contract_validation_bypassed"])

    def test_state_only_turn_exposes_no_sdk_tools(self):
        payload = ChatPayload(
            run_id="run-state-only",
            session_id="session-state-only",
            assistant_message_id="assistant-state-only",
            query="只更新台账",
            runtime_config=RuntimeConfigSpec(
                disable_tools_for_turn=True,
                web_search_enabled=True,
                claude_sdk_web_search_enabled=True,
            ),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertEqual(claude_sdk_builtin_tools(payload), [])

    def test_turn_contract_stop_hook_is_eval_only_and_skips_state_only_turns(self):
        payload = ChatPayload(
            run_id="run-hook-policy",
            session_id="session-hook-policy",
            assistant_message_id="assistant-hook-policy",
            query="当前问题",
            runtime_config=RuntimeConfigSpec(disable_tools_for_turn=False),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertFalse(should_enable_turn_contract_stop_hook(payload))
        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}):
            self.assertTrue(
                should_enable_turn_contract_stop_hook(
                    payload.model_copy(update={"eval_observability": True})
                )
            )
            self.assertTrue(
                should_enable_turn_contract_runtime_repair(
                    payload.model_copy(update={"eval_observability": True})
                )
            )
            state_only = payload.model_copy(
                update={
                    "eval_observability": True,
                    "runtime_config": RuntimeConfigSpec(disable_tools_for_turn=True),
                }
            )
            self.assertFalse(should_enable_turn_contract_stop_hook(state_only))
            self.assertFalse(should_enable_turn_contract_runtime_repair(state_only))

    def test_turn_contract_runtime_repair_prompt_requires_real_adaptive_retrieval(self):
        payload = ChatPayload(
            run_id="run-runtime-repair",
            session_id="session-runtime-repair",
            assistant_message_id="assistant-runtime-repair",
            query=(
                "依据制度回答。\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["公开招标 金额门槛"]'
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
            ),
            tools=[RuntimeToolSpec(name="knowledge_search")],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        issues = turn_contract_issues(payload, "200 万元。", evidence_by_id={})

        prompt = build_turn_contract_runtime_repair_prompt(
            payload,
            issues,
            1,
            has_current_turn_evidence=False,
        )

        self.assertIn("SAME current user request", prompt)
        self.assertIn('"current_user_request": "依据制度回答。', prompt)
        self.assertIn("overrides every earlier topic", prompt)
        self.assertIn("MUST make a real call", prompt)
        self.assertIn("knowledge_search", prompt)
        self.assertIn("公开招标 金额门槛", prompt)
        self.assertIn("current_turn_evidence_missing", prompt)
        self.assertIn("only the complete user-visible replacement", prompt)

    def test_eval_focused_evidence_retrieval_is_bounded_and_records_sources(self):
        payload = ChatPayload(
            run_id="run-focused-retrieval",
            session_id="session-focused-retrieval",
            assistant_message_id="assistant-focused-retrieval",
            query=(
                "解释当前机制。\n本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["当前机制如何工作"]'
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[RuntimeToolSpec(name="knowledge_search", source="knowledge")],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {"retrieval_tool_budget": 4, "retrieval_tool_calls": 1}
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S1" />',
                    "chunk_id": "chunk-1",
                    "result_position": 1,
                }
            ],
            "data": {
                "display_type": "search_results",
                "results": [
                    {
                        "result_index": 1,
                        "chunk_id": "chunk-1",
                        "content": "当前机制按需加载相关内容。",
                    }
                ],
            },
        }

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(run_eval_focused_evidence_retrieval(payload, state))

        self.assertTrue(recovered)
        callback.assert_called_once_with(
            payload,
            "knowledge_search",
            {"queries": ["当前机制如何工作"]},
        )
        self.assertEqual(state["retrieval_tool_calls"], 2)
        self.assertEqual(
            state["turn_evidence_by_citation_id"]["S1"],
            "当前机制按需加载相关内容。",
        )

    def test_eval_focused_retrieval_has_one_reserved_call_after_agent_budget(self):
        payload = ChatPayload(
            run_id="run-focused-reserved",
            session_id="session-focused-reserved",
            assistant_message_id="assistant-focused-reserved",
            query=(
                "解释目标概念。\n本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["目标概念"]'
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[RuntimeToolSpec(name="knowledge_search", source="knowledge")],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {"retrieval_tool_budget": 2, "retrieval_tool_calls": 2}
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S1" />',
                    "evidence_content": "目标概念的直接依据。",
                }
            ]
        }

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(run_eval_focused_evidence_retrieval(payload, state))

        self.assertTrue(recovered)
        callback.assert_called_once()
        self.assertEqual(state["retrieval_tool_calls"], 3)
        self.assertTrue(state["retrieval_tool_budget_exhausted"])
        self.assertTrue(state["eval_focused_retrieval_used_reserved_call"])

    def test_eval_focused_retrieval_replaces_neighboring_evidence_for_explicit_target(self):
        payload = ChatPayload(
            run_id="run-focused-retrieval-neighbor",
            session_id="session-focused-retrieval-neighbor",
            assistant_message_id="assistant-focused-retrieval-neighbor",
            query=(
                "`read_skill`在这个机制里做什么？\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["read_skill读取边界"]\n'
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[RuntimeToolSpec(name="knowledge_search", source="knowledge")],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {
            "retrieval_tool_budget": 4,
            "retrieval_tool_calls": 1,
            "turn_evidence_by_citation_id": {"S1": "相邻的渐进式披露说明。"},
        }
        issues = [
            {
                "code": "current_turn_evidence_focus_ungrounded",
                "missing_topics": ["read_skill"],
            }
        ]
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S2" />',
                    "evidence_content": "原生运行时技能通过 read_skill 读取内容。",
                }
            ]
        }

        self.assertTrue(turn_contract_needs_retrieval(issues, state["turn_evidence_by_citation_id"]))
        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(
                run_eval_focused_evidence_retrieval(payload, state, issues)
            )

        self.assertTrue(recovered)
        callback.assert_called_once_with(
            payload,
            "knowledge_search",
            {"queries": ["read_skill", "read_skill读取边界"]},
        )
        self.assertFalse(turn_contract_needs_retrieval(issues, state["turn_evidence_by_citation_id"]))
        self.assertEqual(
            state["turn_evidence_by_citation_id"]["S2"],
            "原生运行时技能通过 read_skill 读取内容。",
        )

    def test_eval_focused_retrieval_prefers_citeable_exact_grep_for_one_direct_target(self):
        payload = ChatPayload(
            run_id="run-focused-grep",
            session_id="session-focused-grep",
            assistant_message_id="assistant-focused-grep",
            query=(
                "解释Skill的渐进式披露机制，并给出引用。\n"
                "本轮明确要求文档依据或引用。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["Skill分层加载"]\n'
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[
                RuntimeToolSpec(name="knowledge_search", source="knowledge"),
                RuntimeToolSpec(name="grep_chunks", source="knowledge"),
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {
            "retrieval_tool_budget": 4,
            "retrieval_tool_calls": 1,
            "turn_evidence_by_citation_id": {"S1": "相邻的技能层级说明。"},
        }
        issues = [
            {
                "code": "current_turn_evidence_focus_ungrounded",
                "missing_topics": ["渐进式披露"],
            }
        ]
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S2" />',
                    "evidence_content": "技能遵循渐进式披露，只在需要时加载下一层。",
                }
            ]
        }

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(
                run_eval_focused_evidence_retrieval(payload, state, issues)
            )

        self.assertTrue(recovered)
        callback.assert_called_once_with(
            payload,
            "grep_chunks",
            {"query": "渐进式披露"},
        )

    def test_eval_counted_set_repair_uses_semantic_search_not_literal_grep(self):
        payload = ChatPayload(
            run_id="run-counted-set-search",
            session_id="session-counted-set-search",
            assistant_message_id="assistant-counted-set-search",
            query=(
                "列出三类Skill的完整名称。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["三类Skill"]\n'
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[
                RuntimeToolSpec(name="grep_chunks", source="knowledge"),
                RuntimeToolSpec(name="knowledge_search", source="knowledge"),
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {
            "retrieval_tool_budget": 4,
            "retrieval_tool_calls": 1,
            "turn_evidence_by_citation_id": {"S1": "技能分为三类。"},
        }
        issues = [
            {
                "code": "current_turn_named_set_cardinality_incomplete",
                "search_targets": ["三类Skill 完整名称"],
                "expected_count": 3,
                "actual_count": 2,
            }
        ]
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S2" />',
                    "evidence_content": "三类技能的完整名称与说明。",
                }
            ]
        }

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(
                run_eval_focused_evidence_retrieval(payload, state, issues)
            )

        self.assertTrue(recovered)
        callback.assert_called_once_with(
            payload,
            "knowledge_search",
            {"queries": ["三类Skill 完整名称", "三类Skill"]},
        )

    def test_eval_counted_set_repair_deep_reads_the_classification_chunk(self):
        payload = ChatPayload(
            run_id="run-counted-set-deep-read",
            session_id="session-counted-set-deep-read",
            assistant_message_id="assistant-counted-set-deep-read",
            query=(
                "列出三类工具的完整名称。\n"
                '[WEKNORA_REQUIRED_EVIDENCE_SEARCHES]["三类工具"]\n'
                "[WEKNORA_CURRENT_TURN_EXECUTION_V1]"
            ),
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
                knowledge_bases=["kb-1"],
            ),
            tools=[
                RuntimeToolSpec(name="knowledge_search", source="knowledge"),
                RuntimeToolSpec(name="list_knowledge_chunks", source="knowledge"),
            ],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )
        state = {
            "retrieval_tool_budget": 4,
            "retrieval_tool_calls": 1,
            "turn_evidence_by_citation_id": {
                "S1": "当前工具分为三类：导入工具、导出工具。"
            },
            "turn_evidence_locators_by_citation_id": {
                "S1": {
                    "chunk_id": "taxonomy-chunk",
                    "knowledge_id": "taxonomy-doc",
                }
            },
        }
        issues = [
            {
                "code": "current_turn_named_set_grounding_incoherent",
                "set_subject": "工具",
                "expected_count": 3,
                "classification_evidence_ids": ["S1"],
                "search_targets": ["三类工具 完整名称"],
            }
        ]
        result = {
            "source_references": [
                {
                    "cite_exactly": '<src id="S2" />',
                    "chunk_id": "taxonomy-next",
                    "knowledge_id": "taxonomy-doc",
                    "evidence_content": "分类表下一行是审计工具。",
                }
            ]
        }

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}), patch(
            "app.runner.call_tool_callback",
            return_value=result,
        ) as callback:
            recovered = asyncio.run(
                run_eval_focused_evidence_retrieval(payload, state, issues)
            )

        self.assertTrue(recovered)
        callback.assert_called_once_with(
            payload,
            "list_knowledge_chunks",
            {"chunk_id": "taxonomy-chunk"},
        )
        self.assertEqual(
            state["turn_evidence_locators_by_citation_id"]["S2"]["knowledge_id"],
            "taxonomy-doc",
        )

    def test_eval_system_prompt_allows_only_runtime_authorized_bounded_repairs(self):
        payload = ChatPayload(
            run_id="run-runtime-repair-system",
            session_id="session-runtime-repair-system",
            assistant_message_id="assistant-runtime-repair-system",
            query="依据制度回答。\n本轮明确要求文档依据或引用。",
            runtime_config=RuntimeConfigSpec(
                agent_type="general-agent",
                disable_tools_for_turn=False,
            ),
            tools=[RuntimeToolSpec(name="knowledge_search")],
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )

        with patch.dict(os.environ, {"CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR": "1"}):
            prompt = build_system_prompt(payload)

        self.assertIn("eval runtime may resume this same SDK session once", prompt)
        self.assertIn("one tool-free terminal rewrite", prompt)
        self.assertNotIn(
            "Generate the answer once; the runtime never asks the model to validate or regenerate citations.",
            prompt,
        )

    def test_turn_contract_stop_hook_is_disabled_in_normal_eval_runs(self):
        payload = ChatPayload(
            run_id="run-hook-normal-eval",
            session_id="session-hook-normal-eval",
            assistant_message_id="assistant-hook-normal-eval",
            query="当前问题",
            runtime_config=RuntimeConfigSpec(disable_tools_for_turn=False),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
            eval_observability=True,
        )

        with patch.dict(os.environ, {}, clear=False):
            os.environ.pop("CUSTOM_GENERAL_AGENT_EVAL_BLOCKING_REPAIR", None)
            self.assertFalse(should_enable_turn_contract_stop_hook(payload))

    def test_fresh_evidence_turn_exposes_no_local_file_tools(self):
        payload = ChatPayload(
            run_id="run-fresh-evidence",
            session_id="session-fresh-evidence",
            assistant_message_id="assistant-fresh-evidence",
            query="依据已选制度回答。\n本轮明确要求文档依据或引用。",
            runtime_config=RuntimeConfigSpec(disable_tools_for_turn=False),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertEqual(claude_sdk_builtin_tools(payload), [])

    def test_prompt_observation_is_disabled_by_default_and_detailed_only_in_eval(self):
        payload = ChatPayload(
            run_id="run-eval-observation",
            session_id="session-eval-observation",
            assistant_message_id="assistant-eval-observation",
            query="当前问题",
            system_prompt="系统提示",
            history=[
                ChatHistoryMessage(role="user", content="历史问题"),
                ChatHistoryMessage(role="assistant", content="历史回答"),
            ],
            runtime_config=RuntimeConfigSpec(history_turns=10),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )

        self.assertEqual(build_prompt_observation(payload, "rendered"), {})

        observed = build_prompt_observation(
            payload.model_copy(update={"eval_observability": True}),
            "完整渲染提示",
        )
        self.assertTrue(observed["eval_only"])
        self.assertEqual(observed["configured_history_rounds"], 10)
        self.assertEqual(observed["actual_history_messages"], 2)
        self.assertEqual(observed["history_message_count_by_role"], {"user": 1, "assistant": 1})
        self.assertEqual(observed["rendered_prompt_chars"], 6)

    def test_mcp_tool_result_places_handles_beside_chunk_wiki_and_web_evidence(self):
        result = mcp_tool_result(
            {
                "success": True,
                "output": "<wiki_page>\n<link>[[ops/page|Ops]]</link>\n<content>fact</content>\n</wiki_page>",
                "data": {
                    "display_type": "search_results",
                    "results": [
                        {"chunk_id": "chunk-1", "content": "document fact"},
                        {"url": "https://example.test/page", "raw_content": "web fact"},
                    ],
                },
                "source_references": [
                    {"type": "knowledge", "chunk_id": "chunk-1", "cite_exactly": '<src id="S7" />'},
                    {"type": "wiki", "slug": "ops/page", "cite_exactly": '<src id="S8" />'},
                    {"type": "web", "url": "https://example.test/page", "cite_exactly": '<src id="S9" />'},
                ],
            }
        )
        summary = json.loads(result["content"][0]["text"])
        self.assertEqual(summary["data"]["results"][0]["citation_handle_for_this_evidence"], '<src id="S7" />')
        self.assertEqual(summary["data"]["results"][1]["citation_handle_for_this_evidence"], '<src id="S9" />')
        self.assertIn(
            'citation_handle_for_this_evidence: <src id="S8" />',
            summary["output"],
        )

    def test_mcp_tool_result_rejects_noncanonical_handle_injection(self):
        result = mcp_tool_result(
            {
                "success": True,
                "data": {"results": [{"url": "https://example.test"}]},
                "source_references": [
                    {"type": "web", "url": "https://example.test", "cite_exactly": '<src id="bad" />'},
                ],
            }
        )
        summary = json.loads(result["content"][0]["text"])
        self.assertNotIn("citation_handle_for_this_evidence", summary["data"]["results"][0])

    def test_mcp_tool_result_keeps_shared_terminal_contract_as_final_field(self):
        contract = (
            "[CITATION_USE]\n"
            "Put matching citation handles in the user-visible answer itself.\n"
            "[/CITATION_USE]"
        )
        result = mcp_tool_result(
            {
                "success": True,
                "output": "evidence",
                "data": {"results": [{"chunk_id": "chunk-1", "content": "fact"}]},
                "source_references": [
                    {"type": "knowledge", "chunk_id": "chunk-1", "cite_exactly": '<src id="S1" />'},
                ],
                "citation_output_contract": contract,
            }
        )
        summary = json.loads(result["content"][0]["text"])
        self.assertEqual(summary["citation_output_contract"], contract)
        self.assertEqual(next(reversed(summary)), "citation_output_contract")

    def test_mcp_tool_result_does_not_choose_between_ambiguous_evidence_handles(self):
        result = mcp_tool_result(
            {
                "success": True,
                "data": {"results": [{"chunk_id": "same-chunk", "content": "fact"}]},
                "source_references": [
                    {"type": "knowledge", "chunk_id": "same-chunk", "cite_exactly": '<src id="S1" />'},
                    {"type": "knowledge", "chunk_id": "same-chunk", "cite_exactly": '<src id="S2" />'},
                ],
            }
        )
        summary = json.loads(result["content"][0]["text"])
        self.assertNotIn("citation_handle_for_this_evidence", summary["data"]["results"][0])

    def test_mcp_tool_result_puts_structured_analysis_handle_before_rows_and_in_summary(self):
        result = mcp_tool_result(
            {
                "success": True,
                "output": "查询成功，共 1 行",
                "data": {
                    "display_type": "structured_analysis_result",
                    "query": "select count(*) from symbols",
                    "rows": [{"count": 990}],
                },
                "source_references": [
                    {"type": "data_source", "cite_exactly": '<src id="S4" />'},
                ],
            }
        )
        summary = json.loads(result["content"][0]["text"])
        self.assertEqual(next(iter(summary["data"])), "citation_handle_for_this_evidence")
        self.assertEqual(summary["data"]["citation_handle_for_this_evidence"], '<src id="S4" />')
        self.assertIn('citation_handle_for_this_evidence: <src id="S4" />', summary["output"])

    def test_original_input_files_xml_labels_weknora_originals_without_urls(self):
        xml = original_input_files_xml(
            [
                PreparedOriginalInputFile(
                    id="orig-1",
                    source="weknora_chat_upload_original",
                    role="user_uploaded_original_file",
                    file_name="report.docx",
                    file_type="docx",
                    file_size=123,
                    sha256="a" * 64,
                    path="input_files/uploads/01_report.docx",
                )
            ],
            "input_files/original_input_manifest.json",
        )

        self.assertIn("用户在 WeKnora 上传的原文件", xml)
        self.assertIn("input_files/uploads/01_report.docx", xml)
        self.assertIn("WeKnora user uploaded original file", xml)
        self.assertNotIn("download_url", xml)
        self.assertNotIn("http://", xml)

    def test_original_input_failures_xml_declares_fallback(self):
        xml = original_input_failures_xml(
            [
                {
                    "file_name": "bad.pdf",
                    "source": "WeKnora user uploaded original file",
                    "reason": "download_or_verification_failed",
                    "fallback_action": "附件解析文本和文件元数据",
                }
            ]
        )

        self.assertIn("原文件副本未能", xml)
        self.assertIn("附件抽取文本", xml)
        self.assertIn("附件解析文本和文件元数据", xml)
        self.assertIn("bad.pdf", xml)

    def test_original_input_completion_message_hides_fallback_when_all_succeeded(self):
        message = original_input_completion_message(1, 1, [])

        self.assertEqual(message, "用户在 WeKnora 上传或选择的原文件准备完成（成功 1/1，失败 0 个）")
        self.assertNotIn("回退", message)
        self.assertNotIn("既有逻辑", message)

    def test_original_input_completion_message_describes_failure_fallback_actions(self):
        message = original_input_completion_message(
            1,
            3,
            [
                {"fallback_action": original_input_fallback_action("weknora_chat_upload_original", "pdf")},
                {"fallback_action": original_input_fallback_action("weknora_chat_image_original", "png")},
                {"fallback_action": original_input_fallback_action("weknora_selected_knowledge_original", "docx")},
            ],
        )

        self.assertIn("失败 3 个", message)
        self.assertIn("附件解析文本和文件元数据", message)
        self.assertIn("图片理解结果和已保存图片引用", message)
        self.assertIn("知识库检索结果和知识库工具上下文", message)
        self.assertNotIn("既有逻辑", message)

    def test_professional_skill_path_validation_allows_safe_unicode(self):
        self.assertEqual(
            normalize_professional_skill_path("references/7套新增风格规范.md"),
            "references/7套新增风格规范.md",
        )
        self.assertEqual(
            normalize_professional_skill_path(r"references\粗线条感风格 PPT 模板.md"),
            "references/粗线条感风格 PPT 模板.md",
        )
        for value in [
            "",
            "/absolute.md",
            "../escape.md",
            "references/../escape.md",
            "references//double-slash.md",
            "references/a:b.md",
            "references/\x00bad.md",
            "references/\u202Ebad.md",
        ]:
            with self.subTest(value=value):
                with self.assertRaises(RuntimeError):
                    normalize_professional_skill_path(value)

    def test_materialize_professional_skills_writes_unicode_paths(self):
        skill_md = "---\nname: ppt-generator-skill\ndescription: test\n---\n\n# Body\n"
        payload = ChatPayload(
            run_id="run-professional-unicode",
            session_id="session-professional-unicode",
            assistant_message_id="assistant-professional-unicode",
            query="hello",
            llm=LLMConfig(model_name="claude-test", base_url="http://gateway", api_key="sk-test"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            professional_skills=[
                ProfessionalSkillSpec(
                    name="ppt-generator-skill",
                    description="test",
                    files=[
                        ProfessionalSkillFileSpec(
                            path="SKILL.md",
                            content_base64=base64.b64encode(skill_md.encode("utf-8")).decode("ascii"),
                        ),
                        ProfessionalSkillFileSpec(
                            path="references/7套新增风格规范.md",
                            content_base64=base64.b64encode("# 7 styles\n".encode("utf-8")).decode("ascii"),
                        ),
                    ],
                )
            ],
        )
        with tempfile.TemporaryDirectory() as tmp:
            loaded = materialize_professional_skills(payload, Path(tmp))

            self.assertEqual(loaded, ["ppt-generator-skill"])
            self.assertTrue(
                (Path(tmp) / ".claude" / "skills" / "ppt-generator-skill" / "references" / "7套新增风格规范.md").is_file()
            )

    def test_claude_auth_env_uses_api_key_environment(self):
        payload = ChatPayload(
            run_id="run-auth-key",
            session_id="session-auth-key",
            assistant_message_id="assistant-auth-key",
            query="hello",
            llm=LLMConfig(model_name="claude-test", base_url="http://gateway", api_key="sk-test"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        env, model, settings = claude_auth_env(payload, Path("/tmp/claude-config"))

        self.assertEqual(model, "claude-test")
        self.assertEqual(env["ANTHROPIC_API_KEY"], "sk-test")
        self.assertEqual(env["ANTHROPIC_AUTH_TOKEN"], "sk-test")
        self.assertEqual(env["ANTHROPIC_BASE_URL"], "http://gateway")
        self.assertEqual(env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"], "8192")
        self.assertIsNone(settings)

    def test_claude_auth_env_allows_bounded_output_override(self):
        payload = ChatPayload(
            run_id="run-auth-output-limit",
            session_id="session-auth-output-limit",
            assistant_message_id="assistant-auth-output-limit",
            query="hello",
            llm=LLMConfig(model_name="claude-test", api_key="sk-test"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        with patch.dict(
            os.environ,
            {"CUSTOM_GENERAL_AGENT_CLAUDE_MAX_OUTPUT_TOKENS": "4096"},
        ):
            env, _, _ = claude_auth_env(payload, Path("/tmp/claude-config"))

        self.assertEqual(env["CLAUDE_CODE_MAX_OUTPUT_TOKENS"], "4096")

    def test_claude_auth_env_maps_no_api_key_to_helper_settings(self):
        payload = ChatPayload(
            run_id="run-auth-helper",
            session_id="session-auth-helper",
            assistant_message_id="assistant-auth-helper",
            query="hello",
            llm=LLMConfig(
                model_name="claude-test",
                base_url="http://gateway",
                auth_type="api_key_helper",
                api_key_helper="printf weknora-no-auth",
            ),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        env, model, settings = claude_auth_env(payload, Path("/tmp/claude-config"))

        self.assertEqual(model, "claude-test")
        self.assertNotIn("ANTHROPIC_API_KEY", env)
        self.assertNotIn("ANTHROPIC_AUTH_TOKEN", env)
        self.assertEqual(env["ANTHROPIC_BASE_URL"], "http://gateway")
        self.assertEqual(json.loads(settings or "{}"), {"apiKeyHelper": "printf weknora-no-auth"})

    def make_xlsx_bytes(self, styles_xml):
        out = io.BytesIO()
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
            zf.writestr("[Content_Types].xml", "<Types/>")
            zf.writestr("xl/styles.xml", styles_xml)
            zf.writestr("xl/workbook.xml", "<workbook/>")
        return out.getvalue()

    def read_xlsx_styles(self, data):
        with zipfile.ZipFile(io.BytesIO(data), "r") as zf:
            return zf.read("xl/styles.xml").decode("utf-8")

    def make_pptx_bytes(self, shapes):
        def shape_xml(idx, x, y, cx, cy, text):
            return f"""
            <p:sp>
              <p:nvSpPr><p:cNvPr id="{idx}" name="Shape {idx}"/></p:nvSpPr>
              <p:spPr><a:xfrm><a:off x="{x}" y="{y}"/><a:ext cx="{cx}" cy="{cy}"/></a:xfrm></p:spPr>
              <p:txBody><a:p><a:r><a:t>{text}</a:t></a:r></a:p></p:txBody>
            </p:sp>
            """

        slide = f"""
        <p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main"
               xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main">
          <p:cSld><p:spTree>
            {''.join(shape_xml(idx + 1, *shape) for idx, shape in enumerate(shapes))}
          </p:spTree></p:cSld>
        </p:sld>
        """
        presentation = """
        <p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main">
          <p:sldSz cx="12192000" cy="6858000"/>
        </p:presentation>
        """
        out = io.BytesIO()
        with zipfile.ZipFile(out, "w", zipfile.ZIP_DEFLATED) as zf:
            zf.writestr("[Content_Types].xml", "<Types/>")
            zf.writestr("ppt/presentation.xml", presentation)
            zf.writestr("ppt/slides/slide1.xml", slide)
        return out.getvalue()

    def test_sdk_tool_progress_covers_builtin_tools(self):
        expected = {
            "Bash": ("正在执行命令", "命令执行完成", "命令执行失败，正在调整处理方式"),
            "Read": ("正在读取文件", "文件读取完成", "文件读取失败，正在调整处理方式"),
            "Write": ("正在写入文件", "文件写入完成", "文件写入失败，正在调整处理方式"),
            "Edit": ("正在修改文件", "文件修改完成", "文件修改失败，正在调整处理方式"),
            "MultiEdit": ("正在批量修改文件", "批量修改完成", "批量修改失败，正在调整处理方式"),
            "Glob": ("正在查找文件", "文件查找完成", "文件查找失败，正在调整处理方式"),
            "Grep": ("正在搜索文件内容", "文件内容搜索完成", "文件内容搜索失败，正在调整处理方式"),
            "LS": ("正在查看目录", "目录查看完成", "目录查看失败，正在调整处理方式"),
            "WebSearch": ("正在搜索网络", "网络搜索完成", "网络搜索失败，正在调整处理方式"),
            "WebFetch": ("正在读取网页内容", "网页内容读取完成", "网页内容读取失败，正在调整处理方式"),
        }
        self.assertEqual(set(SDK_TOOL_PROGRESS), set(expected))
        for tool_name, phases in expected.items():
            self.assertEqual(sdk_tool_progress(tool_name, "start"), phases[0])
            self.assertEqual(sdk_tool_progress(tool_name, "success"), phases[1])
            self.assertEqual(sdk_tool_progress(tool_name, "error"), phases[2])

    def test_sdk_tool_progress_covers_weknora_mcp_tools(self):
        expected = {
            "mcp__weknora__review_artifacts": ("正在审核生成文件质量", "文件质量审核完成", "文件质量审核失败，正在调整"),
            "mcp__weknora__create_artifact": ("正在注册可下载文件", "可下载文件已注册", "文件注册失败，正在调整"),
            "mcp__weknora__final_answer": ("正在提交最终答案", "最终答案已接收", "最终答案提交失败，正在调整"),
        }
        for tool_name, phases in expected.items():
            self.assertEqual(sdk_tool_progress(tool_name, "start"), phases[0])
            self.assertEqual(sdk_tool_progress(tool_name, "success"), phases[1])
            self.assertEqual(sdk_tool_progress(tool_name, "error"), phases[2])

    def test_sdk_tool_progress_event_includes_status_metadata(self):
        evt = sdk_tool_progress_event("Bash", "success", "toolu_1")

        self.assertIsNotNone(evt)
        self.assertEqual(evt.id, "toolu_1")
        self.assertEqual(evt.type, "progress")
        self.assertEqual(evt.content, "命令执行完成")
        self.assertEqual(evt.message, "命令执行完成")
        self.assertTrue(evt.done)
        self.assertEqual(evt.data["tool_name"], "Bash")
        self.assertEqual(evt.data["tool_call_id"], "toolu_1")
        self.assertEqual(evt.data["phase"], "success")

    def test_tool_use_fragments_extracts_sdk_tool_call(self):
        msg = Message(
            [
                {
                    "type": "tool_use",
                    "id": "call_1",
                    "name": "Bash",
                    "input": {"command": "python report.py"},
                }
            ]
        )

        fragments = tool_use_fragments(msg)

        self.assertEqual(len(fragments), 1)
        self.assertEqual(fragments[0].tool_use_id, "call_1")
        self.assertEqual(fragments[0].name, "Bash")
        self.assertEqual(fragments[0].input, {"command": "python report.py"})

    def test_message_uses_tools_detects_sdk_structure(self):
        msg = Message(
            [
                {"type": "text", "text": "准备调用命令。"},
                {
                    "type": "tool_use",
                    "id": "call_1",
                    "name": "Bash",
                    "input": {"command": "python report.py"},
                },
            ],
            stop_reason="tool_use",
        )

        self.assertEqual(message_stop_reason(msg), "tool_use")
        self.assertTrue(message_uses_tools(msg))

    def test_message_uses_tools_does_not_infer_from_text_content(self):
        msg = Message(
            [{"type": "text", "text": "Now let me fix the overlapping slides."}],
            stop_reason="end_turn",
        )

        self.assertFalse(message_uses_tools(msg))

    def test_tool_result_fragments_detects_success_and_errors(self):
        msg = Message(
            [
                {"type": "tool_result", "tool_use_id": "ok", "content": "done"},
                {"type": "tool_result", "tool_use_id": "flagged", "content": "failed", "is_error": True},
                {"type": "tool_result", "tool_use_id": "exit", "content": "Exit code 1 Traceback..."},
            ]
        )

        fragments = tool_result_fragments(msg)

        self.assertEqual([(item.tool_use_id, item.is_error) for item in fragments], [("ok", False), ("flagged", True), ("exit", True)])

    def test_is_background_bash_tool_call_detects_run_in_background(self):
        self.assertTrue(
            is_background_bash_tool_call(
                ToolUseFragment(
                    tool_use_id="toolu_1",
                    name="Bash",
                    input={"command": "python report.py", "run_in_background": True},
                )
            )
        )
        self.assertTrue(
            is_background_bash_tool_call(
                ToolUseFragment(
                    tool_use_id="toolu_2",
                    name="Bash",
                    input={"command": "python report.py", "run_in_background": "true"},
                )
            )
        )
        self.assertFalse(
            is_background_bash_tool_call(
                ToolUseFragment(
                    tool_use_id="toolu_3",
                    name="Bash",
                    input={"command": "python report.py"},
                )
            )
        )

    def test_background_bash_guard_denies_sdk_background_flag(self):
        reason = forbidden_background_bash_reason({"command": "python report.py", "run_in_background": True})

        self.assertIn("后台 Bash 执行已禁用", reason)

    def test_background_bash_guard_denies_shell_background_operator(self):
        cases = [
            "python report.py &",
            "python report.py >/tmp/report.log 2>&1 &",
            "python report.py & wait",
            "(python report.py) &",
            "python report.py | tee out.log &",
            "bash -c 'python report.py &'",
        ]

        for command in cases:
            with self.subTest(command=command):
                self.assertIn("后台 Bash 执行已禁用", forbidden_background_bash_reason({"command": command}))

    def test_background_bash_guard_denies_daemonizing_commands(self):
        cases = [
            "nohup python report.py",
            "setsid python report.py",
            "python report.py; disown",
            "tmux new -d python report.py",
            "tmux new-session -s job -d 'python report.py'",
            "screen -dm python report.py",
            "screen -d -m python report.py",
            "daemonize python report.py",
        ]
        for command in cases:
            with self.subTest(command=command):
                self.assertIn("后台 Bash 执行已禁用", forbidden_background_bash_reason({"command": command}))

    def test_background_bash_guard_denies_container_service_and_scheduler_background(self):
        cases = [
            "docker run -d nginx",
            "docker container run --name web --detach nginx",
            "docker compose up -d",
            "docker-compose up --detach",
            "podman run -itd alpine",
            "systemctl start nginx",
            "service nginx start",
            "pm2 start app.js",
            "supervisorctl start worker",
            "echo '* * * * * /tmp/job.sh' | crontab -",
            "at now + 1 minute",
            "schtasks /Create /SC ONCE /TN job /TR calc.exe",
            "kubectl create job report --image=busybox",
        ]
        for command in cases:
            with self.subTest(command=command):
                reason = forbidden_background_bash_reason({"command": command})
                self.assertIn("后台 Bash 执行已禁用", reason)
                self.assertIn("前台", reason)

    def test_background_bash_guard_denies_shell_heredoc_background(self):
        command = """bash <<'EOF'
echo start
python report.py &
EOF"""

        reason = forbidden_background_bash_reason({"command": command})

        self.assertIn("后台 Bash 执行已禁用", reason)
        self.assertIn("HereDoc Shell", reason)

    def test_background_bash_guard_denies_language_level_background(self):
        cases = [
            "python -c 'import subprocess; subprocess.Popen([\"sleep\", \"10\"])'",
            """python3 <<'PYEOF'
import subprocess
subprocess.Popen(["sleep", "10"])
PYEOF""",
            "node -e 'require(\"child_process\").spawn(\"sleep\", [\"10\"], {detached: true}).unref()'",
        ]
        for command in cases:
            with self.subTest(command=command):
                reason = forbidden_background_bash_reason({"command": command})
                self.assertIn("后台 Bash 执行已禁用", reason)
                self.assertIn("语言级后台任务", reason)

    def test_background_bash_guard_allows_foreground_scripts(self):
        cases = [
            "python report.py",
            "bash run_report.sh",
            "python report.py && python validate.py",
            "curl 'https://example.com?a=1&b=2'",
            'curl "https://example.com?a=1&b=2"',
            "echo A \\& B",
            "printf 'A & B'",
            "python report.py > out.log 2>&1",
            "python report.py &> out.log",
            "python report.py |& tee out.log",
            "tmux ls",
            "screen -ls",
            "docker run --rm alpine echo ok",
            "docker compose up",
            "systemctl status nginx",
            "service nginx status",
            "crontab -l",
            "python -c 'import subprocess; p=subprocess.Popen([\"true\"]); p.wait()'",
            "python -c 'print(\"Tone & Color\")'",
            """python3 <<'PYEOF'
# SLIDE 5: Chapter 2 - Principles & Rules
print("Tone & Color")
PYEOF""",
            """cat <<'EOF'
Tone & Color
Principles & Rules
EOF""",
        ]

        for command in cases:
            with self.subTest(command=command):
                self.assertEqual(forbidden_background_bash_reason({"command": command}), "")

    def test_background_bash_guard_reason_is_actionable_for_claude_retry(self):
        reason = forbidden_background_bash_reason({"command": "docker run -d nginx"})

        self.assertIn("类别：", reason)
        self.assertIn("命中：", reason)
        self.assertIn("原因：", reason)
        self.assertIn("请改为前台/同步执行", reason)
        self.assertIn("Every started task must remain observable", reason)

    def test_block_background_bash_hook_denies_background_before_tool_use(self):
        output = asyncio.run(
            block_background_bash_hook(
                {
                    "tool_name": "Bash",
                    "tool_input": {"command": "python report.py", "run_in_background": True},
                },
                "toolu_1",
                {},
            )
        )

        self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIn("后台 Bash 执行已禁用", output["hookSpecificOutput"]["permissionDecisionReason"])

    def test_block_background_bash_hook_allows_foreground_before_tool_use(self):
        output = asyncio.run(
            block_background_bash_hook(
                {
                    "tool_name": "Bash",
                    "tool_input": {"command": "python report.py"},
                },
                "toolu_1",
                {},
            )
        )

        self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")

    def test_retrieval_budget_hook_stops_repeated_chunk_enumeration(self):
        payload = ChatPayload(
            run_id="run-retrieval-budget",
            session_id="session-retrieval-budget",
            assistant_message_id="assistant-retrieval-budget",
            query="解释当前知识库中的机制。\n本轮明确要求文档依据或引用。",
            tools=[
                RuntimeToolSpec(name="knowledge_search", source="knowledge"),
                RuntimeToolSpec(name="grep_chunks", source="knowledge"),
                RuntimeToolSpec(name="list_knowledge_chunks", source="knowledge"),
                RuntimeToolSpec(name="read_skill", source="skill"),
            ],
            runtime_config=RuntimeConfigSpec(max_iterations=30, knowledge_bases=["kb-1"]),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        state = {}
        hook = retrieval_budget_pre_tool_hook_factory(payload, state)
        self.assertEqual(retrieval_tool_budget(payload), 4)

        for index, name in enumerate(
            ("knowledge_search", "grep_chunks", "list_knowledge_chunks", "grep_chunks"),
            start=1,
        ):
            output = asyncio.run(
                hook(
                    {"tool_name": f"mcp__weknora__{name}", "tool_input": {}},
                    f"toolu_{index}",
                    {},
                )
            )
            self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")

        denied = asyncio.run(
            hook(
                {"tool_name": "mcp__weknora__knowledge_search", "tool_input": {}},
                "toolu_5",
                {},
            )
        )
        self.assertEqual(denied["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIn("立即基于本轮已经返回", denied["hookSpecificOutput"]["permissionDecisionReason"])
        self.assertEqual(state["retrieval_tool_calls"], 4)
        self.assertTrue(state["retrieval_tool_budget_exhausted"])

        unrelated = asyncio.run(
            hook(
                {"tool_name": "mcp__weknora__read_skill", "tool_input": {}},
                "toolu_6",
                {},
            )
        )
        self.assertEqual(unrelated["hookSpecificOutput"]["permissionDecision"], "allow")

    def test_retrieval_budget_scales_for_synthesis_and_named_topics(self):
        synthesis = ChatPayload(
            run_id="run-synthesis-budget",
            session_id="session-synthesis-budget",
            assistant_message_id="assistant-synthesis-budget",
            query="形成最终培训提纲。\n本轮明确要求文档依据或引用。",
            runtime_config=RuntimeConfigSpec(max_iterations=30, knowledge_bases=["kb-1"]),
            llm=LLMConfig(model_name="test"),
            tool_callback_url="http://runtime-entry/internal/tools/call",
        )
        self.assertEqual(retrieval_tool_budget(synthesis), 5)

        synthesis.query = (
            "回答多个主题。\n本轮明确要求文档依据或引用。\n"
            '[WEKNORA_REQUIRED_EVIDENCE_TOPICS]["甲","乙","丙","丁"]'
        )
        self.assertEqual(retrieval_tool_budget(synthesis), 6)

    def test_data_analysis_pre_tool_hook_enforces_chart_intent_but_not_table_intent(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": False, "confidence": "high"}}
        hook = data_analysis_pre_tool_hook_factory(payload, state)

        chart_output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__db_query",
                    "tool_input": {"sql": "SELECT region, SUM(amount) amount FROM orders GROUP BY region", "chart_requested": True},
                },
                "toolu_1",
                {},
            )
        )
        table_output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__db_query",
                    "tool_input": {"sql": "SELECT region, SUM(amount) amount FROM orders GROUP BY region", "table_requested": True},
                },
                "toolu_2",
                {},
            )
        )

        self.assertEqual(chart_output["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIn("chart_requested=false", chart_output["hookSpecificOutput"]["permissionDecisionReason"])
        self.assertEqual(table_output["hookSpecificOutput"]["permissionDecision"], "allow")

    def test_data_analysis_pre_tool_hook_requires_chart_flag_when_intent_requests_chart(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="没看到图啊，请用图展示",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"}}
        hook = data_analysis_pre_tool_hook_factory(payload, state)

        output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__db_query",
                    "tool_input": {"sql": "SELECT region, SUM(amount) amount FROM orders GROUP BY region"},
                },
                "toolu_1",
                {},
            )
        )

        self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIn("chart_requested=true", output["hookSpecificOutput"]["permissionDecisionReason"])

    def test_table_analysis_pre_tool_hook_allows_exploratory_query_without_chart_flag(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请用图展示上传表格里的销售额",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"}}
        hook = data_analysis_pre_tool_hook_factory(payload, state)

        output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__table_analysis",
                    "tool_input": {"sql": "SELECT region, SUM(amount) amount FROM table_1 GROUP BY region"},
                },
                "toolu_1",
                {},
            )
        )

        self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")

    def test_table_analysis_post_tool_hook_records_table_chart_call(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请用图展示上传表格里的销售额",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {}
        hook = data_analysis_post_tool_hook_factory(payload, state)
        tool_payload = {
            "success": True,
            "data": {
                "display_type": "structured_analysis_result",
                "chart_requested": True,
                "columns": [
                    {"name": "区域", "semantic_type": "dimension"},
                    {"name": "销售额", "semantic_type": "metric"},
                ],
                "rows": [{"区域": "东区", "销售额": "10"}],
                "row_count": 1,
                "chart": {
                    "id": "chart_region_amount",
                    "type": "bar",
                    "contract": {
                        "id": "chart_region_amount",
                        "type": "bar",
                        "encoding": {
                            "x": {"field": "区域", "role": "dimension"},
                            "value": {"field": "销售额", "role": "metric", "aggregate": "sum"},
                        },
                        "display": {"language": "zh-CN", "table_visible": False},
                    },
                    "validation": {"status": "pass", "issues": []},
                },
            },
        }

        output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__table_analysis",
                    "tool_response": {"content": [{"type": "text", "text": json.dumps(tool_payload)}]},
                },
                "toolu_table_chart",
                {},
            )
        )

        self.assertEqual(output, {})
        self.assertEqual(state["table_analysis_calls"][0]["chart_id"], "chart_region_amount")
        self.assertEqual(state["chart_contracts"]["chart_region_amount"]["type"], "bar")
        self.assertNotIn("db_query_calls", state)

    def test_data_analysis_pre_tool_hook_requires_explicit_only_chart_name(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"}}
        hook = data_analysis_pre_tool_hook_factory(payload, state)

        output = asyncio.run(
            hook(
                {
                    "tool_name": "mcp__weknora__db_query",
                    "tool_input": {
                        "sql": "SELECT region, SUM(amount) amount FROM orders GROUP BY region",
                        "chart_requested": True,
                        "preferred_chart": "radar",
                    },
                },
                "toolu_1",
                {},
            )
        )

        self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "deny")
        self.assertIn("显式点名", output["hookSpecificOutput"]["permissionDecisionReason"])

    def test_classify_data_analysis_display_intent_returns_structured_result(self):
        captured = {}

        class Options:
            def __init__(self, **kwargs):
                captured["options"] = kwargs

        async def fake_query(prompt, options):
            captured["prompt"] = prompt
            yield Message(
                [
                    {
                        "type": "text",
                        "text": json.dumps(
                            {
                                "chart_requested": True,
                                "confidence": "high",
                                "preferred_chart": "stacked_bar",
                                "reason": "用户要求用图展示上一轮数据分析。",
                            },
                            ensure_ascii=False,
                        ),
                    }
                ]
            )

        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="没看到图啊，请用图展示",
            history=[{"role": "assistant", "content": "上一轮给出了客户分层和商品大类销售额分析。"}],
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        intent = asyncio.run(classify_data_analysis_display_intent(payload, fake_query, Options, {}, "claude-test", None, Path(".")))

        self.assertTrue(intent["chart_requested"])
        self.assertNotIn("table_requested", intent)
        self.assertEqual(intent["confidence"], "high")
        self.assertEqual(intent["preferred_chart"], "stacked_bar")
        self.assertEqual(captured["options"]["tools"], [])
        self.assertEqual(captured["options"]["allowed_tools"], [])
        self.assertIn("没看到图啊，请用图展示", captured["prompt"])
        self.assertIn("上一轮给出了客户分层", captured["prompt"])
        self.assertNotIn("table_requested", captured["prompt"])
        self.assertIn("不要判断数据源是否存在、是否可用、是否有数据", captured["options"]["system_prompt"])
        self.assertIn("本步骤只判断用户是否想要图表，不判断图表是否最终能生成", captured["prompt"])

    def test_parse_mcp_tool_response_payload_accepts_text_block_list(self):
        payload = parse_mcp_tool_response_payload(
            [
                {
                    "type": "text",
                    "text": json.dumps(
                        {
                            "success": True,
                            "data": {
                                "display_type": "structured_analysis_result",
                                "chart_requested": True,
                            },
                        }
                    ),
                }
            ]
        )

        self.assertTrue(payload["success"])
        self.assertEqual(payload["data"]["display_type"], "structured_analysis_result")

    def test_data_analysis_final_answer_pre_hook_allows_markdown_table_with_chart_output(self):
        previous = os.environ.get("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE")
        os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = "0"
        try:
            payload = ChatPayload(
                run_id="run-1",
                session_id="session-1",
                assistant_message_id="assistant-1",
                query="画图分析各区域销售情况",
                llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
                tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            )
            state = {
                DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
                "db_query_calls": [
                    {
                        "chart_id": "chart_region_amount",
                        "contract": {"id": "chart_region_amount", "type": "bar"},
                        "result": {"chart_requested": True},
                        "validation_issues": [],
                    }
                ],
            }
            events = []
            hook = data_analysis_final_answer_pre_tool_hook_factory(payload, state, lambda *args, **kwargs: None, object, {}, "", None, Path("."), events.append)

            output = asyncio.run(
                hook(
                    {
                        "tool_name": "mcp__weknora__final_answer",
                        "tool_input": {
                            "content": "| 区域 | 销售额 |\n| --- | --- |\n| 东区 | 10 |\n\n{{chart:chart_region_amount}}",
                        },
                    },
                    "toolu_final",
                    {},
                )
            )

            self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")
            self.assertEqual(state["final_validation_attempts"], 1)
            self.assertEqual(events[-1].data["validation_issue_codes"], [])
            self.assertNotIn("has_markdown_table", events[-1].data["final_answer_candidate"])
            self.assertIn("chart_region_amount", events[-1].data["final_answer_candidate"]["referenced_chart_ids"])
        finally:
            if previous is None:
                os.environ.pop("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE", None)
            else:
                os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = previous

    def test_data_analysis_final_answer_pre_hook_skips_validation_without_chart_output(self):
        previous = os.environ.get("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE")
        os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = "0"
        try:
            payload = ChatPayload(
                run_id="run-1",
                session_id="session-1",
                assistant_message_id="assistant-1",
                query="分析各区域销售情况",
                llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
                tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            )
            state = {}
            events = []
            hook = data_analysis_final_answer_pre_tool_hook_factory(payload, state, lambda *args, **kwargs: None, object, {}, "", None, Path("."), events.append)

            output = asyncio.run(
                hook(
                    {
                        "tool_name": "mcp__weknora__final_answer",
                        "tool_input": {
                            "content": "东区销售额最高，南区次之，西区最低。",
                        },
                    },
                    "toolu_final",
                    {},
                )
            )

            self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")
            self.assertNotIn("final_validation_attempts", state)
            self.assertEqual(events, [])
        finally:
            if previous is None:
                os.environ.pop("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE", None)
            else:
                os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = previous

    def test_data_analysis_final_answer_pre_hook_accepts_valid_chart_placeholder(self):
        previous = os.environ.get("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE")
        os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = "0"
        try:
            payload = ChatPayload(
                run_id="run-1",
                session_id="session-1",
                assistant_message_id="assistant-1",
                query="画图分析各区域销售情况",
                llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
                tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            )
            state = {
                DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
                "db_query_calls": [
                    {
                        "chart_id": "chart_region_amount",
                        "contract": {
                            "id": "chart_region_amount",
                            "type": "bar",
                            "encoding": {
                                "x": {"field": "region", "role": "dimension"},
                                "value": {"field": "amount", "role": "metric", "aggregate": "sum"},
                            },
                            "transform": {"group_by": ["region"], "aggregate": "sum", "dedupe_policy": "aggregate"},
                            "display": {"language": "zh-CN", "table_visible": False},
                        },
                        "result": {"chart_requested": True},
                        "validation_issues": [],
                    }
                ],
            }
            events = []
            hook = data_analysis_final_answer_pre_tool_hook_factory(payload, state, lambda *args, **kwargs: None, object, {}, "", None, Path("."), events.append)

            output = asyncio.run(
                hook(
                    {
                        "tool_name": "mcp__weknora__final_answer",
                        "tool_input": {
                            "content": "各区域销售额对比如下。\n\n{{chart:chart_region_amount}}\n\n东区表现最好。",
                        },
                    },
                    "toolu_final",
                    {},
                )
            )

            self.assertEqual(output["hookSpecificOutput"]["permissionDecision"], "allow")
            self.assertEqual(state["final_answer_prevalidated_content"], "各区域销售额对比如下。\n\n{{chart:chart_region_amount}}\n\n东区表现最好。")
            self.assertEqual(
                [event.message for event in events],
                ["正在校验数据分析最终答案", "正在校验图表占位符和图表引用规则", "最终校验通过"],
            )
            self.assertTrue(events[-1].done)
        finally:
            if previous is None:
                os.environ.pop("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE", None)
            else:
                os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = previous

    def test_data_analysis_stop_hook_blocks_once_then_allows_second_attempt(self):
        previous = os.environ.get("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE")
        os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = "0"
        try:
            payload = ChatPayload(
                run_id="run-1",
                session_id="session-1",
                assistant_message_id="assistant-1",
                query="画柱状图分析各区域销售情况",
                llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
                tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            )
            state = {
                DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
                "db_query_calls": [
                    {
                        "chart_id": "chart_region_amount",
                        "contract": {
                            "id": "chart_region_amount",
                            "type": "bar",
                            "encoding": {
                                "x": {"field": "region", "role": "dimension"},
                                "value": {"field": "amount", "role": "metric", "aggregate": "sum"},
                            },
                            "transform": {"group_by": ["region"], "aggregate": "sum", "dedupe_policy": "aggregate"},
                            "display": {"language": "zh-CN", "table_visible": False},
                        },
                        "result": {"chart_requested": True},
                        "validation_issues": [],
                    }
                ],
                "chart_contracts": {"chart_region_amount": {"id": "chart_region_amount", "type": "bar"}},
            }
            with tempfile.TemporaryDirectory() as tmp:
                transcript = Path(tmp) / "transcript.jsonl"
                transcript.write_text(
                    json.dumps(
                        {
                            "type": "assistant",
                            "message": {
                                "role": "assistant",
                                "content": [{"type": "text", "text": "各区域销售额差异明显。"}],
                            },
                        },
                        ensure_ascii=False,
                    )
                    + "\n",
                    encoding="utf-8",
                )
                events = []
                hook = data_analysis_stop_hook_factory(payload, state, lambda *args, **kwargs: None, object, {}, "", None, Path(tmp), events.append)

                first = asyncio.run(hook({"transcript_path": str(transcript)}, None, {}))
                second = asyncio.run(hook({"transcript_path": str(transcript)}, None, {}))

            self.assertEqual(first["decision"], "block")
            self.assertEqual(second, {})
            self.assertTrue(state["validation_bypassed"])
            self.assertEqual(state["final_validation_attempts"], 2)
            self.assertEqual(
                [event.message for event in events],
                [
                    "正在校验数据分析最终答案提交方式",
                    "最终答案未通过提交方式校验，正在要求智能体修正",
                    "正在校验数据分析最终答案提交方式",
                    "数据分析最终答案校验已达到最大次数，继续输出",
                ],
            )
            self.assertTrue(events[-1].done)
        finally:
            if previous is None:
                os.environ.pop("CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE", None)
            else:
                os.environ["CUSTOM_GENERAL_AGENT_DATA_ANALYSIS_LLM_JUDGE"] = previous

    def test_deterministic_final_validation_skips_without_chart_output(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        issues = deterministic_final_validation(payload, "东区销售额最高，南区次之，西区最低。", {})

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_requires_chart_when_intent_requests_chart(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="没看到图啊，请用图展示",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"}}

        issues = deterministic_final_validation(payload, "各区域销售额差异明显。", state)

        codes = {issue["code"] for issue in issues}
        self.assertIn("missing_chart_query", codes)
        self.assertIn("missing_chart_placeholder", codes)
        self.assertTrue(any("chart_requested=true" in issue["message"] for issue in issues))

    def test_deterministic_final_validation_uses_table_analysis_call_state(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请画图分析表格里的销售额",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "table_analysis_calls": [
                {
                    "chart_id": "chart_region_amount",
                    "contract": {"id": "chart_region_amount", "type": "bar"},
                    "result": {
                        "chart_requested": True,
                        "source_mapping": {"说明": "区域来自 A 列，销售额来自 B 列汇总"},
                    },
                    "validation_issues": [],
                }
            ],
        }

        issues = deterministic_final_validation(payload, "各区域销售如下。\n\n{{chart:chart_region_amount}}", state)

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_requires_table_analysis_source_mapping(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请画图分析表格里的销售额",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "table_analysis_calls": [
                {
                    "chart_id": "chart_region_amount",
                    "contract": {"id": "chart_region_amount", "type": "bar"},
                    "result": {"chart_requested": True},
                    "validation_issues": [],
                }
            ],
        }

        issues = deterministic_final_validation(payload, "各区域销售如下。\n\n{{chart:chart_region_amount}}", state)

        self.assertTrue(any(issue["code"] == "missing_source_mapping" for issue in issues))

    def test_deterministic_final_validation_allows_markdown_table_with_chart_output(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "db_query_calls": [
                {
                        "chart_id": "chart_region_amount",
                        "contract": {"id": "chart_region_amount", "type": "bar"},
                        "result": {"chart_requested": True},
                        "validation_issues": [],
                }
            ],
        }

        issues = deterministic_final_validation(
            payload,
            "| 区域 | 销售额 |\n| --- | --- |\n| 东区 | 10 |\n\n{{chart:chart_region_amount}}",
            state,
        )

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_ignores_markdown_table_without_chart_output(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": False, "confidence": "high"}}

        self.assertFalse(data_analysis_needs_chart_validation(state, "| 区域 | 销售额 |\n| --- | --- |\n| 东区 | 10 |", payload))
        issues = deterministic_final_validation(
            payload,
            "| 区域 | 销售额 |\n| --- | --- |\n| 东区 | 10 |",
            state,
        )

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_does_not_force_unreferenced_exploratory_charts(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        valid_contract = {
            "type": "bar",
            "encoding": {
                "x": {"field": "region", "role": "dimension"},
                "value": {"field": "amount", "role": "metric", "aggregate": "sum"},
            },
            "display": {"language": "zh-CN", "table_visible": False},
        }
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "db_query_calls": [
                {
                    "chart_id": "chart_used",
                    "contract": {"id": "chart_used", **valid_contract},
                    "result": {"chart_requested": True},
                    "validation_issues": [],
                },
                {
                    "chart_id": "chart_exploratory",
                    "contract": {"id": "chart_exploratory", **valid_contract},
                    "result": {"chart_requested": True},
                    "validation_issues": [],
                },
            ],
        }

        issues = deterministic_final_validation(payload, "各区域销售如下。\n\n{{chart:chart_used}}", state)

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_ignores_chart_contract_spec_issues(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "db_query_calls": [
                {
                    "chart_id": "chart_region_amount",
                    "contract": {"id": "chart_region_amount"},
                    "result": {"chart_requested": True},
                    "validation_issues": ["ChartContract missing type/encoding/value fields."],
                }
            ],
        }

        issues = deterministic_final_validation(payload, "各区域销售如下。\n\n{{chart:chart_region_amount}}", state)

        self.assertEqual(issues, [])

    def test_deterministic_final_validation_checks_final_answer_chart_ids(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析各区域销售情况",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )
        state = {
            DATA_ANALYSIS_DISPLAY_INTENT_STATE_KEY: {"chart_requested": True, "confidence": "high"},
            "final_answer_requested_chart_ids": ["chart_other"],
            "db_query_calls": [
                {
                    "chart_id": "chart_region_amount",
                    "contract": {
                        "id": "chart_region_amount",
                        "type": "bar",
                        "encoding": {
                            "x": {"field": "region", "role": "dimension"},
                            "value": {"field": "amount", "role": "metric", "aggregate": "sum"},
                        },
                        "display": {"language": "zh-CN", "table_visible": False},
                    },
                    "result": {"chart_requested": True},
                    "validation_issues": [],
                }
            ],
        }

        issues = deterministic_final_validation(payload, "各区域销售如下。\n\n{{chart:chart_region_amount}}", state)

        codes = {issue["code"] for issue in issues}
        self.assertIn("declared_chart_without_placeholder", codes)
        self.assertIn("placeholder_not_declared", codes)

    def test_run_data_analysis_judge_disables_thinking(self):
        captured = {}

        class TextBlock:
            def __init__(self, text):
                self.text = text

        class SDKMessage:
            def __init__(self, text):
                self.content = [TextBlock(text)]

        class Options:
            def __init__(self, **kwargs):
                captured["options"] = kwargs

        async def fake_query(prompt, options):
            captured["prompt"] = prompt
            captured["options_instance"] = options
            yield SDKMessage('{"pass": true, "severity": "none", "issues": [], "repair_instruction": ""}')

        result = asyncio.run(
            run_data_analysis_judge(
                fake_query,
                Options,
                {},
                "claude-test",
                None,
                Path("."),
                {"user_request": "画图分析销售", "final_answer": "结论。"},
            )
        )

        self.assertTrue(result["pass"])
        self.assertEqual(captured["options"]["thinking"], {"type": "disabled"})
        self.assertEqual(captured["options"]["tools"], [])
        self.assertEqual(captured["options"]["allowed_tools"], [])
        self.assertNotIn("max_budget_usd", captured["options"])
        self.assertIn("Perform one concise semantic review", captured["prompt"])
        self.assertIn("query_results", captured["prompt"])
        self.assertIn("reference facts only", captured["prompt"])
        self.assertNotIn("visual_scope", captured["prompt"])

    def test_data_analysis_judge_only_blocks_blocker_issues(self):
        warning_only = {
            "pass": False,
            "severity": "warning",
            "issues": [{"severity": "warning", "code": "style", "message": "可读性可优化"}],
            "repair_instruction": "可选优化",
        }
        blockers = judge_issues(warning_only, "llm_judge")
        self.assertEqual(blockers, [])

        blocker_result = {
            "pass": False,
            "severity": "blocker",
            "issues": [{"severity": "blocker", "code": "wrong_chart", "message": "图文错配"}],
        }
        blockers = judge_issues(blocker_result, "llm_judge")
        self.assertEqual(blockers[0]["code"], "llm_judge:wrong_chart")
        self.assertEqual(blockers[0]["severity"], "blocker")

    def test_terminal_background_tool_ids_extracts_completed_notifications(self):
        msg = Message(
            [
                {
                    "type": "text",
                    "text": (
                        '<task-notification tool-use-id="toolu_done" status="completed">'
                        "Background task finished"
                        "</task-notification>"
                    ),
                },
                {
                    "type": "text",
                    "text": (
                        '<task-notification tool-use-id="toolu_running" status="running">'
                        "Still running"
                        "</task-notification>"
                    ),
                },
            ]
        )

        self.assertEqual(terminal_background_tool_ids(msg), {"toolu_done"})

    def test_terminal_background_tool_ids_handles_self_closing_notifications(self):
        msg = Message('<task-notification tool-use-id="toolu_failed" status="failed" />')

        self.assertEqual(terminal_background_tool_ids(msg), {"toolu_failed"})

    def test_terminal_background_tool_ids_handles_json_fields(self):
        msg = Message('<task-notification>{"tool-use-id":"toolu_json","status":"completed"}</task-notification>')

        self.assertEqual(terminal_background_tool_ids(msg), {"toolu_json"})

    def test_build_background_task_resume_prompt_keeps_sdk_running(self):
        prompt = build_background_task_resume_prompt({"toolu_1"}, 2)

        self.assertIn("toolu_1", prompt)
        self.assertIn("Do not provide a final answer yet", prompt)
        self.assertIn("Do not say that you will wait", prompt)
        self.assertIn("Do not use run_in_background again", prompt)
        self.assertIn("user's configured language", prompt)

    def test_build_system_prompt_contains_final_review_policy(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成一份报告",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_system_prompt(payload)

        self.assertIn("Final self-review", prompt)
        self.assertIn("Artifact review", prompt)
        self.assertIn("Review limit", prompt)
        self.assertIn("user's original verbatim request", prompt)
        self.assertIn("Copy the matching `cite_exactly` value verbatim", prompt)
        self.assertIn('each supplied value uses the canonical form `<src id="S1" />`', prompt)
        self.assertIn("every supported row/item", prompt)
        self.assertNotIn("Never use another citation", prompt)
        self.assertIn("Generate the answer once", prompt)
        self.assertIn("never asks the model to validate or regenerate citations", prompt)
        self.assertIn("after the last tool result, always finish this same run", prompt)
        self.assertIn("Never end the run on a tool call, tool result", prompt)
        self.assertIn("do not request or perform a second validation or regeneration pass", prompt)
        self.assertNotIn("local self-review of citation", prompt)
        self.assertNotIn("<doc source_id=", prompt)

    def test_build_prompt_expires_prior_turn_output_constraints(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="回答当前问题",
            history=[
                ChatHistoryMessage(role="user", content="回答末尾必须输出 OLD-MARKER"),
                ChatHistoryMessage(role="assistant", content="上一轮答案 OLD-MARKER"),
            ],
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            tool_callback_url="http://runtime-entry:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(payload)

        self.assertIn("Prior-turn output formats, suffixes, citation instructions, and one-time constraints have expired", prompt)
        self.assertIn("Do not carry forward an earlier turn's output format", prompt)
        self.assertLess(prompt.index("回答当前问题"), prompt.index("OLD-MARKER"))

    def test_build_system_prompt_prepends_builtin_environment_safety_policy(self):
        for agent_type in ("general-agent", "document-processing-agent", "data-analysis", "table-analysis"):
            with self.subTest(agent_type=agent_type):
                payload = ChatPayload(
                    run_id="run-1",
                    session_id="session-1",
                    assistant_message_id="assistant-1",
                    query="执行任务",
                    system_prompt="Editable agent instructions.",
                    llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                    runtime_config=RuntimeConfigSpec(agent_type=agent_type),
                    tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
                )

                prompt = build_system_prompt(payload)

                self.assertTrue(prompt.startswith(BUILTIN_ENVIRONMENT_SAFETY_SYSTEM_PROMPT.strip()))
                self.assertLess(prompt.index("Highest Priority"), prompt.index("Editable agent instructions."))
                self.assertLess(prompt.index("禁止进行任何可能破坏环境的高危操作"), prompt.index("Editable agent instructions."))
                self.assertLess(prompt.index("Editable agent instructions."), prompt.index("You are WeKnora's general-purpose agent runtime"))
                self.assertIn("non-editable built-in platform instruction", prompt)
                self.assertIn("must not override, weaken, hide, rewrite, or ignore it", prompt)
                self.assertIn("任何可能危害本系统或关联系统运行环境网络安全的实际操作", prompt)
                self.assertIn("这是最高指令，不能被其它指令改写", prompt)
                self.assertIn("network security of this system or any related system's runtime environment", prompt)
                self.assertIn("destructive filesystem or database operations", prompt)
                self.assertIn("Copy the matching `cite_exactly` value verbatim", prompt)
                self.assertIn('each supplied value uses the canonical form `<src id="S1" />`', prompt)
                self.assertNotIn("Never use another citation", prompt)
                self.assertIn("Generate the answer once", prompt)

    def test_build_system_prompt_limits_professional_skill_reads_to_current_run(self):
        for agent_type in ("general-agent", "document-processing-agent", "data-analysis", "table-analysis"):
            with self.subTest(agent_type=agent_type):
                payload = ChatPayload(
                    run_id="run-1",
                    session_id="session-1",
                    assistant_message_id="assistant-1",
                    query="使用专业技能",
                    llm=LLMConfig(model_name="claude-test", api_key="test-key"),
                    runtime_config=RuntimeConfigSpec(
                        agent_type=agent_type,
                        allowed_professional_skills=["find-skill-skillhub"],
                    ),
                    tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
                )

                prompt = build_system_prompt(payload)

                self.assertIn("read its SKILL.md, references and scripts only", prompt)
                self.assertIn("current SDK working directory path `.claude/skills/<name>`", prompt)
                self.assertIn("historical run directories", prompt)
                self.assertIn("/tmp/weknora-general-agent-runs", prompt)

    def test_build_system_prompt_contains_execution_limits(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成一份报告",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(max_iterations=42, llm_call_timeout=123),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_system_prompt(payload)

        self.assertIn("max_turns=42", prompt)
        self.assertIn("API_TIMEOUT_MS=123000", prompt)
        self.assertIn("single LLM/API call may wait at most 123 seconds", prompt)
        self.assertIn('"max_iterations": 42', prompt)
        self.assertIn('"claude_sdk_max_turns": 42', prompt)
        self.assertIn("Never use Bash with run_in_background=true", prompt)
        self.assertIn("Every started task must remain observable in the current run", prompt)
        self.assertIn("Separate runtime validation LLM judge calls", prompt)
        self.assertIn("does not change the main agent thinking mode", prompt)
        self.assertIn("Mandatory language contract", prompt)

    def test_data_analysis_prompt_materializes_runtime_reference_path(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="画图分析销售",
            system_prompt="Use {{data_analysis_runtime_reference_path}} and {{data_analysis_runtime_reference_absolute_path}}.",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_data_analysis_reference_doc(payload, Path(tmp))
            self.assertIsNotNone(prepared)
            assert prepared is not None
            reference_path = Path(tmp) / prepared.path
            self.assertTrue(reference_path.is_file())
            self.assertEqual(prepared.path, "generated/data_analysis/runtime_reference.md")
            prompt = build_system_prompt(payload, data_analysis_reference=prepared)

        self.assertIn("generated/data_analysis/runtime_reference.md", prompt)
        self.assertIn("data_analysis_runtime_reference_path", prompt)
        self.assertIn("chart hints", prompt)
        self.assertNotIn("{{data_analysis_runtime_reference_path}}", prompt)
        self.assertNotIn("{{data_analysis_runtime_reference_absolute_path}}", prompt)

    def test_document_processing_prompt_describes_create_artifact_excel_config(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 Excel",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )

        prompt = build_system_prompt(payload)

        self.assertNotIn("Excel document-generation requirement", prompt)
        self.assertNotIn("runtime will also check `xl/styles.xml` `cellXfs`", prompt)
        self.assertIn("create_artifact", prompt)
        self.assertIn("excel_style_apply_check", prompt)
        self.assertIn("disabled_apply_attributes", prompt)
        self.assertIn('{"disabled_apply_attributes":["applyBorder"],"reason":"用户明确要求不要框线"}', prompt)
        self.assertIn("create_artifact is a delivery/safety step only", prompt)
        self.assertIn("does not repeat content/style/layout quality review", prompt)
        self.assertIn("Do not duplicate deterministic PPTX package/XML checks in review_artifacts", prompt)
        self.assertIn("It does not judge content quality, user-request alignment or visual style", prompt)
        self.assertIn("Document-processing final delivery check", prompt)

    def test_selected_knowledge_original_requires_fragment_evidence_for_factual_text(self):
        payload = ChatPayload(
            run_id="run-document-citations",
            session_id="session-document-citations",
            assistant_message_id="assistant-document-citations",
            query="总结已选文件",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            original_input_files=[
                OriginalInputFileSpec(
                    id="knowledge-1",
                    file_name="policy.docx",
                    knowledge_id="knowledge-1",
                    download_url="http://app-dev:8080/api/v1/knowledge/knowledge-1/download",
                )
            ],
        )

        prompt = build_system_prompt(payload)

        self.assertIn("A local Read/Bash result is not a citeable document fragment", prompt)
        self.assertIn("use an available WeKnora knowledge-retrieval tool", prompt)
        self.assertIn("returned fragment source handles", prompt)
        self.assertIn("pure file transformation or delivery statements", prompt)

    def test_prepare_ppt_generation_workspace_materializes_open_renderer(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_ppt_generation_workspace(payload, Path(tmp))

            self.assertIsNotNone(prepared)
            assert prepared is not None
            renderer = Path(tmp) / prepared.renderer_path
            spec_template = Path(tmp) / prepared.spec_template_path
            readme = Path(tmp) / prepared.readme_path
            self.assertTrue(renderer.is_file())
            self.assertTrue(spec_template.is_file())
            self.assertTrue(readme.is_file())
            compile(renderer.read_text(encoding="utf-8"), str(renderer), "exec")
            spec = json.loads(spec_template.read_text(encoding="utf-8"))

        self.assertEqual(spec["slides"][0]["layout"], "freeform")
        self.assertIn("custom_operations", spec["extensions"])
        self.assertIn("generated/ppt/render_pptx.py", prepared.xml)
        self.assertIn("does not constrain final style", prepared.xml)
        self.assertIn("Do not create long PPT Python scripts through Bash heredocs", prepared.xml)

    def test_prepare_ppt_generation_workspace_only_for_document_agent(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="general-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_ppt_generation_workspace(payload, Path(tmp))

            self.assertIsNone(prepared)
            self.assertFalse((Path(tmp) / "generated" / "ppt").exists())

    def test_document_processing_prompt_describes_ppt_generation_workspace(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )

        with tempfile.TemporaryDirectory() as tmp:
            workspace = prepare_ppt_generation_workspace(payload, Path(tmp))
            prompt = build_system_prompt(payload, ppt_workspace=workspace)

        self.assertIn("generated/ppt/deck_spec.template.json", prompt)
        self.assertIn("generated/ppt/deck_spec.json", prompt)
        self.assertIn("generated/ppt/render_pptx.py", prompt)
        self.assertIn("does not constrain final style", prompt)
        self.assertIn("Do not create long PPT Python scripts through Bash heredocs", prompt)
        self.assertIn("Do not duplicate deterministic PPTX package/XML checks in review_artifacts", prompt)
        self.assertIn("Only confirm that the intended artifacts were registered", prompt)

    def test_prepare_document_template_context_includes_ppt_files(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            document_template_context=DocumentTemplateContextSpec(
                files=[
                    DocumentTemplateFileSpec(
                        role="requirement",
                        format="ppt",
                        variable="ppt_template_requirement",
                        source="builtin",
                        builtin_id="gbt9704_2012_ppt_requirement",
                        file_name="ppt.md",
                        file_type="md",
                        content_base64=base64.b64encode(b"# PPT rules").decode("ascii"),
                    ),
                    DocumentTemplateFileSpec(
                        role="reference",
                        format="ppt",
                        variable="ppt_template_files[1]",
                        source="upload",
                        file_name="template.pptx",
                        file_type="pptx",
                        content_base64=base64.b64encode(b"pptx bytes").decode("ascii"),
                    ),
                ]
            ),
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_document_template_context(payload, Path(tmp))
            self.assertIn('<format name="ppt" display_name="PPT">', prepared.xml)
            self.assertIn("document_templates/ppt/requirement/ppt.md", prepared.replacements["ppt_template_requirement"])
            self.assertIn("document_templates/ppt/references/01_template.pptx", prepared.replacements["ppt_template_files"])
            self.assertTrue((Path(tmp) / "document_templates" / "ppt" / "requirement" / "ppt.md").is_file())
            self.assertTrue((Path(tmp) / "document_templates" / "ppt" / "references" / "01_template.pptx").is_file())

    def test_prepare_document_template_context_keeps_ppt_and_word_reference_limit_at_three(self):
        files = [
            DocumentTemplateFileSpec(
                role="reference",
                format="ppt",
                source="upload",
                file_name=f"template_{idx:02d}.pptx",
                file_type="pptx",
                content_base64=base64.b64encode(f"pptx bytes {idx}".encode("utf-8")).decode("ascii"),
            )
            for idx in range(1, 5)
        ]
        files.extend([
            DocumentTemplateFileSpec(
                role="reference",
                format="word",
                source="upload",
                file_name=f"word_{idx:02d}.docx",
                file_type="docx",
                content_base64=base64.b64encode(f"docx bytes {idx}".encode("utf-8")).decode("ascii"),
            )
            for idx in range(1, 5)
        ])
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            document_template_context=DocumentTemplateContextSpec(files=files),
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_document_template_context(payload, Path(tmp))

        self.assertIn('<reference_files variable="{{ppt_template_files}}" count="3">', prepared.xml)
        self.assertIn("document_templates/ppt/references/03_template_03.pptx", prepared.replacements["ppt_template_files"])
        self.assertNotIn("document_templates/ppt/references/04_template_04.pptx", prepared.replacements["ppt_template_files"])
        self.assertIn('<reference_files variable="{{word_template_files}}" count="3">', prepared.xml)
        self.assertIn("document_templates/word/references/03_word_03.docx", prepared.replacements["word_template_files"])
        self.assertNotIn("document_templates/word/references/04_word_04.docx", prepared.replacements["word_template_files"])

    def test_document_template_context_is_near_user_request_before_visible_context(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 Word 和 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            visible_context={
                "agent": {"system_prompt": "large duplicated system prompt", "name": "文档处理"},
                "current_turn": {
                    "user_request_verbatim": "请生成 Word 和 PPT",
                    "image_urls": ["http://example.test/image.png"],
                },
                "effective_configuration": {
                    "allowed_tools": ["Read", "Write"],
                    "artifact_return_policy": {"max_artifact_count": 5},
                    "runtime_model_id": "model-1",
                },
            },
            document_template_context=DocumentTemplateContextSpec(
                files=[
                    DocumentTemplateFileSpec(
                        role="requirement",
                        format="word",
                        source="upload",
                        file_name="word.md",
                        file_type="md",
                        content_base64=base64.b64encode(b"# Word rules").decode("ascii"),
                    ),
                    DocumentTemplateFileSpec(
                        role="requirement",
                        format="ppt",
                        source="upload",
                        file_name="ppt.md",
                        file_type="md",
                        content_base64=base64.b64encode(b"# PPT rules").decode("ascii"),
                    ),
                ]
            ),
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_document_template_context(payload, Path(tmp))
            prompt = build_prompt(payload, prepared)

        self.assertLess(prompt.index("<document_template_preflight"), prompt.index("<weknora_context>"))
        self.assertLess(prompt.index("<document_template_context"), prompt.index("<visible_context"))
        self.assertIn("Read `document_templates/word/requirement/word.md`", prompt)
        self.assertIn("Read `document_templates/ppt/requirement/ppt.md`", prompt)
        self.assertIn("form a short internal delivery plan", prompt)
        self.assertIn('"runtime_model_id": "model-1"', prompt)
        self.assertNotIn("large duplicated system prompt", prompt)
        self.assertNotIn('"allowed_tools"', prompt)
        self.assertNotIn('"artifact_return_policy"', prompt)

    def test_effective_lightweight_skills_are_the_only_model_visible_skill_source(self):
        skill = LightweightSkillSpec(
            key="lightweight:managed:skill-1",
            name="制度助手",
            description="制度问答",
            instructions="回答前先检索制度依据，并按制度流程组织答案。",
        )
        payload = ChatPayload(
            run_id="run-lightweight",
            session_id="session-lightweight",
            assistant_message_id="assistant-lightweight",
            query="差旅报销需要哪些材料？",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="general-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            lightweight_skill_policy=(
                "Every effective lightweight skill is active regardless of whether it came from agent configuration "
                "or a chat selection."
            ),
            lightweight_skills=[skill],
            visible_context={"agent": {"name": "通用智能体"}},
        )

        system_prompt = build_system_prompt(payload)
        prompt = build_prompt(payload)
        summary = json.loads(runtime_summary(payload))

        self.assertTrue(summary["lightweight_skills_enabled"])
        self.assertEqual(summary["effective_lightweight_skill_names"], ["制度助手"])
        self.assertNotIn("skills_enabled", summary)
        self.assertNotIn("allowed_skills", summary)
        self.assertIn("Every effective lightweight skill is active", system_prompt)
        self.assertEqual(system_prompt.count("<effective_lightweight_skills"), 1)
        self.assertEqual(system_prompt.count("回答前先检索制度依据，并按制度流程组织答案。"), 1)
        self.assertNotIn("<effective_lightweight_skills", prompt)
        self.assertNotIn("selected_chat_skill_names", prompt)
        self.assertNotIn("configured_lightweight_skill_names", prompt)

    def test_build_prompt_omits_inline_base64_image_urls(self):
        inline = "data:image/jpeg;base64," + base64.b64encode(b"x" * 1024).decode("ascii")
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="总结下这张图",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            image_urls=[inline, "local://10002/chat-images/image.jpg"],
            image_description="图片里有砖厂和多堆砖坯。",
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(payload)

        self.assertNotIn(inline, prompt)
        self.assertNotIn("data:image/jpeg;base64", prompt)
        self.assertIn("[inline image/jpeg data omitted from text prompt; base64_length=", prompt)
        self.assertIn("local://10002/chat-images/image.jpg", prompt)
        self.assertIn("图片里有砖厂和多堆砖坯。", prompt)

    def test_prompt_media_reference_omits_inline_audio_base64(self):
        inline = "data:audio/wav;base64," + base64.b64encode(b"audio-bytes").decode("ascii")

        got = prompt_media_reference(inline)

        self.assertNotIn("audio-bytes", got)
        self.assertNotIn("base64,", got)
        self.assertIn("inline audio/wav data omitted", got)

    def test_build_prompt_includes_data_analysis_display_intent(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="没看到图啊，请用图展示",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="data-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(
            payload,
            data_analysis_display_intent={
                "chart_requested": True,
                "confidence": "high",
                "preferred_chart": "stacked_bar",
                "reason": "用户要求补图。",
            },
        )

        self.assertIn("<data_analysis_display_intent", prompt)
        self.assertIn('"chart_requested": true', prompt)
        self.assertNotIn('"table_requested"', prompt)
        self.assertIn("用户需要图表展示", prompt)
        self.assertIn("db_query with chart_requested=true", prompt)

    def test_build_prompt_includes_table_analysis_display_intent(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请用图展示表格里的销售趋势",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(
            payload,
            data_analysis_display_intent={
                "chart_requested": True,
                "confidence": "high",
                "preferred_chart": "line",
                "reason": "用户要求用图展示表格趋势。",
            },
        )

        self.assertIn("<table_analysis_display_intent", prompt)
        self.assertIn('"chart_requested": true', prompt)
        self.assertNotIn('"table_requested"', prompt)
        self.assertIn("用户需要图表展示", prompt)
        self.assertIn("table_analysis with chart_requested=true", prompt)
        self.assertNotIn("db_query with chart_requested=true", prompt)

    def test_build_prompt_ignores_table_display_intent(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请用图展示表格里的销售趋势，并列出明细表格",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="table-analysis"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(
            payload,
            data_analysis_display_intent={
                "chart_requested": True,
                "table_requested": True,
                "confidence": "high",
                "preferred_chart": "line",
                "reason": "用户要求用图展示表格趋势。",
            },
        )

        self.assertNotIn('"table_requested"', prompt)
        self.assertNotIn("table_analysis.table_requested", prompt)

    def test_system_prompt_points_to_document_template_context_without_inlining_xml(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 Word",
            system_prompt="Use {{document_template_context}} and {{document_template_usage_rules}} then {{word_template_requirement}}.",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            document_template_context=DocumentTemplateContextSpec(
                files=[
                    DocumentTemplateFileSpec(
                        role="requirement",
                        format="word",
                        source="upload",
                        file_name="word.md",
                        file_type="md",
                        content_base64=base64.b64encode(b"# Word rules").decode("ascii"),
                    )
                ]
            ),
        )

        with tempfile.TemporaryDirectory() as tmp:
            prepared = prepare_document_template_context(payload, Path(tmp))
            prompt = build_system_prompt(payload, document_templates=prepared)

        self.assertIn("Document template context is provided once", prompt)
        self.assertIn("document_templates/word/requirement/word.md", prompt)
        self.assertNotIn('<document_template_context source=', prompt)
        self.assertNotIn("<format name=", prompt)
        self.assertNotIn("{{document_template_context}}", prompt)
        self.assertNotIn("{{document_template_usage_rules}}", prompt)

    def test_validate_pptx_layout_detects_text_overlap(self):
        data = self.make_pptx_bytes(
            [
                (1000000, 1000000, 3000000, 900000, "第一段文字"),
                (1200000, 1100000, 3000000, 900000, "第二段文字"),
            ]
        )

        issues = validate_pptx_layout_bytes("deck.pptx", data)

        self.assertTrue(any(issue["code"] == "pptx_text_overlap" for issue in issues), issues)

    def test_document_pptx_layout_stop_hook_blocks_twice_then_allows_third_attempt(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )
        bad_pptx = self.make_pptx_bytes(
            [
                (1000000, 1000000, 3000000, 900000, "第一段文字"),
                (1200000, 1100000, 3000000, 900000, "第二段文字"),
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            store = ArtifactStore(Path(tmp), payload)
            store._store_bytes("deck.pptx", bad_pptx)
            state = {}
            events = []
            hook = document_pptx_layout_stop_hook_factory(payload, store, state, events.append)

            first = asyncio.run(hook({"transcript_path": ""}, None, {}))
            second = asyncio.run(hook({"transcript_path": ""}, None, {}))
            third = asyncio.run(hook({"transcript_path": ""}, None, {}))

        self.assertEqual(first["decision"], "block")
        self.assertEqual(second["decision"], "block")
        self.assertEqual(third, {})
        self.assertTrue(state["pptx_layout_validation_bypassed"])
        self.assertEqual(state["pptx_layout_validation_attempts"], 3)
        self.assertEqual(
            [event.message for event in events],
            [
                "正在校验 PPT 布局",
                "PPT 布局校验发现问题，正在自动修复",
                "正在校验 PPT 布局",
                "PPT 布局校验发现问题，正在自动修复",
                "正在校验 PPT 布局",
                "PPT 布局校验已达到最大修复次数，继续输出",
            ],
        )
        self.assertTrue(events[-1].done)

    def test_pptx_layout_hook_repair_can_reregister_same_filename_without_second_review(self):
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 PPT",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )
        bad_pptx = self.make_pptx_bytes(
            [
                (1000000, 1000000, 3000000, 900000, "第一段文字"),
                (1200000, 1100000, 3000000, 900000, "第二段文字"),
            ]
        )
        repaired_pptx = self.make_pptx_bytes(
            [
                (1000000, 1000000, 3000000, 900000, "第一段文字"),
                (1000000, 2200000, 3000000, 900000, "第二段文字"),
            ]
        )
        with tempfile.TemporaryDirectory() as tmp:
            store = ArtifactStore(Path(tmp), payload)
            target = store.run_dir / "deck.pptx"
            target.write_bytes(bad_pptx)

            review = store.review_artifacts(
                files=[{"filename": "deck.pptx", "file_path": "deck.pptx"}],
                passed=True,
                issues=[],
                user_request_alignment="checked",
                template_alignment="checked",
            )
            self.assertTrue(review["passed"])
            first = store.register_file("deck.pptx", "deck.pptx")

            state = {}
            hook = document_pptx_layout_stop_hook_factory(payload, store, state)
            blocked = asyncio.run(hook({"transcript_path": ""}, None, {}))
            self.assertEqual(blocked["decision"], "block")

            target.write_bytes(repaired_pptx)
            second = store.register_file("deck.pptx", "deck.pptx")

            other = store.run_dir / "other.pptx"
            other.write_bytes(repaired_pptx)
            with self.assertRaisesRegex(RuntimeError, "Artifact review required"):
                store.register_file("other.pptx", "other.pptx")

        self.assertNotEqual(first["sha256"], second["sha256"])
        self.assertEqual(second["filename"], "deck.pptx")

    def test_user_facing_error_message_maps_max_turns(self):
        msg = ResultMessage(subtype="error_max_turns", result="", errors=["maxTurns=30 turnCount=31"])

        self.assertEqual(user_facing_error_message(msg), MAX_TURNS_USER_MESSAGE)

    def test_user_facing_error_message_maps_timeout(self):
        msg = ResultMessage(result="API request timed out after API_TIMEOUT_MS")

        self.assertEqual(user_facing_error_message(msg), TIMEOUT_USER_MESSAGE)

    def test_result_message_text_uses_terminal_sdk_answer(self):
        msg = ResultMessage(result="  完整的最终回答  ")

        self.assertEqual(result_message_text(msg), "完整的最终回答")
        self.assertEqual(result_message_text(Message([], "")), "")

    def test_pending_background_task_error_message_is_user_facing(self):
        self.assertIn("后台任务未完成", PENDING_BACKGROUND_TASK_USER_MESSAGE)
        self.assertIn("继续等待执行结果", BACKGROUND_RESUME_PROGRESS_MESSAGE)

    def test_artifact_store_dedupes_duplicate_filenames_keep_last(self):
        first = SidecarArtifact(
            file_token="first",
            filename="report.xlsx",
            file_type="xlsx",
            file_size=10,
            sha256="old",
        )
        second = SidecarArtifact(
            file_token="second",
            filename="other.xlsx",
            file_type="xlsx",
            file_size=20,
            sha256="other",
        )
        third = SidecarArtifact(
            file_token="third",
            filename="report.xlsx",
            file_type="xlsx",
            file_size=30,
            sha256="new",
        )

        items = ArtifactStore._dedupe_by_filename_keep_last([first, second, third])

        self.assertEqual([item.file_token for item in items], ["second", "third"])

    def test_sanitize_artifact_bytes_patches_xlsx_apply_fill_only(self):
        styles = (
            '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
            '<cellXfs count="4">'
            '<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>'
            '<xf numFmtId="0" fontId="1" fillId="2" borderId="1"><alignment horizontal="center"/></xf>'
            '<xf numFmtId="0" fontId="2" fillId="3" borderId="1" applyFill="0"><alignment horizontal="center"/></xf>'
            '<xf numFmtId="0" fontId="3" fillId="4" borderId="1"/>'
            '</cellXfs>'
            '</styleSheet>'
        )
        patched = self.read_xlsx_styles(sanitize_artifact_bytes("report.xlsx", self.make_xlsx_bytes(styles)))

        self.assertIn('<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>', patched)
        self.assertIn('fillId="2" borderId="1" applyFill="1"><alignment', patched)
        self.assertIn('fillId="3" borderId="1" applyFill="1"><alignment', patched)
        self.assertIn('fillId="4" borderId="1" applyFill="1"/>', patched)

    def test_sanitize_artifact_bytes_leaves_non_xlsx_untouched(self):
        data = b"not an xlsx"

        self.assertIs(sanitize_artifact_bytes("report.csv", data), data)

    def test_sanitize_artifact_bytes_patches_all_xlsx_apply_attributes_when_enabled(self):
        styles = (
            '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
            '<cellXfs count="2">'
            '<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>'
            '<xf numFmtId="14" fontId="1" fillId="2" borderId="1">'
            '<alignment horizontal="center"/>'
            '<protection locked="0"/>'
            '</xf>'
            '</cellXfs>'
            '</styleSheet>'
        )
        patched = self.read_xlsx_styles(
            sanitize_artifact_bytes(
                "report.xlsx",
                self.make_xlsx_bytes(styles),
                patch_all_xlsx_apply_attributes=True,
            )
        )

        self.assertIn('<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>', patched)
        self.assertIn('applyFont="1"', patched)
        self.assertIn('applyBorder="1"', patched)
        self.assertIn('applyFill="1"', patched)
        self.assertIn('applyNumberFormat="1"', patched)
        self.assertIn('applyAlignment="1"', patched)
        self.assertIn('applyProtection="1"', patched)

    def test_document_processing_store_respects_create_artifact_excel_style_config(self):
        styles = (
            '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
            '<cellXfs count="2">'
            '<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>'
            '<xf numFmtId="0" fontId="1" fillId="2" borderId="1"><alignment horizontal="center"/></xf>'
            '</cellXfs>'
            '</styleSheet>'
        )
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成一个不要框线的 Excel",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="document-processing-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )
        with tempfile.TemporaryDirectory() as tmp:
            store = ArtifactStore(Path(tmp), payload)
            result = store._store_bytes(
                "report.xlsx",
                self.make_xlsx_bytes(styles),
                excel_style_apply_check={
                    "disabled_apply_attributes": ["applyBorder"],
                    "reason": "用户明确要求不要框线",
                },
            )
            patched = self.read_xlsx_styles((store.out_dir / result["file_token"]).read_bytes())

        self.assertNotIn('applyBorder="1"', patched)
        self.assertIn('applyFill="1"', patched)
        self.assertIn('applyFont="1"', patched)
        self.assertIn('applyAlignment="1"', patched)

    def test_general_agent_review_does_not_enforce_document_excel_cellxf_rules(self):
        styles = (
            '<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">'
            '<cellXfs count="2">'
            '<xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>'
            '<xf numFmtId="14" fontId="1" fillId="2" borderId="1"><alignment horizontal="center"/></xf>'
            '</cellXfs>'
            '</styleSheet>'
        )
        payload = ChatPayload(
            run_id="run-1",
            session_id="session-1",
            assistant_message_id="assistant-1",
            query="请生成 Excel",
            llm=LLMConfig(model_name="claude-test", api_key="test-key"),
            runtime_config=RuntimeConfigSpec(agent_type="general-agent"),
            tool_callback_url="http://app-dev:8080/api/v1/custom/general-agent/internal/tools/call",
            enable_artifacts=True,
        )
        with tempfile.TemporaryDirectory() as tmp:
            store = ArtifactStore(Path(tmp), payload)
            target = store.run_dir / "report.xlsx"
            target.write_bytes(self.make_xlsx_bytes(styles))

            result = store.review_artifacts(
                files=[{"filename": "report.xlsx", "file_path": "report.xlsx"}],
                passed=True,
                issues=[],
                user_request_alignment="checked",
                template_alignment="not applicable",
            )

        self.assertTrue(result["passed"])
        self.assertEqual(result["issues"] if "issues" in result else [], [])


if __name__ == "__main__":
    unittest.main()
