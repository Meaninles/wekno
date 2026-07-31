// Package modeladmission provides cross-instance admission control for remote
// model calls. Redis leases bound aggregate concurrency across application
// Pods; a local implementation keeps Lite mode deterministic.
package modeladmission

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
)

type Kind string

const (
	KindChat       Kind = "chat"
	KindEmbedding  Kind = "embedding"
	KindRerank     Kind = "rerank"
	KindVLM        Kind = "vlm"
	KindASR        Kind = "asr"
	KindParser     Kind = "parser"
	KindDerivative Kind = "derivative"
)

var (
	ErrAdmissionBackendUnavailable = errors.New("model admission backend unavailable")
	ErrAdmissionLeaseLost          = errors.New("model admission lease lost")
	ErrAdmissionDeferred           = errors.New("model admission capacity unavailable")
	ErrProviderCircuitOpen         = errors.New("model provider circuit open")
	ErrProviderUnavailable         = errors.New("model provider temporarily unavailable")
)

// AdmissionDeferredError is returned before a provider call. Durable workers
// ACK and reschedule at RetryAfter without consuming a provider-attempt
// budget; no business processing span is created for this control-plane wait.
type AdmissionDeferredError struct {
	Kind       Kind
	PoolID     string
	RetryAfter time.Duration
}

func (e *AdmissionDeferredError) Error() string {
	delay := e.RetryAfter
	if delay < time.Second {
		delay = time.Second
	}
	return fmt.Sprintf(
		"%s for %s pool %s; retry after %s",
		ErrAdmissionDeferred, e.Kind, e.PoolID, delay.Round(time.Second),
	)
}

func (e *AdmissionDeferredError) Unwrap() error                  { return ErrAdmissionDeferred }
func (e *AdmissionDeferredError) ModelWorkDeferred() bool        { return true }
func (e *AdmissionDeferredError) ModelRetryAfter() time.Duration { return e.RetryAfter }

type Limit struct {
	Concurrency int
	// Background is an explicit background ceiling when positive. Zero keeps
	// the legacy interactive-reserve calculation for compatibility.
	Background  int
	RPM         int
	PerTenant   int
	PerDocument int
}

type Config struct {
	Enabled            bool
	FailClosed         bool
	KeyPrefix          string
	LeaseTTL           time.Duration
	HeartbeatInterval  time.Duration
	InteractiveReserve int
	InteractiveMaxWait time.Duration
	BackgroundMaxWait  time.Duration
	CircuitEnabled     bool
	CircuitThreshold   int
	CircuitWindow      time.Duration
	CircuitOpen        time.Duration
	CircuitProbeTTL    time.Duration
	Limits             map[Kind]Limit
}

const (
	// One failure window must be long enough to include several serialized
	// calls to a slow provider. VLM/ASR transports legitimately use
	// multi-minute request timeouts; the old one-minute window expired
	// between consecutive black-holed calls, so the shared circuit could
	// never reach its threshold and every Pod kept retrying forever.
	defaultCircuitFailureWindow = 10 * time.Minute
	defaultCircuitOpenDuration  = time.Minute
)

func ConfigFromEnv() Config {
	return Config{
		Enabled:            envBool("CUSTOM_MODEL_ADMISSION_ENABLED", true),
		FailClosed:         envBool("CUSTOM_MODEL_ADMISSION_FAIL_CLOSED", true),
		KeyPrefix:          envString("CUSTOM_MODEL_ADMISSION_KEY_PREFIX", "weknora:model-admission:v2:"),
		LeaseTTL:           envDurationSeconds("CUSTOM_MODEL_ADMISSION_LEASE_SECONDS", 45*time.Second),
		HeartbeatInterval:  envDurationSeconds("CUSTOM_MODEL_ADMISSION_HEARTBEAT_SECONDS", 15*time.Second),
		InteractiveReserve: envInt("CUSTOM_MODEL_ADMISSION_INTERACTIVE_RESERVE", 2),
		InteractiveMaxWait: envDurationSeconds("CUSTOM_MODEL_ADMISSION_INTERACTIVE_WAIT_SECONDS", 30*time.Second),
		// Background work already has a durable task lifecycle and is retried
		// when that lifecycle is cancelled. A second, arbitrary admission
		// deadline turns healthy provider saturation into a false task
		// failure. Zero therefore means "inherit the task context"; operators
		// can still opt into a finite bound explicitly.
		BackgroundMaxWait: envOptionalDurationSeconds("CUSTOM_MODEL_ADMISSION_BACKGROUND_WAIT_SECONDS", 0),
		CircuitEnabled:    envBool("CUSTOM_MODEL_CIRCUIT_ENABLED", true),
		CircuitThreshold:  envInt("CUSTOM_MODEL_CIRCUIT_FAILURE_THRESHOLD", 3),
		CircuitWindow:     envDurationSeconds("CUSTOM_MODEL_CIRCUIT_WINDOW_SECONDS", defaultCircuitFailureWindow),
		CircuitOpen:       envDurationSeconds("CUSTOM_MODEL_CIRCUIT_OPEN_SECONDS", defaultCircuitOpenDuration),
		// A half-open probe may legitimately run for the full per-call model
		// timeout. Its ownership TTL must outlive that request so another Pod
		// cannot start a concurrent probe.
		CircuitProbeTTL: envDurationSeconds("CUSTOM_MODEL_CIRCUIT_PROBE_SECONDS", 5*time.Minute),
		Limits: map[Kind]Limit{
			KindChat: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_CHAT_CONCURRENCY", 12),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_CHAT_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_CHAT_PER_TENANT", 6),
			},
			KindEmbedding: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_EMBEDDING_CONCURRENCY", 12),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_EMBEDDING_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_EMBEDDING_PER_TENANT", 6),
			},
			KindRerank: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_RERANK_CONCURRENCY", 12),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_RERANK_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_RERANK_PER_TENANT", 6),
			},
			KindVLM: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_VLM_CONCURRENCY", 4),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_VLM_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_VLM_PER_TENANT", 2),
			},
			KindASR: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_ASR_CONCURRENCY", 2),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_ASR_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_ASR_PER_TENANT", 1),
			},
			KindParser: {
				Concurrency: envInt("CUSTOM_MODEL_ADMISSION_PARSER_CONCURRENCY", 12),
				RPM:         envInt("CUSTOM_MODEL_ADMISSION_PARSER_RPM", 0),
				PerTenant:   envInt("CUSTOM_MODEL_ADMISSION_PARSER_PER_TENANT", 4),
			},
		},
	}
}

