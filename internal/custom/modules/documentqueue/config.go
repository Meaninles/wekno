package documentqueue

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	HeartbeatInterval    time.Duration
	LeaseDuration        time.Duration
	RecoveryInterval     time.Duration
	RecoveryCycleTimeout time.Duration
	RecoveryBatchSize    int
	InstanceStaleAfter   time.Duration
	WorkflowPollInterval time.Duration
	DelegateTimeout      time.Duration
	WorkflowTimeout      time.Duration
	StuckHandlerGrace    time.Duration
	ShutdownDrainTimeout time.Duration
	MaxRetry             int
	// KubernetesRuntimeVerifierEnabled allows a healthy replica to turn an
	// explicitly terminated Kubernetes Pod/container into a durable instance
	// stop proof. It never treats heartbeat age, deletion intent or 404 as proof.
	KubernetesRuntimeVerifierEnabled bool
	KubernetesAPIServer              string
	KubernetesTokenFile              string
	KubernetesCAFile                 string
	KubernetesContainerName          string
	KubernetesRequestTimeout         time.Duration
	// TrustStableInstanceRestart permits a new boot with the same stable
	// instance ID to fence/adopt the prior boot immediately. Enable it only
	// when the ID is enforced by an orchestrator identity which cannot have two
	// live containers at once (for example a Kubernetes Pod UID or Docker
	// container name). Cross-instance recovery never relies on this flag.
	TrustStableInstanceRestart bool
}

func DefaultConfig() Config {
	return Config{
		HeartbeatInterval: 10 * time.Second,
		// Asynq's delivery lease is 30s and its recoverer deliberately waits
		// another 30s for clock skew. Keeping the workflow lease longer means
		// cross-instance takeover cannot race Asynq's own active-task recovery.
		LeaseDuration:            90 * time.Second,
		RecoveryInterval:         10 * time.Second,
		RecoveryCycleTimeout:     30 * time.Second,
		RecoveryBatchSize:        100,
		InstanceStaleAfter:       35 * time.Second,
		WorkflowPollInterval:     2 * time.Second,
		DelegateTimeout:          2 * time.Hour,
		WorkflowTimeout:          48 * time.Hour,
		StuckHandlerGrace:        30 * time.Second,
		ShutdownDrainTimeout:     30 * time.Second,
		MaxRetry:                 3,
		KubernetesAPIServer:      "https://kubernetes.default.svc",
		KubernetesTokenFile:      "/var/run/secrets/weknora-document-queue-kubernetes/token",
		KubernetesCAFile:         "/var/run/secrets/weknora-document-queue-kubernetes/ca.crt",
		KubernetesContainerName:  "app",
		KubernetesRequestTimeout: 5 * time.Second,
	}
}

