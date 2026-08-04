#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 2 || "${2:-}" != "--confirm-stop-the-world-restore" ]]; then
  echo "usage: $0 RELEASE_ID --confirm-stop-the-world-restore" >&2
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
suffix=$(printf '%s' "$release_id" | sha256sum | cut -c1-12)
held_db="weknora_postrelease_$suffix"
report="$cutoff_dir/postgres-rollback-restore.txt"

for required in \
  "$cutoff_dir/knowledge-base-inventory.csv" \
  "$cutoff_dir/postgres-backup-sha256.txt" \
  "$cutoff_dir/postgres-restore-verification.txt"; do
  [[ -s "$required" ]] || { echo "missing rollback evidence: $required" >&2; exit 1; }
done
ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$backup_node" \
  sha256sum -c - <"$cutoff_dir/postgres-backup-sha256.txt" >/dev/null

# No request handler, worker, parser or Agent may reconnect while the database
# name is swapped. The public Ingress must also remain absent.
active_mutators=$(
  kubectl -n "$namespace" get deployment -o json | python3 -c '
import json, sys
data = json.load(sys.stdin)
exact = {
    "weknora-app", "weknora-docreader",
    "weknora-general-agent", "weknora-document-processing-agent",
    "weknora-custom-general-agent", "weknora-custom-document-processing-agent",
}
for item in data.get("items", []):
    name = item["metadata"]["name"]
    replicas = int(item.get("spec", {}).get("replicas") or 0)
    if replicas and (name in exact or name.startswith("weknora-app-")):
        print(f"{name}={replicas}")
'
)
if [[ -n "$active_mutators" ]]; then
  echo "refusing PostgreSQL restore while state-changing deployments are active:" >&2
  printf '%s\n' "$active_mutators" >&2
  exit 1
fi
if kubectl -n "$namespace" get ingress weknora >/dev/null 2>&1; then
  echo "refusing PostgreSQL restore while ingress/weknora still exists" >&2
  exit 1
fi

database_name=$(kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'printf %s "$POSTGRES_DB"')
[[ "$database_name" == "WeKnora" ]] || {
  echo "refusing unexpected database target: $database_name" >&2
  exit 1
}
held_exists=$(kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres' <<SQL
SELECT COUNT(*) FROM pg_database WHERE datname = '$held_db';
SQL
)
[[ "$held_exists" == 0 ]] || {
  echo "refusing to overwrite retained database: $held_db" >&2
  exit 1
}

expected_counts=$(python3 - "$cutoff_dir/knowledge-base-inventory.csv" <<'PY'
import csv
import sys
with open(sys.argv[1], encoding="utf-8", newline="") as handle:
    rows = list(csv.DictReader(handle))
print(len(rows), sum(int(row["document_count"]) for row in rows))
PY
)
read -r expected_kb expected_docs <<<"$expected_counts"
expected_schema=$(awk -F= '$1 == "schema" {sub(/^schema=/, ""); print; exit}' \
  "$cutoff_dir/postgres-restore-verification.txt")
[[ -n "$expected_schema" ]] || { echo "restore drill report has no schema" >&2; exit 1; }

rollback_required=false
rollback_swap() {
  if [[ "$rollback_required" == true ]]; then
    echo "restore failed; restoring the retained post-release database name" >&2
    kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- sh -s -- "$held_db" <<'REMOTE'
set -euo pipefail
held_db=$1
held_exists=$(psql -X -q -At -U "$POSTGRES_USER" -d postgres \
  -c "SELECT COUNT(*) FROM pg_database WHERE datname='$held_db'")
if [[ "$held_exists" == 1 ]]; then
  current_exists=$(psql -X -q -At -U "$POSTGRES_USER" -d postgres \
    -c "SELECT COUNT(*) FROM pg_database WHERE datname='WeKnora'")
  if [[ "$current_exists" == 1 ]]; then
    psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres <<SQL
ALTER DATABASE "WeKnora" WITH ALLOW_CONNECTIONS false;
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname = 'WeKnora' AND pid <> pg_backend_pid();
DROP DATABASE "WeKnora";
SQL
  fi
  psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres <<SQL
ALTER DATABASE "$held_db" RENAME TO "WeKnora";
ALTER DATABASE "WeKnora" WITH ALLOW_CONNECTIONS true;
SQL
fi
REMOTE
  fi
}
trap rollback_swap EXIT

rollback_required=true
kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- sh -s -- "$held_db" <<'REMOTE'
set -euo pipefail
held_db=$1
psql -X -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d postgres <<SQL
ALTER DATABASE "WeKnora" WITH ALLOW_CONNECTIONS false;
SELECT pg_terminate_backend(pid) FROM pg_stat_activity
 WHERE datname = 'WeKnora' AND pid <> pg_backend_pid();
ALTER DATABASE "WeKnora" RENAME TO "$held_db";
SQL
createdb -U "$POSTGRES_USER" -T template0 -O "$POSTGRES_USER" WeKnora
REMOTE

ssh -o BatchMode=yes "root@$backup_node" cat "$archive" \
  | kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
      sh -c 'exec pg_restore -U "$POSTGRES_USER" -d WeKnora --no-owner --no-acl --exit-on-error'

restored=$(kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
  sh -c 'psql -X -q -At -F "|" -v ON_ERROR_STOP=1 -U "$POSTGRES_USER" -d WeKnora' <<'SQL'
SELECT version, dirty FROM schema_migrations;
SELECT COUNT(*) FROM knowledge_bases WHERE deleted_at IS NULL;
SELECT COUNT(*) FROM knowledges WHERE deleted_at IS NULL;
SQL
)
mapfile -t restored_lines <<<"$restored"
[[ ${#restored_lines[@]} -eq 3 ]] || { echo "unexpected restored database output" >&2; exit 1; }
[[ "${restored_lines[0]}" == "$expected_schema" ]] || {
  echo "restored schema ${restored_lines[0]} != $expected_schema" >&2; exit 1;
}
[[ "${restored_lines[1]}" == "$expected_kb" ]] || {
  echo "restored KB count ${restored_lines[1]} != $expected_kb" >&2; exit 1;
}
[[ "${restored_lines[2]}" == "$expected_docs" ]] || {
  echo "restored document count ${restored_lines[2]} != $expected_docs" >&2; exit 1;
}

rollback_required=false
trap - EXIT
{
  printf 'release_id=%s\n' "$release_id"
  printf 'restored_archive=%s:%s\n' "$backup_node" "$archive"
  printf 'retained_postrelease_database=%s (ALLOW_CONNECTIONS=false)\n' "$held_db"
  printf 'schema=%s\n' "${restored_lines[0]}"
  printf 'knowledge_bases=%s\n' "${restored_lines[1]}"
  printf 'documents=%s\n' "${restored_lines[2]}"
  printf 'result=PASS\n'
} >"$report"
echo "PostgreSQL rollback restore verified; retained database: $held_db"
echo "Do not delete the retained database until rollback acceptance is complete."
