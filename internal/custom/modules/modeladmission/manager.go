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
)

type Kind string

const (
	KindChat      Kind = "chat"
	KindEmbedding Kind = "embedding"
	KindRerank    Kind = "rerank"
	KindVLM       Kind = "vlm"
	KindASR       Kind = "asr"
	KindParser    Kind = "parser"
)

var (
	ErrAdmissionBackendUnavailable = errors.New("model admission backend unavailable")
	ErrAdmissionLeaseLost          = errors.New("model admission lease lost")
	ErrProviderCircuitOpen         = errors.New("model provider circuit open")
	ErrProviderUnavailable         = errors.New("model provider temporarily unavailable")
)

type Limit struct {
	Concurrency int
	RPM         int
	PerTenant   int
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
		KeyPrefix:          envString("CUSTOM_MODEL_ADMISSION_KEY_PREFIX", "weknora:model-admission:"),
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

func WithBackground(ctx context.Context) context.Context {
	return context.WithValue(ctx, workloadContextKey{}, backgroundWorkload)
}

func isBackground(ctx context.Context) bool {
	value, _ := ctx.Value(workloadContextKey{}).(workloadClass)
	return value == backgroundWorkload
}

// Spec contains only non-secret identity. Domain is a one-way digest over the
// provider quota account, endpoint, model and call kind.
type Spec struct {
	Kind     Kind
	Domain   string
	TenantID uint64
}

func SpecForModel(kind Kind, model *types.Model, effectiveSecret string) Spec {
	if model == nil {
		return Spec{Kind: kind}
	}
	providerName := strings.ToLower(strings.TrimSpace(model.Parameters.Provider))
	if providerName == "" {
		providerName = strings.ToLower(strings.TrimSpace(string(model.Source)))
	}
	endpoint := normalizeEndpoint(model.Parameters.BaseURL)
	secret := strings.TrimSpace(effectiveSecret)
	if secret == "" {
		secret = strings.TrimSpace(model.Parameters.APIKey)
	}
	credentialHash := sha256.Sum256([]byte(secret))
	material := strings.Join([]string{
		string(kind),
		providerName,
		endpoint,
		strings.ToLower(strings.TrimSpace(model.Name)),
		hex.EncodeToString(credentialHash[:]),
	}, "\x00")
	domainHash := sha256.Sum256([]byte(material))
	return Spec{
		Kind:     kind,
		Domain:   hex.EncodeToString(domainHash[:16]),
		TenantID: model.TenantID,
	}
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

	localMu       sync.Mutex
	localAccounts map[string]*localAccount
	localTenants  map[string]int
	localCircuits map[string]*localCircuit
}

type localAccount struct {
	active           int
	backgroundActive int
	rateTimestamps   []time.Time
}

func NewManager(redisClient *redis.Client) *Manager {
	return newManagerWithConfig(redisClient, ConfigFromEnv())
}

func newManagerWithConfig(redisClient *redis.Client, config Config) *Manager {
	if config.KeyPrefix == "" {
		config.KeyPrefix = "weknora:model-admission:"
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
		redis:         redisClient,
		config:        config,
		localAccounts: make(map[string]*localAccount),
		localTenants:  make(map[string]int),
		localCircuits: make(map[string]*localCircuit),
	}
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

redis.call('ZREMRANGEBYSCORE', KEYS[1], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[2], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[3], '-inf', now)
redis.call('ZREMRANGEBYSCORE', KEYS[4], '-inf', now - rate_window_ms)

local total_count = redis.call('ZCARD', KEYS[1])
local background_count = redis.call('ZCARD', KEYS[2])
local tenant_count = redis.call('ZCARD', KEYS[3])
local rate_count = redis.call('ZCARD', KEYS[4])

local allowed = total_limit <= 0 or total_count < total_limit
if allowed and background == 1 and background_limit > 0 then
  allowed = background_count < background_limit
end
if allowed and tenant_limit > 0 then
  allowed = tenant_count < tenant_limit
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
  if rpm > 0 then
    redis.call('ZADD', KEYS[4], now, token)
  end
  for index = 1, 4 do
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
  local earliest_rate = redis.call('ZRANGE', KEYS[4], 0, 0, 'WITHSCORES')
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
for index = 1, 3 do
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
for index = 1, 3 do
  removed = removed + redis.call('ZREM', KEYS[index], token)
end
return removed
`)

type leaseKeys struct {
	total      string
	background string
	tenant     string
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
	base := m.config.KeyPrefix + hashTag
	return leaseKeys{
		total:      base + ":total",
		background: base + ":background",
		tenant:     base + ":tenant:" + strconv.FormatUint(spec.TenantID, 10),
		rate:       base + ":rate",
		circuit:    base + ":circuit",
		probe:      base + ":circuit-probe",
	}
}

func (k leaseKeys) redisKeys() []string {
	return []string{k.total, k.background, k.tenant, k.rate}
}

func (m *Manager) Acquire(ctx context.Context, spec Spec) (*Lease, error) {
	if m == nil || !m.config.Enabled {
		return newNoopLease(ctx), nil
	}
	limit := m.config.Limits[spec.Kind]
	if limit.Concurrency <= 0 && limit.RPM <= 0 && limit.PerTenant <= 0 {
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
	defer func() {
		if waiting {
			m.waiting.Add(-1)
		}
	}()
	token := uuid.NewString()
	keys := m.keys(spec)
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
			circuitProbe, circuitErr := m.acquireCircuit(waitCtx, spec, keys, token)
			if circuitErr != nil {
				m.releaseAcquired(spec, keys, token, background, local)
				if errors.Is(circuitErr, ErrProviderCircuitOpen) {
					metricResult = "circuit_open"
				} else {
					metricResult = "backend_error"
				}
				return nil, circuitErr
			}
			m.waiting.Add(-1)
			waiting = false
			m.inFlight.Add(1)
			m.acquired.Add(1)
			metricResult = "acquired"
			pipelineobs.ModelAdmissionAcquired(string(spec.Kind), background)
			lease := m.newLease(
				ctx,
				spec,
				keys,
				token,
				background,
				local,
				expectedRedisLeaseKeys(limit, m.backgroundLimit(limit), background),
				circuitProbe,
			)
			return lease, nil
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
			return nil, fmt.Errorf("wait for %s model admission: %w", spec.Kind, waitCtx.Err())
		case <-timer.C:
		}
	}
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
	backgroundLimit := limit.Concurrency - m.config.InteractiveReserve
	if backgroundLimit < 1 && limit.Concurrency > 0 {
		backgroundLimit = 1
	}
	return backgroundLimit
}

func expectedRedisLeaseKeys(limit Limit, backgroundLimit int, background bool) int64 {
	expected := int64(1) // The total lease set is always populated.
	if backgroundLimit > 0 && background {
		expected++
	}
	if limit.PerTenant > 0 {
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
	accountKey := string(spec.Kind) + ":" + spec.Domain
	tenantKey := accountKey + ":" + strconv.FormatUint(spec.TenantID, 10)
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
	case limit.RPM > 0 && len(account.rateTimestamps) >= limit.RPM:
		return false, time.Until(account.rateTimestamps[0].Add(time.Minute))
	}
	account.active++
	if background {
		account.backgroundActive++
	}
	m.localTenants[tenantKey]++
	if limit.RPM > 0 {
		account.rateTimestamps = append(account.rateTimestamps, now)
	}
	return true, 0
}

func (m *Manager) releaseLocal(spec Spec, background bool) {
	m.localMu.Lock()
	defer m.localMu.Unlock()
	accountKey := string(spec.Kind) + ":" + spec.Domain
	tenantKey := accountKey + ":" + strconv.FormatUint(spec.TenantID, 10)
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
		[]string{keys.total, keys.background, keys.tenant},
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

	ctx            context.Context
	cancel         context.CancelCauseFunc
	done           chan struct{}
	once           sync.Once
	resultOnce     sync.Once
	resultRecorded atomic.Bool
	lost           atomic.Bool
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
) *Lease {
	leaseCtx, cancel := context.WithCancelCause(parent)
	lease := &Lease{
		manager:          m,
		spec:             spec,
		keys:             keys,
		token:            token,
		background:       background,
		local:            local,
		expectedRenewals: expectedRenewals,
		circuitProbe:     circuitProbe,
		ctx:              leaseCtx,
		cancel:           cancel,
		done:             make(chan struct{}),
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
		l.manager.recordCircuitResult(l, callErr)
	})
}

// Complete reports the provider result and preserves the original provider
// error while adding a typed, retry-budget-free classification for transport,
// timeout, rate-limit and provider 5xx failures. Business handlers and Asynq
// can therefore distinguish external backpressure from malformed model output
// or deterministic document errors.
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
		retryAfter = l.manager.config.CircuitOpen
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
				[]string{l.keys.total, l.keys.background, l.keys.tenant},
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
		close(l.done)
		l.cancel(context.Canceled)
		if l.local {
			l.manager.releaseLocal(l.spec, l.background)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := releaseScript.Run(
				ctx,
				l.manager.redis,
				[]string{l.keys.total, l.keys.background, l.keys.tenant},
				l.token,
			).Err()
			cancel()
			if err != nil {
				l.manager.backendErrors.Add(1)
			}
		}
		l.manager.inFlight.Add(-1)
		pipelineobs.ModelAdmissionReleased(string(l.spec.Kind), l.background)
	})
}
