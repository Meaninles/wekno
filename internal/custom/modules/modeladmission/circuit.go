package modeladmission

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/redis/go-redis/v9"
)

// CircuitOpenError is returned before a remote call when another Pod has
// already proved that the same provider/model domain is unhealthy.
type CircuitOpenError struct {
	Kind       Kind
	RetryAfter time.Duration
}

func (e *CircuitOpenError) Error() string {
	retryAfter := e.RetryAfter
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	return fmt.Sprintf("%s for %s; retry after %s",
		ErrProviderCircuitOpen, e.Kind, retryAfter.Round(time.Second))
}

func (e *CircuitOpenError) Unwrap() error {
	return ErrProviderCircuitOpen
}

// ProviderUnavailableError wraps a real provider call failure while marking
// it as external backpressure. The cause remains discoverable through
// errors.Is/errors.As, so status-code and transport diagnostics are preserved.
// Durable business queues may add a structural workretry marker when that
// real call must consume their own bounded attempt budget.
type ProviderUnavailableError struct {
	Kind       Kind
	RetryAfter time.Duration
	Cause      error
}

func (e *ProviderUnavailableError) Error() string {
	retryAfter := e.RetryAfter
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	if e.Cause == nil {
		return fmt.Sprintf("%s for %s; retry after %s",
			ErrProviderUnavailable, e.Kind, retryAfter.Round(time.Second))
	}
	return fmt.Sprintf("%s for %s; retry after %s: %v",
		ErrProviderUnavailable, e.Kind, retryAfter.Round(time.Second), e.Cause)
}

func (e *ProviderUnavailableError) Unwrap() error {
	return e.Cause
}

func (e *ProviderUnavailableError) Is(target error) bool {
	return target == ErrProviderUnavailable || errors.Is(e.Cause, target)
}

// IsProviderUnavailable reports whether work must be durably deferred because
// either the shared circuit rejected it or a real provider call proved the
// provider/model domain unavailable.
func IsProviderUnavailable(err error) bool {
	return errors.Is(err, ErrProviderCircuitOpen) ||
		errors.Is(err, ErrProviderUnavailable)
}

// IsProviderCallFailure distinguishes a request that reached the provider
// from a CircuitOpenError rejected before any remote work began. Durable
// business queues use this boundary to count real attempts without exhausting
// their retry budget during a shared outage cooldown.
func IsProviderCallFailure(err error) bool {
	var providerErr *ProviderUnavailableError
	return errors.As(err, &providerErr)
}

// IsModelWorkDeferred includes provider backpressure plus admission backend
// and lease failures. All are infrastructure conditions that durable workers
// must retry without consuming a document's business retry budget.
func IsModelWorkDeferred(err error) bool {
	var budgeted interface {
		ConsumesModelRetryBudget() bool
	}
	if errors.As(err, &budgeted) && budgeted.ConsumesModelRetryBudget() {
		return false
	}
	return IsProviderUnavailable(err) ||
		errors.Is(err, ErrAdmissionBackendUnavailable) ||
		errors.Is(err, ErrAdmissionLeaseLost)
}

// ProviderRetryAfter returns the appropriate delay for both circuit rejects
// and the provider failure that is currently driving the circuit.
func ProviderRetryAfter(err error) (time.Duration, bool) {
	var circuitErr *CircuitOpenError
	if errors.As(err, &circuitErr) {
		return circuitErr.RetryAfter, true
	}
	var providerErr *ProviderUnavailableError
	if errors.As(err, &providerErr) {
		return providerErr.RetryAfter, true
	}
	return 0, false
}

// ModelRetryAfter returns the retry delay for every budget-free model
// infrastructure error, including temporary Redis admission failures.
func ModelRetryAfter(err error) (time.Duration, bool) {
	if retryAfter, ok := ProviderRetryAfter(err); ok {
		return retryAfter, true
	}
	if errors.Is(err, ErrAdmissionBackendUnavailable) ||
		errors.Is(err, ErrAdmissionLeaseLost) {
		return 15 * time.Second, true
	}
	return 0, false
}

