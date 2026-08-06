from __future__ import annotations

import argparse
import asyncio
import hashlib
import json
import os
import time
from collections import OrderedDict
from dataclasses import asdict, is_dataclass
from pathlib import Path
from typing import Any

from claude_agent_sdk import ClaudeAgentOptions, create_sdk_mcp_server, query, tool


CASES = {
    "no_tool_short": (
        "不要调用工具。用三句中文说明事件驱动架构的定义、一个优势和一个风险。"
        "最后一行原样写：PROBE-NO-TOOL-SHORT"
    ),
    "no_tool_long": (
        "不要调用工具。写一份不少于1200个中文字符的事件驱动架构迁移备忘录，"
        "必须包含执行摘要、现状约束、三种方案对比、八步实施计划、五项风险、六项指标和最终建议。"
        "不要只写提纲，最后一行原样写：PROBE-NO-TOOL-LONG"
    ),
    "one_tool": (
        "先调用 lookup_policy 查询项目上会规则，然后仅根据工具结果回答2300万元扩容项目应上什么会、理由是什么。"
        "最后一行原样写：PROBE-ONE-TOOL"
    ),
    "one_tool_long": (
        "先调用 lookup_policy 查询项目上会规则，然后写一份不少于1200个中文字符、可直接提交管理层的完整备忘录，"
        "包含结论、制度依据、决策边界、会前材料清单、审批步骤、风险和建议。"
        "最后一行原样写：PROBE-ONE-TOOL-LONG"
    ),
    "two_tools": (
        "依次调用 lookup_policy 查询上会规则，并调用 calculate_budget 计算2300万元占3000万元预算的比例，"
        "然后给出完整结论和计算过程。最后一行原样写：PROBE-TWO-TOOLS"
    ),
    "parallel_tools": (
        "在同一个工具调用阶段并行调用 lookup_policy 和 calculate_budget（amount=2300，budget=3000），"
        "两个结果都返回后再给出完整结论。最后一行原样写：PROBE-PARALLEL-TOOLS"
    ),
    "tool_error_recovery": (
        "先调用 unstable_lookup 查询制度；如果工具失败，不要重复调用它，改为调用 lookup_policy，"
        "然后依据成功结果给出完整回答。最后一行原样写：PROBE-ERROR-RECOVERY"
    ),
    "max_turns_error": (
        "连续交替调用 lookup_policy 和 calculate_budget 共六次，每次必须等待上一次结果后再调用下一次，"
        "全部完成后再回答。"
    ),
}


def value(block: Any, name: str, default: Any = None) -> Any:
    if isinstance(block, dict):
        return block.get(name, default)
    return getattr(block, name, default)


def kind(block: Any) -> str:
    if isinstance(block, dict):
        return str(block.get("type") or "")
    return block.__class__.__name__


def sha256(text: str) -> str:
    return hashlib.sha256(text.encode("utf-8")).hexdigest()


def compact(value_: Any, limit: int = 240) -> str:
    text = str(value_ or "").replace("\r", "").replace("\n", "\\n")
    return text if len(text) <= limit else text[:limit] + f"...[+{len(text) - limit}]"


def json_safe(item: Any) -> Any:
    if is_dataclass(item):
        return {key: json_safe(val) for key, val in asdict(item).items()}
    if isinstance(item, dict):
        return {str(key): json_safe(val) for key, val in item.items()}
    if isinstance(item, (list, tuple)):
        return [json_safe(val) for val in item]
    if isinstance(item, (str, int, float, bool)) or item is None:
        return item
    return str(item)


