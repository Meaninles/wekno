from __future__ import annotations

import argparse
import json
import os
import re
import sys
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Any, Callable


CANONICAL_SRC_RE = re.compile(r'<src id="(S[1-9][0-9]*)" />')
ANY_SRC_RE = re.compile(r'<\s*/?\s*src\b[^>]*>', re.IGNORECASE)
LEGACY_CITATION_RE = re.compile(
    r'<\s*/?\s*(?:kb|web|wiki|source|citation|doc|document)\b[^>]*>',
    re.IGNORECASE,
)

RETRIEVAL_TOOL_NAMES = {
    "knowledge_search", "search_knowledge", "grep_chunks", "list_knowledge_chunks",
    "wiki_read_page", "wiki_read_source_doc", "web_search", "web_fetch",
    "list_data_sources", "db_catalog", "db_schema", "data_analysis",
    "table_schema", "table_analysis", "db_query",
}


@dataclass(frozen=True)
class Case:
    name: str
    endpoint: str
    agent_id: str
    query: str
    knowledge_base_ids: tuple[str, ...] = ()
    knowledge_ids: tuple[str, ...] = ()
    web_search_enabled: bool = False
    require_retrieval: bool = False
    require_citations: bool = False
    min_documents: int = 0
    min_wiki: int = 0
    min_web: int = 0
    min_data_sources: int = 0
    expected_retrieval_unit: str = "documents"
    required_answer_terms: tuple[str, ...] = ()
    forbidden_tools: tuple[str, ...] = ()


PROCUREMENT_METHOD_TERMS = (
    "招标采购",
    "询比采购",
    "竞价采购",
    "谈判采购",
    "框架协议采购",
    "单源采购",
)


