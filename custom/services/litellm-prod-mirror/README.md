# LiteLLM production non-GLM mirror

This local Docker service mirrors the production `llmgateway-prod` LiteLLM
configuration for diagnosing production behavior without mutating production.
The source snapshot is an internal diagnostic input; document verification date
is 2026-09-13. This README intentionally omits production hosts, network
segments, model endpoints, credentials, and captured response data.

The active config applies exactly two requested transformations to
`config.production.non-glm.yaml`: it excludes deployments that are unreachable
from local Docker, then adds the required `-local` suffix to every remaining
public `model_name`. Run the audit before every restart:

```powershell
python .\verify_mirror.py
```

The production gateway has 11 non-GLM deployment entries and 10 unique public
models. Local Docker can reach 6 entries/6 models. For
`bge-reranker-v2-m3-local`, only the reachable production backend is activated;
the other production backend is excluded. The production Anthropic thinking
patch and LiteLLM 1.92.0 runtime are mirrored as well.

## Runtime

The checked-in Compose file expects secrets through environment variables and
never stores them in Git. It publishes the mirror locally at
`http://127.0.0.1:14000` for local diagnostics only. It may use a private Docker
network alias required by the local test stack; the alias and its upstream
address are deliberately not documented here.

```powershell
docker compose --env-file .env.local up -d
```

Production is read-only for this workflow. Refreshing the snapshot means
reading the production ConfigMap/Secret metadata, updating this directory, and
rerunning `verify_mirror.py`; it never means applying local files to the
cluster.

## Reachability selection

Some production-only deployments are intentionally absent from the local active
config when the local host cannot reach the protected production network. The
production snapshot retains them only for drift audits; no replacement endpoint
or fallback is invented. Exact upstream addresses remain in protected
deployment inputs and must not be copied into public documentation.

## WeKnora registration

`provision_weknora_models.ps1` idempotently maintains nine active WeKnora
records for the six gateway aliases: two interactive chat records, two
derivative-only chat records, VLM records for Qwen3-VL and Qwen2.5-Omni, an
Omni ASR record, one embedding record, and one reranker record. Both derivative
chat records are published; Qwen3.6-27B-tool-local is the local derivative
default.

The registered chat models use
`extra_config.thinking_control=chat_template_kwargs`. Local WeKnora quick
knowledge chat sets `conversation.summary.thinking=false`, the acceptance
agents set `thinking=false`, and final-answer synthesis explicitly sets
`Thinking=false`. This keeps the tested local workflows in non-thinking mode
without altering the production gateway snapshot or its production patch.

## Business acceptance

Run the isolated workflow acceptance with a WeKnora bearer token. It creates
temporary agents, sessions, and a knowledge base, then deletes them in a
`finally` cleanup block.

```powershell
$env:WEKNORA_TOKEN = "<TENANT_BEARER_TOKEN>"
node .\business_workflow_smoke.mjs
```

The 2026-08-01 strict acceptance result is:

- `DeepSeek-V4-Flash-INT8-local`: general agent, knowledge-base RAG, and
  read-only PostgreSQL data analysis passed; every workflow emitted zero
  thinking events.
- `Qwen3.6-27B-tool-local`: general agent and data analysis passed with zero
  thinking events. Knowledge retrieval, citations, and response transport work,
  but strict factual acceptance fails reproducibly because the model adds an
  incorrect RMB conversion for the source value `360 万元`. The model remains
  registered because it is reachable; this is recorded as a model-quality risk
  rather than hidden with a fallback or relaxed assertion.
