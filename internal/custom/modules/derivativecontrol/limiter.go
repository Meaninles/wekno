package derivativecontrol

import (
	"context"
	"crypto/sha256"
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
	"github.com/Tencent/WeKnora/internal/custom/modules/derivativequeue"
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
	rdb       *redis.Client
	admission *modeladmission.Manager
	estimate  *agenttoken.Estimator

	localMu   sync.Mutex
	localNext map[string]time.Time

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
	return NewLimiterWithAdmission(rdb, settings, nil)
}

func NewLimiterWithAdmission(
	rdb *redis.Client,
	_ interfaces.SystemSettingService,
	admission *modeladmission.Manager,
) *Limiter {
	estimator, _ := agenttoken.NewEstimator()
	return &Limiter{
		rdb: rdb, admission: admission, estimate: estimator,
		localNext: make(map[string]time.Time),
	}
}

func (l *Limiter) TPM(ctx context.Context) int64 {
	// Persisted models always resolve TPM from their actual-model resource
	// pool. This fallback exists only for tests/bootstrapping callers without
	// a model record and is deliberately not another operator-owned setting.
	_ = ctx
	return DefaultTPM
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
		_, _ = l.rdb.Ping(probeCtx).Result()
		return snapshot
	}
	return snapshot
}

func (l *Limiter) Wrap(inner chat.Chat) chat.Chat {
	if l == nil || inner == nil {
		return inner
	}
	return l.wrapForPool(inner, poolKeyForName(inner.GetModelName()), nil)
}

func (l *Limiter) WrapForModel(inner chat.Chat, model *types.Model) chat.Chat {
	if l == nil || inner == nil {
		return inner
	}
	return l.wrapForPool(inner, poolKeyForModel(model), model)
}

func (l *Limiter) tokenBudgets(
	ctx context.Context,
	fallbackPoolKey string,
	model *types.Model,
) ([]tokenBudget, error) {
	if l != nil && l.admission != nil && model != nil {
		policy, err := l.admission.ResolvePolicy(
			ctx,
			modeladmission.SpecForModel(modeladmission.KindDerivative, model, ""),
		)
		if err != nil {
			return nil, &DeferredError{
				Reason:     "derivative token policy is temporarily unavailable",
				RetryAfter: 15 * time.Second,
				Cause:      err,
			}
		}
		budgets := make([]tokenBudget, 0, 2)
		if policy.QuotaTPM > 0 && strings.TrimSpace(policy.QuotaPoolID) != "" {
			budgets = append(budgets, tokenBudget{
				key: "quota:" + policy.QuotaPoolID,
				tpm: policy.QuotaTPM,
			})
		}
		if policy.TPM > 0 {
			poolID := strings.TrimSpace(policy.PoolID)
			if poolID == "" {
				poolID = fallbackPoolKey
			}
			budgets = append(budgets, tokenBudget{
				key: "resource:" + poolID,
				tpm: policy.TPM,
			})
		}
		return budgets, nil
	}
	// Compatibility for callers that do not have a persisted model record.
	// Production derivative resolution always takes the branch above.
	return []tokenBudget{{
		key: "resource:" + fallbackPoolKey,
		tpm: l.TPM(ctx),
	}}, nil
}

func (l *Limiter) wrapForPool(
	inner chat.Chat,
	poolKey string,
	model *types.Model,
) chat.Chat {
	if l == nil || inner == nil {
		return inner
	}
	return &limitedChat{limiter: l, inner: inner, poolKey: poolKey, model: model}
}

type limitedChat struct {
	limiter *Limiter
	inner   chat.Chat
	poolKey string
	model   *types.Model
}

func (w *limitedChat) GetModelName() string { return w.inner.GetModelName() }
func (w *limitedChat) GetModelID() string   { return w.inner.GetModelID() }
func (w *limitedChat) ModelAdmissionParallelism(ctx context.Context, requested int) int {
	return modeladmission.EffectiveChatParallelism(ctx, w.inner, requested)
}

func (w *limitedChat) Chat(
	ctx context.Context,
	messages []chat.Message,
	options *chat.ChatOptions,
) (*types.ChatResponse, error) {
	if checkpoint, ok, err := derivativequeue.LookupChatCheckpoint(
		ctx, w.inner.GetModelID(), messages, options,
	); err != nil {
		return nil, err
	} else if ok {
		return checkpoint, nil
	}
	reservation := w.limiter.estimateTokens(messages, options)
	budgets, err := w.limiter.tokenBudgets(ctx, w.poolKey, w.model)
	if err != nil {
		return nil, err
	}
	lease, err := w.limiter.acquireBudgets(ctx, budgets)
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
		// actual-model and quota budgets instead of treating them as zero.
		actual = reservation
	}
	lease.finish(reservation, actual, callErr)
	if callErr == nil && response != nil {
		checkpointCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		checkpointErr := derivativequeue.CheckpointChatResponse(
			checkpointCtx, w.inner.GetModelID(), messages, options, response,
		)
		cancel()
		if checkpointErr != nil {
			return nil, checkpointErr
		}
	}
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
	budgets, err := w.limiter.tokenBudgets(ctx, w.poolKey, w.model)
	if err != nil {
		return nil, err
	}
	lease, err := w.limiter.acquireBudgets(ctx, budgets)
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
		lease.finish(reservation, 0, err)
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
			lease.finish(reservation, actual, streamErr)
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
	ctx     context.Context
	cancel  context.CancelCauseFunc
	local   bool
	once    sync.Once

	callStarted time.Time
	budgets     []tokenBudget
}

