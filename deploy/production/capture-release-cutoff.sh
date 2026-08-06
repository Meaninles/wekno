#!/usr/bin/env bash
set -euo pipefail
umask 077

usage() {
  cat >&2 <<'EOF'
usage: capture-release-cutoff.sh RELEASE_ID [--allow-live] [--output-root ABSOLUTE_PATH]

Without --allow-live the command fails unless every state-changing WeKnora
Deployment is already scaled to zero. Use --allow-live only for a preliminary
baseline; the migration cutoff must always use the default fail-closed mode.
EOF
}

[[ $# -ge 1 ]] || { usage; exit 2; }
release_id=$1
shift
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$ ]] || {
  echo "invalid RELEASE_ID" >&2
  exit 2
}

allow_live=false
output_root=/root/weknora-release-prep
while [[ $# -gt 0 ]]; do
  case "$1" in
    --allow-live)
      allow_live=true
      shift
      ;;
    --output-root)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      output_root=$2
      shift 2
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done
[[ "$output_root" == /* ]] || { echo "output root must be absolute" >&2; exit 2; }

namespace=${WEKNORA_NAMESPACE:-weknora}
postgres_deployment=${WEKNORA_POSTGRES_DEPLOYMENT:-weknora-postgres}
script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
output_dir="$output_root/$release_id"

for command_name in kubectl python3 sha256sum; do
  command -v "$command_name" >/dev/null || {
    echo "missing required command: $command_name" >&2
    exit 1
  }
done

if [[ -d "$output_dir" ]] && find "$output_dir" -mindepth 1 -print -quit | grep -q .; then
  echo "refusing to overwrite non-empty cutoff directory: $output_dir" >&2
  exit 1
fi
install -d -o root -g root -m 0700 "$output_dir"

kubectl config current-context >"$output_dir/kubernetes-context.txt"
kubectl get namespace "$namespace" -o name >"$output_dir/namespace.txt"

active_mutators=$(
  kubectl -n "$namespace" get deployment -o json | python3 -c '
import json, sys
data = json.load(sys.stdin)
exact = {
    "weknora-app", "weknora-docreader",
    "weknora-general-agent", "weknora-document-processing-agent",
    "weknora-custom-general-agent",
    "weknora-custom-document-processing-agent",
}
for item in data.get("items", []):
    name = item["metadata"]["name"]
    replicas = int(item.get("spec", {}).get("replicas") or 0)
    if replicas and (name in exact or name.startswith("weknora-app-")):
        print(f"{name}={replicas}")
'
)
if [[ -n "$active_mutators" && "$allow_live" != true ]]; then
  echo "state-changing deployments are still active:" >&2
  printf '%s\n' "$active_mutators" >&2
  echo "scale them to zero and wait for pod termination before capturing the cutoff" >&2
  exit 1
fi
printf '%s\n' "${active_mutators:-none}" >"$output_dir/active-mutators.txt"

kubectl get nodes -o wide >"$output_dir/nodes.txt"
kubectl -n "$namespace" get deployment,pod,service,ingress,pvc -o wide \
  >"$output_dir/workloads-before.txt"
kubectl -n "$namespace" get deployment,service,ingress,configmap,pvc -o yaml \
  >"$output_dir/rollback-manifests.yaml"
kubectl get pv -o yaml >"$output_dir/persistent-volumes.yaml"
kubectl -n "$namespace" get pod \
  -o custom-columns='NAME:.metadata.name,IMAGE:.spec.containers[*].image,NODE:.spec.nodeName,RESTARTS:.status.containerStatuses[*].restartCount' \
  >"$output_dir/pod-images-before.txt"
kubectl -n "$namespace" get secret \
  -o custom-columns='NAME:.metadata.name,TYPE:.type,RESOURCE_VERSION:.metadata.resourceVersion' \
  >"$output_dir/secret-metadata.txt"

kubectl -n "$namespace" get secret \
  weknora-secrets weknora-obs weknora-agent-internal weknora-model-runtime default-secret \
  --ignore-not-found -o yaml >"$output_dir/protected-secrets.yaml"
chmod 0600 "$output_dir/protected-secrets.yaml"

redis_endpoint=$(
  kubectl -n "$namespace" get endpoints redis \
    -o jsonpath='{.subsets[0].addresses[0].ip}:{.subsets[0].ports[0].port}'
)
redis_secret=$(kubectl -n "$namespace" get secret weknora-secrets -o json)
export REDIS_ADDR=$redis_endpoint
export REDIS_USERNAME=$(
  python3 -c 'import base64,json,sys; d=json.load(sys.stdin).get("data",{}); print(base64.b64decode(d.get("REDIS_USERNAME","")).decode())' \
    <<<"$redis_secret"
)
export REDIS_PASSWORD=$(
  python3 -c 'import base64,json,sys; d=json.load(sys.stdin).get("data",{}); print(base64.b64decode(d.get("REDIS_PASSWORD","")).decode())' \
    <<<"$redis_secret"
)
export REDIS_DB=0
python3 "$script_dir/capture-asynq-state.py" \
  >"$output_dir/redis-asynq-key-inventory.csv"
unset REDIS_ADDR REDIS_USERNAME REDIS_PASSWORD REDIS_DB redis_secret redis_endpoint

repo_root=$(cd -- "$script_dir/../.." && pwd)
if git -C "$repo_root" rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  git -C "$repo_root" rev-parse HEAD >"$output_dir/git-head.txt"
  git -C "$repo_root" status --short >"$output_dir/git-status.txt"
elif [[ "${WEKNORA_SOURCE_GIT_HEAD:-}" =~ ^[0-9a-f]{40}$ ]]; then
  printf '%s\n' "$WEKNORA_SOURCE_GIT_HEAD" >"$output_dir/git-head.txt"
  printf '%s\n' \
    'release bundle has no .git directory; source head supplied by the operator' \
    >"$output_dir/git-status.txt"
else
  echo \
    "release bundle is not a Git worktree and WEKNORA_SOURCE_GIT_HEAD is missing/invalid" \
    >&2
  exit 1
fi
(
  cd "$repo_root"
  find deploy/production helm -type f -print0 \
    | sort -z \
    | xargs -0 sha256sum
) >"$output_dir/release-tool-inputs.sha256"

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -P pager=off' \
  <<'SQL' >"$output_dir/database-facts.txt"
SELECT current_database() AS database_name, current_user AS database_user;
SELECT version, dirty FROM schema_migrations;
SHOW max_connections;
SELECT pg_size_pretty(pg_database_size(current_database())) AS database_size;
SELECT COUNT(*) AS active_connections FROM pg_stat_activity;
SELECT COUNT(*) AS live_knowledge_bases FROM knowledge_bases WHERE deleted_at IS NULL;
SELECT COUNT(*) AS live_documents FROM knowledges WHERE deleted_at IS NULL;
SELECT
    CASE
        WHEN file_path ~ '^[A-Za-z][A-Za-z0-9+.-]*://'
            THEN substring(file_path from '^([A-Za-z][A-Za-z0-9+.-]*)://')
        WHEN COALESCE(file_path, '') = '' THEN '<empty>'
        ELSE '<path>'
    END AS source_scheme,
    COUNT(*)
FROM knowledges
WHERE deleted_at IS NULL
GROUP BY 1
ORDER BY 2 DESC;
SQL

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q --csv -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$script_dir/sql/knowledge-base-inventory.sql" \
  >"$output_dir/knowledge-base-inventory.csv"

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q --csv -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$script_dir/sql/task-recovery-inventory.sql" \
  >"$output_dir/task-recovery-ledger.csv"

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q --csv -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$script_dir/sql/knowledge-rebuild-documents.sql" \
  >"$output_dir/knowledge-rebuild-documents.csv"

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q --csv -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$script_dir/sql/knowledge-folder-inventory.sql" \
  >"$output_dir/knowledge-folder-inventory.csv"

kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q --csv -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$POSTGRES_DB"' \
  <"$script_dir/sql/knowledge-base-reference-inventory.sql" \
  >"$output_dir/knowledge-base-reference-inventory.csv"

python3 - "$output_dir" <<'PY'
import collections
import csv
import pathlib
import shutil
import sys

root = pathlib.Path(sys.argv[1])
with (root / "knowledge-base-inventory.csv").open(encoding="utf-8", newline="") as handle:
    rows = list(csv.DictReader(handle))
with (root / "knowledge-folder-inventory.csv").open(encoding="utf-8", newline="") as handle:
    folder_counts = collections.Counter(
        row["knowledge_base_id"] for row in csv.DictReader(handle)
    )
with (root / "knowledge-base-reference-inventory.csv").open(encoding="utf-8", newline="") as handle:
    reference_counts = collections.Counter(
        row["source_knowledge_base_id"] for row in csv.DictReader(handle)
    )
actions = collections.Counter(row["release_action"] for row in rows)
with (root / "knowledge-base-summary.txt").open("w", encoding="utf-8") as handle:
    handle.write(f"knowledge_bases={len(rows)}\n")
    handle.write(f"hybrid={sum(row['is_hybrid'] == 't' for row in rows)}\n")
    handle.write(f"documents={sum(int(row['document_count']) for row in rows)}\n")
    handle.write(f"noncomplete={sum(int(row['noncomplete_documents']) for row in rows)}\n")
    for action, count in sorted(actions.items()):
        documents = sum(
            int(row["document_count"])
            for row in rows
            if row["release_action"] == action
        )
        handle.write(f"action.{action}.knowledge_bases={count}\n")
        handle.write(f"action.{action}.documents={documents}\n")

tracker_fields = [
    "tenant_id", "source_knowledge_base_id", "source_knowledge_base_name",
    "release_action", "is_hybrid", "document_count",
    "fully_complete_documents", "noncomplete_documents",
    "expected_disabled_documents", "expected_folder_count",
    "expected_external_reference_count",
    "target_knowledge_base_id", "target_knowledge_base_name",
    "clone_task_id", "source_wiki_disabled", "source_seed_complete", "missing_sources_reingested",
    "folders_recreated", "external_references_rebound",
    "batch_reparse_submitted", "all_documents_successful",
    "retrieval_verified", "owner_accepted", "old_kb_disposition", "notes",
]
with (root / "knowledge-base-rebuild-tracker.initial.csv").open("w", encoding="utf-8", newline="") as handle:
    writer = csv.DictWriter(handle, fieldnames=tracker_fields)
    writer.writeheader()
    for row in rows:
        keep = row["release_action"] in {"KEEP_ALL_COMPLETE", "KEEP_EMPTY"}
        in_place = row["release_action"] == "REBUILD_DOCUMENT_KB_IN_PLACE"
        writer.writerow({
            "tenant_id": row["tenant_id"],
            "source_knowledge_base_id": row["knowledge_base_id"],
            "source_knowledge_base_name": row["knowledge_base_name"],
            "release_action": row["release_action"],
            "is_hybrid": row["is_hybrid"],
            "document_count": row["document_count"],
            "fully_complete_documents": row["fully_complete_documents"],
            "noncomplete_documents": row["noncomplete_documents"],
            # A disabled row in this schema is a non-retrievable processing
            # state, not a user preference. Rebuilt documents must converge
            # to enabled; only untouched KEEP_* knowledge bases preserve the
            # exact cutoff count.
            "expected_disabled_documents": row["disabled_documents"] if keep else "0",
            "expected_folder_count": folder_counts[row["knowledge_base_id"]],
            "expected_external_reference_count": reference_counts[row["knowledge_base_id"]],
            "target_knowledge_base_id": row["knowledge_base_id"] if (keep or in_place) else "",
            "target_knowledge_base_name": row["knowledge_base_name"] if (keep or in_place) else row["replacement_name"],
            "source_wiki_disabled": "N/A" if (keep or in_place) else "NO",
            "source_seed_complete": "N/A" if (keep or in_place) else "NO",
            "missing_sources_reingested": "N/A" if (keep or in_place) else "NO",
            "folders_recreated": "N/A" if (keep or in_place) else "NO",
            "external_references_rebound": "N/A" if (keep or in_place) else "NO",
            "batch_reparse_submitted": "N/A" if keep else "NO",
            "all_documents_successful": "YES" if keep else "NO",
            "retrieval_verified": "NO",
            "owner_accepted": "NO",
            "old_kb_disposition": "KEEP" if keep else "KEEP_FOR_ROLLBACK",
        })
shutil.copyfile(
    root / "knowledge-base-rebuild-tracker.initial.csv",
    root / "knowledge-base-rebuild-tracker.csv",
)

with (root / "task-recovery-ledger.csv").open(encoding="utf-8", newline="") as handle:
    task_rows = list(csv.DictReader(handle))
recovery = collections.Counter(row["recovery_action"] for row in task_rows)
with (root / "task-recovery-summary.txt").open("w", encoding="utf-8") as handle:
    handle.write(f"records={len(task_rows)}\n")
    for action, count in sorted(recovery.items()):
        handle.write(f"action.{action}={count}\n")

manual_actions = {
    "MANUAL_REPARSE_LIVE_DOCUMENT",
    "MANUAL_REVIEW_DO_NOT_RAW_REPLAY",
}
manual_fields = list(task_rows[0].keys()) if task_rows else [
    "record_type", "record_id", "task_type", "scope", "scope_id",
    "knowledge_id", "knowledge_base_id", "knowledge_base_action",
    "source_state", "attempt_count", "event_at", "generation_relation",
    "recovery_action",
]
manual_fields += ["resolution_status", "replacement_task_id", "verified_at", "notes"]
with (root / "task-recovery-manual-tracker.initial.csv").open(
    "w", encoding="utf-8", newline=""
) as handle:
    writer = csv.DictWriter(handle, fieldnames=manual_fields)
    writer.writeheader()
    for row in task_rows:
        if row["recovery_action"] not in manual_actions:
            continue
        writer.writerow({**row, "resolution_status": "PENDING"})
shutil.copyfile(
    root / "task-recovery-manual-tracker.initial.csv",
    root / "task-recovery-manual-tracker.csv",
)
PY

(
  cd "$output_dir"
  find . -type f ! -name SHA256SUMS \
    ! -name knowledge-base-rebuild-tracker.csv \
    ! -name task-recovery-manual-tracker.csv -print0 \
    | sort -z \
    | xargs -0 sha256sum >SHA256SUMS
)
chmod -R go-rwx "$output_dir"
echo "release cutoff captured: $output_dir"
echo "verify with: (cd '$output_dir' && sha256sum -c SHA256SUMS)"
