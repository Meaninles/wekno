#!/usr/bin/env python3
"""Fail-closed production verification for the platform and KB rebuild phases."""

from __future__ import annotations

import argparse
import csv
import json
import re
import subprocess
import sys
from collections import Counter
from pathlib import Path


EXPECTED_PLACEMENT = {
    "app": ["10.14.201.1", "10.14.201.2", "10.14.201.7"],
    "parse-worker": ["10.14.201.1", "10.14.201.2", "10.14.201.7"],
    "docreader": ["10.14.201.1", "10.14.201.2", "10.14.201.7"],
    "derivative-worker": ["10.14.201.1", "10.14.201.7"],
    "wiki-worker": ["10.14.201.2", "10.14.201.7"],
    "maintenance": ["10.14.201.1", "10.14.201.2"],
    "general-agent": ["10.14.201.1", "10.14.201.2"],
    "document-processing-agent": ["10.14.201.1", "10.14.201.2"],
    "frontend": ["10.14.201.1", "10.14.201.2"],
    "mobile-web": ["10.14.201.1", "10.14.201.2"],
}
UUID_RE = re.compile(r"^[0-9a-fA-F-]{36}$")
CAPACITY_PLAN_PATH = Path(__file__).with_name("concurrency-plan.json")


class Verification:
    def __init__(self) -> None:
        self.errors: list[str] = []

    def require(self, condition: bool, message: str) -> None:
        if not condition:
            self.errors.append(message)

    def finish(self) -> int:
        if self.errors:
            for error in self.errors:
                print(f"ERROR: {error}")
            print(f"FAILED: {len(self.errors)} verification error(s)")
            return 1
        print("OK: every automated release verification passed")
        return 0


def run(command: list[str], *, stdin: str | None = None) -> str:
    result = subprocess.run(
        command,
        input=stdin,
        text=True,
        encoding="utf-8",
        errors="replace",
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        check=False,
    )
    if result.returncode:
        raise RuntimeError(
            f"command failed ({result.returncode}): {' '.join(command)}\n{result.stderr.strip()}"
        )
    return result.stdout


def kubectl(namespace: str, *arguments: str, stdin: str | None = None) -> str:
    return run(["kubectl", "-n", namespace, *arguments], stdin=stdin)


def psql(namespace: str, sql: str) -> list[list[str]]:
    output = kubectl(
        namespace,
        "exec",
        "-i",
        "deployment/weknora-postgres",
        "--",
        "sh",
        "-c",
        'psql -X -q -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" '
        '-d "$POSTGRES_DB" -At -F \'|\'',
        stdin=sql,
    )
    return [line.split("|") for line in output.splitlines() if line.strip()]


def read_csv(path: Path) -> list[dict[str, str]]:
    with path.open(encoding="utf-8", newline="") as handle:
        return list(csv.DictReader(handle))


def verify_pods(namespace: str, check: Verification) -> None:
    payload = json.loads(kubectl(namespace, "get", "pods", "-o", "json"))
    pods = [
        item
        for item in payload.get("items", [])
        if item.get("metadata", {}).get("deletionTimestamp") is None
    ]
    for component, expected_nodes in EXPECTED_PLACEMENT.items():
        selected = [
            pod
            for pod in pods
            if pod.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/component")
            == component
        ]
        actual_nodes = sorted(pod.get("spec", {}).get("nodeName", "") for pod in selected)
        check.require(
            actual_nodes == sorted(expected_nodes),
            f"{component} placement {actual_nodes}, expected {sorted(expected_nodes)}",
        )
        for pod in selected:
            name = pod["metadata"]["name"]
            conditions = {
                item.get("type"): item.get("status")
                for item in pod.get("status", {}).get("conditions", [])
            }
            check.require(conditions.get("Ready") == "True", f"pod {name} is not Ready")
            check.require(pod.get("status", {}).get("phase") == "Running", f"pod {name} is not Running")
            for container in pod.get("spec", {}).get("containers", []):
                image = container.get("image", "")
                check.require(
                    re.search(r"@sha256:[0-9a-f]{64}$", image) is not None,
                    f"pod {name} container {container.get('name')} is not digest-pinned: {image}",
                )
            for status in pod.get("status", {}).get("containerStatuses", []):
                check.require(
                    int(status.get("restartCount", 0)) == 0,
                    f"pod {name} container {status.get('name')} restarted",
                )

    for component in ("app", "parse-worker", "derivative-worker", "wiki-worker", "maintenance"):
        selected = [
            pod
            for pod in pods
            if pod.get("metadata", {}).get("labels", {}).get("app.kubernetes.io/component")
            == component
        ]
        for pod in selected:
            name = pod["metadata"]["name"]
            response = kubectl(
                namespace,
                "exec",
                name,
                "-c",
                "app",
                "--",
                "curl",
                "-fsS",
                "http://127.0.0.1:8080/ready",
            ).strip()
            check.require('"ready"' in response, f"runtime role {name} readiness response is {response!r}")


