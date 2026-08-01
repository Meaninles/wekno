package modeladmission

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

type WorkLane string

const (
	WorkLaneDerivative WorkLane = "derivative"
	WorkLaneWiki       WorkLane = "wiki"
)

// PoolRuntimeStats is a point-in-time, cross-instance view backed by the same
// Redis lease sets that enforce capacity. Unlike Manager.Snapshot, it is not
// limited to counters in the API process.
type PoolRuntimeStats struct {
	ProviderInFlight       int64 `json:"provider_inflight"`
	ProviderBackground     int64 `json:"provider_background"`
	ProviderDerivative     int64 `json:"provider_derivative"`
	ProviderWiki           int64 `json:"provider_wiki"`
	ProviderDerivativeWait int64 `json:"provider_derivative_waiting"`
	ProviderWikiWait       int64 `json:"provider_wiki_waiting"`
	WorkActive             int64 `json:"work_active"`
	WorkDerivativeActive   int64 `json:"work_derivative_active"`
	WorkWikiActive         int64 `json:"work_wiki_active"`
	WorkDerivativeWait     int64 `json:"work_derivative_waiting"`
	WorkWikiWait           int64 `json:"work_wiki_waiting"`
}

type workLaneContextKey struct{}
type taskWorkLeaseContextKey struct{}

type taskWorkLeaseContext struct {
	poolID string
	lane   WorkLane
}

// WithWorkLane identifies a background model consumer without changing its
// interactive/background class. The provider and work-window schedulers use
// the lane only for work-conserving weighted fairness.
func WithWorkLane(ctx context.Context, lane WorkLane) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	switch lane {
	case WorkLaneDerivative, WorkLaneWiki:
		return context.WithValue(ctx, workLaneContextKey{}, lane)
	default:
		return ctx
	}
}

func hasTaskWorkLease(ctx context.Context, poolID string, lane WorkLane) bool {
	if ctx == nil || !lane.valid() {
		return false
	}
	value, _ := ctx.Value(taskWorkLeaseContextKey{}).(taskWorkLeaseContext)
	return value.poolID == strings.TrimSpace(poolID) && value.lane == lane
}

func workLaneFromContext(ctx context.Context, spec Spec) WorkLane {
	if ctx != nil {
		if lane, _ := ctx.Value(workLaneContextKey{}).(WorkLane); lane.valid() {
			return lane
		}
	}
	if spec.Kind == KindDerivative || spec.DerivativeOnly {
		return WorkLaneDerivative
	}
	return ""
}

func (lane WorkLane) valid() bool {
	return lane == WorkLaneDerivative || lane == WorkLaneWiki
}

func schedulerPolicyFromConfig(config Config) SchedulerPolicy {
	policy := DefaultSchedulerPolicy()
	if config.WorkPrefetchFactor > 0 {
		policy.PrefetchFactor = config.WorkPrefetchFactor
	}
	if config.DerivativeWeight > 0 {
		policy.DerivativeWeight = config.DerivativeWeight
	}
	if config.WikiWeight > 0 {
		policy.WikiWeight = config.WikiWeight
	}
	if config.BackgroundMaxWait > 0 {
		policy.BackgroundMaxWaitSeconds = int(config.BackgroundMaxWait / time.Second)
	}
	if config.DispatchLease > 0 {
		policy.DispatchLeaseSeconds = int(config.DispatchLease / time.Second)
	}
	return policy
}

func (m *Manager) schedulerPolicyForAcquire(ctx context.Context) (SchedulerPolicy, bool, error) {
	if m == nil {
		return SchedulerPolicy{}, false, errors.New("model admission manager is unavailable")
	}
	if m.store != nil {
		policy, err := m.store.SchedulerPolicy(ctx)
		return policy, true, err
	}
	return schedulerPolicyFromConfig(m.config), m.config.WorkWindowEnabled, nil
}

func laneShares(total int, policy SchedulerPolicy) (int, int) {
	if total <= 0 {
		return 0, 0
	}
	if total == 1 {
		// Both lanes may contend for the one shared slot; total capacity remains
		// authoritative and the waiting marker prevents permanent starvation.
		return 1, 1
	}
	derivativeWeight := policy.DerivativeWeight
	wikiWeight := policy.WikiWeight
	if derivativeWeight < 1 {
		derivativeWeight = 2
	}
	if wikiWeight < 1 {
		wikiWeight = 1
	}
	weightTotal := derivativeWeight + wikiWeight
	derivative := (total*derivativeWeight + weightTotal - 1) / weightTotal
	if derivative < 1 {
		derivative = 1
	}
	if derivative >= total {
		derivative = total - 1
	}
	wiki := total - derivative
	if wiki < 1 {
		wiki = 1
		derivative = total - 1
	}
	return derivative, wiki
}