func envString(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func envInt(key string, fallback int) int {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return fallback
	}
	return parsed
}

func envBool(key string, fallback bool) bool {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func envDurationSeconds(key string, fallback time.Duration) time.Duration {
	seconds := envInt(key, int(fallback/time.Second))
	if seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func envOptionalDurationSeconds(key string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds < 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

type workloadClass uint8

const (
	interactiveWorkload workloadClass = iota
	backgroundWorkload
)

type workloadContextKey struct{}
type knowledgeContextKey struct{}

func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, workloadContextKey{}, backgroundWorkload)
}

func isBackground(ctx context.Context) bool {
	value, _ := ctx.Value(workloadContextKey{}).(workloadClass)
	return value == backgroundWorkload
}

// WithKnowledgeID supplies the stable document identity used for the
// per-resource-pool document burst limit.
func WithKnowledgeID(ctx context.Context, knowledgeID string) context.Context {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return ctx
	}
	return context.WithValue(ctx, knowledgeContextKey{}, knowledgeID)
}

func knowledgeIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(knowledgeContextKey{}).(string)
	return strings.TrimSpace(value)
}

// Spec contains only non-secret identity. Domain is a one-way digest over the
// actual compute route. Tenant fairness and call-kind policy are separate
// dimensions and therefore never split one physical GPU into fake pools.
type Spec struct {
	Kind             Kind
	Domain           string
	TenantID         uint64
	ModelID          string
	ModelTenantID    uint64
	RouteFingerprint string
	DerivativeOnly   bool
	KnowledgeID      string
	KeyPrefix        string
}

func SpecForModel(kind Kind, model *types.Model, _ string) Spec {
	if model == nil {
		return Spec{Kind: kind}
	}
	fingerprint := RouteFingerprint(model)
	domainHash := sha256.Sum256([]byte(fingerprint))
	return Spec{
		Kind:             kind,
		Domain:           hex.EncodeToString(domainHash[:16]),
		TenantID:         model.TenantID,
		ModelID:          model.ID,
		ModelTenantID:    model.TenantID,
		RouteFingerprint: fingerprint,
		DerivativeOnly:   model.WorkloadScope.Normalize() == types.ModelWorkloadDerivativeOnly,
	}
}

func RouteFingerprint(model *types.Model) string {
	if model == nil {
		return ""
	}
	providerName := strings.ToLower(strings.TrimSpace(model.Parameters.Provider))
	if providerName == "" {
		providerName = strings.ToLower(strings.TrimSpace(string(model.Source)))
	}
	actualModelName := strings.TrimSpace(model.Parameters.ExtraConfig["remote_model_name"])
	if actualModelName == "" {
		actualModelName = strings.TrimSpace(model.Name)
	}
	return strings.Join([]string{
		providerName,
		normalizeEndpoint(model.Parameters.BaseURL),
		strings.ToLower(actualModelName),
	}, "\x00")
}

func SpecForParser(engine string, tenantID uint64) Spec {
	material := strings.ToLower(strings.TrimSpace(engine))
	if material == "" {
		material = "builtin"
	}
	domainHash := sha256.Sum256([]byte("parser\x00" + material))
	return Spec{
		Kind:     KindParser,
		Domain:   hex.EncodeToString(domainHash[:16]),
		TenantID: tenantID,
	}
}

func normalizeEndpoint(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "provider-default"
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return strings.ToLower(strings.TrimRight(raw, "/"))
	}
	parsed.RawQuery = ""
	parsed.Fragment = ""
	parsed.User = nil
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	return parsed.String()
}

type Stats struct {
	Waiting       int64
	InFlight      int64
	Acquired      uint64
	BackendErrors uint64
	LeaseLost     uint64
	CircuitOpened uint64
	CircuitReject uint64
}