// CircuitRetryAfter is retained for callers compiled against the initial
// circuit API. It now includes the provider failure that opens the circuit,
// because both outcomes have identical durable scheduling semantics.
func CircuitRetryAfter(err error) (time.Duration, bool) {
	return ModelRetryAfter(err)
}

type localCircuit struct {
	failures    int
	windowUntil time.Time
	openUntil   time.Time
	probeToken  string
	probeUntil  time.Time
}

var circuitGateScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local token = ARGV[1]
local probe_ms = tonumber(ARGV[2])
local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')

if open_until > now then
  return {0, open_until - now, 0}
end

-- A non-zero, expired open_until is the half-open state. Exactly one caller
-- across every Pod may own its probe. The probe token is also the owner's
-- normal admission-lease token in KEYS[3]. A process can disappear between
-- acquiring the half-open probe and reporting its result; reclaim that orphan
-- as soon as the renewable admission lease expires instead of blocking every
-- healthy replica for the much longer defensive probe TTL.
if open_until > 0 then
  local current_probe = redis.call('GET', KEYS[2])
  if current_probe then
    local lease_until = tonumber(redis.call('ZSCORE', KEYS[3], current_probe) or '0')
    if lease_until <= now then
      redis.call('DEL', KEYS[2])
      current_probe = false
    end
  end
  if current_probe == token then
    return {1, 0, 1}
  end
  if not current_probe then
    local acquired = redis.call('SET', KEYS[2], token, 'PX', probe_ms, 'NX')
    if acquired then
      return {1, 0, 1}
    end
  end
  local probe_ttl = redis.call('PTTL', KEYS[2])
  if probe_ttl < 25 then
    probe_ttl = 25
  end
  return {0, probe_ttl, 0}
end

return {1, 0, 0}
`)

var circuitResultScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local token = ARGV[1]
local outcome = ARGV[2]
local threshold = tonumber(ARGV[3])
local window_ms = tonumber(ARGV[4])
local open_ms = tonumber(ARGV[5])
local retention_ms = tonumber(ARGV[6])
local current_probe = redis.call('GET', KEYS[2])
local owns_probe = current_probe == token

if outcome == 'success' then
  redis.call('DEL', KEYS[1])
  if owns_probe then
    redis.call('DEL', KEYS[2])
  end
  return {0, 0}
end

if outcome == 'abandon' then
  if owns_probe then
    redis.call('DEL', KEYS[2])
  end
  return {0, 0}
end

if owns_probe then
  redis.call('DEL', KEYS[2])
  local open_until = now + open_ms
  redis.call('HSET', KEYS[1],
    'failures', 0,
    'window_until', now + window_ms,
    'open_until', open_until)
  redis.call('PEXPIRE', KEYS[1], retention_ms)
  return {1, open_until}
end

local window_until = tonumber(redis.call('HGET', KEYS[1], 'window_until') or '0')
local failures = tonumber(redis.call('HGET', KEYS[1], 'failures') or '0')
if window_until <= now then
  failures = 1
  window_until = now + window_ms
else
  failures = failures + 1
end

local open_until = tonumber(redis.call('HGET', KEYS[1], 'open_until') or '0')
if failures >= threshold then
  open_until = now + open_ms
  failures = 0
end
redis.call('HSET', KEYS[1],
  'failures', failures,
  'window_until', window_until,
  'open_until', open_until)
redis.call('PEXPIRE', KEYS[1], retention_ms)
if open_until > now then
  return {1, open_until}
end
return {0, 0}
`)

var abandonProbeScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)

func (m *Manager) acquireCircuit(
	ctx context.Context,
	spec Spec,
	keys leaseKeys,
	token string,
) (bool, error) {
	if m == nil || !m.config.CircuitEnabled {
		return false, nil
	}
	if m.redis != nil {
		allowed, retryAfter, probe, err := m.acquireCircuitRedis(ctx, keys, token)
		if err == nil {
			if !allowed {
				m.circuitReject.Add(1)
				return false, &CircuitOpenError{Kind: spec.Kind, RetryAfter: retryAfter}
			}
			return probe, nil
		}
		m.backendErrors.Add(1)
		if m.config.FailClosed {
			return false, fmt.Errorf("%w: circuit state: %v", ErrAdmissionBackendUnavailable, err)
		}
	}
	return m.acquireCircuitLocal(spec, token)
}

