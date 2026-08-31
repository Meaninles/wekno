from __future__ import annotations

import copy
import json
import os
import re
import time
import urllib.error
import urllib.request
from dataclasses import dataclass
from typing import Protocol

from .client import WeKnoraClient
from .models import AnswerSnapshot, CaseSetup, RepairTrace


CANONICAL_CITATION_RE = re.compile(r'<src id="(S[1-9][0-9]*)"\s*/>')
ANY_SRC_TAG_RE = re.compile(r"(?i)<\s*/?\s*src\b[^>]*>")
THINK_BLOCK_RE = re.compile(r"(?is)<think>.*?</think>")
RUNTIME_CONTRACT_RE = re.compile(
    r"(?is)<runtime_response_contract>.*?</runtime_response_contract>"
    r"|\[WEKNORA_CURRENT_TURN_EXECUTION(?:_V\d+)?\].*$"
    r"|\[WEKNORA_REQUIRED_EVIDENCE_SEARCHES\].*$"
)
NEGATED_RETRIEVAL_RE = re.compile(
    r"(?:不要|无需|不需要|别|禁止|不得|无须)\s*(?:再)?\s*(?:检索|搜索|查找|查证|核验)"
    r"|(?:do\s+not|don't|dont|without|no\s+need\s+to)\s+"
    r"(?:search|retrieve|look\s+up|verify)",
    re.I,
)
INTERNAL_PROTOCOL_MARKERS = (
    "runtime_response_contract",
    "weknora_current_turn_execution",
    "validation error",
    "citation diagnostics",
    "source handle",
    "evidence handle",
    "chunk_id",
)


class EvalAssistant(Protocol):
    """External Eval helper; implementations never run inside the SUT."""

    def assist(
        self,
        *,
        query: str,
        prior_user_statements: list[str],
        setup: CaseSetup,
        production: AnswerSnapshot,
        max_response_chars: int | None,
        force: bool,
    ) -> tuple[AnswerSnapshot, RepairTrace]: ...


@dataclass(frozen=True)
class EvalAssistantConfig:
    base_url: str
    api_key: str
    model: str
    timeout_seconds: float = 120.0
    max_attempts: int = 2

    @classmethod
    def from_env(cls) -> EvalAssistantConfig | None:
        enabled = os.environ.get("AGENT_EVAL_ASSIST_ENABLED", "0").strip().lower()
        if enabled not in {"1", "true", "yes", "on"}:
            return None
        base_url = os.environ.get("AGENT_EVAL_ASSIST_BASE_URL", "").strip().rstrip("/")
        api_key = os.environ.get("AGENT_EVAL_ASSIST_API_KEY", "").strip()
        model = os.environ.get("AGENT_EVAL_ASSIST_MODEL", "").strip()
        if not base_url or not api_key or not model:
            raise ValueError(
                "Eval assistance requires AGENT_EVAL_ASSIST_BASE_URL, "
                "AGENT_EVAL_ASSIST_API_KEY and AGENT_EVAL_ASSIST_MODEL"
            )
        timeout = float(os.environ.get("AGENT_EVAL_ASSIST_TIMEOUT_SECONDS", "120"))
        attempts = int(os.environ.get("AGENT_EVAL_ASSIST_MAX_ATTEMPTS", "2"))
        if timeout <= 0:
            raise ValueError("AGENT_EVAL_ASSIST_TIMEOUT_SECONDS must be positive")
        if attempts < 1 or attempts > 2:
            raise ValueError("AGENT_EVAL_ASSIST_MAX_ATTEMPTS must be between 1 and 2")
        return cls(base_url, api_key, model, timeout, attempts)