type circuitPolicy struct {
	enabled   bool
	threshold int
	window    time.Duration
	open      time.Duration
	probeTTL  time.Duration
}

type Manager struct {
	redis  *redis.Client
	config Config

	waiting       atomic.Int64
	inFlight      atomic.Int64
	acquired      atomic.Uint64
	backendErrors atomic.Uint64
	leaseLost     atomic.Uint64
	circuitOpened atomic.Uint64
	circuitReject atomic.Uint64

	localMu        sync.Mutex
	localAccounts  map[string]*localAccount
	localTenants   map[string]int
	localDocuments map[string]int
	localCircuits  map[string]*localCircuit
	store          *Store
}

type localAccount struct {
	active           int
	backgroundActive int
	rateTimestamps   []time.Time
}

func NewManager(redisClient *redis.Client, db *gorm.DB) *Manager {
	manager := newManagerWithConfig(redisClient, ConfigFromEnv())
	manager.store = NewStore(db)
	return manager
}

func newManagerWithConfig(redisClient *redis.Client, config Config) *Manager {
	if config.KeyPrefix == "" {
		config.KeyPrefix = "weknora:model-admission:v2:"
	}
	if config.LeaseTTL <= 0 {
		config.LeaseTTL = 45 * time.Second
	}
	if config.HeartbeatInterval <= 0 || config.HeartbeatInterval >= config.LeaseTTL {
		config.HeartbeatInterval = config.LeaseTTL / 3
	}
	if config.InteractiveMaxWait <= 0 {
		config.InteractiveMaxWait = 30 * time.Second
	}
	if config.Limits == nil {
		config.Limits = ConfigFromEnv().Limits
	}
	if config.CircuitThreshold <= 0 {
		config.CircuitThreshold = 3
	}
	if config.CircuitWindow <= 0 {
		config.CircuitWindow = defaultCircuitFailureWindow
	}
	if config.CircuitOpen <= 0 {
		config.CircuitOpen = defaultCircuitOpenDuration
	}
	if config.CircuitProbeTTL <= 0 {
		config.CircuitProbeTTL = 5 * time.Minute
	}
	return &Manager{
		redis:          redisClient,
		config:         config,
		localAccounts:  make(map[string]*localAccount),
		localTenants:   make(map[string]int),
		localDocuments: make(map[string]int),
		localCircuits:  make(map[string]*localCircuit),
	}
}

func (m *Manager) Migrate(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("model admission control plane is unavailable")
	}
	return m.store.Migrate(ctx)
}

func (m *Manager) Reconcile(ctx context.Context) error {
	if m == nil || m.store == nil {
		return errors.New("model admission control plane is unavailable")
	}
	if err := m.store.ReconcileModels(ctx); err != nil {
		return err
	}
	m.store.Invalidate()
	return nil
}

// ResolvePolicy returns the current hot control-plane policy for one actual
// provider route without acquiring capacity. Token pacing and operator
// diagnostics use this method so they share the exact same pool/binding
// decision as Acquire.
func (m *Manager) ResolvePolicy(ctx context.Context, spec Spec) (ResolvedPolicy, error) {
	if m == nil {
		return ResolvedPolicy{}, errors.New("model admission manager is unavailable")
	}
	fallback := m.config.Limits[spec.Kind]
	if m.store == nil {
		auto := builtinPolicy(spec.Kind, spec.DerivativeOnly)
		if fallback.Concurrency > 0 || fallback.RPM > 0 || fallback.PerTenant > 0 {
			auto.limit = fallback
		}
		policy := resolvedBuiltinPolicy(spec.Domain, auto, "environment")
		policy.CircuitThreshold = m.config.CircuitThreshold
		policy.CircuitWindow = m.config.CircuitWindow
		policy.CircuitOpen = m.config.CircuitOpen
		return policy, nil
	}
	return m.store.Resolve(ctx, spec, fallback)
}

// ResolveResourcePool exposes the model-to-pool control-plane decision to
// conversation-level admission without coupling that module to GORM.
func (m *Manager) ResolveResourcePool(
	ctx context.Context,
	model *types.Model,
) (*ResourcePool, error) {
	if m == nil || m.store == nil {
		if model == nil {
			return nil, errors.New("model admission manager is unavailable")
		}
		return (&Store{}).ResolveResourcePool(ctx, model)
	}
	return m.store.ResolveResourcePool(ctx, model)
}

// GetResourcePool reloads a pool for hot conversation-queue policy updates.
func (m *Manager) GetResourcePool(ctx context.Context, poolID string) (*ResourcePool, error) {
	if m == nil || m.store == nil {
		return nil, errors.New("model admission control plane is unavailable")
	}
	return m.store.GetResourcePool(ctx, poolID)
}

func (m *Manager) circuitPolicyFor(resolved ResolvedPolicy) circuitPolicy {
	policy := circuitPolicy{
		enabled:   m != nil && m.config.CircuitEnabled,
		threshold: positiveInt(resolved.CircuitThreshold, m.config.CircuitThreshold),
		window:    resolved.CircuitWindow,
		open:      resolved.CircuitOpen,
		probeTTL:  m.config.CircuitProbeTTL,
	}
	if policy.window <= 0 {
		policy.window = m.config.CircuitWindow
	}
	if policy.open <= 0 {
		policy.open = m.config.CircuitOpen
	}
	if resolved.RequestTimeout > policy.probeTTL {
		policy.probeTTL = resolved.RequestTimeout + m.config.LeaseTTL
	}
	if policy.probeTTL <= 0 {
		policy.probeTTL = 5 * time.Minute
	}
	return policy
}

