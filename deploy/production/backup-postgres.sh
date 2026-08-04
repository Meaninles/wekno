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
backup_dir="$backup_root/$release_id"
archive="$backup_dir/WeKnora.dump"
globals="$backup_dir/postgres-globals.sql"
cutoff_dir="${WEKNORA_CUTOFF_ROOT:-/root/weknora-release-prep}/$release_id"

[[ -f "$cutoff_dir/knowledge-base-inventory.csv" ]] || {
  echo "missing cutoff inventory: $cutoff_dir/knowledge-base-inventory.csv" >&2
  exit 1
}
[[ -f "$cutoff_dir/SHA256SUMS" ]] || {
  echo "missing cutoff checksums: $cutoff_dir/SHA256SUMS" >&2
  exit 1
}
(
  cd "$cutoff_dir"
  sha256sum -c SHA256SUMS
) >/dev/null

backup_root_mode=$(
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$backup_node" \
    stat -c %a "$backup_root"
)
[[ "$backup_root_mode" == 700 ]] || {
  echo "backup root must already exist with mode 0700: $backup_node:$backup_root" >&2
  exit 1
}

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
  echo "refusing database backup while state-changing deployments are active:" >&2
  printf '%s\n' "$active_mutators" >&2
  exit 1
fi

ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$backup_node" \
  install -d -o root -g root -m 0700 "$backup_dir"
if ssh -o BatchMode=yes "root@$backup_node" test -e "$archive"; then
  echo "refusing to overwrite existing backup: $backup_node:$archive" >&2
  exit 1
fi

echo "streaming PostgreSQL custom archive to existing node $backup_node"
kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'exec pg_dump -U "$POSTGRES_USER" -d "$POSTGRES_DB" --format=custom --compress=6 --no-owner --no-acl' \
  | ssh -o BatchMode=yes "root@$backup_node" \
      "umask 077; cat > '$archive.partial'"
ssh -o BatchMode=yes "root@$backup_node" \
  mv "$archive.partial" "$archive"

kubectl -n "$namespace" exec "deployment/$postgres_deployment" -- \
  sh -c 'exec pg_dumpall -U "$POSTGRES_USER" --globals-only' \
  | ssh -o BatchMode=yes "root@$backup_node" \
      "umask 077; cat > '$globals.partial'"
ssh -o BatchMode=yes "root@$backup_node" \
  mv "$globals.partial" "$globals"

ssh -o BatchMode=yes "root@$backup_node" \
  sha256sum "$archive" "$globals" >"$cutoff_dir/postgres-backup-sha256.txt"
ssh -o BatchMode=yes "root@$backup_node" \
  sha256sum -c - <"$cutoff_dir/postgres-backup-sha256.txt" >/dev/null
ssh -o BatchMode=yes "root@$backup_node" \
  bash -s -- "$archive" "$globals" \
  >"$cutoff_dir/postgres-backup-files.txt" <<'REMOTE'
set -euo pipefail
stat -c '%n %s bytes %y' "$@"
REMOTE

ssh -o BatchMode=yes "root@$backup_node" cat "$archive" \
  | kubectl -n "$namespace" exec -i "deployment/$postgres_deployment" -- \
      pg_restore --list \
  >"$cutoff_dir/postgres-restore-list.txt"
test -s "$cutoff_dir/postgres-restore-list.txt"

echo "$backup_node:$archive" >"$cutoff_dir/postgres-backup-location.txt"
echo "database backup verified: $backup_node:$archive"
