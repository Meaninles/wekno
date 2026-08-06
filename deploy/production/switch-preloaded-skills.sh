#!/usr/bin/env bash
set -euo pipefail

usage() {
  echo "usage: $0 RELEASE_ID [--check|--apply|--rollback]" >&2
  exit 2
}

[[ $# -ge 1 && $# -le 2 ]] || usage
release_id=$1
mode=${2:---check}
[[ "$release_id" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || usage
[[ "$mode" == --check || "$mode" == --apply || "$mode" == --rollback ]] || usage

nodes=(10.14.201.1 10.14.201.2 10.14.201.7)
current=/app/skills/preloaded
stage="/app/skills/.preloaded-stage-$release_id"
rollback="/app/skills/.preloaded-rollback-$release_id"
stage_manifest="$stage.files.sha256"
active_manifest="/app/skills/.preloaded-active-$release_id.files.sha256"
rollback_manifest="$rollback.files.sha256"
restored_manifest="/app/skills/.preloaded-restored-$release_id.files.sha256"

remote_check() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$stage_manifest" "$active_manifest" \
    "$rollback_manifest" "$restored_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
stage_manifest=$4
active_manifest=$5
rollback_manifest=$6
restored_manifest=$7

verify_tree() {
  local directory=$1
  local manifest=$2
  [[ -d "$directory" && -s "$manifest" ]]
  (cd "$directory" && sha256sum -c "$manifest" >/dev/null)
}

if [[ -d "$current" && -d "$stage" && ! -e "$rollback" && -s "$stage_manifest" ]]; then
  verify_tree "$stage" "$stage_manifest"
  if [[ -s "$restored_manifest" ]]; then
    verify_tree "$current" "$restored_manifest"
    state=rolled-back
  else
    [[ ! -e "$active_manifest" && ! -e "$rollback_manifest" ]]
    state=staged
  fi
elif [[ -d "$current" && ! -e "$stage" && -d "$rollback" ]]; then
  verify_tree "$current" "$active_manifest"
  verify_tree "$rollback" "$rollback_manifest"
  state=active
else
  echo "ERROR: unrecognized skills state" >&2
  exit 1
fi

printf 'SKILLS_STATE=%s host=%s current=%s stage=%s rollback=%s\n' \
  "$state" "$(hostname)" "$current" "$stage" "$rollback"
REMOTE
}

remote_preflight_apply() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$stage_manifest" "$active_manifest" \
    "$rollback_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
stage_manifest=$4
active_manifest=$5
rollback_manifest=$6
[[ -d "$current" && -d "$stage" && -s "$stage_manifest" ]]
[[ ! -e "$rollback" && ! -e "$active_manifest" && ! -e "$rollback_manifest" ]]
[[ $(stat -c %d "$current") == "$(stat -c %d "$stage")" ]]
(cd "$stage" && sha256sum -c "$stage_manifest" >/dev/null)
printf 'SKILLS_APPLY_PREFLIGHT=PASS host=%s\n' "$(hostname)"
REMOTE
}

remote_apply() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$stage_manifest" "$active_manifest" \
    "$rollback_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
stage_manifest=$4
active_manifest=$5
rollback_manifest=$6
temporary_manifest="$rollback_manifest.tmp.$$"

[[ -d "$current" && -d "$stage" && -s "$stage_manifest" ]]
[[ ! -e "$rollback" && ! -e "$active_manifest" && ! -e "$rollback_manifest" ]]
(cd "$stage" && sha256sum -c "$stage_manifest" >/dev/null)
(
  cd "$current"
  find . -type f -print0 | sort -z | xargs -0 -r sha256sum
) >"$temporary_manifest"
[[ -s "$temporary_manifest" ]]
mv "$temporary_manifest" "$rollback_manifest"
mv "$current" "$rollback"
if ! mv "$stage" "$current"; then
  mv "$rollback" "$current"
  rm -f -- "$rollback_manifest"
  exit 1
fi
if ! mv "$stage_manifest" "$active_manifest"; then
  mv "$current" "$stage"
  mv "$rollback" "$current"
  rm -f -- "$rollback_manifest"
  exit 1
fi
if ! (cd "$current" && sha256sum -c "$active_manifest" >/dev/null); then
  mv "$active_manifest" "$stage_manifest"
  mv "$current" "$stage"
  mv "$rollback" "$current"
  rm -f -- "$rollback_manifest"
  exit 1
fi
printf 'SKILLS_APPLY=PASS host=%s\n' "$(hostname)"
REMOTE
}

remote_revert_apply() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$stage_manifest" "$active_manifest" \
    "$rollback_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
stage_manifest=$4
active_manifest=$5
rollback_manifest=$6
[[ -d "$current" && ! -e "$stage" && -d "$rollback" ]]
(cd "$current" && sha256sum -c "$active_manifest" >/dev/null)
(cd "$rollback" && sha256sum -c "$rollback_manifest" >/dev/null)
mv "$active_manifest" "$stage_manifest"
mv "$current" "$stage"
mv "$rollback" "$current"
rm -f -- "$rollback_manifest"
printf 'SKILLS_APPLY_REVERTED=PASS host=%s\n' "$(hostname)"
REMOTE
}

