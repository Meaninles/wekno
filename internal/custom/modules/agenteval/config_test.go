package agenteval

import "testing"

func TestProductionAlwaysNormalizesToMetadata(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "production")
	t.Setenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY", "full")
	cfg := LoadConfigFromEnv()
	if cfg.Mode != ModeProduction || cfg.CapturePolicy != CaptureMetadata || cfg.CaptureContent() {
		t.Fatalf("unexpected production config: %#v", cfg)
	}
}

func TestEvalAllowsFullCapture(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "eval")
	t.Setenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY", "full")
	cfg := LoadConfigFromEnv()
	if !cfg.CaptureContent() {
		t.Fatalf("expected eval full capture: %#v", cfg)
	}
}

func TestInvalidModeFailsOpenToProductionMetadata(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "broken")
	t.Setenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY", "full")
	cfg := LoadConfigFromEnv()
	if cfg.Mode != ModeProduction || cfg.CapturePolicy != CaptureMetadata || cfg.Warning == "" {
		t.Fatalf("unexpected normalized config: %#v", cfg)
	}
}