def verify_migration_job(namespace: str, release_id: str, check: Verification) -> None:
    jobs = json.loads(
        kubectl(
            namespace,
            "get",
            "jobs",
            "-l",
            "app.kubernetes.io/component=migration",
            "-o",
            "json",
        )
    ).get("items", [])
    release_key = release_id.lower().replace("_", "-").replace(".", "-")
    matches = [item for item in jobs if release_key in item["metadata"]["name"]]
    check.require(len(matches) == 1, f"expected one migration Job for {release_id}, found {len(matches)}")
    for job in matches:
        check.require(int(job.get("status", {}).get("succeeded", 0)) == 1, "migration Job did not succeed once")
        check.require(int(job.get("status", {}).get("failed", 0)) == 0, "migration Job has failed pods")


def verify_postgres(namespace: str, check: Verification) -> None:
    deployment = json.loads(kubectl(namespace, "get", "deployment", "weknora-postgres", "-o", "json"))
    container = next(
        item for item in deployment["spec"]["template"]["spec"]["containers"] if item["name"] == "postgres"
    )
    resources = container.get("resources", {})
    check.require(resources.get("requests", {}).get("cpu") == "3", "PostgreSQL CPU request is not 3")
    check.require(resources.get("requests", {}).get("memory") == "8Gi", "PostgreSQL memory request is not 8Gi")
    check.require(resources.get("limits", {}).get("cpu") == "6", "PostgreSQL CPU limit is not 6")
    check.require(resources.get("limits", {}).get("memory") == "12Gi", "PostgreSQL memory limit is not 12Gi")
    mounts = {item["name"]: item["mountPath"] for item in container.get("volumeMounts", [])}
    check.require(mounts.get("postgres-shm") == "/dev/shm", "PostgreSQL /dev/shm mount is missing")
    volumes = {item["name"]: item for item in deployment["spec"]["template"]["spec"].get("volumes", [])}
    shm = volumes.get("postgres-shm", {}).get("emptyDir", {})
    check.require(shm.get("medium") == "Memory", "PostgreSQL shared memory is not tmpfs")
    check.require(shm.get("sizeLimit") == "2Gi", "PostgreSQL shared memory limit is not 2Gi")
    df = kubectl(
        namespace,
        "exec",
        "deployment/weknora-postgres",
        "--",
        "sh",
        "-c",
        "df -Pm /dev/shm | tail -n 1",
    ).split()
    check.require(len(df) >= 2 and int(df[1]) >= 2048, f"PostgreSQL /dev/shm is smaller than 2Gi: {df}")
    recent_logs = kubectl(namespace, "logs", "deployment/weknora-postgres", "--since=30m")
    check.require(
        "could not resize shared memory segment" not in recent_logs,
        "PostgreSQL still reports shared-memory resize errors",
    )


