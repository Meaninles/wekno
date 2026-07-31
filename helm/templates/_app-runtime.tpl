{{/*
Shared environment for split Go runtime roles. Keeping this in one helper is
important: workers construct the same dependency graph as the API process and
therefore need the same database, object-store, model, and encryption config.
*/}}
{{- define "weknora.appRuntimeEnv" -}}
{{- $root := .root -}}
{{- $role := .role -}}
{{- $pool := .pool -}}
{{- $autoMigrate := .autoMigrate | default false -}}
- name: POD_NAMESPACE
  valueFrom:
    fieldRef:
      fieldPath: metadata.namespace
- name: POD_NAME
  valueFrom:
    fieldRef:
      fieldPath: metadata.name
- name: POD_UID
  valueFrom:
    fieldRef:
      fieldPath: metadata.uid
- name: CUSTOM_DOCUMENT_QUEUE_INSTANCE_ID
  value: "k8s/$(POD_NAMESPACE)/$(POD_UID)"
- name: CUSTOM_DOCUMENT_QUEUE_TRUST_STABLE_INSTANCE_RESTART
  value: "true"
- name: WEKNORA_RUNTIME_ROLE
  value: {{ $role | quote }}
- name: CUSTOM_RUNTIME_ENV
  value: {{ $root.Values.app.runtimeRoles.environment | quote }}
- name: CUSTOM_RUNTIME_ROLE_ENFORCED
  value: "true"
- name: CUSTOM_DATABASE_POOL_BUDGET_ENFORCED
  value: "true"
- name: CUSTOM_POSTGRES_MAX_CONNECTIONS
  value: {{ $root.Values.app.runtimeRoles.postgresMaxConnections | quote }}
- name: CUSTOM_RUNTIME_API_REPLICAS
  value: {{ $root.Values.app.replicaCount | quote }}
- name: CUSTOM_RUNTIME_PARSE_WORKER_REPLICAS
  value: {{ ternary (toString $root.Values.app.runtimeRoles.parseWorker.replicaCount) "0" $root.Values.app.runtimeRoles.parseWorker.enabled | quote }}
- name: CUSTOM_RUNTIME_DERIVATIVE_WORKER_REPLICAS
  value: {{ ternary (toString $root.Values.app.runtimeRoles.derivativeWorker.replicaCount) "0" $root.Values.app.runtimeRoles.derivativeWorker.enabled | quote }}
- name: CUSTOM_RUNTIME_WIKI_WORKER_REPLICAS
  value: {{ ternary (toString $root.Values.app.runtimeRoles.wikiWorker.replicaCount) "0" $root.Values.app.runtimeRoles.wikiWorker.enabled | quote }}
- name: CUSTOM_RUNTIME_MAINTENANCE_REPLICAS
  value: {{ ternary (toString $root.Values.app.runtimeRoles.maintenance.replicaCount) "0" $root.Values.app.runtimeRoles.maintenance.enabled | quote }}
- name: CUSTOM_RUNTIME_MIGRATION_REPLICAS
  value: {{ ternary "1" "0" $root.Values.app.runtimeRoles.migration.enabled | quote }}
- name: CUSTOM_RUNTIME_API_MAX_OPEN_CONNS
  value: {{ $root.Values.app.env.CUSTOM_DATABASE_POOL_MAX_OPEN_CONNS | quote }}
- name: CUSTOM_RUNTIME_PARSE_WORKER_MAX_OPEN_CONNS
  value: {{ $root.Values.app.runtimeRoles.parseWorker.maxOpenConns | quote }}
- name: CUSTOM_RUNTIME_DERIVATIVE_WORKER_MAX_OPEN_CONNS
  value: {{ $root.Values.app.runtimeRoles.derivativeWorker.maxOpenConns | quote }}
- name: CUSTOM_RUNTIME_WIKI_WORKER_MAX_OPEN_CONNS
  value: {{ $root.Values.app.runtimeRoles.wikiWorker.maxOpenConns | quote }}
- name: CUSTOM_RUNTIME_MAINTENANCE_MAX_OPEN_CONNS
  value: {{ $root.Values.app.runtimeRoles.maintenance.maxOpenConns | quote }}