func WorkWindow(backgroundCapacity int, policy SchedulerPolicy) int {
	if backgroundCapacity < 1 {
		return 0
	}
	factor := policy.PrefetchFactor
	if factor < 1 {
		factor = 2
	}
	return backgroundCapacity * factor
}

func LaneWorkWindow(backgroundCapacity int, lane WorkLane, policy SchedulerPolicy) int {
	derivative, wiki := laneShares(WorkWindow(backgroundCapacity, policy), policy)
	if lane == WorkLaneWiki {
		return wiki
	}
	return derivative
}

// DispatchWindow is the compatibility projection for callers that need only a
// lane's protected share. Cross-outbox dispatchers use DispatchLimits so they
// can enforce the shared total and lend idle capacity.
func (m *Manager) DispatchWindow(
	ctx context.Context,
	poolID string,
	lane WorkLane,
) (int, time.Duration, error) {
	_, share, lease, err := m.DispatchLimits(ctx, poolID, lane)
	return share, lease, err
}

// DispatchLimits returns both the shared cross-lane prefetch ceiling and this
// lane's protected weighted share. Dispatchers serialize on
// DispatchAdvisoryKey and count reservations from both durable outboxes: an
// idle lane lends its unused share, while simultaneous backlogs converge to
// the configured weights without exceeding the shared total.
func (m *Manager) DispatchLimits(
	ctx context.Context,
	poolID string,
	lane WorkLane,
) (int, int, time.Duration, error) {
	if m == nil || m.store == nil || !lane.valid() {
		return 0, 0, 0, nil
	}
	pool, err := m.GetResourcePool(ctx, poolID)
	if err != nil {
		return 0, 0, 0, err
	}
	policy, err := m.store.SchedulerPolicy(ctx)
	if err != nil {
		return 0, 0, 0, err
	}
	background := EffectiveBackgroundLimit(pool.MaxInflight, pool.InteractiveReserve)
	total := WorkWindow(background, policy)
	return total, LaneWorkWindow(background, lane, policy),
		time.Duration(policy.DispatchLeaseSeconds) * time.Second, nil
}

// DispatchAdvisoryKey is shared by every durable outbox that targets the same
// physical model pool. Keeping the hash here prevents independent dispatchers
// from accidentally using different lock domains.
func DispatchAdvisoryKey(poolID string) int64 {
	hash := fnv.New64a()
	_, _ = hash.Write([]byte("model-work-dispatch\x00" + strings.TrimSpace(poolID)))
	return int64(hash.Sum64())
}

// AcquireTaskWork claims one cross-instance background execution slot for a
// complete durable task (or one independently durable Wiki document). The
// returned context must be used for all provider calls made by that task and
// release must be called when the task stops. Provider wrappers recognise the
// context marker and therefore do not claim a second work-window slot for
// each nested model request; the provider semaphore remains authoritative for
// actual GPU concurrency and applies its independently configured wait bound.
func (m *Manager) AcquireTaskWork(
	ctx context.Context,
	spec Spec,
	lane WorkLane,
) (context.Context, func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	noop := func() {}
	if m == nil || !m.config.Enabled || !lane.valid() {
		return ctx, noop, nil
	}
	resolved, err := m.ResolvePolicy(ctx, spec)
	if err != nil {
		m.backendErrors.Add(1)
		if m.config.FailClosed {
			return ctx, noop, fmt.Errorf(
				"%w: resolve task work resource pool: %v",
				ErrAdmissionBackendUnavailable, err,
			)
		}
		resolved = resolvedBuiltinPolicy(
			spec.Domain,
			builtinPolicy(spec.Kind, spec.DerivativeOnly),
			"builtin",
		)
	}
	if hasTaskWorkLease(ctx, resolved.PoolID, lane) {
		return ctx, noop, nil
	}
	policy, enabled, err := m.schedulerPolicyForAcquire(ctx)
	if err != nil {
		m.backendErrors.Add(1)
		return ctx, noop, fmt.Errorf(
			"%w: resolve task work scheduler policy: %v",
			ErrAdmissionBackendUnavailable, err,
		)
	}
	if !enabled {
		return ctx, noop, nil
	}
	backgroundCapacity := m.backgroundLimit(resolved.Limit)
	lease, err := m.acquireWorkWindow(
		ctx, resolved.PoolID, lane, backgroundCapacity, policy,
	)
	if err != nil {
		pipelineobs.ModelPoolRejected(resolved.PoolID, "task_work_window")
		return ctx, noop, err
	}
	if lease == nil {
		return ctx, noop, nil
	}
	workCtx := context.WithValue(
		WithWorkLane(lease.Context(), lane),
		taskWorkLeaseContextKey{},
		taskWorkLeaseContext{poolID: resolved.PoolID, lane: lane},
	)
	return workCtx, lease.Release, nil
}

