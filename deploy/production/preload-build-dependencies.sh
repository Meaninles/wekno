#!/usr/bin/env bash
set -euo pipefail

usage() {
  cat <<'EOF'
Usage:
  preload-build-dependencies.sh --source-dir DIR --registry HOST/NAMESPACE \
    --output-dir DIR --duckdb-source-image IMAGE [--execute]

Default mode is read-only preflight. --execute:
  1. pulls six linux/amd64 base images through DaoCloud;
  2. pins them in REGISTRY/base and records both mirror/internal digests;
  3. downloads protoc and grpc_health_probe through the GitHub mirror;
  4. downloads the production-matched uv wheel through the PyPI mirror;
  5. exports the Docker CLI and version-matched DuckDB extensions from an
     immutable app image;
  6. writes a hashed package manifest into SOURCE_DIR/packages.

The source tree must contain .source-git-head. This prevents --execute from
silently modifying an ordinary developer checkout.
EOF
}

die() {
  printf 'ERROR: %s\n' "$*" >&2
  exit 1
}

source_dir=""
registry=""
output_dir=""
duckdb_source_image=""
execute=false

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
    --output-dir)
      (($# >= 2)) || die "--output-dir requires a value"
      output_dir=$2
      shift 2
      ;;
    --duckdb-source-image)
      (($# >= 2)) || die "--duckdb-source-image requires a value"
      duckdb_source_image=$2
      shift 2
      ;;
    --execute)
      execute=true
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
[[ -n "$output_dir" ]] || die "--output-dir is required"
[[ -n "$duckdb_source_image" ]] || die "--duckdb-source-image is required"
[[ "$registry" != *://* ]] || die "--registry must not include a URL scheme"

for command_name in curl docker python3 sha256sum find sort xargs readlink \
  install date awk grep tee wc; do
  command -v "$command_name" >/dev/null 2>&1 || die "missing command: $command_name"
done

source_dir=$(readlink -f "$source_dir")
output_dir=$(readlink -m "$output_dir")
[[ -d "$source_dir" ]] || die "source directory does not exist: $source_dir"
[[ -f "$source_dir/.source-git-head" ]] || \
  die "refusing an unstaged checkout without .source-git-head"
[[ "$output_dir" != / && "$output_dir" != "$source_dir" ]] || die "unsafe output directory"

source_head=$(tr -d '\r\n' <"$source_dir/.source-git-head")
[[ "$source_head" =~ ^[0-9a-f]{40}$ ]] || die "invalid source Git head"

base_mirror=${BASE_IMAGE_MIRROR:-docker.m.daocloud.io/library}
apt_mirror_host=${APT_MIRROR_HOST:-mirrors.tencent.com}
pip_index_url=${PIP_INDEX_URL:-https://mirrors.tencent.com/pypi/simple}
npm_registry=${NPM_REGISTRY:-https://mirrors.tencent.com/npm}
github_proxy=${GITHUB_PROXY:-https://ghfast.top/}
playwright_download_host=${PLAYWRIGHT_DOWNLOAD_HOST:-https://npmmirror.com/mirrors/playwright}
goproxy=${GOPROXY:-https://goproxy.cn,direct}
uv_version=${UV_VERSION:-0.11.32}
pip_version=${PIP_VERSION:-26.1.2}
setuptools_version=${SETUPTOOLS_VERSION:-83.0.0}
wheel_version=${WHEEL_VERSION:-0.47.0}
packaging_version=${PACKAGING_VERSION:-26.2}
internal_base_registry=${BASE_IMAGE_REGISTRY:-$registry/base}

install -d -m 0700 "$output_dir"
log_file="$output_dir/preload-build-dependencies.log"
exec > >(tee -a "$log_file") 2>&1

printf 'SOURCE_GIT_HEAD=%s\nMODE=%s\nBASE_IMAGE_MIRROR=%s\nBASE_IMAGE_REGISTRY=%s\n' \
  "$source_head" "$([[ "$execute" == true ]] && echo execute || echo preflight)" \
  "$base_mirror" "$internal_base_registry"
printf 'APT_MIRROR_HOST=%s\nPIP_INDEX_URL=%s\nNPM_REGISTRY=%s\n' \
  "$apt_mirror_host" "$pip_index_url" "$npm_registry"
printf 'GITHUB_PROXY=%s\nPLAYWRIGHT_DOWNLOAD_HOST=%s\nGOPROXY=%s\n' \
  "$github_proxy" "$playwright_download_host" "$goproxy"
printf 'UV_VERSION=%s\nPIP_VERSION=%s\nSETUPTOOLS_VERSION=%s\nWHEEL_VERSION=%s\nPACKAGING_VERSION=%s\n' \
  "$uv_version" "$pip_version" "$setuptools_version" "$wheel_version" "$packaging_version"

existing_cache_valid=false
if [[ -f "$source_dir/packages/SHA256SUMS.release" ]] && \
   (cd "$source_dir/packages" && sha256sum -c SHA256SUMS.release >/dev/null); then
  existing_cache_valid=true
  printf 'EXISTING_PACKAGE_CACHE=PASS %s\n' "$source_dir/packages/SHA256SUMS.release"
else
  printf 'EXISTING_PACKAGE_CACHE=UNAVAILABLE\n'
fi

probe() {
  local name=$1
  local url=$2
  local result
  result=$(curl -sS -L -o /dev/null --connect-timeout 10 --max-time 60 \
    --retry 3 --retry-all-errors \
    -w '%{http_code} %{time_total} %{url_effective}' "$url") || \
    die "dependency source is unreachable: $name $url"
  [[ "$result" == 2* ]] || die "dependency source returned non-2xx: $name $result"
  printf 'SOURCE_PROBE=PASS %s %s\n' "$name" "$result"
}

probe apt "https://${apt_mirror_host}/debian/dists/bookworm/Release"
probe pypi "${pip_index_url%/}/uv/"
probe npm "${npm_registry%/}/pptxgenjs"
probe go "${goproxy%%,*}/github.com/golang-migrate/migrate/v4/@v/list"
probe playwright "${playwright_download_host%/}/"
if [[ "$existing_cache_valid" == true && \
      -s "$source_dir/packages/protoc-3.19.4-linux-x86_64.zip" && \
      -x "$source_dir/packages/grpc_health_probe-linux-amd64" ]]; then
  printf 'SOURCE_PROBE=SKIP github-proxy validated-local-cache\n'
else
  probe github-proxy "${github_proxy}https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.4.24/grpc_health_probe-linux-amd64"
fi

base_images=(
  golang:1.26-bookworm
  debian:12.12-slim
  python:3.10.18-bookworm
  python:3.11-slim
  node:22-alpine
  nginx:stable-alpine
)

base_manifest="$output_dir/base-images.tsv"
: >"$base_manifest"
printf 'short_ref\tmirror_ref\timage_id\tmirror_digest\tinternal_ref\tinternal_digest\n' \
  >>"$base_manifest"

for image in "${base_images[@]}"; do
  name=${image%%:*}
  tag=${image#*:}
  mirror_ref="$base_mirror/$image"
  internal_ref="$internal_base_registry/$name:$tag"

  if ! docker image inspect "$mirror_ref" >/dev/null 2>&1; then
    if [[ "$execute" == true ]]; then
      docker pull "$mirror_ref"
    else
      printf 'BASE_MIRROR=MISSING %s\n' "$mirror_ref"
    fi
  fi
  if ! docker image inspect "$internal_ref" >/dev/null 2>&1; then
    if [[ "$execute" != true ]]; then
      printf 'BASE_INTERNAL=MISSING %s\n' "$internal_ref"
      continue
    fi
  fi

  if [[ "$execute" == true ]]; then
    arch=$(docker image inspect --format '{{.Architecture}}' "$mirror_ref")
    [[ "$arch" == amd64 ]] || die "base image is not amd64: $mirror_ref ($arch)"
    docker tag "$mirror_ref" "$image"
    docker tag "$mirror_ref" "$internal_ref"
    if ! docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
      "$internal_ref" | grep -q "^${internal_ref%:*}@sha256:"; then
      docker push "$internal_ref"
    fi
  fi

  if docker image inspect "$mirror_ref" >/dev/null 2>&1 && \
     docker image inspect "$internal_ref" >/dev/null 2>&1; then
    image_id=$(docker image inspect --format '{{.Id}}' "$mirror_ref")
    mirror_digest=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
      "$mirror_ref" | grep "^${mirror_ref%:*}@sha256:" | tail -1)
    internal_digest=$(docker image inspect --format '{{range .RepoDigests}}{{println .}}{{end}}' \
      "$internal_ref" | grep "^${internal_ref%:*}@sha256:" | tail -1)
    [[ -n "$mirror_digest" && -n "$internal_digest" ]] || \
      die "immutable digest missing for $image"
    printf '%s\t%s\t%s\t%s\t%s\t%s\n' \
      "$image" "$mirror_ref" "$image_id" "$mirror_digest" \
      "$internal_ref" "$internal_digest" >>"$base_manifest"
    printf 'BASE_IMAGE=PASS %s %s\n' "$internal_ref" "$image_id"
  fi
done

if [[ "$execute" != true ]]; then
  [[ $(wc -l <"$base_manifest") -eq 7 ]] || \
    die "base-image preflight is incomplete; rerun with --execute"
  docker image inspect "$duckdb_source_image" >/dev/null 2>&1 || \
    die "DuckDB source image is unavailable: $duckdb_source_image"
  printf 'DEPENDENCY_PRELOAD_PREFLIGHT=PASS\n'
  exit 0
fi

stage="$output_dir/stage"
[[ "$stage" != / ]] || die "unsafe stage directory"
rm -rf "$stage"
install -d -m 0700 "$stage/packages/duckdb"
install -d -m 0755 "$stage/packages/uv"
install -d -m 0755 "$stage/packages/python-build-tools"
install -d -m 0755 "$stage/packages/docker-cli"

protoc=protoc-3.19.4-linux-x86_64.zip
grpc_probe=grpc_health_probe-linux-amd64
if [[ "$existing_cache_valid" == true && \
      -s "$source_dir/packages/$protoc" && \
      -x "$source_dir/packages/$grpc_probe" ]]; then
  cp -p "$source_dir/packages/$protoc" "$stage/packages/$protoc"
  cp -p "$source_dir/packages/$grpc_probe" "$stage/packages/$grpc_probe"
  printf 'PACKAGE_REUSED=%s\nPACKAGE_REUSED=%s\n' "$protoc" "$grpc_probe"
else
  curl -fL --retry 3 --retry-all-errors --connect-timeout 10 \
    "${github_proxy}https://github.com/protocolbuffers/protobuf/releases/download/v3.19.4/$protoc" \
    -o "$stage/packages/$protoc"
  curl -fL --retry 3 --retry-all-errors --connect-timeout 10 \
    "${github_proxy}https://github.com/grpc-ecosystem/grpc-health-probe/releases/download/v0.4.24/$grpc_probe" \
    -o "$stage/packages/$grpc_probe"
fi
chmod 0755 "$stage/packages/$grpc_probe"

python_base_ref="$internal_base_registry/python:3.10.18-bookworm"
if [[ "$existing_cache_valid" == true && \
      $(find "$source_dir/packages/uv" -maxdepth 1 -type f -name "uv-${uv_version}-*.whl" | wc -l) -eq 1 ]]; then
  cp -p "$source_dir"/packages/uv/uv-${uv_version}-*.whl "$stage/packages/uv/"
  printf 'PACKAGE_REUSED=uv-%s-wheel\n' "$uv_version"
else
  docker run --rm \
    -v "$stage/packages/uv:/packages" \
    "$python_base_ref" \
    python -m pip download --no-deps --only-binary=:all: \
      --index-url "$pip_index_url" --dest /packages "uv==$uv_version"
fi
[[ $(find "$stage/packages/uv" -maxdepth 1 -type f -name "uv-${uv_version}-*.whl" | wc -l) -eq 1 ]] || \
  die "exactly one uv wheel was not downloaded for version $uv_version"
if [[ "$existing_cache_valid" == true && \
      $(find "$source_dir/packages/python-build-tools" -maxdepth 1 -type f -name '*.whl' 2>/dev/null | wc -l) -eq 4 ]]; then
  cp -p "$source_dir"/packages/python-build-tools/*.whl "$stage/packages/python-build-tools/"
  printf 'PACKAGE_REUSED=python-build-tools\n'
else
  docker run --rm \
    -v "$stage/packages/python-build-tools:/packages" \
    "$python_base_ref" \
    python -m pip download --no-deps --only-binary=:all: \
      --index-url "$pip_index_url" --dest /packages \
      "pip==$pip_version" "setuptools==$setuptools_version" \
      "wheel==$wheel_version" "packaging==$packaging_version"
fi
[[ $(find "$stage/packages/python-build-tools" -maxdepth 1 -type f -name '*.whl' | wc -l) -eq 4 ]] || \
  die "exactly four pinned Python build-tool wheels were not downloaded"

duckdb_container="weknora-duckdb-cache-${source_head:0:12}"
if docker container inspect "$duckdb_container" >/dev/null 2>&1; then
  die "temporary DuckDB container already exists: $duckdb_container"
fi
docker create --name "$duckdb_container" "$duckdb_source_image" >/dev/null
trap 'docker rm -f "$duckdb_container" >/dev/null 2>&1 || true' EXIT
docker cp "$duckdb_container:/home/appuser/.duckdb/." "$stage/packages/duckdb/"
docker cp "$duckdb_container:/usr/local/bin/docker" "$stage/packages/docker-cli/docker"
docker rm "$duckdb_container" >/dev/null
trap - EXIT
chmod 0755 "$stage/packages/docker-cli/docker"
"$stage/packages/docker-cli/docker" --version

duckdb_root="$stage/packages/duckdb/extensions/v1.5.2/linux_amd64"
[[ -s "$duckdb_root/spatial.duckdb_extension" ]] || die "spatial extension missing"
[[ -s "$duckdb_root/excel.duckdb_extension" ]] || die "excel extension missing"
[[ -x "$stage/packages/docker-cli/docker" ]] || die "preloaded Docker CLI is missing"

(
  cd "$stage/packages"
  find . -type f ! -name SHA256SUMS.release -print0 | sort -z | \
    xargs -0 sha256sum >SHA256SUMS.release
  sha256sum -c SHA256SUMS.release
)

install -d -m 0755 "$source_dir/packages"
cp -a "$stage/packages/." "$source_dir/packages/"
(cd "$source_dir/packages" && sha256sum -c SHA256SUMS.release)

sha256sum "$base_manifest" "$source_dir/packages/SHA256SUMS.release" \
  >"$output_dir/dependency-preload-artifacts.sha256"
date -u +%FT%TZ >"$output_dir/preload.complete"
printf 'DEPENDENCY_PRELOAD=PASS\nPACKAGE_MANIFEST=%s\n' \
  "$source_dir/packages/SHA256SUMS.release"
