import unittest

from docreader.parser.markitdown_parser import MarkitdownParser
from docreader.parser.registry import registry


class PPTRegistryTest(unittest.TestCase):
    def test_builtin_registry_resolves_pptx(self):
        self.assertIs(registry.get_parser_class("", "pptx"), MarkitdownParser)

    def test_builtin_registry_resolves_ppt(self):
        self.assertIs(registry.get_parser_class("", "ppt"), MarkitdownParser)


if __name__ == "__main__":
    unittest.main()
