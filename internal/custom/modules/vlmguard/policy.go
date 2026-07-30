package vlmguard

import (
	"context"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// Production history includes legitimate multimodal preprocessing and
	// scheduler waits above 100 seconds. Keep the no-progress threshold at
	// three minutes so those calls are not confused with a hard stall.
	DefaultFirstTokenTimeout = 180 * time.Second
	DefaultIdleTimeout       = 45 * time.Second
	DefaultTotalTimeout      = 360 * time.Second
	DefaultMaxTokens         = 5000
	DefaultCaptionMaxTokens  = 512

	// go-openai omits a literal zero temperature from JSON. A tiny positive
	// value keeps OCR effectively deterministic while ensuring the provider
	// receives an explicit value instead of falling back to its own default.
	deterministicTemperature = float32(0.000001)
)

type Operation string

const (
	OperationGeneral Operation = "general"
	OperationOCR     Operation = "ocr"
	OperationCaption Operation = "caption"
)

type operationContextKey struct{}

func WithOperation(ctx context.Context, operation Operation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, operationContextKey{}, normalizeOperation(operation))
}

func OperationFromContext(ctx context.Context) Operation {
	if ctx == nil {
		return OperationGeneral
	}
	operation, _ := ctx.Value(operationContextKey{}).(Operation)
	return normalizeOperation(operation)
}

type Config struct {
	Streaming         bool
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
	TotalTimeout      time.Duration

	GeneralMaxTokens int
	OCRMaxTokens     int
	CaptionMaxTokens int

	GeneralTemperature float32
	OCRTemperature     float32
	CaptionTemperature float32
}

type Policy struct {
	Operation         Operation
	Streaming         bool
	FirstTokenTimeout time.Duration
	IdleTimeout       time.Duration
	TotalTimeout      time.Duration
	MaxTokens         int
	Temperature       float32
	DetectRunaway     bool
}

// ConfigFrom combines safe defaults, process-wide environment overrides and
// per-model extra_config overrides. Per-model values take precedence.
func ConfigFrom(extra map[string]any, generalTemperature float32) Config {
	if generalTemperature < 0 {
		generalTemperature = 0
	}
	if generalTemperature == 0 {
		generalTemperature = deterministicTemperature
	}

	totalFallback := durationFromEnv("VLM_HTTP_TIMEOUT_SECONDS", DefaultTotalTimeout)
	config := Config{
		Streaming:          boolFromEnv("VLM_STREAMING_ENABLED", true),
		FirstTokenTimeout:  durationFromEnv("VLM_FIRST_TOKEN_TIMEOUT_SECONDS", DefaultFirstTokenTimeout),
		IdleTimeout:        durationFromEnv("VLM_STREAM_IDLE_TIMEOUT_SECONDS", DefaultIdleTimeout),
		TotalTimeout:       durationFromEnv("VLM_TOTAL_TIMEOUT_SECONDS", totalFallback),
		GeneralMaxTokens:   intFromEnv("VLM_MAX_TOKENS", DefaultMaxTokens),
		OCRMaxTokens:       intFromEnv("VLM_OCR_MAX_TOKENS", DefaultMaxTokens),
		CaptionMaxTokens:   intFromEnv("VLM_CAPTION_MAX_TOKENS", DefaultCaptionMaxTokens),
		GeneralTemperature: generalTemperature,
		OCRTemperature:     deterministicTemperature,
		CaptionTemperature: generalTemperature,
	}

	config.Streaming = boolFromExtra(extra, "vlm_streaming_enabled", config.Streaming)
	config.FirstTokenTimeout = durationFromExtra(
		extra, "vlm_first_token_timeout_seconds", config.FirstTokenTimeout,
	)
	config.IdleTimeout = durationFromExtra(
		extra, "vlm_stream_idle_timeout_seconds", config.IdleTimeout,
	)
	config.TotalTimeout = durationFromExtra(
		extra, "vlm_total_timeout_seconds", config.TotalTimeout,
	)
	config.GeneralMaxTokens = intFromExtra(extra, "vlm_max_tokens", config.GeneralMaxTokens)
	config.OCRMaxTokens = intFromExtra(extra, "vlm_ocr_max_tokens", config.OCRMaxTokens)
	config.CaptionMaxTokens = intFromExtra(
		extra, "vlm_caption_max_tokens", config.CaptionMaxTokens,
	)
	config.OCRTemperature = float32FromExtra(
		extra, "vlm_ocr_temperature", config.OCRTemperature,
	)
	config.CaptionTemperature = float32FromExtra(
		extra, "vlm_caption_temperature", config.CaptionTemperature,
	)

	return config.normalized()
}

