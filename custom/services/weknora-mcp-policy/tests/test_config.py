from __future__ import annotations

import tempfile
import unittest
from pathlib import Path

from app.config import ConfigError, ToolPolicy, load_enabled_tools, strip_jsonc


class JsoncPolicyTests(unittest.TestCase):
    def test_comments_and_urls_inside_strings_are_preserved(self):
        document = strip_jsonc(
            '{\n'
            '  // comment\n'
            '  "url": "https://example.test/a//b",\n'
            '  /* block */ "enabled_tools": ["list_knowledge_bases"]\n'
            '}'
        )
        self.assertIn('"https://example.test/a//b"', document)
        self.assertIn('"list_knowledge_bases"', document)

    def test_policy_rejects_unknown_tools(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "policy.jsonc"
            path.write_text('{"enabled_tools":["not_a_tool"]}', encoding="utf-8")
            with self.assertRaises(ConfigError):
                load_enabled_tools(path, ["list_knowledge_bases"])

    def test_policy_reloads_when_file_changes(self):
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "policy.jsonc"
            path.write_text('{"enabled_tools":["list_knowledge_bases"]}', encoding="utf-8")
            policy = ToolPolicy(path, ["list_knowledge_bases", "hybrid_search"])
            self.assertEqual(policy.snapshot().enabled_tools, {"list_knowledge_bases"})
            path.write_text('{"enabled_tools":["hybrid_search"]}', encoding="utf-8")
            # Ensure filesystems with coarse mtime resolution still exercise
            # the reload path by changing size if necessary.
            if policy.snapshot().enabled_tools != {"hybrid_search"}:
                path.write_text('{"enabled_tools":["hybrid_search", "hybrid_search"]}', encoding="utf-8")
            self.assertEqual(policy.snapshot().enabled_tools, {"hybrid_search"})


if __name__ == "__main__":
    unittest.main()

