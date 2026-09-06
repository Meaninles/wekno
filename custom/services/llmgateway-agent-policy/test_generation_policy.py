import copy
import unittest

from generation_policy import ROUTES, TerminalEvidence, apply, translate_anthropic


class PolicyTests(unittest.TestCase):
    def test_legacy_requests_are_untouched(self):
        for model in ("DeepSeek-V4-Flash", "Qwen3.6-27B", "Qwen3.6-35B-A3B", "Qwen3-VL-32B"):
            for thinking in (None, {"type": "adaptive"}, {"type": "disabled"}):
                data = {"temperature": 0.0, "thinking": thinking, "reasoning_effort": "low"}
                before = copy.deepcopy(data)
                self.assertFalse(apply(model, data))
                self.assertEqual(data, before)

    def test_protocols_match_for_each_mode_and_effort(self):
        for model, (kind, _, _) in ROUTES.items():
            efforts = ("low", "high", "max") if kind == "deepseek0731" else ("low", "medium", "xhigh")
            for enabled, effort in [(False, None)] + [(True, e) for e in efforts]:
                direct = {"thinking": {"type": "enabled" if enabled else "disabled"}}
                request = {"model": model, "thinking": {"type": "adaptive" if enabled else "disabled", "display": "omitted"}}
                if effort:
                    direct["reasoning_effort"] = effort
                    request["output_config"] = {"effort": effort}
                apply(model, direct)
                sdk = {}
                translate_anthropic(request, sdk)
                self.assertEqual(direct, sdk)
                # A second provider pass must not change the resolved policy.
                original = copy.deepcopy(sdk)
                apply(model, sdk)
                self.assertEqual(sdk, original)

    def test_conflicts_fail_without_changing_input(self):
        for data in ({"thinking": {"type": "disabled"}, "reasoning_effort": "low"},
                     {"thinking": {"type": "enabled"}, "chat_template_kwargs": {"enable_thinking": False}},
                     {"reasoning_effort": "high"}, {"chat_template_kwargs": {"enable_thinking": "false"}}):
            original = copy.deepcopy(data)
            with self.assertRaises(ValueError): apply("Qwen3.8-27B-Agent", data)
            self.assertEqual(data, original)

    def test_zero_parameters_and_extra_fields_survive(self):
        data = {"thinking": {"type": "disabled"}, "extra_body": {"other": 3, "chat_template_kwargs": {"custom": "keep"}}}
        apply("Qwen3.8-27B-Agent", data)
        self.assertEqual(data["temperature"], 0.7)
        self.assertEqual(data["min_p"], 0.0)
        self.assertEqual(data["extra_body"]["other"], 3)
        self.assertEqual(data["extra_body"]["chat_template_kwargs"]["custom"], "keep")

    def test_qwen_honors_thinking_controls_and_ds_remains_thinking(self):
        for controls, effort in (({}, "xhigh"),
                                ({"thinking":{"type":"enabled"},"reasoning_effort":"xhigh"}, "xhigh"),
                                ({"chat_template_kwargs":{"enable_thinking":True,"reasoning_effort":"medium"}}, "medium")):
            apply("Qwen3.8-27B-Agent",controls)
            self.assertIs(controls["extra_body"]["chat_template_kwargs"]["enable_thinking"],True)
            self.assertEqual(controls["extra_body"]["chat_template_kwargs"]["reasoning_effort"],effort)
            self.assertEqual((controls["temperature"],controls["top_p"],controls["presence_penalty"]),(1.0,.95,0.0))
        for controls in ({"thinking":{"type":"disabled"}}, {"reasoning_effort":"none"},
                         {"chat_template_kwargs":{"enable_thinking":False}}):
            apply("Qwen3.8-27B-Agent",controls)
            self.assertFalse(controls["extra_body"]["chat_template_kwargs"]["enable_thinking"])
            self.assertNotIn("reasoning_effort",controls["extra_body"]["chat_template_kwargs"])
            self.assertEqual((controls["temperature"],controls["top_p"],controls["presence_penalty"]),(.7,.8,1.5))
        ds={"thinking":{"type":"enabled"},"reasoning_effort":"high"}
        apply("DeepSeek-V4-Flash-Agent",ds)
        self.assertTrue(ds["extra_body"]["chat_template_kwargs"]["thinking"])
        self.assertEqual(ds["extra_body"]["chat_template_kwargs"]["reasoning_effort"],"high")

    def test_empty_thinking_terminal_is_not_success(self):
        evidence = TerminalEvidence()
        evidence.observe({"type": "content_block_start", "content_block": {"type": "thinking"}})
        with self.assertRaisesRegex(ValueError, "upstream_empty_terminal"):
            evidence.observe({"type": "message_delta", "delta": {"stop_reason": "end_turn"}})
        for block in ({"type": "text", "text": "a real answer"}, {"type": "tool_use", "name": "Read"}):
            evidence = TerminalEvidence()
            evidence.observe({"type": "content_block_start", "content_block": block})
            evidence.observe({"type": "message_delta", "delta": {"stop_reason": "end_turn"}})


if __name__ == "__main__": unittest.main()