func (m *Manager) Snapshot() Stats {
	if m == nil {
		return Stats{}
	}
	return Stats{
		Waiting:       m.waiting.Load(),
		InFlight:      m.inFlight.Load(),
		Acquired:      m.acquired.Load(),
		BackendErrors: m.backendErrors.Load(),
		LeaseLost:     m.leaseLost.Load(),
		CircuitOpened: m.circuitOpened.Load(),
		CircuitReject: m.circuitReject.Load(),
	}
}

var acquireScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local total_limit = tonumber(ARGV[1])
local background_limit = tonumber(ARGV[2])
local tenant_limit = tonumber(ARGV[3])
local rpm = tonumber(ARGV[4])
local token = ARGV[5]
local lease_ms = tonumber(ARGV[6])
local rate_window_ms = tonumber(ARGV[7])
local background = tonumber(ARGV[8])
local document_limit = tonumber(ARGV[9])

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[4], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[5], '-inf', now - rate_window_ms)

local total_count = redis.call('ZCARD', KEYS[1])
local background_count = redis.call('ZCARD', KEYS[2])
local tenant_count = redis.call('ZCARD', KEYS[3])
local document_count = redis.call('ZCARD', KEYS[4])
local rate_count = redis.call('ZCARD', KEYS[5])

local allowed = total_limit <= 0 or total_count < total_limit
if allowed and background == 1 and background_limit > 0 then
  allowed = background_count < background_limit
end
if allowed and tenant_limit > 0 then
  allowed = tenant_count < tenant_limit
end
if allowed and document_limit > 0 then
  allowed = document_count < document_limit
end
if allowed and rpm > 0 then
  allowed = rate_count < rpm
end

if allowed then
  local expires = now + lease_ms
  redis.call('ZADD', KEYS[1], expires, token)
  if background == 1 and background_limit > 0 then
    redis.call('ZADD', KEYS[2], expires, token)
  end
  if tenant_limit > 0 then
    redis.call('ZADD', KEYS[3], expires, token)
  end
  if document_limit > 0 then
    redis.call('ZADD', KEYS[4], expires, token)
  end
  if rpm > 0 then
    redis.call('ZADD', KEYS[5], now, token)
  end
  for index = 1, 5 do
    redis.call('PEXPIRE', KEYS[index], math.max(lease_ms * 2, rate_window_ms + 1000))
  end
  return {1, 0}
end

-- A semaphore holder normally releases long before its lease TTL. Sleeping
-- until the earliest lease expiry would therefore turn short model calls into
-- one-call-per-second throughput. Poll semaphore pressure at a bounded cadence;
-- the caller adds a token-stable jitter to avoid a cross-Pod thundering herd.
local wait_ms = 75
if rpm > 0 and rate_count >= rpm then
  local earliest_rate = redis.call('ZRANGE', KEYS[5], 0, 0, 'WITHSCORES')
  if earliest_rate[2] then
    wait_ms = math.max(wait_ms, tonumber(earliest_rate[2]) + rate_window_ms - now)
  end
end
return {0, math.max(25, math.min(wait_ms, 1000))}
`)

var renewScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local token = ARGV[1]
local lease_ms = tonumber(ARGV[2])
local expires = now + lease_ms
local renewed = 0
for index = 1, 4 do
  if redis.call('ZSCORE', KEYS[index], token) then
    redis.call('ZADD', KEYS[index], 'XX', expires, token)
    -- The sorted-set key has its own TTL in addition to every member's
    -- expiry score. Refresh both atomically: otherwise a healthy, long model
    -- request can outlive the key TTL and be fenced as "lease lost" even
    -- though its heartbeat continuously renews the member.
    redis.call('PEXPIRE', KEYS[index], lease_ms * 2)
    renewed = renewed + 1
  end
end
return renewed
`)

var releaseScript = redis.NewScript(`
local token = ARGV[1]
local removed = 0
for index = 1, 4 do
  removed = removed + redis.call('ZREM', KEYS[index], token)
end
return removed
`)

type leaseKeys struct {
	total      string
	background string
	tenant     string
	document   string
	rate       string
	circuit    string
	probe      string
}

func (m *Manager) keys(spec Spec) leaseKeys {
	domain := spec.Domain
	if domain == "" {
		domain = "unknown"
	}
	hashTag := "{" + domain + "}"
	prefix := m.config.KeyPrefix
	if strings.TrimSpace(spec.KeyPrefix) != "" {
		prefix = spec.KeyPrefix
	}
	base := prefix + hashTag
	rateSuffix := ":rate"
	if strings.Contains(prefix, "model-quota") {
		rateSuffix = ":rpm"
	}
	return leaseKeys{
		total:      base + ":total",
		background: base + ":background",
		tenant:     base + ":tenant:" + strconv.FormatUint(spec.TenantID, 10),
		document:   base + ":document:" + documentKey(spec.KnowledgeID),
		rate:       base + rateSuffix,
		circuit:    base + ":circuit",
		probe:      base + ":circuit-probe",
	}
}