CASES = {
    "quick": Case(
        name="quick",
        endpoint="knowledge-chat",
        agent_id="builtin-quick-answer",
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query="仅根据已选《采购管理办法》第三十二条，列出规定的六种采购方式，并在这项正向事实后准确引用。",
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "smart": Case(
        name="smart",
        endpoint="agent-chat",
        agent_id="builtin-smart-reasoning",
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query="请核对已选《采购管理办法》第三十二条规定的六种采购方式，只引用确实列出这些方式的分片。",
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "web": Case(
        name="web",
        endpoint="knowledge-chat",
        agent_id="builtin-simple-chat",
        query="请联网查找 OpenAI Codex 的官方产品页面，用两点概括页面当前介绍的能力，并为每点标注网页引用。",
        web_search_enabled=True,
        require_retrieval=True,
        require_citations=True,
        min_web=1,
    ),
    "wiki": Case(
        name="wiki",
        endpoint="agent-chat",
        agent_id="builtin-wiki-researcher",
        knowledge_base_ids=("60cc7af1-c733-468e-b890-89a8b34f06a9",),
        query="请从当前 Wiki 中核对备用金管理的核心要求，只陈述实际阅读页面支持的内容，并在相邻事实后引用。",
        require_retrieval=True,
        require_citations=True,
        min_wiki=1,
    ),
    "general-mixed": Case(
        name="general-mixed",
        endpoint="agent-chat",
        # A dedicated same-type E2E agent is pinned to a local LiteLLM route so
        # this high-tool-count case never crosses the external gateway/WAF.
        # Built-in general-agent is covered separately by the single-source
        # live acceptance run.
        agent_id="a6d8b358-8fb7-4ba8-ad1c-1a668ee6e54c",
        knowledge_base_ids=("60cc7af1-c733-468e-b890-89a8b34f06a9",),
        # Pin the document leg to one selected file while keeping the Wiki
        # collection available. This exercises all three source protocols
        # without flooding the model context with unrelated collection files.
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query=(
            "请做三项只读检索，不要扩大范围或重复调用同一工具："
            "1）用 grep_chunks 检索已选《采购管理办法》的“第三十二条”，并列出该条规定的六种采购方式；"
            "2）用 wiki_search 搜索“备用金”并用 wiki_read_page 阅读最匹配的一页；"
            "3）用 web_search 搜索 OpenAI Codex 官方页面，必要时只 web_fetch 一个官方结果。"
            "完成后立即用三个项目符号回答，每项只写一句结论，并只复制该项证据旁的 cite_exactly 句柄。"
        ),
        web_search_enabled=True,
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        min_wiki=1,
        min_web=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "general": Case(
        name="general",
        endpoint="agent-chat",
        agent_id="builtin-general-agent",
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query=(
            "这是通用智能体的只读验收。请用 grep_chunks 核对已选《采购管理办法》"
            "第三十二条规定的六种采购方式，只陈述分片明确支持的事实，并在相邻结论后引用。"
        ),
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "document-agent": Case(
        name="document-agent",
        endpoint="agent-chat",
        agent_id="builtin-document-processing",
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query=(
            "这是文档处理智能体的只读问答任务，不生成或修改文件。"
            "请先用 grep_chunks 检索已选《采购管理办法》的相关分片，"
            "再总结采购方式和审批注意事项，并把工具返回的分片引用准确放在相邻事实后。"
        ),
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "custom-rag": Case(
        name="custom-rag",
        endpoint="agent-chat",
        agent_id="82c813d3-8e3f-4267-b718-74045e20f0a0",
        knowledge_ids=("2dc912a3-88d4-4d61-a93e-f15c086bbd97",),
        query="根据已选《采购管理办法》第三十二条，准确列出规定的六种采购方式。只陈述检索分片明确支持的内容，并在相邻事实后引用。",
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        required_answer_terms=PROCUREMENT_METHOD_TERMS,
    ),
    "table": Case(
        name="table",
        endpoint="agent-chat",
        agent_id="builtin-table-analyst",
        knowledge_ids=("b8805436-bcab-4975-816b-90a03412a5da",),
        query="只读分析已选 symbols.csv：先确认字段，再统计总行数并按可用的分类字段给出一个简短汇总；不要修改任何数据。",
        require_retrieval=True,
        require_citations=False,
        min_data_sources=1,
        expected_retrieval_unit="data_sources",
    ),
    "data": Case(
        name="data",
        endpoint="agent-chat",
        agent_id="builtin-data-analyst",
        query="仅做只读分析：列出当前可用数据源；如有可查询表，选择一个小表查看结构并返回不超过5行的聚合统计。禁止任何写操作。",
        require_retrieval=True,
        require_citations=False,
        min_data_sources=1,
        expected_retrieval_unit="data_sources",
    ),
    "knowledge-manager": Case(
        name="knowledge-manager",
        endpoint="agent-chat",
        agent_id="3d0c7a4a-a2c7-4e3d-b6bc-561868516a8e",
        knowledge_base_ids=("999f2134-f8e9-4b82-93fe-5305da1a6fc1",),
        knowledge_ids=("2630b8ab-a35b-4e3b-81c8-54a963627845",),
        query="仅只读核对已选《凤凰台账多实例值班说明》的责任团队和故障确认时限，不要新增、替换或删除任何文档；回答必须引用检索证据。",
        require_retrieval=True,
        require_citations=True,
        min_documents=1,
        forbidden_tools=("kb_add_document", "kb_replace_document", "kb_delete_document"),
    ),
    "auto-operation": Case(
        name="auto-operation",
        endpoint="agent-chat",
        agent_id="839b39bc-152a-46b2-9f12-bd55d95a3dec",
        query="这是只读验收：请说明你当前可提供哪些帮助，不要调用任何会修改外部状态的工具。",
        require_retrieval=False,
        require_citations=False,
    ),
}


class API:
    def __init__(self, base_url: str, api_key: str, timeout: float) -> None:
        self.base_url = base_url.rstrip("/")
        self.api_key = api_key
        self.timeout = timeout

    def request(self, method: str, path: str, body: Any | None = None) -> Any:
        data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
        request = urllib.request.Request(
            self.base_url + path,
            data=data,
            method=method,
            headers={"X-API-Key": self.api_key, "Content-Type": "application/json"},
        )
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                raw = response.read()
        except urllib.error.HTTPError as exc:
            raise RuntimeError(f"{method} {path} failed: HTTP {exc.code}: {exc.read()[:1000]!r}") from exc
        return json.loads(raw) if raw else None

    def stream(
        self,
        path: str,
        body: dict[str, Any],
        on_event: Callable[[dict[str, Any]], None] | None = None,
    ) -> tuple[list[dict[str, Any]], float, float]:
        request = urllib.request.Request(
            self.base_url + path,
            data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
            method="POST",
            headers={
                "X-API-Key": self.api_key,
                "Content-Type": "application/json",
                "Accept": "text/event-stream",
            },
        )
        started = time.perf_counter()
        first_event_at = 0.0
        events: list[dict[str, Any]] = []
        try:
            with urllib.request.urlopen(request, timeout=self.timeout) as response:
                data_lines: list[str] = []
                while True:
                    raw = response.readline()
                    if not raw:
                        break
                    line = raw.decode("utf-8", errors="replace").rstrip("\r\n")
                    if line.startswith("data:"):
                        data_lines.append(line[5:].lstrip())
                        continue
                    if line or not data_lines:
                        continue
                    payload = "\n".join(data_lines)
                    data_lines = []
                    if payload == "[DONE]":
                        continue
                    event = json.loads(payload)
                    if not first_event_at:
                        first_event_at = time.perf_counter()
                    events.append(event)
                    if on_event is not None:
                        on_event(event)
        except urllib.error.HTTPError as exc:
            raise RuntimeError(f"POST {path} failed: HTTP {exc.code}: {exc.read()[:1000]!r}") from exc
        ended = time.perf_counter()
        return events, (first_event_at or ended) - started, ended - started


def unwrap_data(value: Any) -> Any:
    return value.get("data") if isinstance(value, dict) and "data" in value else value


def event_type(event: dict[str, Any]) -> str:
    return str(event.get("response_type") or event.get("type") or "")


def tool_name(event: dict[str, Any]) -> str:
    data = event.get("data") if isinstance(event.get("data"), dict) else {}
    return str(event.get("tool_name") or data.get("tool_name") or "")


def latest_assistant(messages: list[dict[str, Any]]) -> dict[str, Any]:
    assistants = [message for message in messages if message.get("role") == "assistant"]
    if not assistants:
        raise AssertionError("no persisted assistant message")
    return max(assistants, key=lambda message: str(message.get("created_at") or ""))


def load_completed_assistant(api: API, session_id: str, wait_seconds: float = 8.0) -> dict[str, Any]:
    """Wait only for the completed projection already produced by the stream.

    Completion SSE and a subsequent load request can briefly land on different
    API replicas before the persisted message update is visible. This bounded
    read-only poll never repeats generation and prevents that topology detail
    from becoming a false E2E failure.
    """
    deadline = time.monotonic() + wait_seconds
    last: dict[str, Any] | None = None
    while True:
        loaded = unwrap_data(api.request("GET", f"/messages/{session_id}/load?limit=20"))
        try:
            last = latest_assistant(loaded)
        except AssertionError:
            last = None
        if last and last.get("is_completed") and str(last.get("content") or "").strip():
            return last
        if time.monotonic() >= deadline:
            if last is not None:
                return last
            raise AssertionError("no persisted assistant message")
        time.sleep(0.2)


def load_generated_title(api: API, session_id: str, wait_seconds: float = 30.0) -> str:
    deadline = time.monotonic() + wait_seconds
    title = ""
    while True:
        title = str(unwrap_data(api.request("GET", f"/sessions/{session_id}")).get("title") or "").strip()
        if title and title not in {"新对话", "New Conversation"}:
            return title
        if time.monotonic() >= deadline:
            return title
        time.sleep(0.2)


def assert_canonical_citations(message: dict[str, Any], require: bool) -> tuple[list[str], list[dict[str, Any]]]:
    content = str(message.get("content") or "")
    references = message.get("knowledge_references") or []
    if not isinstance(references, list):
        raise AssertionError("knowledge_references is not a list")
    ids = CANONICAL_SRC_RE.findall(content)
    tags = ANY_SRC_RE.findall(content)
    canonical_tags = CANONICAL_SRC_RE.findall("\n".join(tags))
    if len(canonical_tags) != len(tags):
        raise AssertionError(f"malformed src tag survived final filtering: {tags}")
    if legacy := LEGACY_CITATION_RE.findall(content):
        raise AssertionError(f"legacy citation tag survived final filtering: {legacy}")
    for step in message.get("agent_steps") or []:
        if not isinstance(step, dict):
            continue
        thought = str(step.get("thought") or "")
        if legacy := LEGACY_CITATION_RE.findall(thought):
            raise AssertionError(f"model generated legacy citation protocol: {legacy}")
    reference_ids = {
        str((reference.get("metadata") or {}).get("citation_id") or "")
        for reference in references
        if isinstance(reference, dict)
    }
    if any(citation_id not in reference_ids for citation_id in ids):
        raise AssertionError(f"body citation has no matching reference: body={ids}, refs={sorted(reference_ids)}")
    used = list(dict.fromkeys(ids))
    cited_reference_ids = [
        str((reference.get("metadata") or {}).get("citation_id") or "")
        for reference in references
        if str((reference.get("metadata") or {}).get("citation_id") or "") in set(used)
    ]
    if cited_reference_ids != used:
        raise AssertionError(
            "persisted references do not exactly follow first body-use order: "
            f"body={used}, refs={cited_reference_ids}"
        )
    if len(references) != len(used):
        raise AssertionError(
            "uncited references survived final projection: "
            f"body={used}, reference_count={len(references)}"
        )
    for match in CANONICAL_SRC_RE.finditer(content):
        nearby_claim = re.sub(r"\s+", "", content[max(0, match.start() - 80) : match.start()])
        if len(re.findall(r"[\w\u3400-\u9fff]", nearby_claim)) < 2:
            raise AssertionError(f"citation is not adjacent to a substantive claim: {match.group(0)}")
    if require and not used:
        raise AssertionError("expected at least one final citation")
    return used, references


def cited_reference_type_counts(references: list[dict[str, Any]], used_ids: list[str]) -> dict[str, int]:
    used = set(used_ids)
    ids_by_kind: dict[str, set[str]] = {
        "documents": set(), "wiki": set(), "web": set(), "data_sources": set()
    }
    for reference in references:
        if not isinstance(reference, dict):
            continue
        metadata = reference.get("metadata") if isinstance(reference.get("metadata"), dict) else {}
        citation_id = str(metadata.get("citation_id") or "")
        if citation_id not in used:
            continue
        source_type = str(metadata.get("source_type") or "")
        chunk_type = str(reference.get("chunk_type") or "")
        if source_type == "wiki" or chunk_type == "wiki_page":
            kind = "wiki"
        elif source_type == "web" or chunk_type == "web_search":
            kind = "web"
        elif source_type == "data_source" or chunk_type == "data_query_result":
            kind = "data_sources"
        else:
            kind = "documents"
        ids_by_kind[kind].add(citation_id)
    return {kind: len(ids) for kind, ids in ids_by_kind.items()}


def validate_stats(
    message: dict[str, Any],
    require_retrieval: bool,
    expected_unit: str = "documents",
) -> dict[str, Any]:
    stats = message.get("retrieval_stats") or {}
    for key in ("documents", "wiki", "web", "data_sources", "total"):
        if not isinstance(stats.get(key, 0), int) or stats.get(key, 0) < 0:
            raise AssertionError(f"invalid retrieval_stats.{key}: {stats.get(key)!r}")
    expected_total = sum(int(stats.get(key, 0)) for key in ("documents", "wiki", "web", "data_sources"))
    if int(stats.get("total", 0)) != expected_total:
        raise AssertionError(f"retrieval total mismatch: {stats}")
    if require_retrieval and (stats.get("attempted") is not True or expected_total < 1):
        raise AssertionError(f"expected inspected sources, got {stats}")
    if stats.get("unit") != expected_unit:
        raise AssertionError(
            f"retrieval unit mismatch: got {stats.get('unit')!r}, expected {expected_unit!r}"
        )
    return stats


def validate_execution_metadata(message: dict[str, Any], expect_agent_mode: bool) -> int:
    if bool(message.get("agent_mode")) is not expect_agent_mode:
        raise AssertionError(
            f"agent mode mismatch: got {message.get('agent_mode')!r}, expected {expect_agent_mode}"
        )
    authoritative = int(message.get("agent_tool_count") or 0)
    if authoritative < 0:
        raise AssertionError(f"negative agent_tool_count: {authoritative}")
    if not expect_agent_mode:
        return authoritative
    derived = 0
    for step in message.get("agent_steps") or []:
        for call in step.get("tool_calls") or []:
            name = str(call.get("name") or "").strip().lower()
            is_retrieval = any(
                name == canonical or name.endswith(f"__{canonical}") or name.endswith(f"_{canonical}")
                for canonical in RETRIEVAL_TOOL_NAMES
            )
            if name != "final_answer" and not is_retrieval:
                derived += 1
    if authoritative != derived:
        raise AssertionError(
            f"agent tool count mismatch: persisted={authoritative}, derived={derived}"
        )
    return authoritative


def run_case(api: API, case: Case, model_id: str) -> dict[str, Any]:
    # The product represents an untitled conversation with an empty persisted
    # title; the sidebar renders that state as “新对话” until generation ends.
    created = unwrap_data(api.request("POST", "/sessions", {"title": ""}))
    session_id = str(created["id"])
    payload = {
        "query": case.query,
        "knowledge_base_ids": list(case.knowledge_base_ids),
        "knowledge_ids": list(case.knowledge_ids),
        "agent_enabled": case.endpoint == "agent-chat",
        "agent_id": case.agent_id,
        "web_search_enabled": case.web_search_enabled,
        "summary_model_id": model_id,
        "disable_title": False,
        "channel": "api-e2e",
    }
    events, ttfb, total = api.stream(f"/{case.endpoint}/{session_id}", payload)
    errors = [event for event in events if event_type(event) == "error" and event.get("done") is True]
    if errors:
        raise AssertionError(f"stream error: {errors[-1]}")
    message = load_completed_assistant(api, session_id)
    if not str(message.get("content") or "").strip():
        raise AssertionError("persisted answer is empty")
    answer = str(message.get("content") or "")
    missing_terms = [term for term in case.required_answer_terms if term not in answer]
    if missing_terms:
        raise AssertionError(f"answer omitted required source facts: {missing_terms}")
    if not message.get("is_completed"):
        raise AssertionError("persisted answer is not completed")
    if int(message.get("agent_duration_ms") or 0) <= 0:
        raise AssertionError("authoritative duration was not persisted")
    used, references = assert_canonical_citations(message, case.require_citations)
    stats = validate_stats(message, case.require_retrieval, case.expected_retrieval_unit)
    tool_count = validate_execution_metadata(message, case.endpoint == "agent-chat")
    cited_types = cited_reference_type_counts(references, used)
    minimums = {
        "documents": case.min_documents,
        "wiki": case.min_wiki,
        "web": case.min_web,
        "data_sources": case.min_data_sources,
    }
    for kind, minimum in minimums.items():
        if int(stats.get(kind, 0)) < minimum:
            raise AssertionError(f"expected retrieval_stats.{kind} >= {minimum}, got {stats}")
        if kind != "data_sources" and minimum > 0 and cited_types.get(kind, 0) < 1:
            raise AssertionError(
                f"retrieved {kind} evidence but final answer did not cite that source type: "
                f"cited={cited_types}, stats={stats}"
            )
    called_tools = [tool_name(event) for event in events if event_type(event) == "tool_call"]
    prohibited = sorted(set(called_tools).intersection(case.forbidden_tools))
    if prohibited:
        raise AssertionError(f"read-only case invoked mutation tools: {prohibited}")
    result = {
        "case": case.name,
        "session_id": session_id,
        "events": len(events),
        "tools": called_tools,
        "citations": len(used),
        "references": len(references),
        "cited_types": cited_types,
        "retrieval_stats": stats,
        "duration_ms": int(message.get("agent_duration_ms") or 0),
        "agent_tool_count": tool_count,
        "ttfb_seconds": round(ttfb, 3),
        "total_seconds": round(total, 3),
        "title": load_generated_title(api, session_id),
    }
    if result["title"] in {"", "新对话", "New Conversation"}:
        raise AssertionError(f"conversation title was not generated: {result['title']!r}")
    print(json.dumps(result, ensure_ascii=False), flush=True)
    return result


def stream_turn(
    api: API,
    session_id: str,
    case: Case,
    model_id: str,
) -> tuple[dict[str, Any], list[dict[str, Any]]]:
    payload = {
        "query": case.query,
        "knowledge_base_ids": list(case.knowledge_base_ids),
        "knowledge_ids": list(case.knowledge_ids),
        "agent_enabled": case.endpoint == "agent-chat",
        "agent_id": case.agent_id,
        "web_search_enabled": case.web_search_enabled,
        "summary_model_id": model_id,
        "disable_title": False,
        "channel": "api-e2e-multiturn",
    }
    events, _, _ = api.stream(f"/{case.endpoint}/{session_id}", payload)
    errors = [event for event in events if event_type(event) == "error" and event.get("done") is True]
    if errors:
        raise AssertionError(f"multi-turn stream error: {errors[-1]}")
    return load_completed_assistant(api, session_id), events


def validate_turn(
    message: dict[str, Any],
    *,
    require_citations: bool,
    require_retrieval: bool,
    expected_kind: str | None,
    expect_agent_mode: bool,
) -> dict[str, Any]:
    used, references = assert_canonical_citations(message, require_citations)
    stats = validate_stats(message, require_retrieval)
    cited_types = cited_reference_type_counts(references, used)
    tool_count = validate_execution_metadata(message, expect_agent_mode)
    if expected_kind is not None:
        if int(stats.get(expected_kind, 0)) < 1 or int(cited_types.get(expected_kind, 0)) < 1:
            raise AssertionError(
                f"multi-turn source switch did not produce {expected_kind}: "
                f"stats={stats}, cited={cited_types}"
            )
    return {
        "message_id": str(message.get("id") or ""),
        "citations": len(used),
        "cited_types": cited_types,
        "retrieval_stats": stats,
        "agent_tool_count": tool_count,
    }


def run_multi_turn(api: API, model_id: str) -> dict[str, Any]:
    """Exercise source selection, removal, and a different source in one chat.

    This intentionally verifies the persisted projection after every turn so a
    previous turn's references or retrieval counters cannot leak forward.
    """
    session_id = str(unwrap_data(api.request("POST", "/sessions", {"title": ""}))["id"])
    first, _ = stream_turn(api, session_id, CASES["quick"], model_id)
    first_result = validate_turn(
        first,
        require_citations=True,
        require_retrieval=True,
        expected_kind="documents",
        expect_agent_mode=False,
    )

    no_source = Case(
        name="multi-turn-no-source",
        endpoint="knowledge-chat",
        agent_id="builtin-simple-chat",
        query="本轮不使用知识库或联网，只回答：2+2等于多少？",
    )
    second, _ = stream_turn(api, session_id, no_source, model_id)
    second_result = validate_turn(
        second,
        require_citations=False,
        require_retrieval=False,
        expected_kind=None,
        expect_agent_mode=False,
    )
    if int(second_result["retrieval_stats"].get("total", 0)) != 0:
        raise AssertionError(f"removed knowledge selection still retrieved sources: {second_result}")
    if second_result["citations"] != 0:
        raise AssertionError(f"removed knowledge selection inherited stale citations: {second_result}")

    # Use the Claude Agent SDK path for the source-switch turn. The same local
    # model can answer direct RAG turns and drive this Anthropic tool protocol,
    # while some OpenAI-tool routes are model-specific and are covered by the
    # standalone native Wiki case instead of making this state-isolation test
    # depend on a second protocol dialect.
    wiki_switch = Case(
        name="multi-turn-wiki-switch",
        endpoint="agent-chat",
        agent_id="a6d8b358-8fb7-4ba8-ad1c-1a668ee6e54c",
        knowledge_base_ids=("60cc7af1-c733-468e-b890-89a8b34f06a9",),
        query=(
            "本轮只使用当前 Wiki：先搜索并阅读与备用金管理最相关的页面，"
            "再简要列出页面明确支持的核心要求，并在相邻事实后引用。"
        ),
        require_retrieval=True,
        require_citations=True,
        min_wiki=1,
    )
    third, _ = stream_turn(api, session_id, wiki_switch, model_id)
    third_result = validate_turn(
        third,
        require_citations=True,
        require_retrieval=True,
        expected_kind="wiki",
        expect_agent_mode=True,
    )
    if "备用金" not in str(third.get("content") or ""):
        raise AssertionError("third turn cited Wiki evidence but did not answer the current reserve-fund question")
    message_ids = [first_result["message_id"], second_result["message_id"], third_result["message_id"]]
    if len(set(message_ids)) != 3 or any(not value for value in message_ids):
        raise AssertionError(f"multi-turn messages were not independently persisted: {message_ids}")
    result = {
        "case": "multi-turn",
        "session_id": session_id,
        "turns": [first_result, second_result, third_result],
        "title": load_generated_title(api, session_id),
    }
    print(json.dumps(result, ensure_ascii=False), flush=True)
    return result


def run_cancellation(api: API, model_id: str) -> dict[str, Any]:
    """Cancel after retrieval begins and validate the durable partial answer."""
    session_id = str(unwrap_data(api.request("POST", "/sessions", {"title": ""}))["id"])
    case = CASES["smart"]
    assistant_message_id = ""
    stop_requested = False

    def stop_after_retrieval_starts(event: dict[str, Any]) -> None:
        nonlocal assistant_message_id, stop_requested
        if event_type(event) == "agent_query":
            data = event.get("data") if isinstance(event.get("data"), dict) else {}
            assistant_message_id = str(
                event.get("assistant_message_id") or data.get("assistant_message_id") or ""
            )
        if stop_requested or not assistant_message_id:
            return
        if event_type(event) in {"tool_call", "retrieval_progress", "references"}:
            api.request("POST", f"/sessions/{session_id}/stop", {"message_id": assistant_message_id})
            stop_requested = True

    payload = {
        "query": "请仔细检索已选《采购管理办法》，分步骤核对采购方式、决策和审批要求并给出引用。",
        "knowledge_ids": list(case.knowledge_ids),
        "knowledge_base_ids": [],
        "agent_enabled": True,
        "agent_id": case.agent_id,
        "web_search_enabled": False,
        "summary_model_id": model_id,
        "disable_title": False,
        "channel": "api-e2e-cancel",
    }
    events, _, _ = api.stream(f"/{case.endpoint}/{session_id}", payload, on_event=stop_after_retrieval_starts)
    if not stop_requested:
        raise AssertionError(f"stream completed before cancellation point: {[event_type(e) for e in events]}")
    if not any(event_type(event) == "stop" for event in events):
        raise AssertionError("stop API succeeded but SSE did not expose the stop event")
    message = load_completed_assistant(api, session_id)
    if str(message.get("id") or "") != assistant_message_id:
        raise AssertionError("stopped assistant message projection does not match the streamed message")
    used, references = assert_canonical_citations(message, require=False)
    stats = validate_stats(message, require_retrieval=False)
    tool_count = validate_execution_metadata(message, expect_agent_mode=True)
    if int(message.get("agent_duration_ms") or 0) <= 0:
        raise AssertionError("stopped message did not persist integer duration milliseconds")
    result = {
        "case": "cancellation",
        "session_id": session_id,
        "message_id": assistant_message_id,
        "citations": len(used),
        "references": len(references),
        "retrieval_stats": stats,
        "duration_ms": int(message.get("agent_duration_ms") or 0),
        "agent_tool_count": tool_count,
    }
    print(json.dumps(result, ensure_ascii=False), flush=True)
    return result


def main() -> int:
    parser = argparse.ArgumentParser(description="Live citation/retrieval-stat API E2E against the local production topology")
    parser.add_argument("--base-url", default="http://localhost:8080/api/v1")
    parser.add_argument("--model-id", default="6c03306d-f265-4718-8523-11a08665912a")
    parser.add_argument("--timeout", type=float, default=240.0)
    scenario_names = sorted([*CASES, "multi-turn", "cancellation"])
    parser.add_argument("--case", action="append", choices=scenario_names, dest="cases")
    args = parser.parse_args()
    api_key = os.environ.get("WEKNORA_E2E_TENANT_API_KEY", "").strip()
    if not api_key:
        print("WEKNORA_E2E_TENANT_API_KEY is required", file=sys.stderr)
        return 2
    selected = args.cases or [*CASES, "multi-turn", "cancellation"]
    api = API(args.base_url, api_key, args.timeout)
    results: list[dict[str, Any]] = []
    failures: list[dict[str, str]] = []
    for name in selected:
        try:
            if name == "multi-turn":
                results.append(run_multi_turn(api, args.model_id))
            elif name == "cancellation":
                results.append(run_cancellation(api, args.model_id))
            else:
                results.append(run_case(api, CASES[name], args.model_id))
        except Exception as exc:
            failure = {"case": name, "error": str(exc)}
            failures.append(failure)
            print(json.dumps(failure, ensure_ascii=False), file=sys.stderr, flush=True)
    print(json.dumps({"ok": not failures, "passed": len(results), "failed": failures}, ensure_ascii=False))
    return 1 if failures else 0


if __name__ == "__main__":
    raise SystemExit(main())
