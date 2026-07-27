package derivativecontrol

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	agenttoken "github.com/Tencent/WeKnora/internal/agent/token"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	DefaultTPM               int64 = 20_000
	minTPM                   int64 = 100
	maxTPM                   int64 = 2_000_000
	defaultOutputReservation       = 4_096
	leaseTTL                       = 45 * time.Second
	leaseHeartbeat                 = 10 * time.Second
	busyRetryFloor                 = 2 * time.Second
	timeoutSafetyCooldown          = 10 * time.Minute
	failureSafetyCooldown          = 30 * time.Second
)

// DeferredError marks control-plane waits as budget-free model deferrals.
// Asynq and the durable Wiki queue consume ModelRetryAfter instead of burning
// document retry budgets.
type DeferredError struct {
	Reason     string
	RetryAfter time.Duration
	Cause      error
}

func (e *DeferredError) Error() string {
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "derivative model work is temporarily deferred"
	}
	if e.Cause != nil {
		return fmt.Sprintf("%s; retry after %s: %v", reason, e.delay(), e.Cause)
	}
	return fmt.Sprintf("%s; retry after %s", reason, e.delay())
}

func (e *DeferredError) Unwrap() error                  { return e.Cause }
func (e *DeferredError) ModelWorkDeferred() bool        { return true }
func (e *DeferredError) ModelRetryAfter() time.Duration { return e.delay() }

func (e *DeferredError) delay() time.Duration {
	if e.RetryAfter < time.Second {
		return time.Second
	}
	return e.RetryAfter
}

var (
	acquireLeaseScript = redis.NewScript(`
local ok = redis.call('SET', KEYS[1], ARGV[1], 'NX', 'PX', ARGV[2])
if ok then
  return {1, tonumber(ARGV[2])}
end
local ttl = redis.call('PTTL', KEYS[1])
if ttl < 0 then ttl = 2000 end
return {0, ttl}
`)
	renewLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  redis.call('PEXPIRE', KEYS[1], ARGV[2])
  return 1
end
return 0
`)
	releaseLeaseScript = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
  return redis.call('DEL', KEYS[1])
end
return 0
`)
	paceScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local next_at = tonumber(redis.call('GET', KEYS[1]) or tostring(now))
if next_at > now then
  return {0, next_at - now}
end
local interval = tonumber(ARGV[1])
local updated = now + interval
redis.call('SET', KEYS[1], tostring(updated), 'PX', math.max(interval + 3600000, 3600000))
return {1, 0}
`)
	extendPaceScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local next_at = tonumber(redis.call('GET', KEYS[1]) or tostring(now))
if next_at < now then next_at = now end
local requested = now + tonumber(ARGV[1])
if requested > next_at then next_at = requested end
redis.call('SET', KEYS[1], tostring(next_at), 'PX', math.max(tonumber(ARGV[1]) + 3600000, 3600000))
return next_at
`)
	addDebtScript = redis.NewScript(`
local clock = redis.call('TIME')
local now = (tonumber(clock[1]) * 1000) + math.floor(tonumber(clock[2]) / 1000)
local next_at = tonumber(redis.call('GET', KEYS[1]) or tostring(now))
if next_at < now then next_at = now end
next_at = next_at + tonumber(ARGV[1])
redis.call('SET', KEYS[1], tostring(next_at), 'PX', math.max(tonumber(ARGV[1]) + 3600000, 3600000))
return next_at
`)
)

type Limiter struct {
	rdb      *redis.Client
	settings interfaces.SystemSettingService
	estimate *agenttoken.Estimator

	localMu     sync.Mutex
	localActive bool
	localNext   time.Time

	acquired atomic.Uint64
	deferred atomic.Uint64
}

type LimiterSnapshot struct {
	TPM      int64  `json:"tpm"`
	Mode     string `json:"mode"`
	Active   bool   `json:"active"`
	Acquired uint64 `json:"acquired"`
	Deferred uint64 `json:"deferred"`
}

func NewLimiter(rdb *redis.Client, settings interfaces.SystemSettingService) *Limiter {
	estimator, _ := agenttoken.NewEstimator()
	return &Limiter{rdb: rdb, settings: settings, estimate: estimator}
}

