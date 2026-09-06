import json
import unittest
from pathlib import Path


class ProductionUnseenRAGPlanTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.plan = json.loads(
            (
                Path(__file__).parent
                / "fixtures"
                / "production_unseen_rag_plan.v1.json"
            ).read_text(encoding="utf-8")
        )

    def test_plan_uses_three_agents_and_ds_v4_flash(self) -> None:
        self.assertEqual(
            self.plan["required_model_id"],
            "prod-deepseek-v4-flash-int8-chat",
        )
        self.assertEqual(
            self.plan["profiles"],
            ["quick-answer", "rag-reasoning", "general-agent"],
        )
        self.assertGreaterEqual(self.plan["repetitions"], 3)
        self.assertEqual(self.plan["concurrency"], 8)

    def test_plan_cannot_inject_eval_or_reference_material_into_sut(self) -> None:
        for field in (
            "source_answers_in_sut_input",
            "source_references_in_sut_input",
            "reference_answers_in_sut_input",
            "required_claims_in_sut_input",
            "judge_feedback_in_sut_input",
            "sealed_holdout_accessed",
            "eval_assistance_enabled",
        ):
            self.assertIs(self.plan[field], False, field)
        for case in self.plan["cases"]:
            self.assertTrue(case["kb_slug"])
            for turn in case["turns"]:
                self.assertEqual(set(turn), {"source_ordinal", "query"})

    def test_plan_contains_a_real_long_context_composition(self) -> None:
        long_cases = [case for case in self.plan["cases"] if len(case["turns"]) >= 13]
        self.assertEqual(len(long_cases), 1)
        queries = "\n".join(turn["query"] for turn in long_cases[0]["turns"])
        for capability in ("作废", "未知", "引用", "不要修改任何文件", "English"):
            self.assertIn(capability, queries)

    def test_plan_adds_a_post_change_citation_refresh_regression(self) -> None:
        added = [
            case
            for case in self.plan["cases"]
            if case["source_agent_kind"] == "new-post-change-regression"
        ]
        self.assertEqual(len(added), 1)
        queries = "\n".join(turn["query"] for turn in added[0]["turns"])
        for capability in ("重新查当前知识库", "本轮重新取得", "已作废事实", "行动边界", "English"):
            self.assertIn(capability, queries)

    def test_plan_adds_a_new_post_prompt_cross_domain_regression(self) -> None:
        added = [
            case
            for case in self.plan["cases"]
            if case["source_agent_kind"] == "new-post-prompt-regression"
        ]
        self.assertEqual(len(added), 1)
        self.assertEqual(added[0]["kb_slug"], "company-policies")
        self.assertEqual(added[0]["family"], "cross-document-retrieval-parallel-state-and-evidence-refresh")
        queries = "\n".join(turn["query"] for turn in added[0]["turns"])
        for capability in (
            "不同资料",
            "证据不足",
            "两件不同的事",
            "不是要新增",
            "别替我发消息",
            "重新从当前知识库取证",
            "本轮有效引用",
        ):
            self.assertIn(capability, queries)


if __name__ == "__main__":
    unittest.main()
