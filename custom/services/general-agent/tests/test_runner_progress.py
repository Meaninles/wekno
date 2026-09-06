import asyncio
import base64
import io
import json
import os
import sys
import tempfile
import types
import unittest
import zipfile
from pathlib import Path
from unittest import mock


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from app.artifact_store import ArtifactStore
from app.runner import ( MAX_TURNS_USER_MESSAGE, TIMEOUT_USER_MESSAGE, ToolUseFragment, build_prompt, build_prompt_observation, build_system_prompt, claude_auth_env, GeneralAgentRunner, materialize_professional_skills, mcp_tool_result, normalize_professional_skill_path, prepare_data_analysis_reference_doc, prepare_document_template_context, prompt_media_reference, effective_weknora_tool_specs, raw_sdk_error_text, tool_result_fragments, tool_use_fragments, user_facing_error_message)
from app.schemas import (  # noqa: E402
    ChatPayload,
    ChatHistoryMessage,
    AttachmentSpec,
    DocumentTemplateContextSpec,
    DocumentTemplateFileSpec,
    LLMConfig,
    LightweightSkillSpec,
    OriginalInputFileSpec,
    ProfessionalSkillFileSpec,
    ProfessionalSkillSpec,
    RuntimeConfigSpec,
    RuntimeToolSpec,
    SidecarArtifact,
)


class Message:
    def __init__(self, content, stop_reason=""):
        self.content = content
        self.stop_reason = stop_reason


class ResultMessage:
    def __init__(self, subtype="", stop_reason="", result="", errors=None, is_error=False):
        self.subtype = subtype
        self.stop_reason = stop_reason
        self.result = result
        self.errors = errors
        self.is_error = is_error


class RunnerProgressTest(unittest.TestCase):

    def test_mcp_uses_exact_platform_output_without_duplicate_evidence_or_sidecar_handle_rules(self):
        canonical = '<chunk>exact source\ncitation_handle_for_this_evidence: <src id="S17" /></chunk>'
        original = {"success": True, "output": canonical, "data": {"results": [{"content": "duplicated source", "image_ocr": "duplicate image facts"}]},
                    "source_references": [{"cite_exactly": '<src id="S999" />'}]}
        result = mcp_tool_result(original)
        self.assertEqual(result["content"], [{"type": "text", "text": canonical}])
        self.assertEqual(original["data"]["results"][0]["content"], "duplicated source")
        self.assertFalse(result["is_error"])

    def test_mcp_error_and_data_only_results_remain_explicit(self):
        result = mcp_tool_result({"success": False, "error": "stale version"})
        self.assertTrue(result["is_error"])
        self.assertIn("stale version", result["content"][0]["text"])
        result = mcp_tool_result({"success": True, "data": {"next_offset": 42}})
        self.assertEqual(json.loads(result["content"][0]["text"])["data"]["next_offset"], 42)


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
        self.assertIsNone(settings)

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


    def test_platform_prompt_keeps_history_in_typed_messages(self):
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

        self.assertNotIn("OLD-MARKER", prompt)
        self.assertEqual(prompt.count("回答当前问题"), 1)
        self.assertNotIn("<current_user_request_replay", prompt)


    def test_build_prompt_uses_stable_user_sources_and_marks_assistant_history(self):
        payload = ChatPayload(
            run_id="run-source-ledger",
            session_id="session-source-ledger",
            assistant_message_id="assistant-source-ledger",
            query="只汇总当前状态",
            history=[
                ChatHistoryMessage(
                    role="user",
                    content="负责人是Lin",
                    source_id="user_turn_006",
                ),
                ChatHistoryMessage(
                    role="assistant",
                    content="可能还需要一个复核日期",
                    source_id="assistant_after_user_turn_006",
                ),
            ],
            llm=LLMConfig(model_name="claude-test", api_key="test-key", runtime_adapter="claude-sdk"),
            tool_callback_url="http://runtime-entry:8080/api/v1/custom/general-agent/internal/tools/call",
        )

        prompt = build_prompt(payload)

        self.assertIn('source_id="current_user_message" authority="current_user"', prompt)
        self.assertIn(
            '<message role="user" source_id="user_turn_006" authority="user_authored_fact_source">',
            prompt,
        )
        self.assertIn(
            'source_id="assistant_after_user_turn_006" authority="non_factual_unless_later_user_confirmed"',
            prompt,
        )


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
        self.assertNotIn("{{data_analysis_runtime_reference_path}}", prompt)
        self.assertNotIn("{{data_analysis_runtime_reference_absolute_path}}", prompt)


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

        self.assertNotIn("<document_template_preflight", prompt)
        self.assertLess(prompt.index("<document_template_context"), prompt.index("<visible_context"))
        self.assertIn('path="document_templates/word/requirement/word.md"', prompt)
        self.assertIn('path="document_templates/ppt/requirement/ppt.md"', prompt)
        visible_json = prompt.split('role="user_visible_context">\n', 1)[1].split('\n</visible_context>', 1)[0]
        self.assertEqual(json.loads(visible_json)['effective_configuration']['runtime_model_id'], 'model-1')
        self.assertNotIn("large duplicated system prompt", prompt)
        self.assertNotIn('"allowed_tools"', prompt)
        self.assertNotIn('"artifact_return_policy"', prompt)


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


    def test_user_facing_error_message_maps_max_turns(self):
        msg = ResultMessage(subtype="error_max_turns", result="", errors=["maxTurns=30 turnCount=31"])

        self.assertEqual(user_facing_error_message(msg), MAX_TURNS_USER_MESSAGE)

    def test_user_facing_error_message_maps_timeout(self):
        msg = ResultMessage(result="API request timed out after API_TIMEOUT_MS")

        self.assertEqual(user_facing_error_message(msg), TIMEOUT_USER_MESSAGE)


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


    def test_document_processing_store_preserves_original_excel_style_bytes(self):
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
            source = Path(tmp) / "report.xlsx"
            source.write_bytes(self.make_xlsx_bytes(styles))
            result = store.store_file(source.name, source)
            patched = self.read_xlsx_styles((store.out_dir / result["file_token"]).read_bytes())

        self.assertNotIn('applyBorder="1"', patched)
        self.assertEqual(styles, patched)


class GeneralAgentWorkBudgetTest(unittest.TestCase):
    def payload(self, agent_type: str, query: str = "Summarize the current policy evidence.") -> ChatPayload:
        return ChatPayload(
            run_id="budget-test",
            session_id="budget-session",
            assistant_message_id="budget-message",
            query=query,
            llm=LLMConfig(model_name="test", api_key="test"),
            runtime_config=RuntimeConfigSpec(agent_type=agent_type),
            tool_callback_url="http://127.0.0.1/tool",
        )


if __name__ == "__main__":
    unittest.main()
