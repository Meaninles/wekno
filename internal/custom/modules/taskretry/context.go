// Package taskretry carries retry metadata for non-Asynq task executors.
//
// Asynq keeps retry_count/max_retry behind private context keys. Lite mode
// intentionally does not depend on those internals, so the synchronous
// executor publishes equivalent metadata here and task handlers consult this
// package only when Asynq metadata is unavailable.
package taskretry

import "context"

type metadata struct {
	retryCount int
	maxRetry   int
}

type contextKey struct{}

// WithMetadata returns a child context carrying the current zero-based retry
// count and the configured maximum number of retries.
func WithMetadata(ctx context.Context, retryCount, maxRetry int) context.Context {
	if retryCount < 0 {
		retryCount = 0
	}
	if maxRetry < 0 {
		maxRetry = 0
	}
	return context.WithValue(ctx, contextKey{}, metadata{
		retryCount: retryCount,
		maxRetry:   maxRetry,
	})
}

// Metadata extracts Lite retry metadata. ok is false for ordinary request or
// real Asynq contexts, allowing callers to preserve Asynq as the authority.
func Metadata(ctx context.Context) (retryCount, maxRetry int, ok bool) {
	if ctx == nil {
		return 0, 0, false
	}
	m, ok := ctx.Value(contextKey{}).(metadata)
	if !ok {
		return 0, 0, false
	}
	return m.retryCount, m.maxRetry, true
}
