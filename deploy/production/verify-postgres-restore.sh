#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 1 ]]; then
  echo "usage: $0 RELEASE_ID" >&2
  exit 2
fi
release_id=$1
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$ ]] || {
  echo "invalid RELEASE_ID" >&2
  exit 2
}

namespace=${WEKNORA_NAMESPACE:-weknora}
postgres_deployment=${WEKNORA_POSTGRES_DEPLOYMENT:-weknora-postgres}
backup_node=10.14.201.7
backup_root=/mnt/weknora-data/weknora-db-backups
cutoff_dir="${WEKNORA_CUTOFF_ROOT:-/root/weknora-release-prep}/$release_id"
archive="$backup_root/$release_id/WeKnora.dump"
restore_suffix=$(printf '%s' "$release_id" | sha256sum | cut -c1-12)
restore_db="weknora_restorecheck_$restore_suffix"
report="$cutoff_dir/postgres-restore-verification.txt"

for required in \
  "$cutoff_dir/knowledge-base-inventory.csv" \
  "$cutoff_dir/postgres-backup-sha256.txt" \
  "$cutoff_dir/postgres-restore-list.txt"; do
  [[ -s "$required" ]] || { echo "missing backup evidence: $required" >&2; exit 1; }
done
ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$backup_node" \
  sha256sum -c - <"$cutoff_dir/postgres-backup-sha256.txt" >/dev/null

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
if [[ -n "$active_mutators" ]]; then
  echo "refusing restore drill while state-changing deployments are active:" >&2
  printf '%s\n' "$active_mutators" >&2
  exit 1
fi

expected_counts=$(python3 - "$cutoff_dir/knowledge-base-inventory.csv" <<'PY'
import csv
import sys
with open(sys.argv[1], encoding="utf-8", newline="") as handle:
    rows = list(csv.DictReader(handle))
print(len(rows), sum(int(row["document_count"]) for row in rows))
PY
)
read -r expected_kb expected_docs <<<"$expected_counts"
source_schema=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -F "|" -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT version, dirty FROM schema_migrations"')

db_bytes=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT pg_database_size(current_database())"')
free_bytes=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'df -PB1 "$PGDATA" | awk "NR==2 {print \$4}"')
required_free=$((db_bytes * 3 + 2147483648))
if (( free_bytes < required_free )); then
  echo "restore drill needs at least 3x database size plus 2 GiB free: db=$db_bytes free=$free_bytes required=$required_free" >&2
  exit 1
fi

database_exists=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -U "$POSTGRES_USER" -d postgres -c "SELECT datname FROM pg_database"' \
  | awk -v name="$restore_db" '$0 == name {count++} END {print count + 0}')
[[ "$database_exists" == 0 ]] || {
  echo "refusing to reuse existing restore database: $restore_db" >&2
  exit 1
}

cleanup_required=false
cleanup() {
  if [[ "$cleanup_required" == true ]]; then
    kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
      sh -c 'dropdb -U "$POSTGRES_USER" --if-exists "$1"' sh "$restore_db" >/dev/null
  fi
}
trap cleanup EXIT

kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'createdb -U "$POSTGRES_USER" "$1"' sh "$restore_db"
cleanup_required=true
ssh -o BatchMode=yes "root@$backup_node" cat "$archive" \
  | kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
      sh -c 'exec pg_restore -U "$POSTGRES_USER" -d "$1" --no-owner --no-acl --exit-on-error' \
      sh "$restore_db"

restored=$(
  kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
    sh -c 'psql -X -q -At -F "|" -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d "$1"' \
    sh "$restore_db" <<'SQL'
SELECT version, dirty FROM schema_migrations;
SELECT COUNT(*) FROM knowledge_bases WHERE deleted_at IS NULL;
SELECT COUNT(*) FROM knowledges WHERE deleted_at IS NULL;
SQL
)
mapfile -t restored_lines <<<"$restored"
[[ ${#restored_lines[@]} -eq 3 ]] || { echo "unexpected restore verification output" >&2; exit 1; }
[[ "${restored_lines[0]}" == "$source_schema" ]] || {
  echo "restored schema is ${restored_lines[0]}, expected $source_schema" >&2
  exit 1
}
[[ "${restored_lines[1]}" == "$expected_kb" ]] || {
  echo "restored KB count is ${restored_lines[1]}, expected $expected_kb" >&2
  exit 1
}
[[ "${restored_lines[2]}" == "$expected_docs" ]] || {
  echo "restored document count is ${restored_lines[2]}, expected $expected_docs" >&2
  exit 1
}

cleanup
cleanup_required=false
trap - EXIT
remaining=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -U "$POSTGRES_USER" -d postgres -c "SELECT datname FROM pg_database"' \
  | awk -v name="$restore_db" '$0 == name {count++} END {print count + 0}')
[[ "$remaining" == 0 ]] || { echo "temporary restore database was not removed" >&2; exit 1; }
{
  printf 'release_id=%s\n' "$release_id"
  printf 'archive=%s:%s\n' "$backup_node" "$archive"
  printf 'database_bytes=%s\n' "$db_bytes"
  printf 'restore_database=%s (removed after verification)\n' "$restore_db"
  printf 'schema=%s\n' "${restored_lines[0]}"
  printf 'knowledge_bases=%s\n' "${restored_lines[1]}"
  printf 'documents=%s\n' "${restored_lines[2]}"
  printf 'result=PASS\n'
} >"$report"
echo "database backup restored and verified successfully: $report"
