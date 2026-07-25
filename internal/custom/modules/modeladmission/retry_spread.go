package modeladmission

import (
	"hash/fnv"
	"time"
)

const (
	minProviderRetrySpread = 5 * time.Second
	maxProviderRetrySpread = 30 * time.Second
)

// ProviderRetrySpreadWindow returns the positive jitter window appended after
// a shared provider/circuit Retry-After boundary. The window grows with longer
// outages but remains bounded so healthy recovery is not delayed excessively.
//
// Adding (rather than subtracting) the spread is important: no worker may run
// before the distributed circuit says a half-open probe is allowed.
func ProviderRetrySpreadWindow(retryAfter time.Duration) time.Duration {
	if retryAfter <= 0 {
		return 0
	}
	window := retryAfter / 4
	if window < minProviderRetrySpread {
		return minProviderRetrySpread
	}
	if window > maxProviderRetrySpread {
		return maxProviderRetrySpread
	}
	return window
}

// SpreadProviderRetry deterministically distributes durable tasks after the
// provider Retry-After boundary. Every replica computes the same delay for the
// same logical task, while different document/task identities occupy the full
// recovery window instead of waking Redis and PostgreSQL in one burst.
func SpreadProviderRetry(retryAfter time.Duration, identity string) time.Duration {
	window := ProviderRetrySpreadWindow(retryAfter)
	if window <= 0 || identity == "" {
		return retryAfter
	}
	slots := uint64(window / time.Millisecond)
	if slots == 0 {
		return retryAfter
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(identity))
	jitter := time.Duration(hash.Sum64()%slots) * time.Millisecond
	return retryAfter + jitter
}
