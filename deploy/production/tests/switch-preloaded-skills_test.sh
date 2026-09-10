#!/usr/bin/env bash
set -euo pipefail

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
subject="$script_dir/../switch-preloaded-skills.sh"
test_root=$(mktemp -d)
trap 'rm -rf -- "$test_root"' EXIT
export SKILLS_TEST_ROOT="$test_root/nodes"
release_id=empty-preloaded-test
nodes=(10.14.201.1 10.14.201.2 10.14.201.7)

mkdir -p "$test_root/bin" "$SKILLS_TEST_ROOT"
for node in "${nodes[@]}"; do
  skills_root="$SKILLS_TEST_ROOT/$node/app/skills"
  mkdir -p \
    "$skills_root/preloaded/legacy-skill" \
    "$skills_root/.preloaded-stage-$release_id"
  printf 'legacy\n' >"$skills_root/preloaded/legacy-skill/SKILL.md"
  : >"$skills_root/.preloaded-stage-$release_id.files.sha256"
done

cat >"$test_root/bin/ssh" <<'MOCK_SSH'
#!/usr/bin/env bash
set -euo pipefail
node=
while [[ $# -gt 0 ]]; do
  case "$1" in
    root@*) node=${1#root@}; shift; break ;;
    *) shift ;;
  esac
done
[[ -n "$node" ]]
while [[ $# -gt 0 && "$1" != -- ]]; do shift; done
[[ $# -gt 0 ]]
shift
mapped=()
for argument in "$@"; do
  if [[ "$argument" == /app/* ]]; then
    mapped+=("$SKILLS_TEST_ROOT/$node$argument")
  else
    mapped+=("$argument")
  fi
done
bash -s -- "${mapped[@]}"
MOCK_SSH
chmod +x "$test_root/bin/ssh"

run_subject() {
  PATH="$test_root/bin:$PATH" bash "$subject" "$release_id" "$@"
}

run_subject --check | grep -c 'SKILLS_STATE=staged' | grep -qx 3
run_subject --apply | grep -c 'SKILLS_STATE=active' | grep -qx 3

for node in "${nodes[@]}"; do
  skills_root="$SKILLS_TEST_ROOT/$node/app/skills"
  [[ -d "$skills_root/preloaded" ]]
  [[ -z "$(find "$skills_root/preloaded" -type f -print -quit)" ]]
  [[ -f "$skills_root/.preloaded-active-$release_id.files.sha256" ]]
  [[ ! -s "$skills_root/.preloaded-active-$release_id.files.sha256" ]]
  [[ -s "$skills_root/.preloaded-rollback-$release_id.files.sha256" ]]
  [[ -f "$skills_root/.preloaded-rollback-$release_id/legacy-skill/SKILL.md" ]]
done

run_subject --rollback | grep -c 'SKILLS_STATE=rolled-back' | grep -qx 3
for node in "${nodes[@]}"; do
  skills_root="$SKILLS_TEST_ROOT/$node/app/skills"
  grep -qx legacy "$skills_root/preloaded/legacy-skill/SKILL.md"
  [[ -d "$skills_root/.preloaded-stage-$release_id" ]]
  [[ -z "$(find "$skills_root/.preloaded-stage-$release_id" -type f -print -quit)" ]]
done

# Exact-tree verification must reject an unlisted file even when the expected
# release manifest is intentionally empty.
printf 'unexpected\n' > \
  "$SKILLS_TEST_ROOT/${nodes[0]}/app/skills/.preloaded-stage-$release_id/unlisted.txt"
if run_subject --check >/dev/null 2>&1; then
  echo 'empty manifest accepted an unlisted stage file' >&2
  exit 1
fi

echo 'switch-preloaded-skills empty-tree tests: PASS'
