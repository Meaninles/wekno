import asyncio
import unittest

from litellm.types.utils import ModelResponseStream

from stream_normalizer import DeepSeekToolStreamNormalizer


def chunk(delta, *, finish_reason=None, model="/models/DeepSeek-V4-Flash-INT8"):
    return ModelResponseStream(
        id="chatcmpl-test",
        object="chat.completion.chunk",
        created=0,
        model=model,
        choices=[
            {
                "index": 0,
                "delta": delta,
                "finish_reason": finish_reason,
            }
        ],
    )


async def stream(items):
    for item in items:
        yield item


async def normalize(items, model="deepseek-v4-flash-int8-anthropic-upstream"):
    callback = DeepSeekToolStreamNormalizer()
    return [
        item
        async for item in callback.async_post_call_streaming_iterator_hook(
            user_api_key_dict=None,
            response=stream(items),
            request_data={"model": model},
        )
    ]


class DeepSeekToolStreamNormalizerTest(unittest.TestCase):
    def test_removes_only_malformed_suffix(self):
        items = [
            chunk({"content": "我来调用工具。"}),
            chunk(
                {
                    "content": "",
                    "tool_calls": [
                        {
                            "index": 0,
                            "id": "call-1",
                            "type": "function",
                            "function": {"name": "kb_list_documents", "arguments": ""},
                        }
                    ],
                }
            ),
            chunk(
                {
                    "content": "",
                    "tool_calls": [
                        {
                            "index": 0,
                            "type": "function",
                            "function": {"arguments": "{}"},
                        }
                    ],
                }
            ),
            chunk({"content": "\n"}),
            chunk(
                {
                    "content": "",
                    "tool_calls": [
                        {
                            "index": 0,
                            "type": "function",
                            "function": {"arguments": "\"\""},
                        }
                    ],
                }
            ),
            chunk({}, finish_reason="tool_calls"),
        ]

        result = asyncio.run(normalize(items))

        self.assertEqual(result[0].choices[0].delta.content, "我来调用工具。")
        valid_arguments = result[2].choices[0].delta.tool_calls[0].function.arguments
        self.assertEqual(valid_arguments, "{}")
        self.assertEqual(result[3].choices[0].finish_reason, "tool_calls")
        self.assertEqual(len(result), 4)

    def test_keeps_fragment_through_first_complete_object(self):
        items = [
            chunk(
                {
                    "tool_calls": [
                        {
                            "index": 0,
                            "id": "call-1",
                            "type": "function",
                            "function": {"name": "echo", "arguments": '{"text":'},
                        }
                    ]
                }
            ),
            chunk(
                {
                    "tool_calls": [
                        {
                            "index": 0,
                            "type": "function",
                            "function": {"arguments": '"hello"}\"\"'},
                        }
                    ]
                }
            ),
        ]

        result = asyncio.run(normalize(items))

        self.assertEqual(result[1].choices[0].delta.tool_calls[0].function.arguments, '"hello"}')

    def test_does_not_touch_other_models(self):
        item = chunk({"content": "\n"}, model="qwen")

        result = asyncio.run(normalize([item], model="Qwen3.6-27B-tool"))

        self.assertEqual(result[0].choices[0].delta.content, "\n")


if __name__ == "__main__":
    unittest.main()
