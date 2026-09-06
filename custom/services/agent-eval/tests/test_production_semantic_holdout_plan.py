import json
import unittest
from pathlib import Path


class ProductionSemanticHoldoutPlanTests(unittest.TestCase):
    @classmethod
    def setUpClass(cls) -> None:
        cls.plan = json.loads(
            (
                Path(__file__).parent
                / "fixtures"
                / "production_semantic_holdout_plan.v1.json"
            ).read_text(encoding="utf-8")
        )

    def test_holdout_is_model_fixed_and_eval_free(self) -> None:
        self.assertEqual(
            self.plan["required_model_id"],
            "prod-deepseek-v4-flash-int8-chat",
        )
        self.assertEqual(
            self.plan["profiles"],
            ["quick-answer", "rag-reasoning", "general-agent"],
        )
        self.assertEqual(self.plan["repetitions"], 3)
        self.assertEqual(self.plan["concurrency"], 8)
        for key in (
            "source_answers_in_sut_input",
            "source_references_in_sut_input",
            "reference_answers_in_sut_input",
            "required_claims_in_sut_input",
            "judge_feedback_in_sut_input",
            "sealed_holdout_accessed",
            "eval_assistance_enabled",
        ):
            self.assertIs(self.plan[key], False)

    def test_holdout_composes_distinct_capabilities(self) -> None:
        case = self.plan["cases"][0]
        self.assertEqual(case["source_agent_kind"], "new-post-fix-regression")
        queries = "\n".join(turn["query"] for turn in case["turns"])
        for capability in (
            "当前制度库",
            "只分成已知、建议中和未知三栏",
            "本身也不等于‘未批准’",
            "修改聊天里的方案描述",
            "不要声称任何文件已经或没有被改过",
            "恰好两句",
            "本轮准确引用",
        ):
            self.assertIn(capability, queries)
        for turn in case["turns"]:
            self.assertEqual(set(turn), {"source_ordinal", "query"})


if __name__ == "__main__":
    unittest.main()