func LoadConfig() Config {
	cfg := DefaultConfig()
	cfg.HeartbeatInterval = durationEnv("CUSTOM_DOCUMENT_QUEUE_HEARTBEAT_INTERVAL", cfg.HeartbeatInterval)
	cfg.LeaseDuration = durationEnv("CUSTOM_DOCUMENT_QUEUE_LEASE_DURATION", cfg.LeaseDuration)
	cfg.RecoveryInterval = durationEnv("CUSTOM_DOCUMENT_QUEUE_RECOVERY_INTERVAL", cfg.RecoveryInterval)
	cfg.RecoveryCycleTimeout = durationEnv("CUSTOM_DOCUMENT_QUEUE_RECOVERY_CYCLE_TIMEOUT", cfg.RecoveryCycleTimeout)
	cfg.InstanceStaleAfter = durationEnv("CUSTOM_DOCUMENT_QUEUE_INSTANCE_STALE_AFTER", cfg.InstanceStaleAfter)
	cfg.WorkflowPollInterval = durationEnv("CUSTOM_DOCUMENT_QUEUE_POLL_INTERVAL", cfg.WorkflowPollInterval)
	cfg.DelegateTimeout = durationEnv("CUSTOM_DOCUMENT_QUEUE_DELEGATE_TIMEOUT", cfg.DelegateTimeout)
	cfg.DelegateTimeout = durationEnv("WEKNORA_DOCUMENT_PROCESS_TIMEOUT", cfg.DelegateTimeout)
	cfg.WorkflowTimeout = durationEnv("CUSTOM_DOCUMENT_QUEUE_WORKFLOW_TIMEOUT", cfg.WorkflowTimeout)
	cfg.StuckHandlerGrace = durationEnv("CUSTOM_DOCUMENT_QUEUE_STUCK_HANDLER_GRACE", cfg.StuckHandlerGrace)
	cfg.ShutdownDrainTimeout = durationEnv("CUSTOM_DOCUMENT_QUEUE_SHUTDOWN_DRAIN_TIMEOUT", cfg.ShutdownDrainTimeout)
	cfg.KubernetesRuntimeVerifierEnabled = boolEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_RUNTIME_VERIFIER_ENABLED",
		cfg.KubernetesRuntimeVerifierEnabled,
	)
	cfg.KubernetesAPIServer = stringEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_API_SERVER", cfg.KubernetesAPIServer,
	)
	cfg.KubernetesTokenFile = stringEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_TOKEN_FILE", cfg.KubernetesTokenFile,
	)
	cfg.KubernetesCAFile = stringEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_CA_FILE", cfg.KubernetesCAFile,
	)
	cfg.KubernetesContainerName = stringEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_CONTAINER_NAME", cfg.KubernetesContainerName,
	)
	cfg.KubernetesRequestTimeout = durationEnv(
		"CUSTOM_DOCUMENT_QUEUE_KUBERNETES_REQUEST_TIMEOUT", cfg.KubernetesRequestTimeout,
	)
	cfg.TrustStableInstanceRestart = boolEnv(
		"CUSTOM_DOCUMENT_QUEUE_TRUST_STABLE_INSTANCE_RESTART",
		cfg.TrustStableInstanceRestart,
	)
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CUSTOM_DOCUMENT_QUEUE_RECOVERY_BATCH_SIZE"))); err == nil && value > 0 {
		cfg.RecoveryBatchSize = value
	}
	if value, err := strconv.Atoi(strings.TrimSpace(os.Getenv("CUSTOM_DOCUMENT_QUEUE_MAX_RETRY"))); err == nil && value > 0 {
		cfg.MaxRetry = value
	}
	return cfg.normalized()
}

func (c Config) normalized() Config {
	d := DefaultConfig()
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = d.HeartbeatInterval
	}
	if c.LeaseDuration < 3*c.HeartbeatInterval {
		c.LeaseDuration = 3 * c.HeartbeatInterval
	}
	if c.RecoveryInterval <= 0 {
		c.RecoveryInterval = d.RecoveryInterval
	}
	if c.RecoveryCycleTimeout <= 0 {
		c.RecoveryCycleTimeout = d.RecoveryCycleTimeout
	}
	if c.RecoveryBatchSize <= 0 {
		c.RecoveryBatchSize = d.RecoveryBatchSize
	}
	if c.RecoveryBatchSize > 1000 {
		c.RecoveryBatchSize = 1000
	}
	if c.InstanceStaleAfter < 2*c.HeartbeatInterval {
		c.InstanceStaleAfter = 2 * c.HeartbeatInterval
	}
	if c.WorkflowPollInterval <= 0 {
		c.WorkflowPollInterval = d.WorkflowPollInterval
	}
	if c.DelegateTimeout <= 0 {
		c.DelegateTimeout = d.DelegateTimeout
	}
	if c.WorkflowTimeout <= 0 {
		c.WorkflowTimeout = d.WorkflowTimeout
	}
	if c.StuckHandlerGrace <= 0 {
		c.StuckHandlerGrace = d.StuckHandlerGrace
	}
	if c.ShutdownDrainTimeout <= 0 {
		c.ShutdownDrainTimeout = d.ShutdownDrainTimeout
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = d.MaxRetry
	}
	if strings.TrimSpace(c.KubernetesAPIServer) == "" {
		c.KubernetesAPIServer = d.KubernetesAPIServer
	}
	if strings.TrimSpace(c.KubernetesTokenFile) == "" {
		c.KubernetesTokenFile = d.KubernetesTokenFile
	}
	if strings.TrimSpace(c.KubernetesCAFile) == "" {
		c.KubernetesCAFile = d.KubernetesCAFile
	}
	if strings.TrimSpace(c.KubernetesContainerName) == "" {
		c.KubernetesContainerName = d.KubernetesContainerName
	}
	if c.KubernetesRequestTimeout <= 0 {
		c.KubernetesRequestTimeout = d.KubernetesRequestTimeout
	}
	return c
}

func stringEnv(name string, fallback string) string {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	return value
}

func durationEnv(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := time.ParseDuration(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func boolEnv(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(raw)
	if err != nil {
		return fallback
	}
	return parsed
}