// PoolRuntimeSnapshot reads only non-expired lease/waiter members. It does
// not mutate scheduler state and is safe for the settings diagnostics page.
func (m *Manager) PoolRuntimeSnapshot(
	ctx context.Context,
	poolID string,
) (PoolRuntimeStats, error) {
	if m == nil {
		return PoolRuntimeStats{}, errors.New("model admission manager is unavailable")
	}
	if m.redis == nil {
		m.localMu.Lock()
		defer m.localMu.Unlock()
		var result PoolRuntimeStats
		if account := m.localAccounts[poolID]; account != nil {
			result.ProviderInFlight = int64(account.active)
			result.ProviderBackground = int64(account.backgroundActive)
			result.ProviderDerivative = int64(account.laneActive[0])
			result.ProviderWiki = int64(account.laneActive[1])
			now := time.Now()
			if account.laneWaitUntil[0].After(now) {
				result.ProviderDerivativeWait = 1
			}
			if account.laneWaitUntil[1].After(now) {
				result.ProviderWikiWait = 1
			}
		}
		if work := m.localWork[poolID]; work != nil {
			result.WorkDerivativeActive = int64(work.active[0])
			result.WorkWikiActive = int64(work.active[1])
			result.WorkActive = result.WorkDerivativeActive + result.WorkWikiActive
			now := time.Now()
			if work.waitUntil[0].After(now) {
				result.WorkDerivativeWait = 1
			}
			if work.waitUntil[1].After(now) {
				result.WorkWikiWait = 1
			}
		}
		return result, nil
	}

	provider := m.keys(Spec{Domain: poolID})
	work := m.workKeys(poolID)
	minimum := strconv.FormatInt(time.Now().UnixMilli()+1, 10)
	pipe := m.redis.Pipeline()
	commands := []*redis.IntCmd{
		pipe.ZCount(ctx, provider.total, minimum, "+inf"),
		pipe.ZCount(ctx, provider.background, minimum, "+inf"),
		pipe.ZCount(ctx, provider.derivativeActive, minimum, "+inf"),
		pipe.ZCount(ctx, provider.wikiActive, minimum, "+inf"),
		pipe.ZCount(ctx, provider.derivativeWaiting, minimum, "+inf"),
		pipe.ZCount(ctx, provider.wikiWaiting, minimum, "+inf"),
		pipe.ZCount(ctx, work.derivativeActive, minimum, "+inf"),
		pipe.ZCount(ctx, work.wikiActive, minimum, "+inf"),
		pipe.ZCount(ctx, work.derivativeWaiting, minimum, "+inf"),
		pipe.ZCount(ctx, work.wikiWaiting, minimum, "+inf"),
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return PoolRuntimeStats{}, err
	}
	values := make([]int64, len(commands))
	for index, command := range commands {
		values[index] = command.Val()
	}
	return PoolRuntimeStats{
		ProviderInFlight: values[0], ProviderBackground: values[1],
		ProviderDerivative: values[2], ProviderWiki: values[3],
		ProviderDerivativeWait: values[4], ProviderWikiWait: values[5],
		WorkActive:           values[6] + values[7],
		WorkDerivativeActive: values[6], WorkWikiActive: values[7],
		WorkDerivativeWait: values[8], WorkWikiWait: values[9],
	}, nil
}

var acquireWorkScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local total_limit = tonumber(ARGV[1])
local derivative_limit = tonumber(ARGV[2])
local wiki_limit = tonumber(ARGV[3])
local lane = ARGV[4]
local token = ARGV[5]
local lease_ms = tonumber(ARGV[6])
local waiter_ms = tonumber(ARGV[7])