func documentKey(knowledgeID string) string {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return "none"
	}
	sum := sha256.Sum256([]byte(knowledgeID))
	return hex.EncodeToString(sum[:8])
}

func (k leaseKeys) redisKeys() []string {
	return []string{k.total, k.background, k.tenant, k.document, k.rate}
}

func (m *Manager) Acquire(ctx context.Context, spec Spec) (*Lease, error) {
	if m == nil || !m.config.Enabled {
		return newNoopLease(ctx), nil
	}
	if spec.KnowledgeID == "" {
		spec.KnowledgeID = knowledgeIDFromContext(ctx)
	}
	limit := m.config.Limits[spec.Kind]
	resolved, resolveErr := m.ResolvePolicy(ctx, spec)
	if resolveErr != nil {
		m.backendErrors.Add(1)
		if m.config.FailClosed {
			pipelineobs.ModelPoolRejected(spec.Domain, "backend_error")
			return nil, fmt.Errorf("%w: resolve resource pool: %v", ErrAdmissionBackendUnavailable, resolveErr)
		}
		resolved = resolvedBuiltinPolicy(spec.Domain, builtinPolicy(spec.Kind, spec.DerivativeOnly), "builtin")
		spec.Domain = resolved.PoolID
	} else {
		spec.Domain = resolved.PoolID
		limit = resolved.Limit
	}
	if limit.Concurrency <= 0 && limit.RPM <= 0 &&
		limit.PerTenant <= 0 && limit.PerDocument <= 0 {
		return newNoopLease(ctx), nil
	}

	background := isBackground(ctx)
	startedAt := time.Now()
	metricResult := "error"
	defer func() {
		pipelineobs.ObserveModelAdmission(string(spec.Kind), background, metricResult, time.Since(startedAt))
	}()
	maxWait := m.config.InteractiveMaxWait
	if background {
		maxWait = m.config.BackgroundMaxWait
	}
	waitCtx := ctx
	cancelWait := func() {}
	if maxWait > 0 {
		maxDeadline := time.Now().Add(maxWait)
		currentDeadline, hasDeadline := ctx.Deadline()
		if !hasDeadline || maxDeadline.Before(currentDeadline) {
			waitCtx, cancelWait = context.WithDeadline(ctx, maxDeadline)
		}
	}
	defer cancelWait()

	m.waiting.Add(1)
	waiting := true
	pipelineobs.ModelPoolWaiting(spec.Domain, 1)
	defer func() {
		if waiting {
			m.waiting.Add(-1)
			pipelineobs.ModelPoolWaiting(spec.Domain, -1)
		}
	}()
	token := uuid.NewString()
	keys := m.keys(spec)
	circuitPolicy := m.circuitPolicyFor(resolved)
	for {
		var (
			acquired bool
			retry    time.Duration
			local    bool
			err      error
		)
		if m.redis != nil {
			acquired, retry, err = m.tryAcquireRedis(waitCtx, keys, token, limit, background)
			if err != nil {
				m.backendErrors.Add(1)
				if m.config.FailClosed {
					metricResult = "backend_error"
					return nil, fmt.Errorf("%w: %v", ErrAdmissionBackendUnavailable, err)
				}
				local = true
			}
		} else {
			local = true
		}
		if local {
			acquired, retry = m.tryAcquireLocal(spec, limit, background)
		}
		if acquired {
			// Claim the provider circuit only after admission capacity is
			// already ours. Taking the one half-open probe first can strand it
			// behind a full semaphore for its entire TTL.
			circuitProbe, circuitErr := m.acquireCircuit(
				waitCtx, spec, keys, token, circuitPolicy,
			)
			if circuitErr != nil {
				m.releaseAcquired(spec, keys, token, background, local)
				if errors.Is(circuitErr, ErrProviderCircuitOpen) {
					metricResult = "circuit_open"
					pipelineobs.ModelPoolRejected(spec.Domain, "circuit_open")
					pipelineobs.SetModelPoolCircuit(spec.Domain, true)
				} else {
					metricResult = "backend_error"
					pipelineobs.ModelPoolRejected(spec.Domain, "backend_error")
				}
				return nil, circuitErr
			}
			m.waiting.Add(-1)
			waiting = false
			pipelineobs.ModelPoolWaiting(spec.Domain, -1)
			m.inFlight.Add(1)
			m.acquired.Add(1)
			metricResult = "acquired"
			pipelineobs.ModelAdmissionAcquired(string(spec.Kind), background)
			pipelineobs.ModelPoolAcquired(spec.Domain)
			lease := m.newLease(
				ctx,
				spec,
				keys,
				token,
				background,
				local,
				expectedRedisLeaseKeys(
					limit, m.backgroundLimit(limit), background, spec.KnowledgeID,
				),
				circuitProbe,
				circuitPolicy,
			)
			if err := m.acquireAuxiliaryDimensions(
				ctx, lease, spec, resolved, background,
			); err != nil {
				lease.Release()
				metricResult = "deferred"
				pipelineobs.ModelPoolRejected(spec.Domain, "quota_or_gateway")
				return nil, err
			}
			return lease, nil
		}
		if background {
			metricResult = "deferred"
			pipelineobs.ModelPoolRejected(spec.Domain, "capacity")
			if retry < time.Second {
				retry = time.Second
			}
			return nil, &AdmissionDeferredError{
				Kind: spec.Kind, PoolID: spec.Domain, RetryAfter: retry,
			}
		}
		if retry < 25*time.Millisecond {
			retry = 25 * time.Millisecond
		}
		retry += admissionRetryJitter(token)
		timer := time.NewTimer(retry)
		select {
		case <-waitCtx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			metricResult = "timeout"
			if !errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				metricResult = "cancelled"
			}
			pipelineobs.ModelPoolRejected(spec.Domain, metricResult)
			return nil, fmt.Errorf("wait for %s model admission: %w", spec.Kind, waitCtx.Err())
		case <-timer.C:
		}
	}
}