- name: CUSTOM_RUNTIME_MIGRATION_MAX_OPEN_CONNS
  value: {{ $root.Values.app.runtimeRoles.migration.maxOpenConns | quote }}
- name: CUSTOM_RUNTIME_API_ROLLING_MAX
  value: "0"
- name: CUSTOM_RUNTIME_PARSE_WORKER_ROLLING_MAX
  value: "0"
- name: CUSTOM_RUNTIME_DERIVATIVE_WORKER_ROLLING_MAX
  value: "0"
- name: CUSTOM_RUNTIME_WIKI_WORKER_ROLLING_MAX
  value: "0"
- name: CUSTOM_RUNTIME_MAINTENANCE_ROLLING_MAX
  value: "0"
- name: CUSTOM_RUNTIME_MIGRATION_ROLLING_MAX
  value: "0"
- name: CUSTOM_DATABASE_POOL_MAX_OPEN_CONNS
  value: {{ $pool.maxOpenConns | quote }}
- name: CUSTOM_DATABASE_POOL_MAX_IDLE_CONNS
  value: {{ $pool.maxIdleConns | quote }}
- name: AUTO_MIGRATE
  value: {{ ternary "true" "false" $autoMigrate | quote }}
- name: CUSTOM_AUTO_MIGRATE
  value: {{ ternary "true" "false" $autoMigrate | quote }}
- name: GIN_MODE
  value: {{ $root.Values.app.env.GIN_MODE | quote }}
- name: FRONTEND_BASE_URL
  value: {{ $root.Values.app.env.FRONTEND_BASE_URL | quote }}
- name: TZ
  value: {{ $root.Values.app.env.TZ | quote }}
- name: DB_DRIVER
  value: "postgres"
- name: DB_HOST
  value: {{ $root.Values.app.connections.database.host | quote }}
- name: DB_PORT
  value: {{ $root.Values.app.connections.database.port | quote }}
- name: DB_USER
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: DB_USER
- name: DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: DB_PASSWORD
- name: DB_NAME
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: DB_NAME
- name: DB_SSLMODE
  value: {{ $root.Values.app.connections.database.ssl.mode | quote }}
- name: DB_SSLROOTCERT
  value: {{ $root.Values.app.connections.database.ssl.rootCertFile | quote }}
- name: DB_SSLCERT
  value: {{ $root.Values.app.connections.database.ssl.certFile | quote }}
- name: DB_SSLKEY
  value: {{ $root.Values.app.connections.database.ssl.keyFile | quote }}
- name: REDIS_ADDR
  value: {{ $root.Values.app.connections.redis.address | quote }}
- name: REDIS_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: REDIS_USERNAME
      optional: true
- name: REDIS_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: REDIS_PASSWORD
- name: REDIS_DB
  value: {{ $root.Values.app.connections.redis.db | quote }}
- name: REDIS_PREFIX
  value: {{ $root.Values.app.connections.redis.prefix | quote }}
- name: WEKNORA_REDIS_NAMESPACE
  value: {{ $root.Values.app.runtimeRoles.redisNamespace | quote }}
- name: REDIS_TLS_ENABLED
  value: {{ $root.Values.app.connections.redis.tls.enabled | quote }}
- name: REDIS_TLS_CA_FILE
  value: {{ $root.Values.app.connections.redis.tls.caFile | quote }}
- name: REDIS_TLS_CERT_FILE
  value: {{ $root.Values.app.connections.redis.tls.certFile | quote }}
- name: REDIS_TLS_KEY_FILE
  value: {{ $root.Values.app.connections.redis.tls.keyFile | quote }}
- name: REDIS_TLS_SERVER_NAME
  value: {{ $root.Values.app.connections.redis.tls.serverName | quote }}
- name: REDIS_TLS_INSECURE_SKIP_VERIFY
  value: {{ $root.Values.app.connections.redis.tls.insecureSkipVerify | quote }}
- name: STREAM_MANAGER_TYPE
  value: {{ $root.Values.app.env.STREAM_MANAGER_TYPE | quote }}