func (m *Manager) acquireCircuitRedis(
	ctx context.Context,
	keys leaseKeys,
	token string,
) (bool, time.Duration, bool, error) {
	result, err := circuitGateScript.Run(
		ctx,
		m.redis,
		[]string{keys.circuit, keys.probe, keys.total},
		token,
		m.config.CircuitProbeTTL.Milliseconds(),
	).Slice()
	if err != nil {
		return false, 0, false, err
	}
	if len(result) != 3 {
		return false, 0, false, fmt.Errorf("unexpected circuit gate response length %d", len(result))
	}
	allowed, err := redisNumber(result[0])
	if err != nil {
		return false, 0, false, err
	}
	retryMillis, err := redisNumber(result[1])
	if err != nil {
		return false, 0, false, err
	}
	probe, err := redisNumber(result[2])
	if err != nil {
		return false, 0, false, err
	}
	return allowed == 1, time.Duration(retryMillis) * time.Millisecond, probe == 1, nil
}

func (m *Manager) acquireCircuitLocal(spec Spec, token string) (bool, error) {
	m.localMu.Lock()
	defer m.localMu.Unlock()

	key := circuitAccountKey(spec)
	state := m.localCircuits[key]
	if state == nil {
		return false, nil
	}
	now := time.Now()
	if state.openUntil.After(now) {
		m.circuitReject.Add(1)
		return false, &CircuitOpenError{Kind: spec.Kind, RetryAfter: time.Until(state.openUntil)}
	}
	if !state.openUntil.IsZero() {
		if state.probeToken != "" && state.probeUntil.After(now) && state.probeToken != token {
			m.circuitReject.Add(1)
			return false, &CircuitOpenError{Kind: spec.Kind, RetryAfter: time.Until(state.probeUntil)}
		}
		state.probeToken = token
		state.probeUntil = now.Add(m.config.CircuitProbeTTL)
		return true, nil
	}
	return false, nil
}

func circuitAccountKey(spec Spec) string {
	return string(spec.Kind) + ":" + spec.Domain
}

type circuitOutcome string

const (
	circuitSuccess circuitOutcome = "success"
	circuitFailure circuitOutcome = "failure"
	circuitAbandon circuitOutcome = "abandon"
)

func classifyCircuitOutcome(lease *Lease, callErr error) circuitOutcome {
	outcome := circuitSuccess
	if callErr != nil {
		switch {
		case errors.Is(callErr, ErrAdmissionBackendUnavailable),
			errors.Is(callErr, ErrAdmissionLeaseLost),
			errors.Is(callErr, ErrProviderCircuitOpen),
			errors.Is(callErr, context.Canceled):
			outcome = circuitAbandon
		case lease.ctx != nil && lease.ctx.Err() != nil:
			// The document/task deadline or shutdown cancelled the call; this
			// is not evidence that the provider itself is unhealthy.
			outcome = circuitAbandon
		case isCircuitFailure(callErr):
			outcome = circuitFailure
		}
	}
	return outcome
}

func (m *Manager) recordCircuitResult(lease *Lease, callErr error) {
	if m == nil || lease == nil || !m.config.CircuitEnabled {
		return
	}
	outcome := classifyCircuitOutcome(lease, callErr)

	if m.redis != nil {
		opened, err := m.recordCircuitRedis(lease.keys, lease.token, outcome)
		if err == nil {
			if opened {
				m.circuitOpened.Add(1)
			}
			return
		}
		m.backendErrors.Add(1)
	}
	if m.recordCircuitLocal(lease.spec, lease.token, outcome) {
		m.circuitOpened.Add(1)
	}
}

