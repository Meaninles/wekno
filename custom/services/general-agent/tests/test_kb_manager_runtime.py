import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT))

from app.artifact_store import ArtifactStore
from app.runner import ( build_system_prompt, prepare_knowledge_manager_workspace)
from app.schemas import ChatPayload, LLMConfig, RuntimeConfigSpec  # noqa: E402


def manager_payload(system_prompt: str = "") -> ChatPayload:
    return ChatPayload(
        run_id="run-1",
        session_id="session-1",
        assistant_message_id="message-1",
        query="把旧文档替换成我上传的新文档",
        system_prompt=system_prompt,
        runtime_config=RuntimeConfigSpec(
            agent_id="agent-1",
            agent_type="knowledge-base-manager",
            knowledge_bases=["kb-a"],
            knowledge_ids=["doc-a"],
            knowledge_management={
                "explicit_selection": True,
                "whole_knowledge_base_ids": [],
                "documents": {"doc-a": "kb-a"},
                "effective_permissions": {
                    "kb-a": {"add": True, "modify": True, "delete": True}
                },
            },
        ),
        llm=LLMConfig(model_name="test-model", api_key="test-key"),
        tool_callback_url="http://app-dev:8080/internal/tools/call",
        enable_artifacts=True,
    )


class KnowledgeManagerRuntimeTest(unittest.TestCase):


    def test_all_agent_types_deliver_every_registered_file(self):
        with tempfile.TemporaryDirectory() as tmp:
            manager_store = ArtifactStore(Path(tmp) / "manager", manager_payload())
            for index in range(7):
                source = manager_store.run_dir / f"policy-{index + 1}.md"
                source.write_text(f"policy {index + 1}")
                manager_store.store_file(source.name, source)
            manager_result = manager_store.finalize_for_result()

        self.assertEqual(len(manager_result), 7)
        self.assertEqual(manager_store.original_count, 7)
        self.assertEqual(manager_store.returned_count, 7)
        self.assertEqual(manager_store.dropped_count, 0)
        self.assertEqual(manager_store.notice, "")

        general_payload = manager_payload()
        general_payload.runtime_config.agent_type = "general-agent"
        with tempfile.TemporaryDirectory() as tmp:
            general_store = ArtifactStore(Path(tmp) / "general", general_payload)
            for index in range(7):
                source = general_store.run_dir / f"report-{index + 1}.md"
                source.write_text(f"report {index + 1}")
                general_store.store_file(source.name, source)
            general_result = general_store.finalize_for_result()

        self.assertEqual(len(general_result), 7)
        self.assertEqual(general_store.dropped_count, 0)
        self.assertEqual(general_store.notice, "")


    def test_workspace_helpers_are_materialized_and_inspect_without_mutation(self):
        with tempfile.TemporaryDirectory() as tmp:
            root = Path(tmp)
            readme = prepare_knowledge_manager_workspace(manager_payload(), root)
            self.assertEqual(readme, "generated/kb_manager/README.md")
            workspace = root / "generated" / "kb_manager"
            self.assertTrue((workspace / "README.md").is_file())
            self.assertTrue((workspace / "compare_text.py").is_file())
            candidate = root / "candidate.md"
            candidate.write_text("new knowledge\n", encoding="utf-8")
            result = subprocess.run(
                [sys.executable, str(workspace / "inspect_candidate.py"), str(candidate)],
                check=True,
                capture_output=True,
                text=True,
            )
            profile = json.loads(result.stdout)
            self.assertEqual(profile["file_name"], "candidate.md")
            self.assertEqual(profile["text_lines"], 1)
            self.assertEqual(len(profile["sha256"]), 64)
            self.assertFalse(profile["empty"])

    def test_workspace_is_not_added_to_other_general_agents(self):
        payload = manager_payload()
        payload.runtime_config.agent_type = "general-agent"
        with tempfile.TemporaryDirectory() as tmp:
            self.assertEqual(prepare_knowledge_manager_workspace(payload, Path(tmp)), "")
            self.assertFalse((Path(tmp) / "generated" / "kb_manager").exists())


if __name__ == "__main__":
    unittest.main()
