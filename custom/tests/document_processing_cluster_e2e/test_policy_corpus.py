from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path
from types import SimpleNamespace

from custom.tests.document_processing_cluster_e2e.cluster_e2e import E2EFailure
from custom.tests.document_processing_cluster_e2e.policy_corpus import (
    PolicySource,
    evaluate_policy_corpus,
    load_policy_corpus,
    quote_matches_chunks,
    source_id_from_uploaded_filename,
)


class _FakeClient:
    def __init__(self) -> None:
        self._chunks = {
            "knowledge-a": [{"chunk_type": "text", "content": "采购审批必须在三个工作日内完成。"}],
            "knowledge-b": [{"chunk_type": "text", "content": "应急流程先上报，再由负责人确认处置。"}],
        }

    def list_chunks(self, knowledge_id: str, _chunk_types: list[str]) -> list[dict[str, str]]:
        return self._chunks[knowledge_id]

    def hybrid_search(
        self,
        _kb_id: str,
        query: str,
        _knowledge_ids: list[str],
    ) -> list[dict[str, str]]:
        knowledge_id = "knowledge-a" if "审批" in query else "knowledge-b"
        return [{"knowledge_id": knowledge_id}]


class PolicyCorpusTests(unittest.TestCase):
    def test_loads_all_sources_and_stratifies_questions(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fixtures = root / "fixtures"
            fixtures.mkdir()
            (fixtures / "policy-a.docx").write_bytes(b"a")
            (fixtures / "policy-b.pdf").write_bytes(b"b")
            manifest = root / "manifest.json"
            questions = root / "questions.json"
            manifest.write_text(
                json.dumps(
                    [
                        {
                            "sourceId": "policy-a",
                            "policyId": "a",
                            "policyTitle": "A",
                            "role": "正文及附件",
                        },
                        {
                            "sourceId": "policy-b",
                            "policyId": "b",
                            "policyTitle": "B",
                            "role": "流程图",
                        },
                    ]
                ),
                encoding="utf-8",
            )
            rows = []
            for source_id in ("policy-a", "policy-b"):
                for index in range(5):
                    rows.append(
                        {
                            "prompt": f"{source_id} question {index}?",
                            "coverageLabel": f"section {index}",
                            "sourceReferences": [
                                {"id": source_id, "quote": f"quote {source_id} {index}"}
                            ],
                        }
                    )
            questions.write_text(json.dumps(rows), encoding="utf-8")
            corpus = load_policy_corpus(
                manifest,
                questions,
                fixtures,
                queries_per_source=3,
            )
        self.assertEqual(len(corpus.sources), 2)
        self.assertEqual(len(corpus.cases), 6)
        self.assertEqual({path.suffix for path in corpus.fixture_paths}, {".docx", ".pdf"})

    def test_quote_matching_tolerates_spacing_and_punctuation(self) -> None:
        self.assertTrue(
            quote_matches_chunks(
                "采购审批必须在三个工作日内完成。",
                "采购 审批，必须在 3 个工作日内完成。",
                window=4,
            )
        )
        self.assertFalse(quote_matches_chunks("完全不同的要求", "采购审批必须按时完成", window=4))

    def test_uploaded_filename_maps_exactly_one_source(self) -> None:
        sources = (
            PolicySource("policy-a", "a", "A", "正文", Path("policy-a.docx")),
            PolicySource("policy-b", "b", "B", "正文", Path("policy-b.pdf")),
            PolicySource(
                "policy-network",
                "network",
                "Network",
                "正文",
                Path("policy-network.docx"),
            ),
            PolicySource(
                "policy-network-security",
                "network-security",
                "Network security",
                "正文",
                Path("policy-network-security.docx"),
            ),
        )
        self.assertEqual(
            source_id_from_uploaded_filename("run-000-policy-a.docx", sources),
            "policy-a",
        )
        self.assertEqual(
            source_id_from_uploaded_filename(
                "run-007-policy-network-security.docx",
                sources,
            ),
            "policy-network-security",
        )
        with self.assertRaises(E2EFailure):
            source_id_from_uploaded_filename("unknown.docx", sources)

    def test_evaluates_source_coverage_anchors_and_recall(self) -> None:
        with tempfile.TemporaryDirectory() as temp_dir:
            root = Path(temp_dir)
            fixtures = root / "fixtures"
            fixtures.mkdir()
            (fixtures / "policy-a.docx").write_bytes(b"a")
            (fixtures / "policy-b.pdf").write_bytes(b"b")
            manifest = root / "manifest.json"
            questions = root / "questions.json"
            manifest.write_text(
                json.dumps(
                    [
                        {"sourceId": "policy-a", "policyId": "a", "policyTitle": "A", "role": "正文及附件"},
                        {"sourceId": "policy-b", "policyId": "b", "policyTitle": "B", "role": "流程图"},
                    ]
                ),
                encoding="utf-8",
            )
            questions.write_text(
                json.dumps(
                    [
                        {
                            "prompt": "采购审批时限是什么？",
                            "sourceReferences": [
                                {"id": "policy-a", "quote": "采购审批必须在三个工作日内完成"}
                            ],
                        },
                        {
                            "prompt": "应急流程如何处置？",
                            "sourceReferences": [
                                {"id": "policy-b", "quote": "应急流程先上报，再由负责人确认处置"}
                            ],
                        },
                    ]
                ),
                encoding="utf-8",
            )
            corpus = load_policy_corpus(manifest, questions, fixtures, queries_per_source=1)

        runner = SimpleNamespace(
            kb_id="kb",
            observations={
                "knowledge-a": SimpleNamespace(filename="run-policy-a.docx"),
                "knowledge-b": SimpleNamespace(filename="run-policy-b.pdf"),
            },
        )
        evidence = evaluate_policy_corpus(
            _FakeClient(),
            runner,
            corpus,
            recall_at_k=1,
            min_recall=1.0,
        )
        self.assertEqual(evidence["recall"], 1.0)
        self.assertEqual(evidence["source_anchor_coverage"], 1.0)


if __name__ == "__main__":
    unittest.main()