class TerminalEpochCollector:
    def __init__(self) -> None:
        self.epoch = 0
        self.messages: OrderedDict[str, str] = OrderedDict()
        self.operational_message_ids: set[str] = set()
        self.tool_calls: list[dict[str, str]] = []

    def observe_assistant(self, message: Any) -> dict[str, Any]:
        content = value(message, "content", [])
        blocks = content if isinstance(content, list) else []
        tool_blocks = [block for block in blocks if kind(block) in {"ToolUseBlock", "tool_use"}]
        text = "".join(
            str(value(block, "text", "") or "")
            for block in blocks
            if kind(block) in {"TextBlock", "text"}
        )
        message_id = str(value(message, "message_id", "") or "")
        callback_uuid = str(value(message, "uuid", "") or "")
        # Claude SDK can emit separate AssistantMessage callbacks for text and
        # ToolUse blocks that belong to the same underlying assistant message.
        # The callback UUID is unique per callback, while message_id is the
        # canonical grouping key.
        message_key = message_id or callback_uuid or f"anonymous-{len(self.messages) + 1}"
        if tool_blocks:
            self.epoch += 1
            self.operational_message_ids.add(message_key)
            self.messages.clear()
            for block in tool_blocks:
                self.tool_calls.append(
                    {
                        "epoch": str(self.epoch),
                        "id": str(value(block, "id", "") or ""),
                        "name": str(value(block, "name", "") or ""),
                    }
                )
        elif text and message_key not in self.operational_message_ids:
            # Multiple text blocks from the same underlying assistant message
            # are kept in SDK order. If a later callback with the same
            # message_id contains ToolUse, the branch above invalidates them.
            self.messages[message_key] = self.messages.get(message_key, "") + text
        return {
            "uuid": callback_uuid,
            "message_id": message_id,
            "collector_message_key": message_key,
            "collector_message_is_operational": message_key in self.operational_message_ids,
            "stop_reason": str(value(message, "stop_reason", "") or ""),
            "parent_tool_use_id": str(value(message, "parent_tool_use_id", "") or ""),
            "text_length": len(text),
            "text_sha256": sha256(text) if text else "",
            "text_preview": compact(text),
            "tool_uses": [
                {
                    "id": str(value(block, "id", "") or ""),
                    "name": str(value(block, "name", "") or ""),
                    "input": json_safe(value(block, "input", {})),
                }
                for block in tool_blocks
            ],
            "collector_epoch_after": self.epoch,
            "collector_message_count_after": len(self.messages),
        }

    def answer(self) -> str:
        return "".join(self.messages.values()).strip()


def mcp_text(text: str, *, is_error: bool = False) -> dict[str, Any]:
    result: dict[str, Any] = {"content": [{"type": "text", "text": text}]}
    if is_error:
        result["is_error"] = True
    return result


@tool(
    "lookup_policy",
    "查询本地固定的项目上会规则。",
    {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": False,
    },
)
async def lookup_policy(args: dict[str, Any]) -> dict[str, Any]:
    return mcp_text(
        "POLICY-EVIDENCE-20260731：单项投资金额达到或超过2000万元的扩容项目，应提交公司投资决策委员会审议。"
    )


@tool(
    "calculate_budget",
    "计算本地预算比例。",
    {
        "type": "object",
        "properties": {
            "amount": {"type": "number"},
            "budget": {"type": "number"},
        },
        "required": ["amount", "budget"],
        "additionalProperties": False,
    },
)
async def calculate_budget(args: dict[str, Any]) -> dict[str, Any]:
    amount = float(args.get("amount") or 0)
    budget = float(args.get("budget") or 0)
    if budget <= 0:
        return mcp_text("预算必须大于0", is_error=True)
    return mcp_text(f"BUDGET-EVIDENCE-20260731：比例={amount / budget:.6f}。")


@tool(
    "unstable_lookup",
    "固定返回错误的本地查询工具，用于验证失败后恢复。",
    {
        "type": "object",
        "properties": {"query": {"type": "string"}},
        "required": ["query"],
        "additionalProperties": False,
    },
)
async def unstable_lookup(args: dict[str, Any]) -> dict[str, Any]:
    return mcp_text("PROBE-FIXED-ERROR：模拟查询失败。", is_error=True)


