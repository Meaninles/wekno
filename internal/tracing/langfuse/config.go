// Package langfuse maps WeKnora's tracing facade to Langfuse v4's
// OpenTelemetry endpoint. Observability is always fail-open for business work.
package langfuse

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/agenteval"
)

type Config struct {
	Enabled                 bool
	Host                    string
	PublicKey               string
	SecretKey               string
	FlushAt                 int
	FlushInterval           time.Duration
	QueueSize               int
	RequestTimeout          time.Duration
	Release                 string
	Environment             string
	SampleRate              float64
	ProductionMaxSampleRate float64
	MaxAttributeBytes       int
	Debug                   bool
	AgentEval               agenteval.Config
}

func LoadConfigFromEnv() Config {
	evalConfig := agenteval.LoadConfigFromEnv()
	defaultSampleRate := 0.01
	if evalConfig.Mode == agenteval.ModeEval {
		defaultSampleRate = 1.0
	}
	cfg := Config{
		Host:                    firstNonEmpty(os.Getenv("LANGFUSE_HOST"), "https://cloud.langfuse.com"),
		PublicKey:               strings.TrimSpace(os.Getenv("LANGFUSE_PUBLIC_KEY")),
		SecretKey:               strings.TrimSpace(os.Getenv("LANGFUSE_SECRET_KEY")),
		Release:                 strings.TrimSpace(os.Getenv("LANGFUSE_RELEASE")),
		Environment:             strings.TrimSpace(os.Getenv("LANGFUSE_ENVIRONMENT")),
		FlushAt:                 128,
		FlushInterval:           3 * time.Second,
		QueueSize:               2048,
		RequestTimeout:          5 * time.Second,
		SampleRate:              defaultSampleRate,
		ProductionMaxSampleRate: 0.01,
		MaxAttributeBytes:       256 * 1024,
		AgentEval:               evalConfig,
	}
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_ENABLED")); value != "" {
		cfg.Enabled = parseBool(value)
	} else {
		cfg.Enabled = cfg.PublicKey != "" && cfg.SecretKey != ""
	}
	parsePositiveIntEnv("LANGFUSE_FLUSH_AT", &cfg.FlushAt)
	parsePositiveIntEnv("LANGFUSE_QUEUE_SIZE", &cfg.QueueSize)
	parsePositiveIntEnv("LANGFUSE_MAX_ATTRIBUTE_BYTES", &cfg.MaxAttributeBytes)
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_FLUSH_INTERVAL")); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			cfg.FlushInterval = duration
		} else if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
			cfg.FlushInterval = time.Duration(seconds) * time.Second
		}
	}
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_REQUEST_TIMEOUT")); value != "" {
		if duration, err := time.ParseDuration(value); err == nil && duration > 0 {
			cfg.RequestTimeout = duration
		}
	}
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_SAMPLE_RATE")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && parsed <= 1 {
			cfg.SampleRate = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_PRODUCTION_MAX_SAMPLE_RATE")); value != "" {
		if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed >= 0 && parsed <= 1 {
			cfg.ProductionMaxSampleRate = parsed
		}
	}
	if cfg.AgentEval.Mode == agenteval.ModeProduction && cfg.SampleRate > cfg.ProductionMaxSampleRate {
		cfg.SampleRate = cfg.ProductionMaxSampleRate
	}
	if value := strings.TrimSpace(os.Getenv("LANGFUSE_DEBUG")); value != "" {
		cfg.Debug = parseBool(value)
	}
	return cfg
}

func (c Config) Validate() error {
	if !c.Enabled || c.SampleRate == 0 {
		return nil
	}
	if strings.TrimSpace(c.Host) == "" {
		return fmt.Errorf("langfuse: host is required when enabled")
	}
	if c.PublicKey == "" || c.SecretKey == "" {
		return fmt.Errorf("langfuse: public_key and secret_key are required when enabled")
	}
	if c.QueueSize <= 0 || c.FlushAt <= 0 || c.FlushAt > c.QueueSize {
		return fmt.Errorf("langfuse: invalid queue/batch sizes")
	}
	return nil
}

func (c Config) OTLPTraceEndpoint() string {
	return strings.TrimRight(c.Host, "/") + "/api/public/otel/v1/traces"
}

func (c Config) CaptureContent() bool {
	return c.AgentEval.CaptureContent()
}

func parsePositiveIntEnv(name string, target *int) {
	if value := strings.TrimSpace(os.Getenv(name)); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil && parsed > 0 {
			*target = parsed
		}
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseBool(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "t", "yes", "y", "on":
		return true
	default:
		return false
	}
}
