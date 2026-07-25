from __future__ import annotations

import json
from concurrent.futures import ThreadPoolExecutor, as_completed
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Mapping, Sequence

if __package__ in {None, ""}:
    from cluster_e2e import APIClient, ClusterE2ERunner, E2EFailure, first_value  # type: ignore
else:
    from .cluster_e2e import APIClient, ClusterE2ERunner, E2EFailure, first_value


@dataclass(frozen=True)
class PolicySource:
    source_id: str
    policy_id: str
    policy_title: str
    role: str
    fixture_path: Path


@dataclass(frozen=True)
class RetrievalCase:
    source_ids: tuple[str, ...]
    prompt: str
    quotes: tuple[str, ...]
    coverage_label: str


@dataclass(frozen=True)
class PolicyCorpus:
    sources: tuple[PolicySource, ...]
    cases: tuple[RetrievalCase, ...]

    @property
    def fixture_paths(self) -> tuple[Path, ...]:
        return tuple(source.fixture_path for source in self.sources)


def _load_json(path: Path) -> Any:
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise E2EFailure(f"cannot load policy corpus JSON {path}: {exc}") from exc


def _resolve_fixture(fixture_dir: Path, source_id: str) -> Path:
    matches = sorted(
        path
        for path in fixture_dir.glob(f"{source_id}.*")
        if path.is_file()
    )
    if len(matches) != 1:
        raise E2EFailure(
            f"policy source {source_id!r} must resolve to exactly one fixture in "
            f"{fixture_dir}, got {[str(path) for path in matches]}"
        )
    return matches[0]


def load_policy_corpus(
    manifest_path: Path,
    questions_path: Path,
    fixture_dir: Path,
    *,
    queries_per_source: int = 2,
) -> PolicyCorpus:
    if queries_per_source <= 0:
        raise E2EFailure("policy queries_per_source must be positive")
    manifest = _load_json(manifest_path)
    raw_questions = _load_json(questions_path)
    if not isinstance(manifest, list) or not isinstance(raw_questions, list):
        raise E2EFailure("policy manifest and questions must both be JSON arrays")

    sources: list[PolicySource] = []
    seen_source_ids: set[str] = set()
    for raw in manifest:
        if not isinstance(raw, Mapping):
            raise E2EFailure("policy manifest entries must be objects")
        source_id = str(raw.get("sourceId", "")).strip()
        if not source_id or source_id in seen_source_ids:
            raise E2EFailure(f"policy manifest has missing or duplicate sourceId: {source_id!r}")
        seen_source_ids.add(source_id)
        sources.append(
            PolicySource(
                source_id=source_id,
                policy_id=str(raw.get("policyId", "")).strip(),
                policy_title=str(raw.get("policyTitle", "")).strip(),
                role=str(raw.get("role", "")).strip(),
                fixture_path=_resolve_fixture(fixture_dir, source_id),
            )
        )

    questions_by_source: dict[str, list[RetrievalCase]] = {
        source.source_id: [] for source in sources
    }
    for raw in raw_questions:
        if not isinstance(raw, Mapping):
            continue
        prompt = str(raw.get("prompt", "")).strip()
        raw_refs = raw.get("sourceReferences", [])
        if not prompt or not isinstance(raw_refs, list):
            continue
        source_ids: list[str] = []
        quotes: list[str] = []
        for raw_ref in raw_refs:
            if not isinstance(raw_ref, Mapping):
                continue
            source_id = str(raw_ref.get("id", "")).strip()
            if source_id not in seen_source_ids:
                continue
            if source_id not in source_ids:
                source_ids.append(source_id)
            quote = str(raw_ref.get("quote", "")).strip()
            if quote:
                quotes.append(quote)
        if not source_ids:
            continue
        case = RetrievalCase(
            source_ids=tuple(source_ids),
            prompt=prompt,
            quotes=tuple(quotes),
            coverage_label=str(raw.get("coverageLabel", "")).strip(),
        )
        for source_id in source_ids:
            questions_by_source[source_id].append(case)

    selected: list[RetrievalCase] = []
    selection_keys: set[tuple[tuple[str, ...], str]] = set()
    for source in sources:
        available = questions_by_source[source.source_id]
        if not available:
            raise E2EFailure(f"policy source {source.source_id!r} has no retrieval oracle questions")
        indexes = _stratified_indexes(len(available), min(queries_per_source, len(available)))
        for index in indexes:
            case = available[index]
            key = (case.source_ids, case.prompt)
            if key in selection_keys:
                continue
            selection_keys.add(key)
            selected.append(case)

    return PolicyCorpus(sources=tuple(sources), cases=tuple(selected))