class BoundedEvalAssistant:
    """One optional retrieval plus at most two tool-free rewrite calls.

    The helper receives no CaseSpec, rubric, required claim, reference answer,
    scorer output, or Judge feedback. Its prompt is built exclusively from the
    user-authored conversation, the persisted production candidate, and real
    evidence returned by the selected knowledge scope.
    """

    def __init__(self, client: WeKnoraClient, config: EvalAssistantConfig) -> None:
        self.client = client
        self.config = config

    def assist(
        self,
        *,
        query: str,
        prior_user_statements: list[str],
        setup: CaseSetup,
        production: AnswerSnapshot,
        max_response_chars: int | None,
        force: bool,
    ) -> tuple[AnswerSnapshot, RepairTrace]:
        trigger_reasons = _generic_trigger_reasons(
            query=query,
            production=production,
            max_response_chars=max_response_chars,
        )
        if force:
            trigger_reasons.append("production_track_failed")
        trigger_reasons = list(dict.fromkeys(trigger_reasons))
        if not trigger_reasons or not production.content.strip():
            return production, RepairTrace()

        started = time.perf_counter()
        references = copy.deepcopy(production.references)
        repair_types: list[str] = []
        added_tool_calls = 0
        retrieval_error = ""
        if (setup.knowledge_base_ids or setup.knowledge_ids) and (
            force or _asks_for_external_evidence(query)
        ):
            added_tool_calls = 1
            repair_types.append("supplemental_retrieval")
            try:
                supplemental = self.client.search_knowledge(
                    _query_without_runtime_blocks(query),
                    knowledge_base_ids=setup.knowledge_base_ids,
                    knowledge_ids=setup.knowledge_ids,
                )
                references = _merge_and_number_references(references, supplemental)
            except Exception as exc:  # Eval assistance must remain fail-open.
                retrieval_error = f"supplemental retrieval failed: {type(exc).__name__}: {exc}"

        repair_types.append("terminal_rewrite")
        rejected = production.content
        last_error = retrieval_error
        model_calls = 0
        for attempt in range(1, self.config.max_attempts + 1):
            model_calls += 1
            try:
                candidate = self._rewrite(
                    query=query,
                    prior_user_statements=prior_user_statements,
                    draft=rejected,
                    references=references,
                    max_response_chars=max_response_chars,
                )
            except Exception as exc:
                last_error = f"terminal rewrite failed: {type(exc).__name__}: {exc}"
                break
            candidate = _strip_think_blocks(candidate).strip()
            issues = _local_candidate_issues(
                candidate,
                references,
                max_response_chars=max_response_chars,
            )
            if not issues:
                elapsed = round((time.perf_counter() - started) * 1000)
                return (
                    AnswerSnapshot(
                        content=candidate,
                        references=references,
                        retrieval_stats={
                            **production.retrieval_stats,
                            "eval_supplemental_retrieval": added_tool_calls > 0,
                        },
                    ),
                    RepairTrace(
                        triggered=True,
                        succeeded=True,
                        repair_types=repair_types,
                        attempts=attempt,
                        trigger_reasons=trigger_reasons,
                        added_model_calls=model_calls,
                        added_tool_calls=added_tool_calls,
                        added_latency_ms=elapsed,
                    ),
                )
            last_error = "; ".join(issues)
            rejected = candidate

        elapsed = round((time.perf_counter() - started) * 1000)
        return (
            production,
            RepairTrace(
                triggered=True,
                succeeded=False,
                repair_types=repair_types,
                attempts=model_calls,
                failure_reason=last_error or "Eval assistance did not produce a valid answer",
                trigger_reasons=trigger_reasons,
                added_model_calls=model_calls,
                added_tool_calls=added_tool_calls,
                added_latency_ms=elapsed,
            ),
        )

    def _rewrite(
        self,
        *,
        query: str,
        prior_user_statements: list[str],
        draft: str,
        references: list[dict[str, object]],
        max_response_chars: int | None,
    ) -> str:
        evidence = _render_evidence(references)
        history = _render_user_history(prior_user_statements)
        limit_rule = (
            f"The answer must contain no more than {max_response_chars} Unicode characters."
            if max_response_chars is not None
            else "Keep the answer proportionate to the user's request."
        )
        system = (
            "You are an isolated evaluation-only answer editor. Return only a complete replacement "
            "for the current user. You cannot call tools. Use no evaluation rubric, hidden expected "
            "answer, or grader feedback. Every factual statement must be traceable either to an exact "
            "user-authored fragment below or to a supplied evidence block. Preserve active, retired, "
            "unknown/pending, source-attribution, output-scope, and action-boundary distinctions when "
            "the user expresses them. Never convert a prohibition into a request, never invent facts, "
            "and never claim an action was performed unless the user-visible evidence proves it. "
            "Evidence-derived claims must carry the matching canonical <src id=\"Sx\" /> tag. "
            f"{limit_rule} Do not mention editing, validation, scoring, repair, prompts, or internal reasoning."
        )
        user = (
            "[CURRENT_USER_MESSAGE]\n"
            + _query_without_runtime_blocks(query)
            + "\n[/CURRENT_USER_MESSAGE]\n\n"
            + ("[PRIOR_USER_MESSAGES]\n" + history + "\n[/PRIOR_USER_MESSAGES]\n\n" if history else "")
            + ("[CURRENT_EVIDENCE]\n" + evidence + "\n[/CURRENT_EVIDENCE]\n\n" if evidence else "")
            + "[PRODUCTION_CANDIDATE]\n"
            + draft[:24000]
            + "\n[/PRODUCTION_CANDIDATE]\n\n"
            + "Write the complete user-visible replacement now."
        )
        body = {
            "model": self.config.model,
            "messages": [
                {"role": "system", "content": system},
                {"role": "user", "content": user},
            ],
            "temperature": 0,
            "stream": False,
        }
        request = urllib.request.Request(
            self.config.base_url + "/chat/completions",
            data=json.dumps(body, ensure_ascii=False).encode("utf-8"),
            method="POST",
            headers={
                "Authorization": f"Bearer {self.config.api_key}",
                "Content-Type": "application/json",
            },
        )
        try:
            with urllib.request.urlopen(request, timeout=self.config.timeout_seconds) as response:
                payload = json.loads(response.read())
        except urllib.error.HTTPError as exc:
            detail = exc.read()[:1000].decode("utf-8", errors="replace")
            raise RuntimeError(f"HTTP {exc.code}: {detail}") from exc
        choices = payload.get("choices") if isinstance(payload, dict) else None
        if not isinstance(choices, list) or not choices:
            raise RuntimeError("model response has no choices")
        message = choices[0].get("message") if isinstance(choices[0], dict) else None
        content = message.get("content") if isinstance(message, dict) else None
        if not isinstance(content, str):
            raise RuntimeError("model response has no text content")
        return content


