#!/usr/bin/env bash

set -euo pipefail

repository_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
fixture_directory="$(mktemp -d)"
cleanup() {
  rm -rf -- "${fixture_directory}"
}
trap cleanup EXIT

export FAKE_KUBE_STATE="${fixture_directory}/state"
mkdir -p "${FAKE_KUBE_STATE}"
export PATH="${fixture_directory}:${PATH}"

# Git Bash/WSL on Windows exposes the WinGet binary as jq.exe, while the
# production script intentionally checks for the Linux command name `jq`.
# Provide a test-only shim so the same fixture runs on both workstations and
# Linux release hosts.
if ! command -v jq >/dev/null 2>&1 && command -v jq.exe >/dev/null 2>&1; then
  cat >"${fixture_directory}/jq" <<'JQ_SHIM'
#!/usr/bin/env bash
arguments=()
for argument in "$@"; do
  if [[ "${argument}" == /* && -e "${argument}" ]]; then
    arguments+=("$(wslpath -w "${argument}")")
  else
    arguments+=("${argument}")
  fi
done
set -o pipefail
jq.exe "${arguments[@]}" | tr -d '\r'
JQ_SHIM
  chmod +x "${fixture_directory}/jq"
fi

model_api_key="test-sensitive-model-key"
printf '%s' "${model_api_key}" | base64 >"${FAKE_KUBE_STATE}/legacy-key.b64"

cat >"${fixture_directory}/kubectl" <<'FAKE_KUBECTL'
#!/usr/bin/env bash
set -euo pipefail

if [[ "${1:-}" == "-n" ]]; then
  shift 2
fi

if [[ "${1:-}" == "get" && "${2:-}" == "deployment" ]]; then
  cat <<'JSON'
{
  "spec": {
    "template": {
      "spec": {
        "containers": [
          {
            "name": "app",
            "env": [
              {"name": "ANTHROPIC_BASE_URL", "value": "http://live-gateway:4000"},
              {"name": "ANTHROPIC_MODEL", "value": "/models/live-model"},
              {
                "name": "ANTHROPIC_API_KEY",
                "valueFrom": {
                  "secretKeyRef": {"name": "legacy-model", "key": "apiKey"}
                }
              },
              {"name": "WEKNORA_SANDBOX_ALLOW_NETWORK", "value": "true"}
            ]
          }
        ]
      }
    }
  }
}
JSON
  exit 0
fi

if [[ "${1:-}" == "get" && "${2:-}" == "secret" ]]; then
  secret_name="${3}"
  if [[ "${secret_name}" == "legacy-model" ]]; then
    encoded="$(tr -d '\r\n' <"${FAKE_KUBE_STATE}/legacy-key.b64")"
    printf '{"data":{"apiKey":"%s"}}\n' "${encoded}"
    exit 0
  fi
  if [[ -f "${FAKE_KUBE_STATE}/${secret_name}.json" ]]; then
    cat "${FAKE_KUBE_STATE}/${secret_name}.json"
    exit 0
  fi
  exit 1
fi

if [[ "${1:-}" == "create" && "${2:-}" == "secret" &&
  "${3:-}" == "generic" ]]; then
  secret_name="${4}"
  shift 4
  output='{}'
  for argument in "$@"; do
    if [[ "${argument}" == --from-file=* ]]; then
      pair="${argument#--from-file=}"
      key="${pair%%=*}"
      path="${pair#*=}"
      encoded="$(base64 <"${path}" | tr -d '\r\n')"
      output="$(
        jq -c \
          --arg key "${key}" \
          --arg value "${encoded}" \
          '. + {($key): $value}' \
          <<<"${output}"
      )"
    fi
  done
  jq -n --argjson data "${output}" '{data: $data}' \
    >"${FAKE_KUBE_STATE}/${secret_name}.json"
  exit 0
fi

echo "unsupported fake kubectl invocation: $*" >&2
exit 1
FAKE_KUBECTL
chmod +x "${fixture_directory}/kubectl"

output="$(
  bash "${repository_root}/deploy/production/capture-model-runtime-secret.sh" \
    weknora weknora-app app weknora-model-runtime 2>&1
)"

if grep -Fq "${model_api_key}" <<<"${output}"; then
  echo "capture script leaked the API key" >&2
  exit 1
fi

secret_json="${FAKE_KUBE_STATE}/weknora-model-runtime.json"
jq -e \
  --arg expected "${model_api_key}" \
  '
    (.data.ANTHROPIC_BASE_URL | @base64d) == "http://live-gateway:4000" and
    (.data.ANTHROPIC_MODEL | @base64d) == "/models/live-model" and
    (.data.ANTHROPIC_API_KEY | @base64d) == $expected and
    (.data.WEKNORA_SANDBOX_ALLOW_NETWORK | @base64d) == "true" and
    (.data | has("ANTHROPIC_AUTH_TOKEN") | not)
  ' "${secret_json}" >/dev/null

# The second run exercises the preserve-and-validate branch.
bash "${repository_root}/deploy/production/capture-model-runtime-secret.sh" \
  weknora weknora-app app weknora-model-runtime >/dev/null

echo "capture-model-runtime-secret tests passed"