def _stratified_indexes(total: int, count: int) -> list[int]:
    if total <= 0 or count <= 0:
        return []
    if count >= total:
        return list(range(total))
    if count == 1:
        return [total // 2]
    indexes = {
        round(index * (total - 1) / (count - 1))
        for index in range(count)
    }
    return sorted(indexes)


def normalize_policy_text(value: str) -> str:
    return "".join(character.casefold() for character in value if character.isalnum())


def quote_matches_chunks(
    quote: str,
    chunk_text: str,
    *,
    window: int = 18,
) -> bool:
    normalized_quote = normalize_policy_text(quote)
    normalized_chunks = normalize_policy_text(chunk_text)
    if not normalized_quote or not normalized_chunks:
        return False
    if normalized_quote in normalized_chunks:
        return True
    if len(normalized_quote) < window:
        return normalized_quote in normalized_chunks
    step = max(1, window // 2)
    return any(
        normalized_quote[start : start + window] in normalized_chunks
        for start in range(0, len(normalized_quote) - window + 1, step)
    )


def source_id_from_uploaded_filename(
    filename: str,
    sources: Sequence[PolicySource],
) -> str:
    normalized_filename = Path(filename).name
    normalized_stem = Path(normalized_filename).stem
    matches = [
        source.source_id
        for source in sources
        if normalized_filename == source.fixture_path.name
        or normalized_filename.endswith(f"-{source.fixture_path.name}")
        or normalized_stem == source.source_id
        or normalized_stem.endswith(f"-{source.source_id}")
    ]
    if len(matches) != 1:
        raise E2EFailure(
            f"uploaded filename {filename!r} does not map to exactly one policy source: {matches}"
        )
    return matches[0]


def evaluate_policy_corpus(
    client: APIClient,
    runner: ClusterE2ERunner,
    corpus: PolicyCorpus,
    *,
    recall_at_k: int = 5,
    min_recall: float = 0.90,
    query_concurrency: int = 16,
) -> dict[str, Any]:
    if recall_at_k <= 0:
        raise E2EFailure("policy recall_at_k must be positive")
    if not 0 < min_recall <= 1:
        raise E2EFailure("policy min_recall must be in (0, 1]")
    if query_concurrency <= 0:
        raise E2EFailure("policy query concurrency must be positive")

    source_to_knowledge: dict[str, str] = {}
    for knowledge_id, observation in runner.observations.items():
        source_id = source_id_from_uploaded_filename(observation.filename, corpus.sources)
        if source_id in source_to_knowledge:
            raise E2EFailure(f"policy source {source_id!r} was uploaded more than once")
        source_to_knowledge[source_id] = knowledge_id
    expected_source_ids = {source.source_id for source in corpus.sources}
    if set(source_to_knowledge) != expected_source_ids:
        missing = sorted(expected_source_ids - set(source_to_knowledge))
        extra = sorted(set(source_to_knowledge) - expected_source_ids)
        raise E2EFailure(f"policy upload mapping mismatch: missing={missing}, extra={extra}")

    all_knowledge_ids = list(source_to_knowledge.values())
    chunk_text_by_source: dict[str, str] = {}
    chunk_count_by_source: dict[str, int] = {}
    chunk_types = [
        "text",
        "parent_text",
        "image_ocr",
        "image_caption",
        "summary",
        "entity",
        "relationship",
        "table_summary",
        "table_column",
    ]
    for source_id, knowledge_id in source_to_knowledge.items():
        chunks = client.list_chunks(knowledge_id, chunk_types)
        chunk_count_by_source[source_id] = len(chunks)
        chunk_text_by_source[source_id] = "\n".join(
            str(first_value(chunk, ("content", "text", "page_content"), ""))
            for chunk in chunks
        )

    anchored_sources: set[str] = set()
    for case_index, case in enumerate(corpus.cases):
        for source_id in case.source_ids:
            if source_id in anchored_sources:
                continue
            if any(
                quote_matches_chunks(quote, chunk_text_by_source[source_id])
                for quote in case.quotes
            ):
                anchored_sources.add(source_id)

    def evaluate_case(
        case_index: int,
        case: RetrievalCase,
    ) -> tuple[int, tuple[str, ...], int | None]:
        expected_knowledge_ids = {
            source_to_knowledge[source_id] for source_id in case.source_ids
        }
        results = client.hybrid_search(
            runner.kb_id,
            case.prompt,
            all_knowledge_ids,
        )
        ranking = [
            str(item.get("knowledge_id", "")) for item in results[:recall_at_k]
        ]
        rank = next(
            (
                index + 1
                for index, knowledge_id in enumerate(ranking)
                if knowledge_id in expected_knowledge_ids
            ),
            None,
        )
        return case_index, case.source_ids, rank

    successes = 0
    reciprocal_rank_total = 0.0
    successful_sources: set[str] = set()
    failed_case_ids: list[str] = []
    with ThreadPoolExecutor(
        max_workers=min(query_concurrency, max(1, len(corpus.cases)))
    ) as pool:
        futures = {
            pool.submit(evaluate_case, case_index, case): case_index
            for case_index, case in enumerate(corpus.cases)
        }
        completed = 0
        for future in as_completed(futures):
            case_index, source_ids, rank = future.result()
            if rank is not None:
                successes += 1
                reciprocal_rank_total += 1.0 / rank
                successful_sources.update(source_ids)
            else:
                failed_case_ids.append(f"case-{case_index:04d}")
            completed += 1
            if completed % 100 == 0 or completed == len(corpus.cases):
                recorder = getattr(runner, "recorder", None)
                if recorder is not None:
                    recorder.emit(
                        "policy.retrieval_progress",
                        completed=completed,
                        total=len(corpus.cases),
                        successes=successes,
                    )
    failed_case_ids.sort()

    total_cases = len(corpus.cases)
    recall = successes / total_cases if total_cases else 0.0
    mean_reciprocal_rank = reciprocal_rank_total / total_cases if total_cases else 0.0
    missing_retrieval_sources = sorted(expected_source_ids - successful_sources)
    missing_anchor_sources = sorted(expected_source_ids - anchored_sources)
    if missing_retrieval_sources:
        raise E2EFailure(
            "policy retrieval did not return at least one curated query for every source: "
            f"{missing_retrieval_sources}"
        )
    if missing_anchor_sources:
        raise E2EFailure(
            "policy persisted chunks lack a quoted正文/附件/流程 anchor for sources: "
            f"{missing_anchor_sources}"
        )
    if recall < min_recall:
        raise E2EFailure(
            f"policy recall@{recall_at_k} {recall:.4f} is below {min_recall:.4f}; "
            f"failed_cases={failed_case_ids}"
        )

    flow_sources = sorted(
        source.source_id for source in corpus.sources if "flow" in source.source_id
    )
    attachment_sources = sorted(
        source.source_id for source in corpus.sources if "附件" in source.role
    )
    return {
        "sources": len(corpus.sources),
        "policy_units": len({source.policy_id for source in corpus.sources}),
        "curated_queries": total_cases,
        "recall_at_k": recall_at_k,
        "recall": recall,
        "mean_reciprocal_rank": mean_reciprocal_rank,
        "minimum_recall": min_recall,
        "query_concurrency": query_concurrency,
        "source_retrieval_coverage": len(successful_sources) / len(expected_source_ids),
        "source_anchor_coverage": len(anchored_sources) / len(expected_source_ids),
        "flow_sources": flow_sources,
        "attachment_sources": attachment_sources,
        "chunk_counts": chunk_count_by_source,
        "failed_case_ids": failed_case_ids,
    }
