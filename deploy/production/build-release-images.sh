#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  build-release-images.sh --source-dir DIR --registry HOST/NAMESPACE \
    --tag TAG --output-dir DIR [--execute] [--legacy-builder] \
    [--finalize-existing]

The default mode is fail-closed preflight only. It never pulls a public base
image. --execute builds serially, runs offline smoke checks, pushes six release
repositories, records immutable digests, and exports the app's built-in skills.
--finalize-existing resumes only the smoke/push/export phase after verifying
that all six local images carry the requested release tag and Git revision.

Required preloaded base images under BASE_IMAGE_REGISTRY (defaults to
REGISTRY/base):
  golang:1.26-bookworm
  debian:12.12-slim
  python:3.10.18-bookworm
  python:3.11-slim
  node:22-alpine
  nginx:stable-alpine

Environment overrides:
  BUILD_CPUS=2
  BUILD_MEMORY=10g
  MIN_DOCKER_FREE_GIB=80
  GOPROXY=https://goproxy.cn,direct
  GOSUMDB=off
  GOPRIVATE=
  BASE_IMAGE_REGISTRY=REGISTRY/base
  APT_MIRROR_HOST=mirrors.tencent.com
  PIP_INDEX_URL=https://mirrors.tencent.com/pypi/simple
  NPM_REGISTRY=https://mirrors.tencent.com/npm
  GITHUB_PROXY=https://ghfast.top/
  PLAYWRIGHT_DOWNLOAD_HOST=https://npmmirror.com/mirrors/playwright
  NODE_BUILD_HEAP_MB=6144
  MIGRATE_VERSION=v4.19.1
  PIP_VERSION=26.1.2
  SETUPTOOLS_VERSION=83.0.0
  WHEEL_VERSION=0.47.0
  PACKAGING_VERSION=26.2
  BUILD_TIME_OVERRIDE='YYYY-MM-DD HH:MM:SS UTC'
  FORCE_NO_CACHE=false
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

source_dir=""
registry=""
release_tag=""
output_dir=""
execute=false
force_legacy=false
finalize_existing=false

