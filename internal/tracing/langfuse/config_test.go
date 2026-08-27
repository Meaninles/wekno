package langfuse

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/agenteval"
)

func TestLoadConfigFromEnvProductionIsBoundedMetadataOnly(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "production")
	t.Setenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY", "full")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	t.Setenv("LANGFUSE_SAMPLE_RATE", "1")
	cfg := LoadConfigFromEnv()
	if !cfg.Enabled || cfg.SampleRate != 0.01 {
		t.Fatalf("unexpected bounded production config: %#v", cfg)
	}
	if cfg.CaptureContent() || cfg.AgentEval.CapturePolicy != agenteval.CaptureMetadata {
		t.Fatalf("production must be metadata-only: %#v", cfg.AgentEval)
	}
}

func TestLoadConfigFromEnvEvalUsesFullSampling(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "eval")
	t.Setenv("CUSTOM_AGENT_EVAL_CAPTURE_POLICY", "full")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	t.Setenv("LANGFUSE_SAMPLE_RATE", "")
	cfg := LoadConfigFromEnv()
	if cfg.SampleRate != 1 || !cfg.CaptureContent() {
		t.Fatalf("unexpected eval config: %#v", cfg)
	}
}

func TestSampleRateZeroReallyDisablesRecording(t *testing.T) {
	t.Setenv("CUSTOM_AGENT_EVAL_MODE", "eval")
	t.Setenv("LANGFUSE_PUBLIC_KEY", "pk-test")
	t.Setenv("LANGFUSE_SECRET_KEY", "sk-test")
	t.Setenv("LANGFUSE_SAMPLE_RATE", "0")
	cfg := LoadConfigFromEnv()
	if cfg.SampleRate != 0 {
		t.Fatalf("sample rate zero changed to %v", cfg.SampleRate)
	}
}

func TestFlushIntervalAcceptsDurationAndSeconds(t *testing.T) {
	t.Setenv("LANGFUSE_FLUSH_INTERVAL", "500ms")
	if got := LoadConfigFromEnv().FlushInterval; got != 500*time.Millisecond {
		t.Fatalf("got %s", got)
	}
	t.Setenv("LANGFUSE_FLUSH_INTERVAL", "7")
	if got := LoadConfigFromEnv().FlushInterval; got != 7*time.Second {
		t.Fatalf("got %s", got)
	}
}

func TestConfigValidate(t *testing.T) {
	valid := Config{
		Enabled: true, Host: "https://x", PublicKey: "pk", SecretKey: "sk",
		SampleRate: 1, QueueSize: 128, FlushAt: 32,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid config failed: %v", err)
	}
	invalid := valid
	invalid.FlushAt = 256
	if err := invalid.Validate(); err == nil {
		t.Fatal("expected invalid batch size")
	}
}