func (m *Manager) acquireAuxiliaryDimensions(
	ctx context.Context,
	parent *Lease,
	resourceSpec Spec,
	policy ResolvedPolicy,
	background bool,
) error {
	if parent == nil {
		return nil
	}
	type dimension struct {
		id     string
		prefix string
		limit  Limit
	}
	dimensions := []dimension{
		{
			id: policy.QuotaPoolID, prefix: "weknora:model-quota:v2:",
			limit: policy.QuotaLimit,
		},
		{
			id: policy.GatewayPoolID, prefix: "weknora:model-gateway:v2:",
			limit: policy.GatewayLimit,
		},
	}
	acquired := make([]*Lease, 0, len(dimensions))
	for _, candidate := range dimensions {
		if candidate.id == "" ||
			(candidate.limit.Concurrency <= 0 && candidate.limit.RPM <= 0) {
			continue
		}
		spec := Spec{
			Kind: resourceSpec.Kind, Domain: candidate.id,
			TenantID: resourceSpec.TenantID, KnowledgeID: resourceSpec.KnowledgeID,
			KeyPrefix: candidate.prefix,
		}
		lease, retry, err := m.tryAcquireDimension(ctx, spec, candidate.limit, background)
		if err != nil {
			for _, held := range acquired {
				held.Release()
			}
			return err
		}
		if lease == nil {
			for _, held := range acquired {
				held.Release()
			}
			if retry < time.Second {
				retry = time.Second
			}
			return &AdmissionDeferredError{
				Kind: spec.Kind, PoolID: spec.Domain, RetryAfter: retry,
			}
		}
		acquired = append(acquired, lease)
	}
	parent.extras = acquired
	return nil
}

func (m *Manager) tryAcquireDimension(
	ctx context.Context,
	spec Spec,
	limit Limit,
	background bool,
) (*Lease, time.Duration, error) {
	token := uuid.NewString()
	keys := m.keys(spec)
	local := m.redis == nil
	var (
		acquired bool
		retry    time.Duration
		err      error
	)
	if !local {
		acquired, retry, err = m.tryAcquireRedis(ctx, keys, token, limit, background)
		if err != nil {
			m.backendErrors.Add(1)
			if m.config.FailClosed {
				return nil, 0, fmt.Errorf("%w: %v", ErrAdmissionBackendUnavailable, err)
			}
			local = true
		}
	}
	if local {
		acquired, retry = m.tryAcquireLocal(spec, limit, background)
	}
	if !acquired {
		return nil, retry, nil
	}
	lease := m.newLease(
		ctx, spec, keys, token, background, local,
		expectedRedisLeaseKeys(
			limit, m.backgroundLimit(limit), background, spec.KnowledgeID,
		),
		false,
		circuitPolicy{enabled: false},
	)
	lease.circuitEnabled = false
	lease.tracked = false
	return lease, 0, nil
}

func admissionRetryJitter(token string) time.Duration {
	sum := sha256.Sum256([]byte(token))
	return time.Duration(sum[0]%50) * time.Millisecond
}

func (m *Manager) tryAcquireRedis(
	ctx context.Context,
	keys leaseKeys,
	token string,
	limit Limit,
	background bool,
) (bool, time.Duration, error) {
	backgroundLimit := m.backgroundLimit(limit)
	backgroundValue := 0
	if background {
		backgroundValue = 1
	}
	documentLimit := limit.PerDocument
	if strings.TrimSpace(keys.document) == "" ||
		strings.HasSuffix(keys.document, ":document:none") {
		documentLimit = 0
	}
	result, err := acquireScript.Run(
		ctx,
		m.redis,
		keys.redisKeys(),
		limit.Concurrency,
		backgroundLimit,
		limit.PerTenant,
		limit.RPM,
		token,
		m.config.LeaseTTL.Milliseconds(),
		time.Minute.Milliseconds(),
		backgroundValue,
		documentLimit,
	).Slice()
	if err != nil {
		return false, 0, err
	}
	if len(result) != 2 {
		return false, 0, fmt.Errorf("unexpected admission script response length %d", len(result))
	}
	allowed, err := redisNumber(result[0])
	if err != nil {
		return false, 0, err
	}
	waitMillis, err := redisNumber(result[1])
	if err != nil {
		return false, 0, err
	}
	return allowed == 1, time.Duration(waitMillis) * time.Millisecond, nil
}