for index = 1, 4 do
  redis.call('ZREMRANGEBYSCORE', KEYS[index], '-inf', now)
end

local own_active = KEYS[1]
local other_active = KEYS[2]
local own_waiting = KEYS[3]
local other_waiting = KEYS[4]
local own_limit = derivative_limit
if lane == 'wiki' then
  own_active = KEYS[2]
  other_active = KEYS[1]
  own_waiting = KEYS[4]
  other_waiting = KEYS[3]
  own_limit = wiki_limit
end

if redis.call('ZSCORE', own_active, token) then
  redis.call('ZREM', own_waiting, token)
  return {1, 0}
end

redis.call('ZADD', own_waiting, now + waiter_ms, token)
local total_count = redis.call('ZCARD', KEYS[1]) + redis.call('ZCARD', KEYS[2])
local own_count = redis.call('ZCARD', own_active)
local other_wait_count = redis.call('ZCARD', other_waiting)
local allowed = total_count < total_limit
if allowed and own_count >= own_limit and other_wait_count > 0 then
  allowed = false
end

if allowed then
  local expires = now + lease_ms
  redis.call('ZREM', own_waiting, token)
  redis.call('ZADD', own_active, expires, token)
  for index = 1, 4 do
    redis.call('PEXPIRE', KEYS[index], lease_ms * 2)
  end
  return {1, 0}
end
for index = 1, 4 do
  redis.call('PEXPIRE', KEYS[index], math.max(lease_ms * 2, waiter_ms * 2))
end
return {0, 15000}
`)

var renewWorkScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local token = ARGV[1]
local lease_ms = tonumber(ARGV[2])
local expires = now + lease_ms
for index = 1, 2 do
  if redis.call('ZSCORE', KEYS[index], token) then
    redis.call('ZADD', KEYS[index], 'XX', expires, token)
    redis.call('PEXPIRE', KEYS[index], lease_ms * 2)
    return 1
  end
end
return 0
`)

var releaseWorkScript = redis.NewScript(`
local token = ARGV[1]
local removed = 0
for index = 1, 4 do
  removed = removed + redis.call('ZREM', KEYS[index], token)
end
return removed
`)

type workKeys struct {
	derivativeActive  string
	wikiActive        string
	derivativeWaiting string
	wikiWaiting       string
}

func (m *Manager) workKeys(poolID string) workKeys {
	poolID = strings.TrimSpace(poolID)
	if poolID == "" {
		poolID = "unknown"
	}
	base := m.config.KeyPrefix + "work:{" + poolID + "}"
	return workKeys{
		derivativeActive:  base + ":active:derivative",
		wikiActive:        base + ":active:wiki",
		derivativeWaiting: base + ":waiting:derivative",
		wikiWaiting:       base + ":waiting:wiki",
	}
}

func (keys workKeys) all() []string {
	return []string{
		keys.derivativeActive, keys.wikiActive,
		keys.derivativeWaiting, keys.wikiWaiting,
	}
}

type WorkLease struct {
	manager *Manager
	keys    workKeys
	poolID  string
	lane    WorkLane
	token   string
	local   bool

	ctx    context.Context
	cancel context.CancelCauseFunc
	done   chan struct{}
	once   sync.Once
	lost   atomic.Bool
}

func (m *Manager) acquireWorkWindow(
	ctx context.Context,
	poolID string,
	lane WorkLane,
	backgroundCapacity int,
	policy SchedulerPolicy,
) (*WorkLease, error) {
	if !lane.valid() || backgroundCapacity < 1 {
		return nil, nil
	}
	totalLimit := WorkWindow(backgroundCapacity, policy)
	derivativeLimit, wikiLimit := laneShares(totalLimit, policy)
	keys := m.workKeys(poolID)
	token := uuid.NewString()
	local := m.redis == nil
	allowed := false
	retryAfter := 15 * time.Second
	if local {
		allowed = m.tryAcquireLocalWork(poolID, lane, token, totalLimit, derivativeLimit, wikiLimit)
	} else {
		result, err := acquireWorkScript.Run(
			ctx, m.redis, keys.all(), totalLimit, derivativeLimit, wikiLimit,
			string(lane), token, m.config.LeaseTTL.Milliseconds(), int64(5*time.Second/time.Millisecond),
		).Slice()
		if err != nil {
			m.backendErrors.Add(1)
			if m.config.FailClosed {
				return nil, fmt.Errorf("%w: acquire background work window: %v", ErrAdmissionBackendUnavailable, err)
			}
			local = true
			allowed = m.tryAcquireLocalWork(poolID, lane, token, totalLimit, derivativeLimit, wikiLimit)
		} else if len(result) == 2 {
			value, parseErr := redisNumber(result[0])
			if parseErr != nil {
				return nil, parseErr
			}
			allowed = value == 1
			if millis, parseErr := redisNumber(result[1]); parseErr == nil && millis > 0 {
				retryAfter = time.Duration(millis) * time.Millisecond
			}
		}
	}
	if !allowed {
		m.workDeferred.Add(1)
		return nil, &AdmissionDeferredError{
			Kind: KindDerivative, PoolID: poolID, RetryAfter: retryAfter,
		}
	}
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &WorkLease{
		manager: m, keys: keys, poolID: poolID, lane: lane, token: token,
		local: local, ctx: leaseCtx, cancel: cancel, done: make(chan struct{}),
	}
	m.workActive.Add(1)
	if lane == WorkLaneWiki {
		m.workWikiActive.Add(1)
	} else {
		m.workDerivativeActive.Add(1)
	}
	if !local {
		go lease.heartbeat()
	}
	return lease, nil
}