func (l *Limiter) TPM(ctx context.Context) int64 {
	value := DefaultTPM
	if l != nil && l.settings != nil {
		value = l.settings.GetInt(
			ctx, "derivative.tpm", "WEKNORA_DERIVATIVE_TPM", DefaultTPM,
		)
	}
	if value < minTPM {
		return minTPM
	}
	if value > maxTPM {
		return maxTPM
	}
	return value
}

func (l *Limiter) Snapshot(ctx context.Context) LimiterSnapshot {
	snapshot := LimiterSnapshot{
		TPM: l.TPM(ctx), Mode: "local",
		Acquired: l.acquired.Load(), Deferred: l.deferred.Load(),
	}
	if l.rdb != nil {
		snapshot.Mode = "redis"
		probeCtx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		exists, err := l.rdb.Exists(probeCtx, l.key("active")).Result()
		if err == nil {
			snapshot.Active = exists > 0
		}
		return snapshot
	}
	l.localMu.Lock()
	snapshot.Active = l.localActive
	l.localMu.Unlock()
	return snapshot
}

func (l *Limiter) Wrap(inner chat.Chat) chat.Chat {
	if l == nil || inner == nil {
		return inner
	}
	return &limitedChat{limiter: l, inner: inner}
}

type limitedChat struct {
	limiter *Limiter
	inner   chat.Chat
}

func (w *limitedChat) GetModelName() string { return w.inner.GetModelName() }
func (w *limitedChat) GetModelID() string   { return w.inner.GetModelID() }

