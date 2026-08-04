#!/usr/bin/env bash
#
# Copy the current production app's model/sandbox runtime environment into a
# dedicated Kubernetes Secret without printing secret values or placing them
# in command-line arguments. Run this before deleting the old app Deployment.

set -euo pipefail
set +x
umask 077

namespace="${1:-weknora}"
source_deployment="${2:-weknora-app}"
source_container="${3:-app}"
target_secret="${4:-weknora-model-runtime}"

for command_name in kubectl python3 base64 mktemp; do
  if ! command -v "${command_name}" >/dev/null 2>&1; then
    echo "required command not found: ${command_name}" >&2
    exit 1
  fi
done

validate_target_secret() {
  kubectl -n "${namespace}" get secret "${target_secret}" -o json |
    python3 -c '
import json, sys
data = json.load(sys.stdin).get("data", {})
required = ("ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "ANTHROPIC_API_KEY")
missing = [name for name in required if not data.get(name)]
if missing:
    raise SystemExit("target model runtime Secret is missing mandatory key(s): " + ",".join(missing))
' >/dev/null
}

if kubectl -n "${namespace}" get secret "${target_secret}" >/dev/null 2>&1; then
  validate_target_secret
  echo "preserving existing ${namespace}/${target_secret}; mandatory keys are present"
  exit 0
fi

# jq is needed only for the one-time conversion of a legacy Deployment. A
# release host that already has the dedicated Secret can validate and preserve
# it without installing another host package.
command -v jq >/dev/null 2>&1 || {
  echo "required command not found for legacy Deployment conversion: jq" >&2
  exit 1
}

temporary_directory="$(mktemp -d)"
cleanup() {
  rm -f -- "${temporary_directory}"/*
  rmdir -- "${temporary_directory}"
}
trap cleanup EXIT

deployment_json="${temporary_directory}/deployment.json"
kubectl -n "${namespace}" get deployment "${source_deployment}" -o json \
  >"${deployment_json}"

capture_environment_value() {
  local environment_name="$1"
  local required="$2"
  local entry
  local value
  local referenced_secret
  local referenced_key

  entry="$(
    jq -c \
      --arg container "${source_container}" \
      --arg environment "${environment_name}" \
      '[
        .spec.template.spec.containers[]
        | select(.name == $container)
        | .env[]?
        | select(.name == $environment)
      ] | if length == 1 then .[0] else null end' \
      "${deployment_json}"
  )"

  if [[ "${entry}" == "null" ]]; then
    if [[ "${required}" == "required" ]]; then
      echo \
        "current ${source_deployment}/${source_container} is missing required ${environment_name}" \
        >&2
      exit 1
    fi
    return 0
  fi

  if jq -e 'has("value")' <<<"${entry}" >/dev/null; then
    value="$(jq -r '.value' <<<"${entry}")"
  elif jq -e '.valueFrom.secretKeyRef.name and .valueFrom.secretKeyRef.key' \
    <<<"${entry}" >/dev/null; then
    referenced_secret="$(jq -r '.valueFrom.secretKeyRef.name' <<<"${entry}")"
    referenced_key="$(jq -r '.valueFrom.secretKeyRef.key' <<<"${entry}")"
    value="$(
      kubectl -n "${namespace}" get secret "${referenced_secret}" -o json |
        jq -r --arg key "${referenced_key}" '.data[$key] // empty' |
        base64 --decode
    )"
  else
    echo \
      "${environment_name} uses an unsupported env source; migrate it explicitly" \
      >&2
    exit 1
  fi

  if [[ -z "${value}" ]]; then
    if [[ "${required}" == "required" ]]; then
      echo "current ${environment_name} is empty" >&2
      exit 1
    fi
    return 0
  fi

  printf '%s' "${value}" >"${temporary_directory}/${environment_name}"
  unset value
}

capture_environment_value ANTHROPIC_BASE_URL required
capture_environment_value ANTHROPIC_MODEL required
capture_environment_value ANTHROPIC_API_KEY required
capture_environment_value ANTHROPIC_AUTH_TOKEN optional
capture_environment_value WEKNORA_SANDBOX_ALLOW_NETWORK optional
capture_environment_value WEKNORA_SANDBOX_PASSTHROUGH_ENV optional

secret_arguments=()
for value_file in "${temporary_directory}"/ANTHROPIC_* \
  "${temporary_directory}"/WEKNORA_SANDBOX_*; do
  if [[ -f "${value_file}" ]]; then
    secret_arguments+=("--from-file=$(basename "${value_file}")=${value_file}")
  fi
done

kubectl -n "${namespace}" create secret generic "${target_secret}" \
  "${secret_arguments[@]}"
validate_target_secret

echo "created ${namespace}/${target_secret} from the live app; no values were printed"