while (($#)); do
  case "$1" in
    --source-dir)
      (($# >= 2)) || die "--source-dir requires a value"
      source_dir=$2
      shift 2
      ;;
    --registry)
      (($# >= 2)) || die "--registry requires a value"
      registry=$2
      shift 2
      ;;
    --tag)
      (($# >= 2)) || die "--tag requires a value"
      release_tag=$2
      shift 2
      ;;
    --output-dir)
      (($# >= 2)) || die "--output-dir requires a value"
      output_dir=$2
      shift 2
      ;;
    --execute)
      execute=true
      shift
      ;;
    --legacy-builder)
      force_legacy=true
      shift
      ;;
    --finalize-existing)
      execute=true
      finalize_existing=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      die "unknown argument: $1"
      ;;
  esac
done

[[ -n "$source_dir" ]] || die "--source-dir is required"
[[ -n "$registry" ]] || die "--registry is required"
[[ -n "$release_tag" ]] || die "--tag is required"
[[ -n "$output_dir" ]] || die "--output-dir is required"
[[ "$registry" != *://* ]] || die "--registry must not include a URL scheme"
[[ "$registry" =~ ^[A-Za-z0-9._:-]+/[A-Za-z0-9._/-]+$ ]] || die "invalid --registry"
[[ "$release_tag" =~ ^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$ ]] || die "invalid --tag"

for command_name in docker python3 sha256sum find sort xargs awk sed grep diff \
  stat df date tee nice ionice readlink install; do
  command -v "$command_name" >/dev/null 2>&1 || die "missing command: $command_name"
done

source_dir=$(readlink -f "$source_dir")
output_dir=$(readlink -m "$output_dir")
[[ -d "$source_dir" ]] || die "source directory does not exist: $source_dir"
[[ "$output_dir" != / && "$output_dir" != "$source_dir" ]] || die "unsafe output directory"
[[ -f "$source_dir/.source-git-head" ]] || die "missing $source_dir/.source-git-head"
[[ -f "$source_dir/docker/Dockerfile.app" ]] || die "source tree is incomplete"
[[ -f "$source_dir/packages/SHA256SUMS.release" ]] || die "release package manifest is missing"

(cd "$source_dir/packages" && sha256sum -c SHA256SUMS.release)
[[ -x "$source_dir/packages/grpc_health_probe-linux-amd64" ]] || die "gRPC probe cache is missing"
[[ -s "$source_dir/packages/protoc-3.19.4-linux-x86_64.zip" ]] || die "protoc cache is missing"
[[ -x "$source_dir/packages/docker-cli/docker" ]] || die "Docker CLI cache is missing"
[[ -s "$source_dir/packages/duckdb/extensions/v1.5.2/linux_amd64/spatial.duckdb_extension" ]] || \
  die "DuckDB spatial cache is missing"
[[ -s "$source_dir/packages/duckdb/extensions/v1.5.2/linux_amd64/excel.duckdb_extension" ]] || \
  die "DuckDB excel cache is missing"
[[ $(find "$source_dir/packages/uv" -maxdepth 1 -type f -name 'uv-*.whl' | wc -l) -eq 1 ]] || \
  die "uv wheel cache is incomplete"
[[ $(find "$source_dir/packages/python-build-tools" -maxdepth 1 -type f -name '*.whl' | wc -l) -eq 4 ]] || \
  die "Python build-tool wheel cache is incomplete"

source_head=$(tr -d '\r\n' <"$source_dir/.source-git-head")
[[ "$source_head" =~ ^[0-9a-f]{40}$ ]] || die "invalid source Git head: $source_head"
tag_commit_prefix=${release_tag%%-*}
[[ "$tag_commit_prefix" =~ ^[0-9a-f]{7,40}$ ]] || \
  die "release tag must begin with a 7-40 character Git prefix"
[[ "$source_head" == "$tag_commit_prefix"* ]] || \
  die "release tag Git prefix does not match $source_head"

build_cpus=${BUILD_CPUS:-2}
build_memory=${BUILD_MEMORY:-10g}
min_docker_free_gib=${MIN_DOCKER_FREE_GIB:-80}
base_image_registry=${BASE_IMAGE_REGISTRY:-$registry/base}
apt_mirror_host=${APT_MIRROR_HOST:-mirrors.tencent.com}
pip_index_url=${PIP_INDEX_URL:-https://mirrors.tencent.com/pypi/simple}
npm_registry=${NPM_REGISTRY:-https://mirrors.tencent.com/npm}
github_proxy=${GITHUB_PROXY:-https://ghfast.top/}
playwright_download_host=${PLAYWRIGHT_DOWNLOAD_HOST:-https://npmmirror.com/mirrors/playwright}
uv_version=${UV_VERSION:-0.11.32}
node_build_heap_mb=${NODE_BUILD_HEAP_MB:-6144}
migrate_version=${MIGRATE_VERSION:-v4.19.1}
pip_version=${PIP_VERSION:-26.1.2}
setuptools_version=${SETUPTOOLS_VERSION:-83.0.0}
wheel_version=${WHEEL_VERSION:-0.47.0}
packaging_version=${PACKAGING_VERSION:-26.2}
build_time=${BUILD_TIME_OVERRIDE:-$(date -u '+%Y-%m-%d %H:%M:%S UTC')}
force_no_cache=${FORCE_NO_CACHE:-false}
[[ "$build_cpus" =~ ^[1-9][0-9]*$ ]] || die "BUILD_CPUS must be a positive integer"
[[ "$build_memory" =~ ^[1-9][0-9]*[gGmM]$ ]] || die "invalid BUILD_MEMORY"
[[ "$min_docker_free_gib" =~ ^[1-9][0-9]*$ ]] || die "invalid MIN_DOCKER_FREE_GIB"
[[ "$node_build_heap_mb" =~ ^[1-9][0-9]*$ ]] || die "invalid NODE_BUILD_HEAP_MB"
[[ "$migrate_version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]] || die "invalid MIGRATE_VERSION"
[[ -n "$build_time" && "$build_time" != *$'\n'* ]] || die "invalid BUILD_TIME_OVERRIDE"
[[ "$base_image_registry" != *://* ]] || die "BASE_IMAGE_REGISTRY must not include a URL scheme"
[[ "$force_no_cache" == true || "$force_no_cache" == false ]] || \
  die "FORCE_NO_CACHE must be true or false"

install -d -m 0700 "$output_dir"
log_file="$output_dir/build-release-images.log"
exec > >(tee -a "$log_file") 2>&1

printf 'SOURCE_DIR=%s\nSOURCE_GIT_HEAD=%s\nREGISTRY=%s\nTAG=%s\n' \
  "$source_dir" "$source_head" "$registry" "$release_tag"
if [[ "$finalize_existing" == true ]]; then
  mode=finalize-existing
elif [[ "$execute" == true ]]; then
  mode=execute
else
  mode=preflight
fi
printf 'BUILD_CPUS=%s\nBUILD_MEMORY=%s\nMODE=%s\n' \
  "$build_cpus" "$build_memory" "$mode"
printf 'BASE_IMAGE_REGISTRY=%s\nAPT_MIRROR_HOST=%s\nPIP_INDEX_URL=%s\nNPM_REGISTRY=%s\n' \
  "$base_image_registry" "$apt_mirror_host" "$pip_index_url" "$npm_registry"
printf 'GITHUB_PROXY=%s\nPLAYWRIGHT_DOWNLOAD_HOST=%s\nUV_VERSION=%s\nNODE_BUILD_HEAP_MB=%s\nFORCE_NO_CACHE=%s\n' \
  "$github_proxy" "$playwright_download_host" "$uv_version" "$node_build_heap_mb" "$force_no_cache"
printf 'MIGRATE_VERSION=%s\nPIP_VERSION=%s\nSETUPTOOLS_VERSION=%s\nWHEEL_VERSION=%s\nPACKAGING_VERSION=%s\n' \
  "$migrate_version" "$pip_version" "$setuptools_version" "$wheel_version" "$packaging_version"
printf 'BUILD_TIME=%s\n' "$build_time"

docker version >/dev/null
docker_root=$(docker info --format '{{.DockerRootDir}}')
[[ -d "$docker_root" ]] || die "Docker root is unavailable: $docker_root"
docker_free_gib=$(df -Pk "$docker_root" | awk 'NR == 2 {print int($4 / 1024 / 1024)}')
((docker_free_gib >= min_docker_free_gib)) || \
  die "Docker free space ${docker_free_gib}GiB is below ${min_docker_free_gib}GiB"
printf 'DOCKER_ROOT=%s\nDOCKER_FREE_GIB=%s\n' "$docker_root" "$docker_free_gib"

base_images=(
  golang:1.26-bookworm
  debian:12.12-slim
  python:3.10.18-bookworm
  python:3.11-slim
  node:22-alpine
  nginx:stable-alpine
)
missing_images=()
for image in "${base_images[@]}"; do
  base_ref="$base_image_registry/$image"
  if docker image inspect "$base_ref" >/dev/null 2>&1; then
    printf 'BASE_IMAGE=PASS %s %s\n' "$base_ref" \
      "$(docker image inspect --format '{{.Id}}' "$base_ref")"
  else
    printf 'BASE_IMAGE=MISSING %s\n' "$base_ref"
    missing_images+=("$base_ref")
  fi
done
if ((${#missing_images[@]})); then
  die "required base images are not preloaded; import approved images before building"
fi

server_version=$(docker version --format '{{.Server.Version}}')
server_major=${server_version%%.*}
legacy_builder=$force_legacy
if [[ "$server_major" =~ ^[0-9]+$ ]] && ((server_major < 19)); then
  legacy_builder=true
fi
printf 'DOCKER_SERVER_VERSION=%s\nLEGACY_BUILDER=%s\n' "$server_version" "$legacy_builder"
if [[ "$legacy_builder" == true ]]; then
  buildkit_enabled=0
else
  buildkit_enabled=1
fi

app_dockerfile="$source_dir/docker/Dockerfile.app"
if [[ "$legacy_builder" == true ]]; then
  app_dockerfile="$output_dir/Dockerfile.app.legacy"
  python3 - "$source_dir/docker/Dockerfile.app" "$app_dockerfile" <<'PY'
from pathlib import Path
import sys

source = Path(sys.argv[1])
target = Path(sys.argv[2])
prefix = b"RUN --mount=type=cache,target=/go/pkg/mod "
before = source.read_bytes()
changed = before.count(prefix)
if changed != 3:
    raise SystemExit(f"expected exactly 3 legacy cache rewrites, got {changed}")
after = before.replace(prefix, b"RUN ")
if after.count(prefix) != 0:
    raise SystemExit("legacy cache prefix remains after rewrite")
target.write_bytes(after)
PY
  chmod 0600 "$app_dockerfile"
  diff -u "$source_dir/docker/Dockerfile.app" "$app_dockerfile" \
    >"$output_dir/Dockerfile.app.legacy.diff" || true
  [[ $(grep -c '^[-+]RUN ' "$output_dir/Dockerfile.app.legacy.diff") -eq 6 ]] || \
    die "legacy Dockerfile diff is not limited to three RUN lines"
  sha256sum "$source_dir/docker/Dockerfile.app" "$app_dockerfile" \
    >"$output_dir/Dockerfile.app.sha256"
fi

if [[ "$execute" != true ]]; then
  printf 'RELEASE_IMAGE_PREFLIGHT=PASS\n'
  exit 0
fi

cpu_period=100000
cpu_quota=$((build_cpus * cpu_period))
common_build_args=(
  --pull=false
  --cpu-period "$cpu_period"
  --cpu-quota "$cpu_quota"
  --memory "$build_memory"
  --label "org.opencontainers.image.revision=$source_head"
  --label "com.weknora.release.tag=$release_tag"
)
if [[ "$force_no_cache" == true ]]; then
  common_build_args+=(--no-cache)
fi

app_ref="$registry/weknora-app:$release_tag"
docreader_ref="$registry/weknora-docreader:$release_tag"
general_agent_ref="$registry/weknora-general-agent:$release_tag"
document_agent_ref="$registry/weknora-document-processing-agent:$release_tag"
frontend_ref="$registry/weknora-frontend:$release_tag"
mobile_ref="$registry/weknora-mobile-web:$release_tag"

version=$(tr -d '\r\n' <"$source_dir/VERSION")
go_version="go$(awk '$1 == "go" {print $2; exit}' "$source_dir/go.mod")"

run_build() {
  local name=$1
  shift
  printf 'BUILD_START=%s %s\n' "$name" "$(date -u +%FT%TZ)"
  nice -n 10 ionice -c 2 -n 7 env DOCKER_BUILDKIT="$buildkit_enabled" \
    docker build "${common_build_args[@]}" "$@"
  printf 'BUILD_DONE=%s %s\n' "$name" "$(date -u +%FT%TZ)"
}

if [[ "$finalize_existing" == true ]]; then
  for image in "$app_ref" "$docreader_ref" "$general_agent_ref" \
    "$document_agent_ref" "$frontend_ref" "$mobile_ref"; do
    docker image inspect "$image" >/dev/null 2>&1 || \
      die "release image is not available locally: $image"
    [[ $(docker image inspect --format '{{index .Config.Labels "org.opencontainers.image.revision"}}' \
      "$image") == "$source_head" ]] || die "release image revision mismatch: $image"
    [[ $(docker image inspect --format '{{index .Config.Labels "com.weknora.release.tag"}}' \
      "$image") == "$release_tag" ]] || die "release image tag label mismatch: $image"
    printf 'EXISTING_IMAGE=PASS %s %s\n' "$image" \
      "$(docker image inspect --format '{{.Id}}' "$image")"
  done
else
  run_build general-agent \
    --build-arg "BASE_IMAGE_REGISTRY_ARG=$base_image_registry" \
    --build-arg "APT_MIRROR_ARG=$apt_mirror_host" \
    --build-arg "PIP_INDEX_URL_ARG=$pip_index_url" \
    --build-arg "NPM_REGISTRY_ARG=$npm_registry" \
    -f "$source_dir/custom/services/general-agent/Dockerfile" \
    -t "$general_agent_ref" "$source_dir"

  run_build document-processing-agent \
    --build-arg "BASE_IMAGE_REGISTRY_ARG=$base_image_registry" \
    --build-arg "APT_MIRROR_ARG=$apt_mirror_host" \
    --build-arg "PIP_INDEX_URL_ARG=$pip_index_url" \
    -f "$source_dir/custom/services/document-processing-agent/Dockerfile" \
    -t "$document_agent_ref" "$source_dir"

  run_build docreader \
    --build-arg "BASE_IMAGE_REGISTRY_ARG=$base_image_registry" \
    --build-arg TARGETARCH=amd64 \
    --build-arg "APT_MIRROR_ARG=$apt_mirror_host" \
    --build-arg "PIP_INDEX_URL_ARG=$pip_index_url" \
    --build-arg "GITHUB_PROXY_ARG=$github_proxy" \
    --build-arg "PLAYWRIGHT_DOWNLOAD_HOST_ARG=$playwright_download_host" \
    --build-arg "UV_VERSION=$uv_version" \
    -f "$source_dir/docker/Dockerfile.docreader" \
    -t "$docreader_ref" "$source_dir"

  run_build frontend-mobile \
    --build-arg "BASE_IMAGE_REGISTRY_ARG=$base_image_registry" \
    --build-arg "NPM_REGISTRY_ARG=$npm_registry" \
    --build-arg "NODE_OPTIONS_ARG=--max-old-space-size=$node_build_heap_mb" \
    -f "$source_dir/frontend/Dockerfile.mobile" \
    -t "$frontend_ref" "$source_dir/frontend"
  docker tag "$frontend_ref" "$mobile_ref"

  run_build app \
    --build-arg "BASE_IMAGE_REGISTRY_ARG=$base_image_registry" \
    --build-arg "MIGRATE_VERSION_ARG=$migrate_version" \
    --build-arg "GOPRIVATE_ARG=${GOPRIVATE:-}" \
    --build-arg "GOPROXY_ARG=${GOPROXY:-https://goproxy.cn,direct}" \
    --build-arg "GOSUMDB_ARG=${GOSUMDB:-off}" \
    --build-arg "APK_MIRROR_ARG=$apt_mirror_host" \
    --build-arg "PIP_INDEX_URL_ARG=$pip_index_url" \
    --build-arg "UV_VERSION_ARG=$uv_version" \
    --build-arg "PIP_VERSION_ARG=$pip_version" \
    --build-arg "SETUPTOOLS_VERSION_ARG=$setuptools_version" \
    --build-arg "WHEEL_VERSION_ARG=$wheel_version" \
    --build-arg "PACKAGING_VERSION_ARG=$packaging_version" \
    --build-arg "VERSION_ARG=$version" \
    --build-arg "COMMIT_ID_ARG=$source_head" \
    --build-arg "BUILD_TIME_ARG=$build_time" \
    --build-arg "GO_VERSION_ARG=$go_version" \
    -f "$app_dockerfile" -t "$app_ref" "$source_dir"
fi

smoke_shell() {
  local name=$1
  local image=$2
  local check=$3
  docker run --rm --network none --read-only --entrypoint /bin/sh \
    "$image" -ec "$check"
  printf 'OFFLINE_IMAGE_SMOKE=PASS %s %s\n' "$name" "$image"
}

smoke_shell app "$app_ref" \
  'test -x /app/WeKnora && test -d /app/skills/preloaded && test -x /usr/local/bin/docker && test -x /usr/local/bin/uvx && test -s /home/appuser/.duckdb/extensions/v1.5.2/linux_amd64/spatial.duckdb_extension && test -s /home/appuser/.duckdb/extensions/v1.5.2/linux_amd64/excel.duckdb_extension'
smoke_shell docreader "$docreader_ref" \
  'test -f /app/docreader/main.py && test -x /bin/grpc_health_probe && test -d /root/.cache/ms-playwright'
smoke_shell general-agent "$general_agent_ref" 'test -f /app/app/main.py'
smoke_shell document-agent "$document_agent_ref" 'test -f /app/app/main.py'
smoke_shell frontend "$frontend_ref" \
  'test -f /usr/share/nginx/html/index.html && test -f /usr/share/nginx/html/mobile/mobile.html'
smoke_shell mobile "$mobile_ref" \
  'test -f /usr/share/nginx/html/index.html && test -f /usr/share/nginx/html/mobile/mobile.html'
printf 'OFFLINE_IMAGE_SMOKE=ALL_PASS\n'

digest_file="$output_dir/image-digests.env"
: >"$digest_file"
chmod 0600 "$digest_file"

push_and_record() {
  local key=$1
  local reference=$2
  docker push "$reference"
  local repository=${reference%:*}
  local repo_digest
  repo_digest=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
    "$reference" | grep -F "$repository@sha256:" | tail -1)
  [[ "$repo_digest" =~ @sha256:[0-9a-f]{64}$ ]] || \
    die "registry digest missing after push: $reference"
  printf '%s_REFERENCE=%q\n%s_DIGEST=%q\n' \
    "$key" "$reference" "$key" "${repo_digest#*@}" >>"$digest_file"
  printf 'PUSHED=%s\n' "$repo_digest"
}

push_and_record APP "$app_ref"
push_and_record DOCREADER "$docreader_ref"
push_and_record GENERAL_AGENT "$general_agent_ref"
push_and_record DOCUMENT_AGENT "$document_agent_ref"
push_and_record FRONTEND "$frontend_ref"
push_and_record MOBILE_WEB "$mobile_ref"

skills_dir="$output_dir/app-skills-preloaded"
skills_container="weknora-skills-${release_tag//[^A-Za-z0-9_.-]/-}"
docker rm -f "$skills_container" >/dev/null 2>&1 || true
docker create --name "$skills_container" "$app_ref" >/dev/null
trap 'docker rm -f "$skills_container" >/dev/null 2>&1 || true' EXIT
rm -rf "$skills_dir"
docker cp "$skills_container:/app/skills/preloaded" "$skills_dir"
docker rm "$skills_container" >/dev/null
trap - EXIT
(
  cd "$skills_dir"
  find . -type f -print0 | sort -z | xargs -0 -r sha256sum
) >"$output_dir/app-skills-preloaded.files.sha256"
sha256sum "$output_dir/app-skills-preloaded.files.sha256" \
  >"$output_dir/app-skills-preloaded.aggregate.sha256"

sha256sum "$digest_file" "$output_dir/app-skills-preloaded.aggregate.sha256" \
  "$output_dir/app-skills-preloaded.files.sha256" \
  >"$output_dir/release-image-artifacts.sha256"
printf 'RELEASE_IMAGE_BUILD=PASS\nDIGEST_FILE=%s\n' "$digest_file"