async def run_case(case_name: str, prompt: str, model: str, max_turns: int) -> dict[str, Any]:
    server = create_sdk_mcp_server(
        "probe",
        version="1.0.0",
        tools=[lookup_policy, calculate_budget, unstable_lookup],
    )
    options = ClaudeAgentOptions(
        cwd="/tmp",
        env={
            "ANTHROPIC_API_KEY": os.environ["ANTHROPIC_API_KEY"],
            "ANTHROPIC_AUTH_TOKEN": os.environ.get(
                "ANTHROPIC_AUTH_TOKEN",
                os.environ["ANTHROPIC_API_KEY"],
            ),
            "ANTHROPIC_BASE_URL": os.environ["ANTHROPIC_BASE_URL"],
            "API_TIMEOUT_MS": os.environ.get("API_TIMEOUT_MS", "300000"),
            "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
            "CLAUDE_CODE_MAX_RETRIES": "0",
        },
        system_prompt=(
            "你是一个正常工作的中文助手。准确完成用户请求；需要工具时使用可用工具；"
            "工具完成后直接给出完整回答。不要提及本探针或消息协议。"
        ),
        tools=[],
        mcp_servers={"probe": server},
        strict_mcp_config=True,
        allowed_tools=[
            "mcp__probe__lookup_policy",
            "mcp__probe__calculate_budget",
            "mcp__probe__unstable_lookup",
        ],
        permission_mode="dontAsk",
        include_partial_messages=True,
        max_turns=max_turns,
        model=model,
    )

    collector = TerminalEpochCollector()
    messages: list[dict[str, Any]] = []
    stream_text_parts: list[str] = []
    result_text = ""
    result_summary: dict[str, Any] = {}
    terminal_exception = ""
    started = time.monotonic()

    try:
        async for message in query(prompt=prompt, options=options):
            message_type = message.__class__.__name__
            entry: dict[str, Any] = {"sequence": len(messages) + 1, "type": message_type}
            if message_type == "AssistantMessage":
                entry.update(collector.observe_assistant(message))
            elif message_type == "StreamEvent":
                event = value(message, "event", {}) or {}
                entry.update(
                    {
                        "uuid": str(value(message, "uuid", "") or ""),
                        "parent_tool_use_id": str(value(message, "parent_tool_use_id", "") or ""),
                        "event_type": str(event.get("type") or ""),
                    }
                )
                if event.get("type") == "content_block_delta":
                    delta = event.get("delta") or {}
                    entry["delta_type"] = str(delta.get("type") or "")
                    if delta.get("type") == "text_delta":
                        text = str(delta.get("text") or "")
                        stream_text_parts.append(text)
                        entry["text_length"] = len(text)
                        entry["text_preview"] = compact(text, 120)
                elif event.get("type") == "message_delta":
                    entry["stop_reason"] = str((event.get("delta") or {}).get("stop_reason") or "")
            elif message_type == "ResultMessage":
                result_text = str(value(message, "result", "") or "").strip()
                result_summary = {
                    "subtype": str(value(message, "subtype", "") or ""),
                    "is_error": bool(value(message, "is_error", False)),
                    "num_turns": int(value(message, "num_turns", 0) or 0),
                    "stop_reason": str(value(message, "stop_reason", "") or ""),
                    "uuid": str(value(message, "uuid", "") or ""),
                    "result_length": len(result_text),
                    "result_sha256": sha256(result_text) if result_text else "",
                    "result_preview": compact(result_text),
                    "errors": json_safe(value(message, "errors", None)),
                }
                entry.update(result_summary)
            else:
                entry["raw"] = json_safe(message)
            messages.append(entry)
    except Exception as exc:
        terminal_exception = f"{exc.__class__.__name__}: {exc}"

    assistant_answer = collector.answer()
    return {
        "case": case_name,
        "prompt": prompt,
        "model": model,
        "elapsed_ms": round((time.monotonic() - started) * 1000),
        "collector": {
            "terminal_epoch": collector.epoch,
            "terminal_message_count": len(collector.messages),
            "answer_length": len(assistant_answer),
            "answer_sha256": sha256(assistant_answer) if assistant_answer else "",
            "answer_preview": compact(assistant_answer),
            "tool_calls": collector.tool_calls,
        },
        "result": result_summary,
        "terminal_exception": terminal_exception,
        "comparison": {
            "has_success_result": (
                bool(result_summary)
                and result_summary.get("subtype") == "success"
                and not result_summary.get("is_error", False)
            ),
            "assistant_equals_result": assistant_answer == result_text,
            "assistant_in_result": bool(assistant_answer) and assistant_answer in result_text,
            "result_in_assistant": bool(result_text) and result_text in assistant_answer,
            "length_delta": len(assistant_answer) - len(result_text),
            "stream_text_total_length": len("".join(stream_text_parts)),
        },
        "messages": messages,
    }


async def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--model", required=True)
    parser.add_argument("--case", action="append", choices=sorted(CASES))
    parser.add_argument("--max-turns", type=int, default=12)
    parser.add_argument("--output", required=True)
    args = parser.parse_args()

    selected = args.case or list(CASES)
    report = {
        "sdk_version": "0.2.110",
        "base_url": os.environ.get("ANTHROPIC_BASE_URL", ""),
        "model": args.model,
        "cases": [],
    }
    for case_name in selected:
        result = await run_case(case_name, CASES[case_name], args.model, args.max_turns)
        report["cases"].append(result)
        print(
            json.dumps(
                {
                    "case": case_name,
                    "elapsed_ms": result["elapsed_ms"],
                    **result["comparison"],
                    "assistant_length": result["collector"]["answer_length"],
                    "result_length": result["result"].get("result_length", 0),
                    "tool_calls": result["collector"]["tool_calls"],
                },
                ensure_ascii=False,
            ),
            flush=True,
        )
    Path(args.output).write_text(
        json.dumps(report, ensure_ascii=False, indent=2),
        encoding="utf-8",
    )


if __name__ == "__main__":
    asyncio.run(main())
