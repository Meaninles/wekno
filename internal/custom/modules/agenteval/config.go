package agenteval

import (
	"os"
	"strings"
)

type Mode string

const (
	ModeProduction Mode = "production"
	ModeEval       Mode = "eval"
)

type CapturePolicy string

const (
	CaptureMetadata CapturePolicy = "metadata"
	CaptureFull     CapturePolicy = "full"
)

// Config is deliberately small. The SUT records in both modes, but only an
// external Codex-operated runner is allowed to execute datasets, judge output
// or apply gates. Invalid values normalize to the cheapest production policy
// so observability configuration can never prevent the business app starting.
type Config struct {
	Mode          Mode
	CapturePolicy CapturePolicy
	Warning       string
}

func LoadConfigFromEnv() Config {
	mode := Mode(strings.ToLower(strings.TrimSpace(os.Getenv("CUSTOM_AGENT_EVAL_MODE"))))
	if mode == "" {
		mode = ModeProduction
	}
	policy := CapturePolicy(strings.ToLower(strings.TrimSpace(os.Getenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY"))))
	if policy == "" {
		if mode == ModeEval {
			policy = CaptureFull
		} else {
			policy = CaptureMetadata
		}
	}
	cfg := Config{Mode: mode, CapturePolicy: policy}
	if mode != ModeProduction && mode != ModeEval {
		cfg.Mode = ModeProduction
		cfg.CapturePolicy = CaptureMetadata
		cfg.Warning = "invalid CUSTOM_AGENT_EVAL_MODE; normalized to production/metadata"
		return cfg
	}
	if policy != CaptureMetadata && policy != CaptureFull {
		cfg.CapturePolicy = CaptureMetadata
		cfg.Warning = "invalid CUSTOM_AGENT_EVAL_CAPTURE_POLICY; normalized to metadata"
	}
	if cfg.Mode == ModeProduction && cfg.CapturePolicy != CaptureMetadata {
		cfg.CapturePolicy = CaptureMetadata
		cfg.Warning = "production mode enforces metadata-only capture"
	}
	return cfg
}

func (c Config) CaptureContent() bool {
	return c.Mode == ModeEval && c.CapturePolicy == CaptureFull
}
