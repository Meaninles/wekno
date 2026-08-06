#!/usr/bin/env bash
set -euo pipefail
umask 077

if [[ $# -ne 3 ]]; then
  echo "usage: $0 RELEASE_ID /secure/values-site.yaml ABSOLUTE_OUTPUT_DIR" >&2
  exit 2
fi
release_id=$1
site_values=$2
output_dir=$3
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,79}$ ]] || {
  echo "invalid RELEASE_ID" >&2
  exit 2
}
[[ -f "$site_values" ]] || { echo "site values not found: $site_values" >&2; exit 2; }
[[ "$output_dir" == /* ]] || { echo "output directory must be absolute" >&2; exit 2; }

for command_name in helm kubectl kubeconform python3 sed sha256sum timeout; do
  command -v "$command_name" >/dev/null || {
    echo "missing required command: $command_name" >&2
    exit 1
  }
done

script_dir=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)
repo_root=$(cd -- "$script_dir/../.." && pwd)
chart="$repo_root/helm"
production_values="$chart/values-production-ha.yaml"
migration_values="$script_dir/values-migration.example.yaml"
kubeconform_cache=${KUBECONFORM_CACHE_DIR:-}
kubeconform_kubernetes_version=1.25.0
kubeconform_schema_location='https://ghfast.top/https://raw.githubusercontent.com/yannh/kubernetes-json-schema/master/v1.25.0-standalone-strict/{{.ResourceKind}}{{.KindSuffix}}.json'

[[ "$kubeconform_cache" == /* && -d "$kubeconform_cache" ]] || {
  echo "KUBECONFORM_CACHE_DIR must be an existing absolute directory" >&2
  exit 1
}
[[ -s "$kubeconform_cache/SHA256SUMS" && -s "$kubeconform_cache/SCHEMA_SOURCE.txt" ]] || {
  echo "kubeconform cache audit files are missing" >&2
  exit 1
}
(cd "$kubeconform_cache" && sha256sum -c SHA256SUMS)
grep -Fxq "KUBERNETES_VERSION=$kubeconform_kubernetes_version" \
  "$kubeconform_cache/SCHEMA_SOURCE.txt" || {
    echo "kubeconform cache Kubernetes version mismatch" >&2
    exit 1
  }
grep -Fxq "SCHEMA_LOCATION=$kubeconform_schema_location" \
  "$kubeconform_cache/SCHEMA_SOURCE.txt" || {
    echo "kubeconform cache schema location mismatch" >&2
    exit 1
  }

if grep -Eq 'REPLACE_[A-Z0-9_]+' "$site_values"; then
  echo "site values still contain REPLACE_* placeholders" >&2
  exit 1
fi
if ! grep -Eq 'dockerImage:[[:space:]]+[^[:space:]]+@sha256:[0-9a-f]{64}[[:space:]]*$' "$site_values"; then
  echo "sandbox image in site values must be pinned by sha256 digest" >&2
  exit 1
fi
if [[ -d "$output_dir" ]] && find "$output_dir" -mindepth 1 -print -quit | grep -q .; then
  echo "refusing to overwrite non-empty render directory: $output_dir" >&2
  exit 1
fi
install -d -m 0700 "$output_dir"

python3 "$script_dir/validate-concurrency-plan.py" \
  "$script_dir/concurrency-plan.json" >"$output_dir/concurrency-validation.txt"

helm lint "$chart" \
  -f "$production_values" \
  -f "$site_values" \
  --set-string app.runtimeRoles.migration.render=false \
  >"$output_dir/helm-lint.txt"

helm template weknora "$chart" \
  --namespace weknora \
  -f "$production_values" \
  -f "$site_values" \
  --set-string app.runtimeRoles.migration.render=false \
  --set ingress.enabled=false \
  >"$output_dir/workloads.yaml"

helm template weknora "$chart" \
  --namespace weknora \
  -f "$production_values" \
  -f "$site_values" \
  --set-string app.runtimeRoles.migration.render=false \
  --set app.runtimeRoles.startSuspended=true \
  --set ingress.enabled=false \
  >"$output_dir/workloads-gated.yaml"

helm template weknora "$chart" \
  --namespace weknora \
  -f "$production_values" \
  -f "$migration_values" \
  -f "$site_values" \
  --show-only templates/app-migration-job.yaml \
  >"$output_dir/migration-job.yaml"

# Some operator-host Helm builds emit CRLF even on Linux. Normalize the three
# immutable release artifacts before exact kind/image checks and hashing.
sed -i 's/\r$//' \
  "$output_dir/workloads.yaml" \
  "$output_dir/workloads-gated.yaml" \
  "$output_dir/migration-job.yaml"

if grep -Eq 'REPLACE_[A-Z0-9_]+' \
  "$output_dir/workloads.yaml" "$output_dir/workloads-gated.yaml" "$output_dir/migration-job.yaml"; then
  echo "rendered manifests still contain REPLACE_* placeholders" >&2
  exit 1
fi

python3 "$script_dir/validate-rendered-manifest.py" \
  normal "$output_dir/workloads.yaml" >"$output_dir/topology-normal.txt"
python3 "$script_dir/validate-rendered-manifest.py" \
  gated "$output_dir/workloads-gated.yaml" >"$output_dir/topology-gated.txt"
python3 "$script_dir/validate-rendered-manifest.py" \
  migration "$output_dir/migration-job.yaml" --release-id "$release_id" \
  >"$output_dir/topology-migration.txt"

mapfile -t workload_kinds < <(awk '$1 == "kind:" {print $2}' "$output_dir/workloads.yaml")
for forbidden in Ingress Job Secret StatefulSet PersistentVolume PersistentVolumeClaim; do
  if printf '%s\n' "${workload_kinds[@]}" | grep -Fxq "$forbidden"; then
    echo "workload manifest contains forbidden kind: $forbidden" >&2
    exit 1
  fi
done
mapfile -t gated_kinds < <(awk '$1 == "kind:" {print $2}' "$output_dir/workloads-gated.yaml")
for forbidden in Job Secret StatefulSet PersistentVolume PersistentVolumeClaim; do
  if printf '%s\n' "${gated_kinds[@]}" | grep -Fxq "$forbidden"; then
    echo "gated workload manifest contains forbidden kind: $forbidden" >&2
    exit 1
  fi
done
mapfile -t migration_kinds < <(awk '$1 == "kind:" {print $2}' "$output_dir/migration-job.yaml")
if [[ ${#migration_kinds[@]} -ne 1 || "${migration_kinds[0]}" != Job ]]; then
  echo "migration render must contain exactly one Job" >&2
  exit 1
fi

mapfile -t workload_images < <(awk '$1 == "image:" {gsub(/"/, "", $2); print $2}' "$output_dir/workloads.yaml")
mapfile -t gated_images < <(awk '$1 == "image:" {gsub(/"/, "", $2); print $2}' "$output_dir/workloads-gated.yaml")
mapfile -t migration_images < <(awk '$1 == "image:" {gsub(/"/, "", $2); print $2}' "$output_dir/migration-job.yaml")
for image in "${workload_images[@]}" "${gated_images[@]}" "${migration_images[@]}"; do
  [[ "$image" =~ @sha256:[0-9a-f]{64}$ ]] || {
    echo "container image is not pinned by sha256 digest: $image" >&2
    exit 1
  }
done

kubectl apply --dry-run=client --validate=false -f "$output_dir/migration-job.yaml" \
  >"$output_dir/kubectl-dry-run-migration.txt"
kubectl apply --dry-run=client --validate=false -f "$output_dir/workloads.yaml" \
  >"$output_dir/kubectl-dry-run-workloads.txt"
kubectl apply --dry-run=client --validate=false -f "$output_dir/workloads-gated.yaml" \
  >"$output_dir/kubectl-dry-run-workloads-gated.txt"

timeout 60 kubectl apply --dry-run=server --validate=true \
  -f "$output_dir/migration-job.yaml" >"$output_dir/kubectl-server-dry-run-migration.txt"
timeout 60 kubectl apply --dry-run=server --validate=true \
  -f "$output_dir/workloads.yaml" >"$output_dir/kubectl-server-dry-run-workloads.txt"
timeout 60 kubectl apply --dry-run=server --validate=true \
  -f "$output_dir/workloads-gated.yaml" >"$output_dir/kubectl-server-dry-run-workloads-gated.txt"

# Deliberately poison every proxy. A missing schema must fail immediately
# instead of reaching the Internet during the maintenance window.
env \
  HTTP_PROXY=http://127.0.0.1:9 HTTPS_PROXY=http://127.0.0.1:9 \
  ALL_PROXY=http://127.0.0.1:9 NO_PROXY= \
  http_proxy=http://127.0.0.1:9 https_proxy=http://127.0.0.1:9 \
  all_proxy=http://127.0.0.1:9 no_proxy= \
  timeout 60 kubeconform -n 1 -strict -summary \
  -kubernetes-version "$kubeconform_kubernetes_version" \
  -cache "$kubeconform_cache" \
  -schema-location "$kubeconform_schema_location" \
  "$output_dir/migration-job.yaml" "$output_dir/workloads-gated.yaml" \
  "$output_dir/workloads.yaml" \
  >"$output_dir/kubeconform.txt"

sha256sum \
  "$site_values" \
  "$script_dir/concurrency-plan.json" \
  "$kubeconform_cache/SHA256SUMS" \
  "$kubeconform_cache/SCHEMA_SOURCE.txt" \
  "$output_dir/migration-job.yaml" \
  "$output_dir/workloads-gated.yaml" \
  "$output_dir/workloads.yaml" \
  >"$output_dir/release-inputs.sha256"
chmod -R go-rwx "$output_dir"
echo "release manifests rendered and validated: $output_dir"