def verify_database_platform(namespace: str, cutoff: Path, check: Verification) -> None:
    inventory = read_csv(cutoff / "knowledge-base-inventory.csv")
    expected_kb = len(inventory)
    expected_docs = sum(int(row["document_count"]) for row in inventory)
    expected_objects = sum(int(row["documents_with_source_object"]) for row in inventory)
    rows = psql(
        namespace,
        """
SELECT version, dirty FROM schema_migrations;
SELECT current_setting('max_connections'), COUNT(*) FROM pg_stat_activity;
SELECT
  COUNT(DISTINCT kb.id),
  COUNT(k.id),
  COUNT(k.id) FILTER (WHERE NULLIF(k.file_path, '') IS NOT NULL)
FROM knowledge_bases kb
LEFT JOIN knowledges k ON k.knowledge_base_id=kb.id AND k.deleted_at IS NULL
WHERE kb.deleted_at IS NULL;
""",
    )
    check.require(rows[0] == ["95", "f"], f"schema version/dirty is {rows[0]}, expected 95|f")
    max_connections, active_connections = map(int, rows[1])
    check.require(max_connections == 100, f"PostgreSQL max_connections is {max_connections}, expected 100")
    check.require(active_connections <= 60, f"active DB connections {active_connections} exceed 60")
    actual_kb, actual_docs, actual_objects = map(int, rows[2])
    check.require(actual_kb == expected_kb, f"live KB count {actual_kb}, expected {expected_kb}")
    check.require(actual_docs == expected_docs, f"live document count {actual_docs}, expected {expected_docs}")
    check.require(actual_objects == expected_objects, f"source-object count {actual_objects}, expected {expected_objects}")

    policies = psql(
        namespace,
        """
SELECT name, resource_kind,
       chat_max_concurrent, chat_max_waiting,
       im_max_concurrent, im_max_waiting,
       max_inflight, max_background_inflight, interactive_reserve,
       tenant_guaranteed, tenant_burst,
       document_guaranteed, document_burst,
       rpm, tpm, token_burst, request_timeout_seconds,
       circuit_threshold, circuit_window_seconds, circuit_open_seconds,
       state
FROM custom_model_resource_pools
ORDER BY id;
SELECT prefetch_factor, derivative_weight, wiki_weight,
       background_max_wait_seconds, dispatch_lease_seconds
FROM custom_model_scheduler_policies WHERE id=1;
""",
    )
    pool_columns = [
        "name",
        "resource_kind",
        "chat_max_concurrent",
        "chat_max_waiting",
        "im_max_concurrent",
        "im_max_waiting",
        "max_inflight",
        "max_background_inflight",
        "interactive_reserve",
        "tenant_guaranteed",
        "tenant_burst",
        "document_guaranteed",
        "document_burst",
        "rpm",
        "tpm",
        "token_burst",
        "request_timeout_seconds",
        "circuit_threshold",
        "circuit_window_seconds",
        "circuit_open_seconds",
        "state",
    ]
    pool_rows = [dict(zip(pool_columns, row)) for row in policies if len(row) == len(pool_columns)]
    scheduler = [row for row in policies if len(row) == 5]
    try:
        capacity_plan = json.loads(CAPACITY_PLAN_PATH.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        check.require(False, f"cannot load capacity plan {CAPACITY_PLAN_PATH}: {exc}")
        return

    database_fields = set(pool_columns) - {"name", "resource_kind"}
    for route in capacity_plan.get("model_routes", []):
        route_key = str(route.get("key", "<missing>"))
        try:
            name_pattern = re.compile(str(route["name_regex"]))
        except (KeyError, re.error) as exc:
            check.require(False, f"capacity route {route_key} has invalid name regex: {exc}")
            continue
        allowed_kinds = set(route.get("resource_kinds", []))
        matches = [
            row
            for row in pool_rows
            if row["resource_kind"] in allowed_kinds and name_pattern.search(row["name"])
        ]
        check.require(
            len(matches) == 1,
            f"capacity route {route_key} matched {len(matches)} database pools",
        )
        if len(matches) != 1:
            continue
        actual = matches[0]
        target = route.get("target", {})
        for field in sorted(database_fields & target.keys()):
            expected_value = "" if target[field] is None else str(target[field])
            check.require(
                actual[field] == expected_value,
                f"capacity route {route_key} field {field} is {actual[field]!r}, expected {expected_value!r}",
            )

    scheduler_target = capacity_plan.get("model_scheduler", {})
    expected_scheduler = [[
        str(scheduler_target.get("prefetch_factor")),
        str(scheduler_target.get("derivative_weight")),
        str(scheduler_target.get("wiki_weight")),
        str(scheduler_target.get("background_max_wait_seconds")),
        str(scheduler_target.get("dispatch_lease_seconds")),
    ]]
    check.require(scheduler == expected_scheduler, f"scheduler policy is {scheduler}, expected {expected_scheduler}")


def sql_literal(value: str) -> str:
    return "'" + value.replace("'", "''") + "'"


def verify_final_rebuild(namespace: str, cutoff: Path, check: Verification) -> None:
    tracker = read_csv(cutoff / "knowledge-base-rebuild-tracker.csv")
    check.require(bool(tracker), "knowledge-base rebuild tracker is empty")
    values: list[str] = []
    for row in tracker:
        source = row["source_knowledge_base_id"]
        target = row["target_knowledge_base_id"]
        action = row["release_action"]
        check.require(UUID_RE.fullmatch(source) is not None, f"invalid source KB id {source!r}")
        check.require(UUID_RE.fullmatch(target) is not None, f"missing/invalid target KB id for {source}")
        if action == "CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST":
            check.require(target != source, f"hybrid KB {source} must use a distinct target")
            for field in (
                "source_wiki_disabled", "source_seed_complete",
                "missing_sources_reingested", "folders_recreated",
                "external_references_rebound", "batch_reparse_submitted",
            ):
                check.require(row[field] == "YES", f"{source} tracker field {field} is not YES")
        elif action == "REBUILD_DOCUMENT_KB_IN_PLACE":
            check.require(target == source, f"in-place KB {source} target must equal source")
            check.require(row["batch_reparse_submitted"] == "YES", f"{source} reparse was not recorded")
        for field in ("all_documents_successful", "retrieval_verified", "owner_accepted"):
            check.require(row[field] == "YES", f"{source} tracker field {field} is not YES")
        values.append(
            "(" + ",".join(
                [
                    sql_literal(source),
                    sql_literal(target),
                    sql_literal(action),
                    str(int(row["document_count"])),
                    str(int(row["expected_disabled_documents"])),
                ]
            ) + ")"
        )

    query = f"""
WITH target(source_id,target_id,release_action,expected_docs,expected_disabled) AS (
  VALUES {','.join(values)}
), document_signature AS (
  SELECT
    k.knowledge_base_id,
    COALESCE(k.file_hash, '') AS file_hash,
    COALESCE(k.file_name, '') AS file_name,
    COALESCE(k.title, '') AS title,
    COALESCE(folder.path, '') AS folder_path,
    COALESCE(
      JSONB_AGG(
        JSONB_BUILD_ARRAY(tag.name, COALESCE(tag.color, ''), tag.sort_order)
        ORDER BY tag.sort_order, tag.name, tag.id
      ) FILTER (WHERE tag.id IS NOT NULL),
      '[]'::JSONB
    ) AS tags
  FROM knowledges k
  LEFT JOIN knowledge_tag_relations rel ON rel.knowledge_id=k.id
  LEFT JOIN knowledge_tags tag ON tag.id=rel.tag_id AND tag.deleted_at IS NULL
  LEFT JOIN custom_knowledge_folders folder
    ON folder.id=k.folder_id AND folder.knowledge_base_id=k.knowledge_base_id
  WHERE k.deleted_at IS NULL
    AND k.knowledge_base_id IN (
      SELECT source_id FROM target UNION SELECT target_id FROM target
    )
  GROUP BY k.id, k.knowledge_base_id, k.file_hash, k.file_name, k.title, folder.path
), tag_signature AS (
  SELECT knowledge_base_id, name, COALESCE(color, '') AS color, sort_order
  FROM knowledge_tags
  WHERE deleted_at IS NULL
    AND knowledge_base_id IN (
      SELECT source_id FROM target UNION SELECT target_id FROM target
    )
), folder_signature AS (
  SELECT knowledge_base_id, path, name, description, depth, sort_order
  FROM custom_knowledge_folders
  WHERE knowledge_base_id IN (
    SELECT source_id FROM target UNION SELECT target_id FROM target
  )
), result AS (
  SELECT
    t.source_id, t.target_id, t.release_action, t.expected_docs, t.expected_disabled,
    (src.id IS NOT NULL AND src.deleted_at IS NULL) AS source_live,
    LOWER(COALESCE(src.indexing_strategy->>'wiki_enabled','false')) = 'true' AS source_wiki_enabled,
    (dst.id IS NOT NULL AND dst.deleted_at IS NULL) AS target_live,
    dst.type,
    LOWER(COALESCE(dst.indexing_strategy->>'wiki_enabled','false')) = 'true' AS wiki_enabled,
    (SELECT COUNT(*) FROM wiki_pages wp WHERE wp.knowledge_base_id=dst.id AND wp.deleted_at IS NULL) AS wiki_pages,
    (SELECT COUNT(*) FROM knowledges k WHERE k.knowledge_base_id=dst.id AND k.deleted_at IS NULL) AS documents,
    (SELECT COUNT(*) FROM knowledges k WHERE k.knowledge_base_id=dst.id AND k.deleted_at IS NULL AND LOWER(COALESCE(k.enable_status,''))='disabled') AS disabled_documents,
    (SELECT COUNT(*) FROM knowledges k
       WHERE k.knowledge_base_id=dst.id AND k.deleted_at IS NULL
         AND NOT (
           LOWER(COALESCE(k.parse_status,''))='completed'
           AND LOWER(COALESCE(k.summary_status,'none')) IN ('none','completed')
           AND LOWER(COALESCE(k.enrichment_status,'none')) IN ('none','completed')
           AND COALESCE(k.pending_subtasks_count,0)=0
           AND (
             (LOWER(COALESCE(dst.indexing_strategy->>'wiki_enabled','false')) <> 'true'
               AND LOWER(COALESCE(k.wiki_status,'none')) IN ('none','completed'))
             OR (LOWER(COALESCE(dst.indexing_strategy->>'wiki_enabled','false')) = 'true'
               AND LOWER(COALESCE(k.wiki_status,'none'))='completed')
           )
         )) AS incomplete_documents,
    (SELECT COUNT(*) FROM custom_document_queue_workflows w
       WHERE w.knowledge_base_id=dst.id
         AND w.state NOT IN ('completed','failed','cancelled','superseded')) AS nonterminal_workflows
    ,(
      src.description IS NOT DISTINCT FROM dst.description
      AND src.embedding_model_id IS NOT DISTINCT FROM dst.embedding_model_id
      AND src.summary_model_id IS NOT DISTINCT FROM dst.summary_model_id
      AND src.derivative_model_id IS NOT DISTINCT FROM dst.derivative_model_id
      AND src.vector_store_id IS NOT DISTINCT FROM dst.vector_store_id
      AND src.chunking_config::JSONB IS NOT DISTINCT FROM dst.chunking_config::JSONB
      AND src.image_processing_config::JSONB IS NOT DISTINCT FROM dst.image_processing_config::JSONB
      AND src.vlm_config::JSONB IS NOT DISTINCT FROM dst.vlm_config::JSONB
      AND src.asr_config::JSONB IS NOT DISTINCT FROM dst.asr_config::JSONB
      AND src.storage_provider_config::JSONB IS NOT DISTINCT FROM dst.storage_provider_config::JSONB
      AND src.cos_config::JSONB IS NOT DISTINCT FROM dst.cos_config::JSONB
      AND src.extract_config::JSONB IS NOT DISTINCT FROM dst.extract_config::JSONB
      AND src.faq_config::JSONB IS NOT DISTINCT FROM dst.faq_config::JSONB
      AND src.question_generation_config::JSONB IS NOT DISTINCT FROM dst.question_generation_config::JSONB
      AND (COALESCE(src.indexing_strategy::JSONB, '{{}}'::JSONB) - 'wiki_enabled')
          IS NOT DISTINCT FROM
          (COALESCE(dst.indexing_strategy::JSONB, '{{}}'::JSONB) - 'wiki_enabled')
    ) AS configuration_match,
    NOT EXISTS (
      (
        SELECT name, color, sort_order FROM tag_signature WHERE knowledge_base_id=t.source_id
        EXCEPT ALL
        SELECT name, color, sort_order FROM tag_signature WHERE knowledge_base_id=t.target_id
      )
      UNION ALL
      (
        SELECT name, color, sort_order FROM tag_signature WHERE knowledge_base_id=t.target_id
        EXCEPT ALL
        SELECT name, color, sort_order FROM tag_signature WHERE knowledge_base_id=t.source_id
      )
    ) AS tag_definitions_match,
    NOT EXISTS (
      (
        SELECT path, name, description, depth, sort_order
        FROM folder_signature WHERE knowledge_base_id=t.source_id
        EXCEPT ALL
        SELECT path, name, description, depth, sort_order
        FROM folder_signature WHERE knowledge_base_id=t.target_id
      )
      UNION ALL
      (
        SELECT path, name, description, depth, sort_order
        FROM folder_signature WHERE knowledge_base_id=t.target_id
        EXCEPT ALL
        SELECT path, name, description, depth, sort_order
        FROM folder_signature WHERE knowledge_base_id=t.source_id
      )
    ) AS folder_structure_match,
    NOT EXISTS (
      (
        SELECT file_hash, file_name, title, folder_path, tags
        FROM document_signature WHERE knowledge_base_id=t.source_id
        EXCEPT ALL
        SELECT file_hash, file_name, title, folder_path, tags
        FROM document_signature WHERE knowledge_base_id=t.target_id
      )
      UNION ALL
      (
        SELECT file_hash, file_name, title, folder_path, tags
        FROM document_signature WHERE knowledge_base_id=t.target_id
        EXCEPT ALL
        SELECT file_hash, file_name, title, folder_path, tags
        FROM document_signature WHERE knowledge_base_id=t.source_id
      )
    ) AS document_tags_match
  FROM target t
  LEFT JOIN knowledge_bases src ON src.id=t.source_id
  LEFT JOIN knowledge_bases dst ON dst.id=t.target_id
)
SELECT source_id,target_id,release_action,expected_docs,expected_disabled,
       source_live,source_wiki_enabled,target_live,type,wiki_enabled,wiki_pages,documents,
       disabled_documents,incomplete_documents,nonterminal_workflows,
       configuration_match,tag_definitions_match,folder_structure_match,document_tags_match
FROM result ORDER BY source_id;
"""
    rows = psql(namespace, query)
    check.require(len(rows) == len(tracker), f"target verification returned {len(rows)} rows for {len(tracker)} KBs")
    for row in rows:
        (
            source, target, action, expected_docs, expected_disabled,
            source_live, source_wiki_enabled, target_live, kb_type, wiki_enabled, wiki_pages,
            documents, disabled_documents, incomplete_documents, nonterminal,
            configuration_match, tag_definitions_match, folder_structure_match,
            document_tags_match,
        ) = row
        check.require(source_live == "t", f"source KB {source} is no longer live; rollback copy was lost")
        check.require(target_live == "t", f"target KB {target} is not live")
        check.require(kb_type == "document", f"target KB {target} type is {kb_type}")
        check.require(int(documents) == int(expected_docs), f"target KB {target} has {documents}/{expected_docs} documents")
        check.require(int(disabled_documents) == int(expected_disabled), f"target KB {target} disabled count is {disabled_documents}/{expected_disabled}")
        check.require(int(incomplete_documents) == 0, f"target KB {target} has {incomplete_documents} incomplete documents")
        check.require(int(nonterminal) == 0, f"target KB {target} has {nonterminal} nonterminal workflows")
        check.require(configuration_match == "t", f"target KB {target} lost non-Wiki source configuration")
        check.require(tag_definitions_match == "t", f"target KB {target} tag definitions differ from source")
        check.require(folder_structure_match == "t", f"target KB {target} folder hierarchy differs from source")
        check.require(document_tags_match == "t", f"target KB {target} document tags/folders differ from source")
        if action in ("CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST", "REBUILD_DOCUMENT_KB_IN_PLACE"):
            check.require(wiki_enabled == "f", f"rebuilt KB {target} still has Wiki enabled")
            check.require(int(wiki_pages) == 0, f"rebuilt KB {target} has {wiki_pages} Wiki pages")
        if action == "CREATE_NON_WIKI_REPLACEMENT_AND_REINGEST":
            check.require(source_wiki_enabled == "f", f"source hybrid KB {source} still has Wiki enabled")

    document_cutoff = read_csv(cutoff / "knowledge-rebuild-documents.csv")
    ids = [row["source_knowledge_id"] for row in document_cutoff]
    for knowledge_id in ids:
        check.require(UUID_RE.fullmatch(knowledge_id) is not None, f"invalid source document id {knowledge_id!r}")
    source_rows = psql(
        namespace,
        "SELECT id, file_hash, file_path, (deleted_at IS NULL)::text "
        f"FROM knowledges WHERE id IN ({','.join(sql_literal(item) for item in ids)});",
    )
    current = {row[0]: row[1:] for row in source_rows}
    for row in document_cutoff:
        knowledge_id = row["source_knowledge_id"]
        actual = current.get(knowledge_id)
        check.require(actual is not None, f"source document {knowledge_id} disappeared")
        if actual is None:
            continue
        check.require(actual[0] == row["file_hash"], f"source document {knowledge_id} hash changed")
        check.require(actual[1] == row["source_object_path"], f"source document {knowledge_id} object path changed")
        check.require(actual[2] == "true", f"source document {knowledge_id} was soft-deleted")

    reference_cutoff = read_csv(cutoff / "knowledge-base-reference-inventory.csv")
    if reference_cutoff:
        target_by_source = {
            row["source_knowledge_base_id"]: row["target_knowledge_base_id"]
            for row in tracker
        }
        reference_kb_ids = sorted({
            kb_id
            for row in reference_cutoff
            for kb_id in (
                row["source_knowledge_base_id"],
                target_by_source[row["source_knowledge_base_id"]],
            )
        })
        kb_sql = ",".join(sql_literal(item) for item in reference_kb_ids)
        actual_reference_rows = psql(
            namespace,
            f"""
WITH records AS (
  SELECT 'CUSTOM_AGENT'::text kind, x.kb_id, a.id reference_id,
         ''::text reference_sub_id, ''::text reference_state
  FROM custom_agents a
  CROSS JOIN LATERAL JSONB_ARRAY_ELEMENTS_TEXT(
    CASE WHEN JSONB_TYPEOF(a.config->'knowledge_bases')='array'
         THEN a.config->'knowledge_bases' ELSE '[]'::JSONB END
  ) x(kb_id)
  WHERE a.deleted_at IS NULL AND x.kb_id IN ({kb_sql})
  UNION ALL
  SELECT 'SCHEDULED_CHAT', x.kb_id, t.id, '', ''
  FROM custom_scheduled_chat_tasks t
  CROSS JOIN LATERAL JSONB_ARRAY_ELEMENTS_TEXT(
    CASE WHEN JSONB_TYPEOF(t.request_context->'knowledge_base_ids')='array'
         THEN t.request_context->'knowledge_base_ids' ELSE '[]'::JSONB END
  ) x(kb_id)
  WHERE t.deleted_at IS NULL AND x.kb_id IN ({kb_sql})
  UNION ALL
  SELECT 'KB_SHARE', s.knowledge_base_id, s.id, s.organization_id, s.permission
  FROM kb_shares s WHERE s.deleted_at IS NULL AND s.knowledge_base_id IN ({kb_sql})
  UNION ALL
  SELECT 'USER_PIN', p.kb_id, p.user_id, '', ''
  FROM user_kb_pins p WHERE p.kb_id IN ({kb_sql})
  UNION ALL
  SELECT 'DATA_SOURCE', d.knowledge_base_id, d.id, d.type, ''
  FROM data_sources d WHERE d.deleted_at IS NULL AND d.knowledge_base_id IN ({kb_sql})
  UNION ALL
  SELECT 'IM_CHANNEL', c.knowledge_base_id, c.id, c.platform, ''
  FROM im_channels c WHERE c.deleted_at IS NULL AND c.knowledge_base_id IN ({kb_sql})
  UNION ALL
  SELECT 'SESSION', s.knowledge_base_id, s.id, '', ''
  FROM sessions s WHERE s.deleted_at IS NULL AND s.knowledge_base_id IN ({kb_sql})
)
SELECT kind,kb_id,reference_id,reference_sub_id,reference_state
FROM records ORDER BY kind,kb_id,reference_id,reference_sub_id;
""",
        )
        actual_references = {tuple(row) for row in actual_reference_rows}
        for row in reference_cutoff:
            kind = row["reference_type"]
            source = row["source_knowledge_base_id"]
            target = target_by_source[source]
            reference_id = row["reference_id"]
            reference_sub_id = row["reference_sub_id"]
            expected_state = ""
            if kind == "KB_SHARE":
                expected_state = row["required_action"].partition(":")[2]
                expected = (kind, target, "", reference_sub_id, expected_state)
                matched = any(
                    item[0] == expected[0]
                    and item[1] == expected[1]
                    and item[3] == expected[3]
                    and item[4] == expected[4]
                    for item in actual_references
                )
            else:
                expected = (kind, target, reference_id, reference_sub_id, expected_state)
                matched = expected in actual_references
            check.require(
                matched,
                f"{kind} reference {reference_id} was not rebound/duplicated to target KB {target}",
            )
            if kind in {"CUSTOM_AGENT", "SCHEDULED_CHAT", "DATA_SOURCE", "IM_CHANNEL", "SESSION"}:
                stale = any(
                    item[0] == kind and item[1] == source and item[2] == reference_id
                    for item in actual_references
                )
                check.require(not stale, f"{kind} reference {reference_id} still points to source KB {source}")

    manual_recovery = read_csv(cutoff / "task-recovery-manual-tracker.csv")
    allowed_resolution = {
        "REISSUED_SUCCESS",
        "NO_LIVE_TARGET_CONFIRMED",
        "ALREADY_SUCCESS_CONFIRMED",
    }
    for row in manual_recovery:
        resolution = row["resolution_status"]
        record = f"{row['record_type']}:{row['record_id']}"
        check.require(
            resolution in allowed_resolution,
            f"task recovery {record} has unresolved status {resolution!r}",
        )
        if resolution == "REISSUED_SUCCESS":
            check.require(
                bool(row["replacement_task_id"].strip()),
                f"task recovery {record} has no replacement_task_id",
            )

    live_nonterminal = psql(
        namespace,
        """
SELECT COUNT(*)
FROM custom_document_queue_workflows w
JOIN knowledges k ON k.id=w.knowledge_id AND k.deleted_at IS NULL
JOIN knowledge_bases kb ON kb.id=k.knowledge_base_id AND kb.deleted_at IS NULL
WHERE w.state NOT IN ('completed','failed','cancelled','superseded');
""",
    )
    check.require(
        live_nonterminal == [["0"]],
        f"live knowledge bases still have nonterminal workflows: {live_nonterminal}",
    )


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("phase", choices=("platform", "final"))
    parser.add_argument("release_id")
    parser.add_argument("cutoff_dir", type=Path)
    parser.add_argument("--namespace", default="weknora")
    args = parser.parse_args()
    cutoff = args.cutoff_dir.resolve()
    required = [
        cutoff / "knowledge-base-inventory.csv",
        cutoff / "knowledge-rebuild-documents.csv",
        cutoff / "knowledge-folder-inventory.csv",
        cutoff / "knowledge-base-reference-inventory.csv",
        cutoff / "knowledge-base-rebuild-tracker.csv",
        cutoff / "task-recovery-manual-tracker.csv",
    ]
    missing = [str(path) for path in required if not path.is_file()]
    if missing:
        print("missing cutoff files: " + ", ".join(missing), file=sys.stderr)
        return 2

    check = Verification()
    try:
        verify_pods(args.namespace, check)
        verify_migration_job(args.namespace, args.release_id, check)
        verify_postgres(args.namespace, check)
        if args.phase == "platform":
            verify_database_platform(args.namespace, cutoff, check)
        else:
            verify_final_rebuild(args.namespace, cutoff, check)
    except (RuntimeError, KeyError, ValueError, StopIteration, json.JSONDecodeError) as exc:
        check.errors.append(str(exc))
    return check.finish()


if __name__ == "__main__":
    raise SystemExit(main())