func (m *Manager) backgroundLimit(limit Limit) int {
	if limit.Background > 0 {
		return limit.Background
	}
	backgroundLimit := limit.Concurrency - m.config.InteractiveReserve
	if backgroundLimit < 1 && limit.Concurrency > 0 {
		backgroundLimit = 1
	}
	return backgroundLimit
}

func expectedRedisLeaseKeys(
	limit Limit,
	backgroundLimit int,
	background bool,
	knowledgeID string,
) int64 {
	expected := int64(1) // The total lease set is always populated.
	if backgroundLimit > 0 && background {
		expected++
	}
	if limit.PerTenant > 0 {
		expected++
	}
	if limit.PerDocument > 0 && strings.TrimSpace(knowledgeID) != "" {
		expected++
	}
	return expected
}

func redisNumber(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("unexpected Redis numeric type %T", value)
	}
}

func (m *Manager) tryAcquireLocal(
	spec Spec,
	limit Limit,
	background bool,
) (bool, time.Duration) {
	m.localMu.Lock()
	defer m.localMu.Unlock()
	accountKey := spec.Domain
	tenantKey := accountKey + ":" + strconv.FormatUint(spec.TenantID, 10)
	documentLimit := limit.PerDocument
	documentID := strings.TrimSpace(spec.KnowledgeID)
	documentAccountKey := accountKey + ":" + documentID
	if documentID == "" {
		documentLimit = 0
	}
	account := m.localAccounts[accountKey]
	if account == nil {
		account = &localAccount{}
		m.localAccounts[accountKey] = account
	}
	now := time.Now()
	cutoff := now.Add(-time.Minute)
	validRates := account.rateTimestamps[:0]
	for _, timestamp := range account.rateTimestamps {
		if timestamp.After(cutoff) {
			validRates = append(validRates, timestamp)
		}
	}
	account.rateTimestamps = validRates
	backgroundLimit := m.backgroundLimit(limit)
	switch {
	case limit.Concurrency > 0 && account.active >= limit.Concurrency:
		return false, 25 * time.Millisecond
	case background && backgroundLimit > 0 && account.backgroundActive >= backgroundLimit:
		return false, 25 * time.Millisecond
	case limit.PerTenant > 0 && m.localTenants[tenantKey] >= limit.PerTenant:
		return false, 25 * time.Millisecond
	case documentLimit > 0 && m.localDocuments[documentAccountKey] >= documentLimit:
		return false, 25 * time.Millisecond
	case limit.RPM > 0 && len(account.rateTimestamps) >= limit.RPM:
		return false, time.Until(account.rateTimestamps[0].Add(time.Minute))
	}
	account.active++
	if background {
		account.backgroundActive++
	}
	m.localTenants[tenantKey]++
	if documentLimit > 0 {
		m.localDocuments[documentAccountKey]++
	}
	if limit.RPM > 0 {
		account.rateTimestamps = append(account.rateTimestamps, now)
	}
	return true, 0
}

func (m *Manager) releaseLocal(spec Spec, background bool) {
	m.localMu.Lock()
	defer m.localMu.Unlock()
	accountKey := spec.Domain
	tenantKey := accountKey + ":" + strconv.FormatUint(spec.TenantID, 10)
	documentID := strings.TrimSpace(spec.KnowledgeID)
	documentAccountKey := accountKey + ":" + documentID
	if account := m.localAccounts[accountKey]; account != nil {
		if account.active > 0 {
			account.active--
		}
		if background && account.backgroundActive > 0 {
			account.backgroundActive--
		}
	}
	if m.localTenants[tenantKey] > 1 {
		m.localTenants[tenantKey]--
	} else {
		delete(m.localTenants, tenantKey)
	}
	if documentID != "" {
		if m.localDocuments[documentAccountKey] > 1 {
			m.localDocuments[documentAccountKey]--
		} else {
			delete(m.localDocuments, documentAccountKey)
		}
	}
}

