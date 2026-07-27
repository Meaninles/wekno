package workretry

import (
	"errors"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/stretchr/testify/require"
)

func TestConfigFromEnv(t *testing.T) {
	t.Setenv("CUSTOM_WORK_RETRY_IMAGE_MAX_ATTEMPTS", "12")
	t.Setenv("CUSTOM_WORK_RETRY_WIKI_MAX_ATTEMPTS", "11")
	t.Setenv("CUSTOM_WORK_RETRY_WIKI_INLINE_ATTEMPTS", "2")
	t.Setenv("CUSTOM_WORK_RETRY_WIKI_CALL_TIMEOUT_SECONDS", "900")

	config := ConfigFromEnv()
	require.Equal(t, 12, config.ImageMaxAttempts)
	require.Equal(t, 12, config.ImageMaxRetries()+1)
	require.Equal(t, 11, config.WikiMaxAttempts)
	require.Equal(t, 2, config.WikiInlineAttempts)
	require.Equal(t, 15*time.Minute, config.WikiCallTimeout)
}

func TestConsumeProviderFailureDistinguishesCallFromCircuitReject(t *testing.T) {
	circuit := &modeladmission.CircuitOpenError{
		Kind:       modeladmission.KindVLM,
		RetryAfter: time.Minute,
	}
	require.Same(t, circuit, ConsumeProviderFailure(circuit))
	require.False(t, ConsumesBudget(circuit))

	provider := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindVLM,
		RetryAfter: time.Minute,
		Cause:      errors.New("upstream timeout"),
	}
	budgeted := ConsumeProviderFailure(provider)
	require.True(t, ConsumesBudget(budgeted))
	require.ErrorIs(t, budgeted, modeladmission.ErrProviderUnavailable)
	require.ErrorContains(t, budgeted, "upstream timeout")
}