- name: JWT_SECRET
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: JWT_SECRET
- name: TENANT_AES_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: TENANT_AES_KEY
- name: SYSTEM_AES_KEY
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: SYSTEM_AES_KEY
- name: RETRIEVE_DRIVER
  value: {{ $root.Values.app.env.RETRIEVE_DRIVER | quote }}
- name: STORAGE_TYPE
  value: {{ $root.Values.app.env.STORAGE_TYPE | quote }}
- name: LOCAL_STORAGE_BASE_DIR
  value: {{ $root.Values.app.env.LOCAL_STORAGE_BASE_DIR | quote }}
- name: CUSTOM_SKILLHUB_PROFESSIONAL_STORAGE_PROVIDER
  value: {{ $root.Values.app.professionalSkillStorage.provider | quote }}
- name: CUSTOM_SKILLHUB_PROFESSIONAL_BUCKET
  value: {{ $root.Values.app.professionalSkillStorage.bucket | quote }}
- name: CUSTOM_SKILLHUB_PROFESSIONAL_PATH_PREFIX
  value: {{ $root.Values.app.professionalSkillStorage.pathPrefix | quote }}
- name: DOCREADER_ADDR
  value: {{ $root.Values.app.connections.docreader.address | quote }}
- name: AUTO_RECOVER_DIRTY
  value: {{ $root.Values.app.env.AUTO_RECOVER_DIRTY | quote }}
- name: CONCURRENCY_POOL_SIZE
  value: {{ $root.Values.app.env.CONCURRENCY_POOL_SIZE | quote }}
- name: WEKNORA_ASYNQ_CONCURRENCY
  value: {{ $root.Values.app.env.WEKNORA_ASYNQ_CONCURRENCY | quote }}
- name: WEKNORA_WIKI_MAP_TASK_CONCURRENCY
  value: {{ $root.Values.app.env.WEKNORA_WIKI_MAP_TASK_CONCURRENCY | quote }}
- name: ENABLE_GRAPH_RAG
  value: {{ $root.Values.app.env.ENABLE_GRAPH_RAG | quote }}
- name: WEKNORA_SANDBOX_MODE
  value: {{ $root.Values.app.sandbox.mode | quote }}
- name: WEKNORA_SANDBOX_TIMEOUT
  value: {{ $root.Values.app.sandbox.timeoutSeconds | quote }}
- name: WEKNORA_SANDBOX_DOCKER_IMAGE
  value: {{ $root.Values.app.sandbox.dockerImage | quote }}
- name: NEO4J_ENABLE
  value: {{ ternary "true" "false" (or $root.Values.neo4j.enabled (eq (lower (toString $root.Values.app.env.ENABLE_GRAPH_RAG)) "true")) | quote }}
- name: WEKNORA_METRICS_ENABLED
  value: {{ $root.Values.metrics.enabled | quote }}
{{- if $root.Values.app.documentQueue.kubernetesRuntimeVerifier.enabled }}
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_RUNTIME_VERIFIER_ENABLED
  value: "true"
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_API_SERVER
  value: {{ $root.Values.app.documentQueue.kubernetesRuntimeVerifier.apiServer | quote }}
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_CONTAINER_NAME
  value: app
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_REQUEST_TIMEOUT
  value: {{ $root.Values.app.documentQueue.kubernetesRuntimeVerifier.requestTimeout | quote }}
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_TOKEN_FILE
  value: /var/run/secrets/weknora-document-queue-kubernetes/token
- name: CUSTOM_DOCUMENT_QUEUE_KUBERNETES_CA_FILE
  value: /var/run/secrets/weknora-document-queue-kubernetes/ca.crt
{{- end }}
{{- if $root.Values.app.agentIntegration.enabled }}
- name: CUSTOM_GENERAL_AGENT_URL
  value: {{ $root.Values.app.agentIntegration.generalAgentURL | quote }}
- name: CUSTOM_DOCUMENT_PROCESSING_AGENT_URL
  value: {{ $root.Values.app.agentIntegration.documentProcessingAgentURL | quote }}
