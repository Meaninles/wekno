# WeKnora Helm Chart

[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/weknora)](https://artifacthub.io/packages/helm/weknora/weknora)
[![License](https://img.shields.io/badge/License-MIT-blue.svg)](https://opensource.org/licenses/MIT)

Helm chart for deploying [WeKnora](https://github.com/Tencent/WeKnora) - an AI-powered Knowledge RAG Platform.

## Overview

WeKnora is an intelligent knowledge base platform that combines:
- Document parsing and understanding
- Vector search with BM25 hybrid retrieval
- LLM integration for conversational AI
- Multi-tenant support with encryption

## Prerequisites

- Kubernetes 1.25+
- Helm 3.10+
- PV provisioner support in the underlying infrastructure
- Ingress controller (nginx-ingress recommended) for external access

## Quick Start

```bash
# Add required secrets
helm install weknora ./helm \
  --namespace weknora \
  --create-namespace \
  --set secrets.dbPassword=<your-db-password> \
  --set secrets.redisPassword=<your-redis-password> \
  --set secrets.jwtSecret=<your-jwt-secret>
```

## Architecture

```
                    ┌─────────────┐
                    │   Ingress   │
                    └──────┬──────┘
                           │
           ┌───────────────┴───────────────┐
           │                               │
           ▼                               ▼
    ┌─────────────┐                 ┌─────────────┐
    │  Frontend   │                 │   Backend   │
    │  (Vue.js)   │                 │   (Go/Gin)  │
    └─────────────┘                 └──────┬──────┘
                                           │
                    ┌──────────────────────┼──────────────────────┐
                    │                      │                      │
                    ▼                      ▼                      ▼
             ┌─────────────┐        ┌─────────────┐        ┌─────────────┐
             │  Docreader  │        │  PostgreSQL │        │    Redis    │
             │   (gRPC)    │        │  (ParadeDB) │        │   (Queue)   │
             └─────────────┘        └─────────────┘        └─────────────┘
```

## Installation

### Basic Installation

```bash
helm install weknora ./helm \
  --namespace weknora \
  --create-namespace \
  --set secrets.dbPassword=secure-password \
  --set secrets.redisPassword=secure-password \
  --set secrets.jwtSecret=$(openssl rand -base64 32)
```

### With Ingress

```bash
helm install weknora ./helm \
  --namespace weknora \
  --create-namespace \
  --set ingress.enabled=true \
  --set ingress.host=weknora.example.com \
  --set ingress.tls.enabled=true \
  --set ingress.tls.secretName=weknora-tls \
  --set secrets.dbPassword=secure-password \
  --set secrets.redisPassword=secure-password \
  --set secrets.jwtSecret=$(openssl rand -base64 32)
```

### With External LLM (Ollama)

```bash
helm install weknora ./helm \
  --namespace weknora \
  --create-namespace \
  --set app.extraEnv[0].name=OLLAMA_BASE_URL \
  --set app.extraEnv[0].value=http://ollama.ollama:11434 \
  --set app.extraEnv[1].name=INIT_LLM_MODEL_NAME \
  --set app.extraEnv[1].value=qwen2.5:7b \
  --set secrets.dbPassword=secure-password \
  --set secrets.redisPassword=secure-password \
  --set secrets.jwtSecret=$(openssl rand -base64 32)
```

### Production Installation

For production, use a values file:

```yaml
# values-production.yaml
global:
  storageClass: "fast-ssd"

app:
  replicaCount: 3
  env:
    STORAGE_TYPE: obs
  extraEnv:
    - name: OBS_PATH_PREFIX
      value: weknora/__weknora_private_knowledge_objects_v1__/deployment/prod-cce-wk-6a9d12b0/namespace/6a9d12b0-48d2-46b4-9e40-1a407860838d/
  topologySpread:
    whenUnsatisfiable: DoNotSchedule
  podDisruptionBudget:
    enabled: true
    minAvailable: 2
  resources:
    requests:
      cpu: 500m
      memory: 1Gi
    limits:
      cpu: 2
      memory: 4Gi

# Every app replica is a complete document-processing worker. The administrator
# setting `asynq.concurrency` is the per-replica complete-document concurrency.
docreader:
  replicaCount: 3
  service:
    headless: true
  topologySpread:
    whenUnsatisfiable: DoNotSchedule
  podDisruptionBudget:
    enabled: true
    minAvailable: 2

# Production horizontal scaling uses a private object store for all durable
# objects. Each purpose has a disjoint deployment-scoped namespace; do not use
# a generic `weknora/` root and do not reuse the namespace UUID in another
# cluster, restore drill, or test environment.
#
# Claude SDK original-input transfer objects use only:
#   <original-input-prefix>/temp/<tenant-id>/<transfer-uuid>.<ext>
# They never contain source filenames or usernames. The app deletes them after
# success, failure, cancellation, or panic. Configure an OBS lifecycle rule
# scoped only to this original-input prefix with an expiration of at most 24
# hours as the hard-kill fallback; never apply that rule to knowledge objects
# or final Agent artifacts.

# Local storage is disposable parse/Agent scratch only. The production profile
# does not require or mount an RWX volume.
dataFiles:
  persistence:
    enabled: false

postgresql:
  persistence:
    size: 100Gi

ingress:
  enabled: true
  host: weknora.company.com
  tls:
    enabled: true
    secretName: weknora-tls

mobileWeb:
  enabled: true
  image:
    repository: registry.company.com/weknora-mobile-web
    tag: v0.6.3

secrets:
  existingSecret: weknora-secrets  # Use pre-created secret
```

```bash
helm install weknora ./helm \
  --namespace weknora \
  --create-namespace \
  -f values-production.yaml
```

## Configuration

### Global Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `global.storageClass` | Storage class for PVCs | `""` |
| `global.imagePullSecrets` | Image pull secrets | `[]` |
| `global.podSecurityContext` | Pod security context | See values.yaml |
| `global.containerSecurityContext` | Container security context | See values.yaml |

### ServiceAccount

| Parameter | Description | Default |
|-----------|-------------|---------|
| `serviceAccount.create` | Create ServiceAccount | `true` |
| `serviceAccount.name` | ServiceAccount name | `""` |
| `serviceAccount.annotations` | ServiceAccount annotations | `{}` |
| `serviceAccount.automountServiceAccountToken` | Automount a general token on every component (the app verifier uses a dedicated projected token instead) | `false` |

### App (Backend)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `app.enabled` | Enable backend | `true` |
| `app.replicaCount` | Number of replicas | `1` |
| `app.image.repository` | Image repository | `wechatopenai/weknora-app` |
| `app.image.tag` | Image tag | `""` (uses appVersion) |
| `app.resources` | Resource limits | See values.yaml |
| `app.env` | Environment variables | See values.yaml |
| `app.extraEnv` | Additional env vars | `[]` |
| `app.extraVolumes` / `app.extraVolumeMounts` | Mount external database/Redis CA or mTLS Secrets | `[]` |
| `app.connections.database.ssl.mode` | PostgreSQL TLS mode; use `verify-full` in production | `disable` |
| `app.connections.database.ssl.rootCertFile` | Mounted PostgreSQL CA path | `""` |
| `app.connections.database.ssl.certFile` / `keyFile` | Optional PostgreSQL mTLS identity | `""` |
| `app.connections.redis.tls.enabled` | Enable TLS for Redis | `false` |
| `app.connections.redis.tls.caFile` | Mounted Redis CA path | `""` |
| `app.connections.redis.tls.certFile` / `keyFile` | Optional Redis mTLS identity | `""` |
| `app.connections.redis.tls.serverName` | Redis certificate DNS name | `""` |
| `app.documentQueue.kubernetesRuntimeVerifier.enabled` | Verify exact terminated Pod UIDs before automatic cross-Pod takeover | `true` |
| `app.documentQueue.kubernetesRuntimeVerifier.containerName` | App container whose current terminated state is authoritative | `app` |
| `app.env.WEKNORA_ASYNQ_CONCURRENCY` | Complete document workflows admitted per app replica (runtime System Admin setting takes precedence) | `"4"` |
| `app.env.WEKNORA_WIKI_MAP_TASK_CONCURRENCY` | Document-local Wiki Map consumers per app replica, isolated from ordinary derivatives | `"4"` |

The app replicas share one durable PostgreSQL document-workflow outbox and one
Redis delivery queue. Scaling `app.replicaCount` adds complete document workers;
idle replicas claim globally waiting documents. A process restart with the same
pod identity resumes its leases, while an expired lease from a failed/replaced
pod is reassigned only after the delivery is confirmed inactive and the old
boot has a runtime-backed termination proof. A normal SIGTERM publishes that
proof after local handlers drain; an in-Pod container restart keeps the Pod UID
and atomically adopts the old boot. By default, healthy replicas use a narrowly
scoped projected service-account token to list Pods only in their namespace,
match the exact immutable UID encoded in `instance_id`, and accept only the
current `app` container's `terminated` state, including its immutable
`containerID` and `finishedAt`. A terminal Pod phase by itself is not accepted. A
`deletionTimestamp`, missing UID/404, heartbeat age, or lease expiry is never
termination proof. When Kubernetes cannot retain an explicit terminal status
(notably some node-partition/forced-deletion cases), fence the node/runtime and
use the SystemAdmin-only
`POST /api/v1/custom/document-queue/instances/termination-attestation` endpoint
with the exact old `instance_id` and `boot_id`. Do not use pod-local file
storage with multiple replicas.

Every app replica also consumes the isolated `wiki_map` lane. Its per-replica
consumer count scales with app replicas, while shared Redis model admission
still caps aggregate provider/model/tenant concurrency. Keeping this lane
separate prevents a large Wiki backlog from occupying the generic derivative
worker pool.

### DocReader

| Parameter | Description | Default |
|-----------|-------------|---------|
| `docreader.enabled` | Enable document parser | `true` |
| `docreader.replicaCount` | Number of parser replicas | `1` |
| `docreader.service.headless` | Expose all parser endpoints for gRPC round-robin | `true` |

For worker high availability, run at least two app replicas and two DocReader
replicas across failure domains, with disruption budgets enabled. PostgreSQL,
Redis, object storage and the selected vector/graph/model
providers must also use their own HA deployments; worker scaling does not make
those external dependencies highly available. The production HA values file
uses PostgreSQL `verify-full` and Redis TLS and mounts their CA Secrets; replace
the example service names and certificates with the endpoints you already
operate rather than disabling verification.

### Frontend

| Parameter | Description | Default |
|-----------|-------------|---------|
| `frontend.enabled` | Enable frontend | `true` |
| `frontend.replicaCount` | Number of replicas | `1` |
| `frontend.image.repository` | Image repository | `wechatopenai/weknora-ui` |
| `frontend.image.tag` | Image tag | `latest` |

### Mobile Web

When `mobileWeb.enabled=true`, the chart creates a `mobile-web` Deployment/Service
and adds an Ingress path `/mobile` before the desktop frontend catch-all. The
mobile image must be built from `frontend/Dockerfile.mobile`; it serves the SPA
under `/mobile/` and static assets under `/mobile/assets/`.

Chat share links use the shared path `/share/chat/:token`. In the default chart
Ingress, `/share/` is handled by the desktop frontend catch-all and the page
switches between desktop and mobile layouts by viewport. If you customize the
Ingress to route `/share/` to `mobile-web` instead, also route `/assets/` to
`mobile-web`; the mobile image includes the desktop SPA at `/` specifically for
that deployment shape. Set the app env `FRONTEND_BASE_URL` to the public origin
if the create-share API should return absolute URLs; otherwise it returns a
host-relative `/share/chat/:token` path.

| Parameter | Description | Default |
|-----------|-------------|---------|
| `mobileWeb.enabled` | Enable mobile web mounted at `/mobile/` | `false` |
| `mobileWeb.replicaCount` | Number of replicas | `1` |
| `mobileWeb.image.repository` | Image repository | `weknora-mobile-web` |
| `mobileWeb.image.tag` | Image tag | `local` |
| `mobileWeb.service.port` | Service port | `80` |
| `mobileWeb.appHost` | Backend app service host | `app` |
| `mobileWeb.appPort` | Backend app service port | `8080` |

### PostgreSQL (ParadeDB)

| Parameter | Description | Default |
|-----------|-------------|---------|
| `postgresql.enabled` | Enable PostgreSQL | `true` |
| `postgresql.image.repository` | Image repository | `paradedb/paradedb` |
| `postgresql.image.tag` | Image tag | `v0.18.9-pg17` |
| `postgresql.persistence.enabled` | Enable persistence | `true` |
| `postgresql.persistence.size` | PVC size | `10Gi` |

### Redis

| Parameter | Description | Default |
|-----------|-------------|---------|
| `redis.enabled` | Enable Redis | `true` |
| `redis.image.repository` | Image repository | `redis` |
| `redis.image.tag` | Image tag | `7-alpine` |
| `redis.persistence.enabled` | Enable persistence | `true` |
| `redis.persistence.size` | PVC size | `1Gi` |

### Uploaded File Storage

| Parameter | Description | Default |
|-----------|-------------|---------|
| `dataFiles.persistence.enabled` | Persist uploaded files | `true` |
| `dataFiles.persistence.size` | PVC size | `10Gi` |
| `dataFiles.persistence.accessModes` | PVC access modes | `[ReadWriteOnce]` |

### Ingress

| Parameter | Description | Default |
|-----------|-------------|---------|
| `ingress.enabled` | Enable ingress | `false` |
| `ingress.className` | Ingress class | `nginx` |
| `ingress.host` | Hostname | `weknora.example.com` |
| `ingress.tls.enabled` | Enable TLS | `false` |
| `ingress.tls.secretName` | TLS secret name | `""` |

### Secrets

| Parameter | Description | Default |
|-----------|-------------|---------|
| `secrets.dbUser` | Database username | `postgres` |
| `secrets.dbPassword` | Database password | `""` (required) |
| `secrets.dbName` | Database name | `weknora` |
| `secrets.redisPassword` | Redis password | `""` (required) |
| `secrets.jwtSecret` | JWT signing secret | `""` (required) |
| `secrets.existingSecret` | Use existing secret | `""` |

### Optional Components

These map to docker-compose profiles:

| Parameter | Description | Default |
|-----------|-------------|---------|
| `minio.enabled` | Enable MinIO storage | `false` |
| `neo4j.enabled` | Enable Neo4j (GraphRAG) | `false` |
| `qdrant.enabled` | Enable Qdrant vector DB | `false` |

## Security Best Practices

### Secret Management

**Never commit secrets to Git!** Use one of these approaches:

1. **Helm --set flags** (for testing)
   ```bash
   helm install weknora ./helm --set secrets.dbPassword=xxx
   ```

2. **External Secrets Operator** (recommended for production)
   ```yaml
   secrets:
     existingSecret: weknora-external-secret
   ```

3. **Sealed Secrets** (for GitOps)
   ```bash
   kubeseal < secret.yaml > sealed-secret.yaml
   ```

### Pod Security

The chart follows CNCF security best practices:
- Runs as non-root user
- Read-only root filesystem where possible
- Drops all capabilities
- Uses seccomp profiles

## Upgrading

```bash
helm upgrade weknora ./helm \
  --namespace weknora \
  --reuse-values
```

## Uninstalling

```bash
helm uninstall weknora --namespace weknora

# Optional: Remove PVCs
kubectl delete pvc -n weknora -l app.kubernetes.io/instance=weknora
```

## Troubleshooting

### Check Pod Status
```bash
kubectl get pods -n weknora
```

### View Logs
```bash
# Backend logs
kubectl logs -n weknora -l app.kubernetes.io/component=app -f

# Frontend logs
kubectl logs -n weknora -l app.kubernetes.io/component=frontend -f
```

### Common Issues

**Pod stuck in Pending**
- Check if PVCs are bound: `kubectl get pvc -n weknora`
- Verify storage class exists: `kubectl get sc`

**Connection refused errors**
- Wait for all pods to be Ready
- Check service endpoints: `kubectl get endpoints -n weknora`

**Database connection errors**
- Verify secrets are correct
- Check PostgreSQL logs: `kubectl logs -n weknora -l app.kubernetes.io/component=database`

## Contributing

See [CONTRIBUTING.md](https://github.com/Tencent/WeKnora/blob/main/CONTRIBUTING.md) in the main repository.

## References

This Helm chart follows best practices from:
- [Helm Best Practices](https://helm.sh/docs/chart_best_practices/)
- [ArgoCD Helm Chart](https://github.com/argoproj/argo-helm)
- [Prometheus Helm Charts](https://github.com/prometheus-community/helm-charts)
- [cert-manager Helm Chart](https://github.com/cert-manager/cert-manager)

## License

This chart is licensed under the MIT License - see the [LICENSE](https://github.com/Tencent/WeKnora/blob/main/LICENSE) file for details.