remote_preflight_rollback() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$active_manifest" "$rollback_manifest" \
    "$restored_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
active_manifest=$4
rollback_manifest=$5
restored_manifest=$6
[[ -d "$current" && ! -e "$stage" && -d "$rollback" ]]
[[ -s "$active_manifest" && -s "$rollback_manifest" && ! -e "$restored_manifest" ]]
(cd "$current" && sha256sum -c "$active_manifest" >/dev/null)
(cd "$rollback" && sha256sum -c "$rollback_manifest" >/dev/null)
printf 'SKILLS_ROLLBACK_PREFLIGHT=PASS host=%s\n' "$(hostname)"
REMOTE
}

remote_rollback() {
  local node=$1
  ssh -o BatchMode=yes -o ConnectTimeout=10 "root@$node" bash -s -- \
    "$current" "$stage" "$rollback" "$stage_manifest" "$active_manifest" \
    "$rollback_manifest" "$restored_manifest" <<'REMOTE'
set -euo pipefail
current=$1
stage=$2
rollback=$3
stage_manifest=$4
active_manifest=$5
rollback_manifest=$6
restored_manifest=$7
(cd "$current" && sha256sum -c "$active_manifest" >/dev/null)
(cd "$rollback" && sha256sum -c "$rollback_manifest" >/dev/null)
mv "$active_manifest" "$stage_manifest"
mv "$current" "$stage"
mv "$rollback" "$current"
mv "$rollback_manifest" "$restored_manifest"
(cd "$current" && sha256sum -c "$restored_manifest" >/dev/null)
printf 'SKILLS_ROLLBACK=PASS host=%s\n' "$(hostname)"
REMOTE
}

if [[ "$mode" == --check ]]; then
  for node in "${nodes[@]}"; do
    remote_check "$node"
  done
  exit 0
fi

if [[ "$mode" == --apply ]]; then
  for node in "${nodes[@]}"; do
    remote_preflight_apply "$node"
  done
  activated=()
  for node in "${nodes[@]}"; do
    if remote_apply "$node"; then
      activated+=("$node")
      continue
    fi
    echo "ERROR: skills activation failed on $node; reverting activated nodes" >&2
    for ((index=${#activated[@]} - 1; index >= 0; index--)); do
      remote_revert_apply "${activated[$index]}"
    done
    exit 1
  done
else
  for node in "${nodes[@]}"; do
    remote_preflight_rollback "$node"
  done
  for node in "${nodes[@]}"; do
    remote_rollback "$node"
  done
fi

for node in "${nodes[@]}"; do
  remote_check "$node"
done