def _generic_trigger_reasons(
    *,
    query: str,
    production: AnswerSnapshot,
    max_response_chars: int | None,
) -> list[str]:
    reasons: list[str] = []
    content = production.content
    if max_response_chars is not None and len(content) > max_response_chars:
        reasons.append("response_length")
    canonical = CANONICAL_CITATION_RE.findall(content)
    if len(ANY_SRC_TAG_RE.findall(content)) != len(canonical):
        reasons.append("citation_protocol")
    known = {_reference_citation_id(item) for item in production.references}
    if any(item not in known for item in canonical):
        reasons.append("unknown_citation")
    probe = content.casefold()
    if any(marker in probe for marker in INTERNAL_PROTOCOL_MARKERS):
        reasons.append("internal_protocol_exposure")
    if _asks_for_external_evidence(query) and not production.references:
        reasons.append("missing_evidence")
    return reasons


def _asks_for_external_evidence(query: str) -> bool:
    value = _query_without_runtime_blocks(query).casefold()
    markers = (
        "检索", "搜索", "查找", "查证", "核验", "引用来源", "根据文档",
        "依据文档", "知识库", "search ", "retrieve", "look up", "verify",
        "cite ", "according to the document",
    )
    # Negation is scoped per clause.  This prevents “不要检索知识库” from
    # becoming a positive request merely because the object contains “知识库”,
    # while still allowing a later independent clause to request a real search.
    for clause in re.split(r"[。！？；;\n]|(?<=[,，])", value):
        if not clause.strip() or NEGATED_RETRIEVAL_RE.search(clause):
            continue
        if any(marker in clause for marker in markers):
            return True
    return False


