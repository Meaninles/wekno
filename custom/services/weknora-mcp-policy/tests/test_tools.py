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


if __name__ == "__main__":
    unittest.main()

