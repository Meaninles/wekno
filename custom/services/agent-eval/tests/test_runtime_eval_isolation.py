from __future__ import annotations

import re
import unittest
from pathlib import Path


REPO_ROOT = Path(__file__).resolve().parents[4]
RUNTIME_ROOTS = (
    REPO_ROOT / "internal" / "agent",
    REPO_ROOT / "internal" / "application" / "service",
    REPO_ROOT / "internal" / "custom" / "modules" / "conversationmemory",
    REPO_ROOT / "internal" / "custom" / "modules" / "generalagent",
    REPO_ROOT / "internal" / "custom" / "modules" / "chatretrieval",
    REPO_ROOT / "internal" / "custom" / "modules" / "sourcerefs",
    REPO_ROOT / "custom" / "services" / "general-agent" / "app",
)


def runtime_sources() -> dict[Path, str]:
    sources: dict[Path, str] = {}
    for root in RUNTIME_ROOTS:
        for path in root.rglob("*"):
            if not path.is_file() or path.suffix not in {".go", ".py"}:
                continue
            if path.name.endswith(("_test.go", "_test.py")) or "tests" in path.parts:
                continue
            sources[path] = path.read_text(encoding="utf-8")
    return sources


class RuntimeEvalIsolationTests(unittest.TestCase):
    def test_runtime_has_no_eval_answer_contract_or_fixed_dataset_branch(self) -> None:
        forbidden = (
            "case_id",
            "required_claims",
            "reference_answer",
            "judge_feedback",
            "eval_rubric",
            "runtime_response_contract",
            "采购文档",
            "培训说明",
            "供应商d",
            "不安装、不执行、不修改",
        )
        violations: list[str] = []
        for path, source in runtime_sources().items():
            probe = source.casefold()
            for token in forbidden:
                if token.casefold() in probe:
                    violations.append(f"{path.relative_to(REPO_ROOT)}: {token}")
        self.assertEqual(violations, [])

    def test_runtime_has_no_literal_uuid_scope_branch(self) -> None:
        literal_scope_branch = re.compile(
            r"(?i)(?:knowledge_base_id|knowledge_id|document_id)\s*==\s*"
            r"[\"'][0-9a-f]{8}-[0-9a-f-]{27,}[\"']"
        )
        violations = [
            str(path.relative_to(REPO_ROOT))
            for path, source in runtime_sources().items()
            if literal_scope_branch.search(source)
        ]
        self.assertEqual(violations, [])

    def test_removed_in_sut_semantic_repair_modules_stay_absent(self) -> None:
        removed = (
            REPO_ROOT
            / "internal"
            / "custom"
            / "modules"
            / "agenteval"
            / "response_contract.go",
            REPO_ROOT
            / "internal"
            / "custom"
            / "modules"
            / "agentresponse"
            / "response_repair.go",
        )
        self.assertEqual([str(path) for path in removed if path.exists()], [])

        # Production citation finalization is a local transport pass restored
        # independently of Eval. It receives only answer bytes plus the
        # immutable current-turn reference registry: no query, case, rubric,
        # judge feedback, retrieval client, or model client is available.
        citation_repair_path = (
            REPO_ROOT
            / "internal"
            / "custom"
            / "modules"
            / "sourcerefs"
            / "repair.go"
        )
        citation_repair = citation_repair_path.read_text(encoding="utf-8")
        self.assertIn(
            "func RepairAnswerCitations(answer string, refs []*types.SearchResult) string",
            citation_repair,
        )
        for forbidden in (
            "case_id",
            "required_claim",
            "reference_answer",
            "judge_feedback",
            "chat.Chat",
            "KnowledgeSearch",
        ):
            self.assertNotIn(forbidden.casefold(), citation_repair.casefold())

        # More invasive extractive/topic helpers are intentionally not wired
        # into any production path. Their presence must never become an
        # implicit terminal answer rewrite.
        production_sources = runtime_sources()
        production_sources.pop(citation_repair_path, None)
        for helper in (
            "RepairNamedTopicCitationBindings(",
            "EnsureNamedTopicDefinitions(",
            "RecoverOffTopicNarrowEvidenceAnswer(",
        ):
            callers = [
                str(path.relative_to(REPO_ROOT))
                for path, source in production_sources.items()
                if helper in source
            ]
            self.assertEqual(callers, [], helper)

    def test_production_observability_is_metadata_only_and_batched(self) -> None:
        eval_config = (
            REPO_ROOT
            / "internal"
            / "custom"
            / "modules"
            / "agenteval"
            / "config.go"
        ).read_text(encoding="utf-8")
        manager = (
            REPO_ROOT / "internal" / "tracing" / "langfuse" / "manager.go"
        ).read_text(encoding="utf-8")
        tracing_config = (
            REPO_ROOT / "internal" / "tracing" / "langfuse" / "config.go"
        ).read_text(encoding="utf-8")
        self.assertIn("cfg.Mode == ModeProduction && cfg.CapturePolicy", eval_config)
        self.assertIn("c.Mode == ModeEval && c.CapturePolicy == CaptureFull", eval_config)
        self.assertIn("NewBatchSpanProcessor", manager)
        self.assertIn("ProductionMaxSampleRate: 0.01", tracing_config)

    def test_current_eval_loop_waits_for_codex_instead_of_auto_judging(self) -> None:
        eval_loop = (
            REPO_ROOT / "custom" / "services" / "agent-eval" / "eval-loop.ps1"
        ).read_text(encoding="utf-8")
        self.assertIn("unseen-capability-matrix.v1.jsonl", eval_loop)
        self.assertIn("codex-review-export", eval_loop)
        self.assertIn("WAITING_FOR_CODEX_REVIEW", eval_loop)
        self.assertNotRegex(eval_loop, r'(?m)^\s*"judge"\s*,')
        self.assertNotIn("calibration\", \"run", eval_loop)

    def test_current_runner_binding_has_no_fixed_document_dependency(self) -> None:
        preparation = (
            REPO_ROOT
            / "custom"
            / "services"
            / "agent-eval"
            / "prepare-runner-env.ps1"
        ).read_text(encoding="utf-8")
        self.assertNotRegex(
            preparation,
            r'\$KnowledgeTitle\s*=\s*["\'][^"\']+["\']',
        )
        self.assertIn('profiles/unseen-capability-matrix.v1.json', preparation)


if __name__ == "__main__":
    unittest.main()