func (m *Manager) recordCircuitRedis(
	keys leaseKeys,
	token string,
	outcome circuitOutcome,
) (bool, error) {
	retention := 2 * (m.config.CircuitWindow + m.config.CircuitOpen + m.config.CircuitProbeTTL)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := circuitResultScript.Run(
		ctx,
		m.redis,
		[]string{keys.circuit, keys.probe},
		token,
		string(outcome),
		m.config.CircuitThreshold,
		m.config.CircuitWindow.Milliseconds(),
		m.config.CircuitOpen.Milliseconds(),
		retention.Milliseconds(),
	).Slice()
	if err != nil {
		return false, err
	}
	if len(result) != 2 {
		return false, fmt.Errorf("unexpected circuit result response length %d", len(result))
	}
	opened, err := redisNumber(result[0])
	if err != nil {
		return false, err
	}
	return opened == 1, nil
}

func (m *Manager) recordCircuitLocal(spec Spec, token string, outcome circuitOutcome) bool {
	m.localMu.Lock()
	defer m.localMu.Unlock()

	key := circuitAccountKey(spec)
	state := m.localCircuits[key]
	if outcome == circuitSuccess {
		delete(m.localCircuits, key)
		return false
	}
	if state == nil {
		state = &localCircuit{}
		m.localCircuits[key] = state
	}
	ownsProbe := state.probeToken == token && token != ""
	if outcome == circuitAbandon {
		if ownsProbe {
			state.probeToken = ""
			state.probeUntil = time.Time{}
		}
		return false
	}

	now := time.Now()
	if ownsProbe {
		state.probeToken = ""
		state.probeUntil = time.Time{}
		state.failures = 0
		state.windowUntil = now.Add(m.config.CircuitWindow)
		state.openUntil = now.Add(m.config.CircuitOpen)
		return true
	}
	if !state.windowUntil.After(now) {
		state.failures = 1
		state.windowUntil = now.Add(m.config.CircuitWindow)
	} else {
		state.failures++
	}
	if state.failures >= m.config.CircuitThreshold {
		state.failures = 0
		state.openUntil = now.Add(m.config.CircuitOpen)
		return true
	}
	return false
}

func (m *Manager) abandonCircuitProbe(spec Spec, keys leaseKeys, token string) {
	if m == nil || !m.config.CircuitEnabled || token == "" {
		return
	}
	if m.redis != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		err := abandonProbeScript.Run(ctx, m.redis, []string{keys.probe}, token).Err()
		cancel()
		if err == nil {
			return
		}
		m.backendErrors.Add(1)
	}
	m.localMu.Lock()
	defer m.localMu.Unlock()
	state := m.localCircuits[circuitAccountKey(spec)]
	if state != nil && state.probeToken == token {
		state.probeToken = ""
		state.probeUntil = time.Time{}
	}
}

func isCircuitFailure(err error) bool {
	if err == nil ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, ErrAdmissionBackendUnavailable) ||
		errors.Is(err, ErrAdmissionLeaseLost) ||
		errors.Is(err, ErrProviderCircuitOpen) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if status, ok := chat.HTTPStatusCode(err); ok {
		return status == 401 || status == 403 || status == 404 ||
			status == 408 || status == 429 || status >= 500
	}
	var networkErr net.Error
	if errors.As(err, &networkErr) {
		return true
	}

	lower := strings.ToLower(err.Error())
	if status, ok := statusCodeFromText(lower); ok {
		return status == 401 || status == 403 || status == 404 ||
			status == 408 || status == 429 || status >= 500
	}
	for _, fragment := range []string{
		"context deadline exceeded",
		"timeout",
		"timed out",
		"connection refused",
		"connection reset",
		"connection closed",
		"broken pipe",
		"no such host",
		"no route to host",
		"network is unreachable",
		"i/o timeout",
		"unexpected eof",
		"tls handshake",
		"service unavailable",
		"temporarily unavailable",
		"too many requests",
		"unauthorized",
		"authentication failed",
		"model not found",
	} {
		if strings.Contains(lower, fragment) {
			return true
		}
	}
	return false
}

func statusCodeFromText(message string) (int, bool) {
	const marker = "status "
	start := strings.Index(message, marker)
	if start < 0 {
		return 0, false
	}
	start += len(marker)
	if len(message) < start+3 {
		return 0, false
	}
	status, err := strconv.Atoi(message[start : start+3])
	if err != nil {
		return 0, false
	}
	return status, true
}