type tokenBudget struct {
	key string
	tpm int64
}

func (l *Limiter) acquire(ctx context.Context, poolKey string) (*limiterLease, error) {
	return l.acquireBudgets(ctx, []tokenBudget{{
		key: poolKey,
		tpm: l.TPM(ctx),
	}})
}

func (l *Limiter) acquireBudgets(
	ctx context.Context,
	budgets []tokenBudget,
) (*limiterLease, error) {
	leaseCtx, cancel := context.WithCancelCause(ctx)
	lease := &limiterLease{
		limiter: l, ctx: leaseCtx, cancel: cancel, budgets: budgets,
	}
	lease.local = l.rdb == nil
	l.acquired.Add(1)
	return lease, nil
}

func (l *limiterLease) pace(ctx context.Context, estimatedTokens int) error {
	for _, budget := range l.budgets {
		if err := l.paceBudget(ctx, budget, estimatedTokens); err != nil {
			return err
		}
	}
	return nil
}

func (l *limiterLease) paceBudget(
	ctx context.Context,
	budget tokenBudget,
	estimatedTokens int,
) error {
	tpm := clampTPM(budget.tpm)
	charged := int64(estimatedTokens)
	if charged < 1 {
		charged = 1
	}
	// One request may legitimately exceed a minute's token budget (large Wiki
	// rewrites). Charge one full minute up front; actual overage is added as
	// debt after a fast response. Each actual-model pool paces independently.
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
		next := l.limiter.localNext[budget.key]
		if next.After(now) {
			wait = time.Until(next)
		} else {
			l.limiter.localNext[budget.key] = now.Add(interval)
		}
		l.limiter.localMu.Unlock()
	} else {
		result, err := paceScript.Run(
			ctx, l.limiter.rdb, []string{l.limiter.key("pool:" + budget.key + ":pace")},
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
	// Do not occupy an Asynq worker or a model-admission lease while waiting
	// for a long token interval or provider cooldown. The caller's durable
	// task is rescheduled without consuming its retry budget. paceScript does
	// not reserve a new interval on this branch, so repeated early wakeups
	// cannot create phantom TPM debt.
	return &DeferredError{
		Reason:     "derivative model TPM pool is pacing this request",
		RetryAfter: wait,
	}
}

func clampTPM(value int64) int64 {
	if value < minTPM {
		return minTPM
	}
	if value > maxTPM {
		return maxTPM
	}
	return value
}

func (l *limiterLease) finish(reserved, actual int, callErr error) {
	if l == nil || l.limiter == nil {
		return
	}
	for _, budget := range l.budgets {
		tpm := clampTPM(budget.tpm)
		charged := min(reserved, int(tpm))
		if actual > charged {
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
				// overage) credits time the provider spent generating the result.
				l.limiter.extendCooldown(budget.key, remaining)
			}
		}
	}
	if callErr == nil {
		return
	}
	cooldown := failureSafetyCooldown
	if isTimeoutLike(callErr) {
		cooldown = timeoutSafetyCooldown
	}
	for _, budget := range l.budgets {
		l.limiter.extendCooldown(budget.key, cooldown)
	}
}

func (l *Limiter) addDebt(poolKey string, debt time.Duration) {
	if debt <= 0 {
		return
	}
	if l.rdb == nil {
		l.localMu.Lock()
		now := time.Now()
		next := l.localNext[poolKey]
		if next.Before(now) {
			next = now
		}
		l.localNext[poolKey] = next.Add(debt)
		l.localMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = addDebtScript.Run(
		ctx, l.rdb, []string{l.key("pool:" + poolKey + ":pace")}, debt.Milliseconds(),
	).Result()
}

func (l *Limiter) extendCooldown(poolKey string, cooldown time.Duration) {
	if cooldown <= 0 {
		return
	}
	if l.rdb == nil {
		l.localMu.Lock()
		target := time.Now().Add(cooldown)
		if l.localNext[poolKey].Before(target) {
			l.localNext[poolKey] = target
		}
		l.localMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = extendPaceScript.Run(
		ctx, l.rdb, []string{l.key("pool:" + poolKey + ":pace")}, cooldown.Milliseconds(),
	).Result()
}

func (l *limiterLease) release() {
	if l == nil {
		return
	}
	l.once.Do(func() {
		l.cancel(nil)
	})
}

func (l *Limiter) key(suffix string) string {
	base := "weknora:derivative-control:" + suffix
	if namespace := strings.TrimSpace(os.Getenv("WEKNORA_REDIS_NAMESPACE")); namespace != "" {
		return base + ":" + namespace
	}
	return base
}

func poolKeyForModel(model *types.Model) string {
	if model == nil {
		return poolKeyForName("")
	}
	return digestPoolKey(modeladmission.RouteFingerprint(model))
}

func poolKeyForName(name string) string {
	return digestPoolKey(strings.ToLower(strings.TrimSpace(name)))
}

func digestPoolKey(material string) string {
	sum := sha256.Sum256([]byte(material))
	return hex.EncodeToString(sum[:16])
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
