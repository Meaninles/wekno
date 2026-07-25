from __future__ import annotations

import tempfile
import time
import unittest
from pathlib import Path

from .cluster_e2e import APIError, E2EFailure, JsonlRecorder
from .multitenant_e2e import MultiTenantObservation, TestPrincipal
from .run_dual_real_corpus_e2e import (
    DualCorpusRunner,
    RealCorpusFactory,
    _canonical_entries,
    _inventory,
)


class RealCorpusInventoryTests(unittest.TestCase):
    def test_office_lock_files_are_excluded_even_with_supported_suffix(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            document = root / "制度.docx"
            lock_file = root / "~$制度.docx"
            document.write_bytes(b"document")
            lock_file.write_bytes(b"owner metadata")

            supported, excluded = _inventory((root,))

            self.assertEqual(supported, [document])
            self.assertEqual(excluded, [lock_file])

    def test_duplicate_content_keeps_one_canonical_source_and_alias_evidence(self) -> None:
        with tempfile.TemporaryDirectory() as raw:
            root = Path(raw)
            first = root / "现行制度.docx"
            duplicate = root / "废止目录副本.docx"
            distinct = root / "另一制度.docx"
            first.write_bytes(b"same office bytes")
            duplicate.write_bytes(b"same office bytes")
            distinct.write_bytes(b"different office bytes")

            entries, aliases = _canonical_entries(
                [first, duplicate, distinct],
                index_offset=100,
            )

            self.assertEqual(
                [(entry.source_index, entry.path) for entry in entries],
                [(100, first), (102, distinct)],
            )
            self.assertEqual(len(aliases), 1)
            self.assertEqual(aliases[0]["source_index"], 101)
            self.assertEqual(aliases[0]["canonical_source_index"], 100)
            factory = RealCorpusFactory(entries)
            self.assertTrue(factory.build(0, "run").marker.endswith("-00100"))
            self.assertTrue(factory.build(1, "run").marker.endswith("-00102"))

    def test_isolation_is_checked_from_ordinary_tenant_toward_admin_tenant(self) -> None:
        class OrdinaryClient:
            def __init__(self, leaks: bool) -> None:
                self.leaks = leaks
                self.requested: list[str] = []

            def get_knowledge(self, knowledge_id: str) -> object:
                self.requested.append(knowledge_id)
                if self.leaks:
                    return {"id": knowledge_id}
                raise APIError("GET", f"http://unit/{knowledge_id}", 404, "")

        def observation() -> MultiTenantObservation:
            return MultiTenantObservation(
                index=0,
                principal_index=0,
                tenant_id=10000,
                user_id="admin",
                kb_id="policy-kb",
                knowledge_id="policy-document",
                filename="policy.docx",
                marker="marker",
                extension="docx",
                size_class="small",
                target_kib=1,
                source_bytes=1,
                content_unique=True,
                uploaded_at=time.monotonic(),
            )

        with tempfile.TemporaryDirectory() as raw:
            ordinary = OrdinaryClient(leaks=False)
            runner = DualCorpusRunner(
                None,  # type: ignore[arg-type]
                [
                    TestPrincipal(0, "admin", "admin", 10000, object()),  # type: ignore[arg-type]
                    TestPrincipal(1, "ordinary", "ordinary", 10001, ordinary),  # type: ignore[arg-type]
                ],
                object(),  # type: ignore[arg-type]
                JsonlRecorder(Path(raw) / "events.jsonl"),
                run_id="run",
                policy_count=1,
            )
            runner.observations["policy-document"] = observation()
            runner.verify_cross_tenant_isolation()
            self.assertEqual(ordinary.requested, ["policy-document"])

            leaking = OrdinaryClient(leaks=True)
            runner.principals[1].client = leaking  # type: ignore[assignment]
            with self.assertRaisesRegex(E2EFailure, "ordinary secondary tenant"):
                runner.verify_cross_tenant_isolation()


if __name__ == "__main__":
    unittest.main()