func (config Config) Policy(operation Operation) Policy {
	config = config.normalized()
	operation = normalizeOperation(operation)
	policy := Policy{
		Operation:         operation,
		Streaming:         config.Streaming,
		FirstTokenTimeout: config.FirstTokenTimeout,
		IdleTimeout:       config.IdleTimeout,
		TotalTimeout:      config.TotalTimeout,
		MaxTokens:         config.GeneralMaxTokens,
		Temperature:       config.GeneralTemperature,
	}
	switch operation {
	case OperationOCR:
		policy.MaxTokens = config.OCRMaxTokens
		policy.Temperature = config.OCRTemperature
		policy.DetectRunaway = true
	case OperationCaption:
		policy.MaxTokens = config.CaptionMaxTokens
		policy.Temperature = config.CaptionTemperature
		policy.DetectRunaway = true
	}
	return policy
}

func (config Config) normalized() Config {
	if config.FirstTokenTimeout <= 0 {
		config.FirstTokenTimeout = DefaultFirstTokenTimeout
	}
	if config.IdleTimeout <= 0 {
		config.IdleTimeout = DefaultIdleTimeout
	}
	if config.TotalTimeout <= 0 {
		config.TotalTimeout = DefaultTotalTimeout
	}
	if config.FirstTokenTimeout > config.TotalTimeout {
		config.FirstTokenTimeout = config.TotalTimeout
	}
	if config.IdleTimeout > config.TotalTimeout {
		config.IdleTimeout = config.TotalTimeout
	}
	if config.GeneralMaxTokens <= 0 {
		config.GeneralMaxTokens = DefaultMaxTokens
	}
	if config.OCRMaxTokens <= 0 {
		config.OCRMaxTokens = DefaultMaxTokens
	}
	if config.CaptionMaxTokens <= 0 {
		config.CaptionMaxTokens = DefaultCaptionMaxTokens
	}
	if config.GeneralTemperature <= 0 {
		config.GeneralTemperature = deterministicTemperature
	}
	if config.OCRTemperature <= 0 {
		config.OCRTemperature = deterministicTemperature
	}
	if config.CaptionTemperature <= 0 {
		config.CaptionTemperature = deterministicTemperature
	}
	return config
}

func normalizeOperation(operation Operation) Operation {
	switch operation {
	case OperationOCR, OperationCaption:
		return operation
	default:
		return OperationGeneral
	}
}

func durationFromEnv(name string, fallback time.Duration) time.Duration {
	return durationFromString(os.Getenv(name), fallback)
}

func intFromEnv(name string, fallback int) int {
	return intFromString(os.Getenv(name), fallback)
}

func boolFromEnv(name string, fallback bool) bool {
	return boolFromString(os.Getenv(name), fallback)
}

func durationFromExtra(extra map[string]any, key string, fallback time.Duration) time.Duration {
	value, ok := extraValue(extra, key)
	if !ok {
		return fallback
	}
	return durationFromString(value, fallback)
}

func intFromExtra(extra map[string]any, key string, fallback int) int {
	value, ok := extraValue(extra, key)
	if !ok {
		return fallback
	}
	return intFromString(value, fallback)
}

func boolFromExtra(extra map[string]any, key string, fallback bool) bool {
	value, ok := extraValue(extra, key)
	if !ok {
		return fallback
	}
	return boolFromString(value, fallback)
}

func float32FromExtra(extra map[string]any, key string, fallback float32) float32 {
	value, ok := extraValue(extra, key)
	if !ok {
		return fallback
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 32)
	if err != nil || parsed < 0 {
		return fallback
	}
	if parsed == 0 {
		return deterministicTemperature
	}
	return float32(parsed)
}

func extraValue(extra map[string]any, key string) (string, bool) {
	if len(extra) == 0 {
		return "", false
	}
	value, ok := extra[key]
	if !ok || value == nil {
		return "", false
	}
	switch typed := value.(type) {
	case string:
		return typed, true
	case []byte:
		return string(typed), true
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64), true
	case bool:
		return strconv.FormatBool(typed), true
	default:
		return "", false
	}
}

func durationFromString(value string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func intFromString(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func boolFromString(value string, fallback bool) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return fallback
	}
	return parsed
}
