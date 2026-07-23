package wikiqueue

import (
	"context"
	cryptorand "crypto/rand"
	"math/big"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

const (
	defaultLLMRetryBase = 15 * time.Second
	defaultLLMRetryMin  = 1 * time.Second
	defaultLLMRetryMax  = 2 * time.Minute

	// Conservative defaults protect a shared synthesis model during bulk
	// uploads. Knowledge bases with provider-sized quotas retain explicit
	// overrides through ResolveIngestParallelism.
	DefaultMapParallel    = 2
	DefaultReduceParallel = 2
)

// RetryPolicy centralizes Wiki's response to transient LLM failures. Wait and
// Jitter are injectable so unit tests can validate minute-scale production
// delays instantly and deterministically.
type RetryPolicy struct {
	BaseDelay time.Duration
	MinDelay  time.Duration
	MaxDelay  time.Duration
	Jitter    func(time.Duration) time.Duration
	Wait      func(context.Context, time.Duration) error
}

// DefaultRetryPolicy returns provider-safe production settings. A missing
// Retry-After uses 15s, 30s, ... exponential waits plus up to 50% positive
// jitter. Provider-directed delays get a smaller positive spread.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		BaseDelay: defaultLLMRetryBase,
		MinDelay:  defaultLLMRetryMin,
		MaxDelay:  defaultLLMRetryMax,
		Jitter:    secureRetryJitter,
		Wait:      waitWithContext,
	}
}

func (p RetryPolicy) normalized() RetryPolicy {
	defaults := DefaultRetryPolicy()
	if p.BaseDelay <= 0 {
		p.BaseDelay = defaults.BaseDelay
	}
	if p.MinDelay <= 0 {
		p.MinDelay = defaults.MinDelay
	}
	if p.MaxDelay <= 0 {
		p.MaxDelay = defaults.MaxDelay
	}
	if p.MinDelay > p.MaxDelay {
		p.MinDelay = p.MaxDelay
	}
	if p.BaseDelay > p.MaxDelay {
		p.BaseDelay = p.MaxDelay
	}
	if p.Jitter == nil {
		p.Jitter = defaults.Jitter
	}
	if p.Wait == nil {
		p.Wait = defaults.Wait
	}
	return p
}

// Delay returns the delay before retryNumber (1 is the retry after the initial
// call) and whether it came from a valid Retry-After header. Fallback delays
// are bounded by MaxDelay; provider-directed delays are never shortened.
// Jitter is additive, never subtractive.
func (p RetryPolicy) Delay(err error, retryNumber int) (time.Duration, bool) {
	p = p.normalized()
	if retryAfter, ok := chat.RetryAfterDuration(err); ok {
		// A provider directive is a lower bound, not a hint we may shorten.
		// Only apply the anti-busy-loop floor; the fallback MaxDelay does not
		// apply here. If the delay exceeds the task deadline, WaitForRetry exits
		// through ctx and the durable PG operation remains available to recovery
		// without sending another request earlier than instructed.
		base := retryAfter
		if base < p.MinDelay {
			base = p.MinDelay
		}
		window := base / 10
		if window > 10*time.Second {
			window = 10 * time.Second
		}
		return addProviderRetryJitter(base, window, p.Jitter), true
	}

	if retryNumber < 1 {
		retryNumber = 1
	}
	base := p.BaseDelay
	for n := 1; n < retryNumber && base < p.MaxDelay; n++ {
		if base > p.MaxDelay/2 {
			base = p.MaxDelay
			break
		}
		base *= 2
	}
	base = p.clamp(base)
	return p.addJitter(base, base/2), false
}

func addProviderRetryJitter(base, window time.Duration, jitter func(time.Duration) time.Duration) time.Duration {
	if window <= 0 {
		return base
	}
	extra := jitter(window)
	if extra < 0 {
		extra = 0
	}
	if extra > window {
		extra = window
	}
	const maxDuration = time.Duration(1<<63 - 1)
	if base > maxDuration-extra {
		return maxDuration
	}
	return base + extra
}

// WaitForRetry waits without leaking a timer and returns ctx.Err verbatim on
// cancellation so errors.Is remains useful to the task runner.
func (p RetryPolicy) WaitForRetry(ctx context.Context, delay time.Duration) error {
	p = p.normalized()
	return p.Wait(ctx, delay)
}

func (p RetryPolicy) clamp(delay time.Duration) time.Duration {
	if delay < p.MinDelay {
		return p.MinDelay
	}
	if delay > p.MaxDelay {
		return p.MaxDelay
	}
	return delay
}

func (p RetryPolicy) addJitter(base, window time.Duration) time.Duration {
	if window <= 0 {
		return p.clamp(base)
	}
	extra := p.Jitter(window)
	if extra < 0 {
		extra = 0
	}
	if extra > window {
		extra = window
	}
	if base >= p.MaxDelay-extra {
		return p.MaxDelay
	}
	return p.clamp(base + extra)
}

// secureRetryJitter is concurrency-safe and shares no PRNG state between Wiki
// workers. Entropy failure safely degrades to the unjittered delay.
func secureRetryJitter(window time.Duration) time.Duration {
	if window <= 0 {
		return 0
	}
	value, err := cryptorand.Int(cryptorand.Reader, big.NewInt(int64(window)+1))
	if err != nil {
		return 0
	}
	return time.Duration(value.Int64())
}

func waitWithContext(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// ResolveIngestParallelism applies conservative defaults only to unset values.
// Explicit per-KB settings remain authoritative and are deliberately not
// capped, because operators may know the quota of a dedicated provider.
func ResolveIngestParallelism(config *types.WikiConfig) (mapParallel, reduceParallel int) {
	return config.IngestMapParallelOrDefault(DefaultMapParallel),
		config.IngestReduceParallelOrDefault(DefaultReduceParallel)
}