func (m *Manager) tryAcquireLocalWork(
	poolID string,
	lane WorkLane,
	token string,
	totalLimit, derivativeLimit, wikiLimit int,
) bool {
	m.localMu.Lock()
	defer m.localMu.Unlock()
	now := time.Now()
	state := m.localWork[poolID]
	if state == nil {
		state = &localWorkState{tokens: make(map[string]WorkLane)}
		m.localWork[poolID] = state
	}
	if existing, ok := state.tokens[token]; ok {
		return existing == lane
	}
	state.waitUntil[lane.index()] = now.Add(5 * time.Second)
	total := state.active[0] + state.active[1]
	index := lane.index()
	other := 1 - index
	limit := derivativeLimit
	if lane == WorkLaneWiki {
		limit = wikiLimit
	}
	if total >= totalLimit || (state.active[index] >= limit && state.waitUntil[other].After(now)) {
		return false
	}
	state.active[index]++
	state.tokens[token] = lane
	state.waitUntil[index] = time.Time{}
	return true
}

func (lane WorkLane) index() int {
	if lane == WorkLaneWiki {
		return 1
	}
	return 0
}

func (lease *WorkLease) Context() context.Context {
	if lease == nil || lease.ctx == nil {
		return context.Background()
	}
	return lease.ctx
}

func (lease *WorkLease) heartbeat() {
	ticker := time.NewTicker(lease.manager.config.HeartbeatInterval)
	defer ticker.Stop()
	consecutiveErrors := 0
	for {
		select {
		case <-lease.done:
			return
		case <-lease.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), lease.manager.config.HeartbeatInterval)
			renewed, err := renewWorkScript.Run(
				ctx, lease.manager.redis, lease.keys.all()[:2], lease.token,
				lease.manager.config.LeaseTTL.Milliseconds(),
			).Int64()
			cancel()
			if err != nil {
				consecutiveErrors++
				lease.manager.backendErrors.Add(1)
				if consecutiveErrors < 2 {
					continue
				}
			} else if renewed == 1 {
				consecutiveErrors = 0
				continue
			}
			lease.lost.Store(true)
			lease.manager.leaseLost.Add(1)
			lease.cancel(ErrAdmissionLeaseLost)
			return
		}
	}
}

func (lease *WorkLease) Release() {
	if lease == nil || lease.manager == nil {
		return
	}
	lease.once.Do(func() {
		close(lease.done)
		lease.cancel(context.Canceled)
		if lease.local {
			lease.manager.localMu.Lock()
			if state := lease.manager.localWork[lease.poolID]; state != nil {
				if lane, ok := state.tokens[lease.token]; ok {
					index := lane.index()
					if state.active[index] > 0 {
						state.active[index]--
					}
					delete(state.tokens, lease.token)
				}
			}
			lease.manager.localMu.Unlock()
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			err := releaseWorkScript.Run(ctx, lease.manager.redis, lease.keys.all(), lease.token).Err()
			cancel()
			if err != nil {
				lease.manager.backendErrors.Add(1)
			}
		}
		lease.manager.workActive.Add(-1)
		if lease.lane == WorkLaneWiki {
			lease.manager.workWikiActive.Add(-1)
		} else {
			lease.manager.workDerivativeActive.Add(-1)
		}
	})
}
