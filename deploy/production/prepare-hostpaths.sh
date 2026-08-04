#!/usr/bin/env bash
set -euo pipefail

mode=check
if [[ "${1:-}" == "--apply" ]]; then
  mode=apply
elif [[ $# -ne 0 ]]; then
  echo "usage: $0 [--apply]" >&2
  exit 2
fi

nodes=(10.14.201.1 10.14.201.2 10.14.201.7)
scratch_root=/mnt/weknora-data/weknora-v2-scratch
roles=(api parse docreader derivative wiki maintenance migration general-agent document-agent)
backup_root=/mnt/weknora-data/weknora-db-backups

for node in "${nodes[@]}"; do
  echo "[$node]"
  if [[ "$mode" == apply ]]; then
    ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" \
      bash -s -- "$scratch_root" "${roles[@]/#/$scratch_root/}" <<'REMOTE'
set -euo pipefail
install -d -o root -g root -m 1777 "$@"
REMOTE
    if [[ "$node" == "10.14.201.7" ]]; then
      ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" \
        install -d -o root -g root -m 0700 "$backup_root"
    fi
  fi

  remote_paths=("$scratch_root" "${roles[@]/#/$scratch_root/}")
  if [[ "$node" == "10.14.201.7" ]]; then
    remote_paths+=("$backup_root")
  fi
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" \
    bash -s -- "${remote_paths[@]}" <<'REMOTE'
set -euo pipefail
stat -c '%a %U:%G %n' "$@"
df -h /mnt/weknora-data
REMOTE
done