- name: CUSTOM_GENERAL_AGENT_TOOL_CALLBACK_URL
  value: {{ $root.Values.app.agentIntegration.toolCallbackURL | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_URL
  value: {{ $root.Values.app.agentIntegration.artifactUploadURL | quote }}
- name: CUSTOM_GENERAL_AGENT_API_KEY
  valueFrom:
    secretKeyRef:
      name: {{ $root.Values.app.agentIntegration.apiKeySecret.name | quote }}
      key: {{ $root.Values.app.agentIntegration.apiKeySecret.key | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_STORAGE_PROVIDER
  value: {{ $root.Values.app.agentIntegration.artifactStorage.provider | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_BUCKET
  value: {{ $root.Values.app.agentIntegration.artifactStorage.bucket | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_PATH_PREFIX
  value: {{ $root.Values.app.agentIntegration.artifactStorage.pathPrefix | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_MIGRATION_ALLOW_MISSING
  value: {{ $root.Values.app.agentIntegration.artifactStorage.migrationAllowMissing | quote }}
- name: CUSTOM_GENERAL_AGENT_ARTIFACT_MIGRATION_ALLOW_INVALID
  value: {{ $root.Values.app.agentIntegration.artifactStorage.migrationAllowInvalid | quote }}
{{- end }}
{{- if or $root.Values.neo4j.enabled (eq (lower (toString $root.Values.app.env.ENABLE_GRAPH_RAG)) "true") }}
- name: NEO4J_URI
  value: {{ $root.Values.app.connections.neo4j.uri | quote }}
- name: NEO4J_USERNAME
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: NEO4J_USERNAME
- name: NEO4J_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "weknora.secretName" $root }}
      key: NEO4J_PASSWORD
{{- end }}
{{- with $root.Values.app.extraEnv }}
{{ toYaml . }}
{{- end }}
{{- end }}

{{- define "weknora.appRuntimeVolumes" -}}
{{- $root := . -}}
- name: data-files
  {{- if $root.Values.dataFiles.persistence.enabled }}
  persistentVolumeClaim:
    claimName: {{ $root.Values.dataFiles.persistence.existingClaim | default (printf "%s-data-files" (include "weknora.fullname" $root)) }}
  {{- else }}
  emptyDir:
    {{- with $root.Values.dataFiles.emptyDir.sizeLimit }}
    sizeLimit: {{ . }}
    {{- end }}
  {{- end }}
- name: scratch
  {{- if eq ($root.Values.app.scratch.volumeType | default "emptyDir") "ephemeral" }}
  ephemeral:
    volumeClaimTemplate:
      {{- with $root.Values.app.scratch.ephemeral.annotations }}
      metadata:
        annotations:
          {{- toYaml . | nindent 10 }}
      {{- end }}
      spec:
        accessModes:
          {{- toYaml $root.Values.app.scratch.ephemeral.accessModes | nindent 10 }}
        storageClassName: {{ $root.Values.app.scratch.ephemeral.storageClassName | quote }}
        resources:
          requests:
            storage: {{ $root.Values.app.scratch.ephemeral.size }}
  {{- else if eq ($root.Values.app.scratch.volumeType | default "emptyDir") "hostPath" }}
  hostPath:
    path: {{ $root.Values.app.scratch.hostPath.path | default "/mnt/weknora-data/scratch-app" | quote }}
    type: DirectoryOrCreate
  {{- else }}
  emptyDir:
    {{- with $root.Values.app.scratch.sizeLimit }}
    sizeLimit: {{ . }}
    {{- end }}
  {{- end }}
{{- if $root.Values.app.documentQueue.kubernetesRuntimeVerifier.enabled }}
- name: document-queue-kubernetes-api
  projected:
    defaultMode: 0444
    sources:
      - serviceAccountToken:
          path: token
          expirationSeconds: {{ $root.Values.app.documentQueue.kubernetesRuntimeVerifier.tokenExpirationSeconds }}
      - configMap:
          name: kube-root-ca.crt
          items:
            - key: ca.crt
              path: ca.crt
{{- end }}
{{- with $root.Values.app.extraVolumes }}
{{ toYaml . }}
{{- end }}
{{- end }}
