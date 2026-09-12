from __future__ import annotations

import unittest

from app.tools import TOOL_NAMES, TOOL_SPECS, _write_projection


class ToolCatalogueTests(unittest.TestCase):
    def test_catalogue_has_expected_size_and_unique_names(self):
        self.assertEqual(len(TOOL_SPECS), 32)
        self.assertEqual(len(TOOL_NAMES), len(set(TOOL_NAMES)))
        self.assertIn("create_knowledge_base", TOOL_NAMES)
        self.assertIn("create_knowledge_from_content", TOOL_NAMES)

    def test_ingestion_write_projection_does_not_echo_content(self):
        projected = _write_projection(
            "create_knowledge_from_content",
            {
                "success": True,
                "data": {
                    "id": "knowledge-id",
                    "title": "title",
                    "content": "sensitive document body",
                    "parse_status": "pending",
                },
            },
        )
        self.assertEqual(projected["id"], "knowledge-id")
        self.assertNotIn("content", projected)

    def test_every_tool_has_complete_mcp_metadata(self):
        for spec in TOOL_SPECS:
            tool = spec.as_mcp_tool()
            self.assertTrue(tool.title, spec.name)
            self.assertTrue(tool.description, spec.name)
            self.assertEqual(tool.inputSchema.get("type"), "object", spec.name)
            properties = tool.inputSchema.get("properties", {})
            self.assertTrue(
                set(tool.inputSchema.get("required", [])).issubset(properties),
                spec.name,
            )
            self.assertTrue(tool.outputSchema, spec.name)
            self.assertEqual(tool.outputSchema.get("required"), ["result"], spec.name)
            self.assertIsNotNone(tool.annotations, spec.name)

    def test_file_input_requires_content_or_explicit_server_path(self):
        tool = next(spec.as_mcp_tool() for spec in TOOL_SPECS if spec.name == "create_knowledge_from_file")
        self.assertEqual(
            tool.inputSchema["oneOf"],
            [{"required": ["content_base64"]}, {"required": ["file_path"]}],
        )

    def test_external_network_tools_are_marked_open_world(self):
        for name in ("create_knowledge_from_url", "chat", "agent_chat"):
            tool = next(spec.as_mcp_tool() for spec in TOOL_SPECS if spec.name == name)
            self.assertTrue(tool.annotations.openWorldHint, name)


if __name__ == "__main__":
    unittest.main()