func (w *limitedChat) Chat(
	ctx context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (*types.ChatResponse, error) {
	reservation := w.limiter.estimateTokens(messages, options)
	charged := min(reservation, int(w.limiter.TPM(ctx)))
	lease, err := w.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer lease.release()
	if err := lease.pace(ctx, reservation); err != nil {
		return nil, err
	}
	lease.callStarted = time.Now()

	response, callErr := w.inner.Chat(
		modeladmission.WithBackground(lease.ctx),
		messages,
		options,
	)
	actual := 0
	if response != nil {
		actual = response.Usage.TotalTokens
		if actual <= 0 {
			actual = response.Usage.PromptTokens + response.Usage.CompletionTokens
		}
	}
	if actual <= 0 {
		// OpenAI-compatible proxies do not always return usage. Charging the
		// conservative reservation keeps such responses inside the same
		// global budget instead of silently treating them as zero-token calls.
		actual = reservation
	}
	lease.finish(charged, actual, callErr)
	if lost := context.Cause(lease.ctx); lost != nil && ctx.Err() == nil {
		return response, &DeferredError{
			Reason:     "derivative limiter lease was lost",
			RetryAfter: 15 * time.Second,
			Cause:      lost,
		}
	}
	return response, callErr
}

func (w *limitedChat) ChatStream(
	ctx context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (<-chan types.StreamResponse, error) {
	reservation := w.limiter.estimateTokens(messages, options)
	charged := min(reservation, int(w.limiter.TPM(ctx)))
	lease, err := w.limiter.acquire(ctx)
	if err != nil {
		return nil, err
	}
	if err := lease.pace(ctx, reservation); err != nil {
		lease.release()
		return nil, err
	}
	lease.callStarted = time.Now()
	stream, err := w.inner.ChatStream(
		modeladmission.WithBackground(lease.ctx),
		messages,
		options,
	)
	if err != nil {
		lease.finish(charged, 0, err)
		lease.release()
		return nil, err
	}
	out := make(chan types.StreamResponse, 16)
	go func() {
		defer close(out)
		defer lease.release()
		actual := 0
		var streamErr error
		defer func() {
			if actual <= 0 {
				actual = reservation
			}
			lease.finish(charged, actual, streamErr)
		}()
		for {
			select {
			case <-lease.ctx.Done():
				streamErr = context.Cause(lease.ctx)
				select {
				case out <- types.StreamResponse{
					ResponseType: types.ResponseTypeError,
					Content:      streamErr.Error(),
					Done:         true,
				}:
				case <-ctx.Done():
				}
				return
			case item, ok := <-stream:
				if !ok {
					return
				}
				if item.Usage != nil {
					actual = item.Usage.TotalTokens
					if actual <= 0 {
						actual = item.Usage.PromptTokens + item.Usage.CompletionTokens
					}
				}
				if item.ResponseType == types.ResponseTypeError {
					streamErr = errors.New(item.Content)
				}
				select {
				case out <- item:
				case <-ctx.Done():
					streamErr = ctx.Err()
					return
				}
			}
		}
	}()
	return out, nil
}

func (l *Limiter) estimateTokens(messages []chat.Message, options *chat.ChatOptions) int {
	prompt := 0
	if l.estimate != nil {
		prompt = l.estimate.EstimateMessages(messages)
		for _, message := range messages {
			for _, part := range message.MultiContent {
				prompt += l.estimate.EstimateString(part.Text)
				if part.ImageURL != nil && part.ImageURL.URL != "" {
					// A conservative fixed reservation for provider-side
					// image tokenization, which varies by resolution/model.
					prompt += 1_024
				}
			}
		}
	} else {
		for _, message := range messages {
			prompt += (len([]rune(message.Content)) + 1) / 2
		}
	}
	output := defaultOutputReservation
	if options != nil {
		switch {
		case options.MaxCompletionTokens > 0:
			output = options.MaxCompletionTokens
		case options.MaxTokens > 0:
			output = options.MaxTokens
		}
	}
	if prompt < 1 {
		prompt = 1
	}
	if output < 1 {
		output = 1
	}
	return prompt + output
}

type limiterLease struct {
	limiter *Limiter
	token   string
	ctx     context.Context
	cancel  context.CancelCauseFunc
	local   bool
	stop    chan struct{}
	once    sync.Once

	callStarted time.Time
}

func (l *Limiter) acquire(ctx context.Context) (*limiterLease, error) {
	token := randomToken()
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &limiterLease{
		limiter: l, token: token, ctx: leaseCtx, cancel: cancel,
		stop: make(chan struct{}),
	}
	if l.rdb == nil {
		l.localMu.Lock()
		if l.localActive {
			l.localMu.Unlock()
			l.deferred.Add(1)
			cancel(nil)
			return nil, &DeferredError{
				Reason:     "another derivative model call is active",
				RetryAfter: busyRetryFloor,
			}
		}
		l.localActive = true
		l.localMu.Unlock()
		lease.local = true
		l.acquired.Add(1)
		return lease, nil
	}

	result, err := acquireLeaseScript.Run(
		ctx, l.rdb, []string{l.key("active")},
		token, leaseTTL.Milliseconds(),
	).Slice()
	if err != nil {
		cancel(nil)
		l.deferred.Add(1)
		return nil, &DeferredError{
			Reason:     "derivative limiter Redis admission failed",
			RetryAfter: 15 * time.Second,
			Cause:      err,
		}
	}
	acquired, _ := resultInt64(result, 0)
	ttlMs, _ := resultInt64(result, 1)
	if acquired != 1 {
		cancel(nil)
		l.deferred.Add(1)
		retry := time.Duration(ttlMs) * time.Millisecond
		if retry < busyRetryFloor {
			retry = busyRetryFloor
		}
		return nil, &DeferredError{
			Reason:     "another derivative model call is active",
			RetryAfter: retry,
		}
	}
	l.acquired.Add(1)
	go lease.heartbeat()
	return lease, nil
}

func (l *limiterLease) pace(ctx context.Context, estimatedTokens int) error {
	tpm := l.limiter.TPM(ctx)
	charged := int64(estimatedTokens)
	if charged < 1 {
		charged = 1
	}
	// One request may legitimately exceed a minute's token budget (large Wiki
	// rewrites). Serialize it and charge one full minute up front; actual
	// overage is added as debt after a fast response. A long-running response
	// naturally amortizes its tokens while holding the global lease.
	if charged > tpm {
		charged = tpm
	}
	interval := time.Duration((charged*int64(time.Minute) + tpm - 1) / tpm)
	if interval < time.Millisecond {
		interval = time.Millisecond
	}

	var wait time.Duration
	if l.local {
		l.limiter.localMu.Lock()
		now := time.Now()
		if l.limiter.localNext.After(now) {
			wait = time.Until(l.limiter.localNext)
		} else {
			l.limiter.localNext = now.Add(interval)
		}
		l.limiter.localMu.Unlock()
	} else {
		result, err := paceScript.Run(
			ctx, l.limiter.rdb, []string{l.limiter.key("pace")},
			interval.Milliseconds(),
		).Slice()
		if err != nil {
			l.limiter.deferred.Add(1)
			return &DeferredError{
				Reason:     "derivative TPM pacing failed closed",
				RetryAfter: 15 * time.Second,
				Cause:      err,
			}
		}
		admitted, _ := resultInt64(result, 0)
		waitMs, _ := resultInt64(result, 1)
		if admitted != 1 {
			wait = time.Duration(waitMs) * time.Millisecond
		}
	}
	if wait <= 0 {
		return nil
	}
	l.limiter.deferred.Add(1)
	if err := context.Cause(l.ctx); err != nil {
		return err
	}
	// Do not occupy an Asynq worker and the global active lease while waiting
	// for a long token interval or provider cooldown. The caller's durable
	// task is rescheduled without consuming its retry budget. paceScript does
	// not reserve a new interval on this branch, so repeated early wakeups
	// cannot create phantom TPM debt.
	return &DeferredError{
		Reason:     "global derivative TPM budget is pacing this request",
		RetryAfter: wait,
	}
}

func (l *limiterLease) finish(reserved, actual int, callErr error) {
	if l == nil || l.limiter == nil {
		return
	}
	tpm := l.limiter.TPM(context.Background())
	if actual > reserved {
		required := time.Duration(
			(int64(actual)*int64(time.Minute) + tpm - 1) / tpm,
		)
		started := l.callStarted
		if started.IsZero() {
			started = time.Now()
		}
		remaining := required - time.Since(started)
		if remaining > 0 {
			// pace() already reserved the initial estimate. Extending to the
			// remaining total-duration target (rather than blindly adding the
			// overage) credits time the provider spent generating the result
			// and avoids throttling far below the configured TPM.
			l.limiter.extendCooldown(remaining)
		}
	}
	if callErr == nil {
		return
	}
	cooldown := failureSafetyCooldown
	if isTimeoutLike(callErr) {
		cooldown = timeoutSafetyCooldown
	}
	l.limiter.extendCooldown(cooldown)
}

func (l *Limiter) addDebt(debt time.Duration) {
	if debt <= 0 {
		return
	}
	if l.rdb == nil {
		l.localMu.Lock()
		now := time.Now()
		if l.localNext.Before(now) {
			l.localNext = now
		}
		l.localNext = l.localNext.Add(debt)
		l.localMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = addDebtScript.Run(
		ctx, l.rdb, []string{l.key("pace")}, debt.Milliseconds(),
	).Result()
}

func (l *Limiter) extendCooldown(cooldown time.Duration) {
	if cooldown <= 0 {
		return
	}
	if l.rdb == nil {
		l.localMu.Lock()
		target := time.Now().Add(cooldown)
		if l.localNext.Before(target) {
			l.localNext = target
		}
		l.localMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = extendPaceScript.Run(
		ctx, l.rdb, []string{l.key("pace")}, cooldown.Milliseconds(),
	).Result()
}

func (l *limiterLease) heartbeat() {
	ticker := time.NewTicker(leaseHeartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-l.ctx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			ok, err := renewLeaseScript.Run(
				ctx, l.limiter.rdb, []string{l.limiter.key("active")},
				l.token, leaseTTL.Milliseconds(),
			).Int()
			cancel()
			if err != nil || ok != 1 {
				if err == nil {
					err = errors.New("derivative limiter lease ownership changed")
				}
				l.cancel(err)
				return
			}
		}
	}
}

func (l *limiterLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		close(l.stop)
		l.cancel(nil)
		if l.local {
			l.limiter.localMu.Lock()
			l.limiter.localActive = false
			l.limiter.localMu.Unlock()
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = releaseLeaseScript.Run(
			ctx, l.limiter.rdb, []string{l.limiter.key("active")}, l.token,
		).Result()
	})
}

func (l *Limiter) key(suffix string) string {
	base := "weknora:derivative-control:" + suffix
	if namespace := strings.TrimSpace(os.Getenv("WEKNORA_REDIS_NAMESPACE")); namespace != "" {
		return base + ":" + namespace
	}
	return base
}

func randomToken() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err == nil {
		return hex.EncodeToString(raw[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func resultInt64(values []interface{}, index int) (int64, bool) {
	if index < 0 || index >= len(values) {
		return 0, false
	}
	switch value := values[index].(type) {
	case int64:
		return value, true
	case string:
		var parsed int64
		_, err := fmt.Sscan(value, &parsed)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func isTimeoutLike(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timeout") ||
		strings.Contains(text, "timed out") ||
		strings.Contains(text, "deadline exceeded")
}