// releaseAcquired rolls back a semaphore claim when the provider circuit is
// checked immediately afterwards and rejects the call. The lease was never
// exposed, so in-flight metrics must not be changed here.
func (m *Manager) releaseAcquired(
	spec Spec,
	keys leaseKeys,
	token string,
	background bool,
	local bool,
) {
	if local {
		m.releaseLocal(spec, background)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := releaseScript.Run(
		ctx,
		m.redis,
		[]string{keys.total, keys.background, keys.tenant, keys.document},
		token,
	).Err()
	cancel()
	if err != nil {
		m.backendErrors.Add(1)
	}
}

type Lease struct {
	manager          *Manager
	spec             Spec
	keys             leaseKeys
	token            string
	background       bool
	local            bool
	noop             bool
	expectedRenewals int64
	circuitProbe     bool
	circuitEnabled   bool
	circuitPolicy    circuitPolicy
	tracked          bool
	extras           []*Lease

	ctx               context.Context
	cancel            context.CancelCauseFunc
	done              chan struct{}
	once              sync.Once
	resultOnce        sync.Once
	resultRecorded    atomic.Bool
	lost              atomic.Bool
	providerStartedAt time.Time
}

func newNoopLease(ctx context.Context) *Lease {
	return &Lease{ctx: ctx, noop: true}
}

func (m *Manager) newLease(
	parent context.Context,
	spec Spec,
	keys leaseKeys,
	token string,
	background bool,
	local bool,
	expectedRenewals int64,
	circuitProbe bool,
	policy circuitPolicy,
) *Lease {
	leaseCtx, cancel := context.WithCancelCause(parent)
	lease := &Lease{
		manager:           m,
		spec:              spec,
		keys:              keys,
		token:             token,
		background:        background,
		local:             local,
		expectedRenewals:  expectedRenewals,
		circuitProbe:      circuitProbe,
		circuitEnabled:    policy.enabled,
		circuitPolicy:     policy,
		tracked:           true,
		providerStartedAt: time.Now(),
		ctx:               leaseCtx,
		cancel:            cancel,
		done:              make(chan struct{}),
	}
	if !local {
		go lease.heartbeat()
	}
	return lease
}

func (l *Lease) Context() context.Context {
	if l == nil {
		return context.Background()
	}
	return l.ctx
}

func (l *Lease) FencingError() error {
	if l != nil && l.lost.Load() {
		return ErrAdmissionLeaseLost
	}
	return nil
}

// Finish reports the provider result before releasing admission capacity.
// Across Pods this drives one shared circuit for the provider/model domain.
func (l *Lease) Finish(callErr error) {
	if l == nil || l.noop || l.manager == nil {
		return
	}
	l.resultOnce.Do(func() {
		l.resultRecorded.Store(true)
		errorClass := ""
		if callErr != nil {
			errorClass = "provider"
		}
		pipelineobs.ObserveModelPoolProvider(
			l.spec.Domain, errorClass, time.Since(l.providerStartedAt),
		)
		if callErr == nil {
			pipelineobs.SetModelPoolCircuit(l.spec.Domain, false)
		}
		if l.circuitEnabled {
			l.manager.recordCircuitResult(l, callErr)
		}
	})
}

// Complete reports the provider result and preserves the original provider
// error while adding a typed external-backpressure classification for
// transport, timeout, rate-limit and provider 5xx failures. Business handlers
// can distinguish an actual remote call from a pre-call circuit rejection;
// durable queues may then count the former in their bounded business budget.
func (l *Lease) Complete(callErr error) error {
	if l == nil || l.noop || l.manager == nil {
		return callErr
	}
	outcome := classifyCircuitOutcome(l, callErr)
	l.Finish(callErr)
	if outcome != circuitFailure {
		return callErr
	}
	retryAfter := 15 * time.Second
	if l.circuitProbe {
		retryAfter = l.circuitPolicy.open
	}
	if retryAfter <= 0 {
		retryAfter = 15 * time.Second
	}
	return &ProviderUnavailableError{
		Kind:       l.spec.Kind,
		RetryAfter: retryAfter,
		Cause:      callErr,
	}
}

func (l *Lease) heartbeat() {
	ticker := time.NewTicker(l.manager.config.HeartbeatInterval)
	defer ticker.Stop()
	consecutiveErrors := 0
	for {
		select {
		case <-l.done:
			return
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), l.manager.config.HeartbeatInterval)
			renewed, err := renewScript.Run(
				ctx,
				l.manager.redis,
				[]string{l.keys.total, l.keys.background, l.keys.tenant, l.keys.document},
				l.token,
				l.manager.config.LeaseTTL.Milliseconds(),
			).Int64()
			cancel()
			if err != nil {
				consecutiveErrors++
				l.manager.backendErrors.Add(1)
				if consecutiveErrors < 2 {
					continue
				}
			} else if renewed == l.expectedRenewals {
				consecutiveErrors = 0
				continue
			}
			l.lost.Store(true)
			l.manager.leaseLost.Add(1)
			l.cancel(ErrAdmissionLeaseLost)
			return
		}
	}
}

func (l *Lease) Release() {
	if l == nil || l.noop {
		return
	}
	// A caller that obtained the single half-open probe but exited without
	// reporting a provider result must not strand that probe. Preserve the
	// expired-open state so another Pod can probe immediately.
	l.resultOnce.Do(func() {
		l.resultRecorded.Store(true)
		if l.circuitProbe {
			l.manager.abandonCircuitProbe(l.spec, l.keys, l.token)
		}
	})
	l.once.Do(func() {
		for _, extra := range l.extras {
			extra.Release()
		}
		close(l.done)
		l.cancel(context.Canceled)
		if l.local {
			l.manager.releaseLocal(l.spec, l.background)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := releaseScript.Run(
				ctx,
				l.manager.redis,
				[]string{l.keys.total, l.keys.background, l.keys.tenant, l.keys.document},
				l.token,
			).Err()
			cancel()
			if err != nil {
				l.manager.backendErrors.Add(1)
			}
		}
		if l.tracked {
			l.manager.inFlight.Add(-1)
			pipelineobs.ModelAdmissionReleased(string(l.spec.Kind), l.background)
			pipelineobs.ModelPoolReleased(l.spec.Domain)
		}
	})
}
