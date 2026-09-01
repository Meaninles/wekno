package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
)

func TestLLMRetryDelayUsesShortFallbackForUntypedTransportFailure(t *testing.T) {
	if got := llmRetryDelay(errors.New("connection reset"), 2); got != 2*time.Second {
		t.Fatalf("llmRetryDelay() = %s, want 2s", got)
	}
}

func TestLLMRetryDelayHonorsProviderCooldown(t *testing.T) {
	err := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 15 * time.Second,
		Cause:      errors.New("remote EOF"),
	}
	if got := llmRetryDelay(err, 1); got != 15*time.Second {
		t.Fatalf("llmRetryDelay() = %s, want 15s", got)
	}
}

func TestIsTransientErrorRecognizesTypedCircuitCooldown(t *testing.T) {
	err := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindChat,
		RetryAfter: 48 * time.Second,
	}
	if !isTransientError(err) {
		t.Fatal("typed circuit cooldown must be retryable")
	}
}

func TestIsTransientErrorRejectsPermanentFailure(t *testing.T) {
	if isTransientError(errors.New("invalid model configuration")) {
		t.Fatal("permanent configuration failure must not be retryable")
	}
}