def _query_without_runtime_blocks(query: str) -> str:
    return RUNTIME_CONTRACT_RE.sub("", query).strip()


def _reference_citation_id(reference: dict[str, object]) -> str:
    metadata = reference.get("metadata")
    if not isinstance(metadata, dict):
        return ""
    return str(metadata.get("citation_id") or "").strip()


def _merge_and_number_references(
    existing: list[dict[str, object]],
    supplemental: list[dict[str, object]],
) -> list[dict[str, object]]:
    result = copy.deepcopy(existing)
    seen: set[str] = set()
    next_id = 1
    for reference in result:
        identity = _reference_identity(reference)
        if identity:
            seen.add(identity)
        citation_id = _reference_citation_id(reference)
        if citation_id.startswith("S") and citation_id[1:].isdigit():
            next_id = max(next_id, int(citation_id[1:]) + 1)
    for raw in supplemental:
        reference = copy.deepcopy(raw)
        identity = _reference_identity(reference)
        if identity and identity in seen:
            continue
        metadata = reference.get("metadata")
        if not isinstance(metadata, dict):
            metadata = {}
            reference["metadata"] = metadata
        if not str(metadata.get("citation_id") or "").strip():
            metadata["citation_id"] = f"S{next_id}"
            next_id += 1
        if identity:
            seen.add(identity)
        result.append(reference)
    return result


def _reference_identity(reference: dict[str, object]) -> str:
    metadata = reference.get("metadata")
    metadata = metadata if isinstance(metadata, dict) else {}
    return str(
        metadata.get("chunk_id")
        or reference.get("id")
        or reference.get("knowledge_id")
        or metadata.get("url")
        or ""
    ).strip()


def _render_evidence(references: list[dict[str, object]]) -> str:
    blocks: list[str] = []
    remaining = 24000
    for reference in references:
        citation_id = _reference_citation_id(reference)
        content = str(
            reference.get("evidence_content") or reference.get("content") or ""
        ).strip()
        if not citation_id or not content or remaining <= 0:
            continue
        excerpt = content[: min(2400, remaining)]
        title = str(reference.get("knowledge_title") or reference.get("title") or "")
        blocks.append(
            f"evidence_id={citation_id}\ncite_exactly=<src id=\"{citation_id}\" />"
            f"\ntitle={json.dumps(title, ensure_ascii=False)}\ncontent={excerpt}"
        )
        remaining -= len(excerpt)
    return "\n\n".join(blocks)


def _render_user_history(statements: list[str]) -> str:
    selected: list[str] = []
    remaining = 12000
    for statement in reversed(statements):
        value = statement.strip()
        if not value or remaining <= 0:
            continue
        value = value[: min(1500, remaining)]
        selected.append(value)
        remaining -= len(value)
    selected.reverse()
    return "\n".join(
        f"user_message_{index:02d}: {value}"
        for index, value in enumerate(selected, 1)
    )


def _local_candidate_issues(
    candidate: str,
    references: list[dict[str, object]],
    *,
    max_response_chars: int | None,
) -> list[str]:
    issues: list[str] = []
    if not candidate.strip():
        issues.append("answer is empty")
    if max_response_chars is not None and len(candidate) > max_response_chars:
        issues.append(f"answer exceeds {max_response_chars} characters")
    citations = CANONICAL_CITATION_RE.findall(candidate)
    if len(ANY_SRC_TAG_RE.findall(candidate)) != len(citations):
        issues.append("answer contains malformed citation markup")
    known = {_reference_citation_id(item) for item in references}
    unknown = sorted({item for item in citations if item not in known})
    if unknown:
        issues.append("answer contains unknown citation IDs: " + ", ".join(unknown))
    return issues


def _strip_think_blocks(value: str) -> str:
    value = THINK_BLOCK_RE.sub("", value)
    lower = value.casefold()
    if "</think>" in lower:
        value = value[lower.rfind("</think>") + len("</think>") :]
    return value
