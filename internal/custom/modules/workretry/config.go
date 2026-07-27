// Package workretry centralizes bounded retry policy for long-running model
// work whose durable business item must eventually leave the active queue.
package workretry

import (
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultImageMaxAttempts   = 10
	DefaultWikiMaxAttempts    = 10
	DefaultWikiInlineAttempts = 1
	DefaultWikiCallTimeout    = 15 * time.Minute

	maxConfiguredAttempts = 100
)

// Config is deliberately small and read from the environment at the decision
// boundary. Tests can therefore use t.Setenv without process-global caches,
// while production changes still take effect only after the normal Pod
// restart that accompanies a Helm rollout.
type Config struct {
	ImageMaxAttempts   int
	WikiMaxAttempts    int
	WikiInlineAttempts int
	WikiCallTimeout    time.Duration
}

func ConfigFromEnv() Config {
	return Config{
		ImageMaxAttempts: boundedEnvInt(
			"CUSTOM_WORK_RETRY_IMAGE_MAX_ATTEMPTS",
			DefaultImageMaxAttempts,
			1,
			maxConfiguredAttempts,
		),
		WikiMaxAttempts: boundedEnvInt(
			"CUSTOM_WORK_RETRY_WIKI_MAX_ATTEMPTS",
			DefaultWikiMaxAttempts,
			1,
			maxConfiguredAttempts,
		),
		// Inline retries keep the KB/materialization worker occupied for the
		// full model timeout. One attempt hands control back to the durable
		// per-op queue, where fairness and the outer attempt budget apply.
		WikiInlineAttempts: boundedEnvInt(
			"CUSTOM_WORK_RETRY_WIKI_INLINE_ATTEMPTS",
			DefaultWikiInlineAttempts,
			1,
			3,
		),
		WikiCallTimeout: time.Duration(boundedEnvInt(
			"CUSTOM_WORK_RETRY_WIKI_CALL_TIMEOUT_SECONDS",
			int(DefaultWikiCallTimeout/time.Second),
			1,
			int((2*time.Hour)/time.Second),
		)) * time.Second,
	}
}

// ImageMaxRetries converts the human-facing total-attempt budget into
// Asynq's "number of retries after the initial delivery" convention.
func (c Config) ImageMaxRetries() int {
	if c.ImageMaxAttempts <= 1 {
		return 0
	}
	return c.ImageMaxAttempts - 1
}

func boundedEnvInt(key string, fallback, minimum, maximum int) int {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return fallback
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < minimum || value > maximum {
		return fallback
	}
	return value
}
