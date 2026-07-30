package vlmguard

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestOperationContextDefaultsAndRoundTrips(t *testing.T) {
	require.Equal(t, OperationGeneral, OperationFromContext(nil))
	require.Equal(t, OperationGeneral, OperationFromContext(context.Background()))
	require.Equal(
		t,
		OperationOCR,
		OperationFromContext(WithOperation(context.Background(), OperationOCR)),
	)
	require.Equal(
		t,
		OperationCaption,
		OperationFromContext(WithOperation(context.Background(), OperationCaption)),
	)
}

func TestConfigUsesIndependentOCRAndCaptionBudgets(t *testing.T) {
	t.Setenv("VLM_STREAMING_ENABLED", "true")
	t.Setenv("VLM_FIRST_TOKEN_TIMEOUT_SECONDS", "100")
	t.Setenv("VLM_STREAM_IDLE_TIMEOUT_SECONDS", "40")
	t.Setenv("VLM_TOTAL_TIMEOUT_SECONDS", "300")
	t.Setenv("VLM_OCR_MAX_TOKENS", "4500")
	t.Setenv("VLM_CAPTION_MAX_TOKENS", "400")

	config := ConfigFrom(map[string]any{
		"vlm_first_token_timeout_seconds": "110",
		"vlm_caption_max_tokens":          "256",
	}, 0.1)

	ocr := config.Policy(OperationOCR)
	require.True(t, ocr.Streaming)
	require.Equal(t, 110*time.Second, ocr.FirstTokenTimeout)
	require.Equal(t, 40*time.Second, ocr.IdleTimeout)
	require.Equal(t, 300*time.Second, ocr.TotalTimeout)
	require.Equal(t, 4500, ocr.MaxTokens)
	require.Less(t, ocr.Temperature, float32(0.00001))
	require.True(t, ocr.DetectRunaway)

	caption := config.Policy(OperationCaption)
	require.Equal(t, 256, caption.MaxTokens)
	require.InDelta(t, 0.1, caption.Temperature, 0.00001)
}

func TestLegacyHTTPTimeoutOnlyControlsTotalBudget(t *testing.T) {
	t.Setenv("VLM_TOTAL_TIMEOUT_SECONDS", "")
	t.Setenv("VLM_HTTP_TIMEOUT_SECONDS", "240")

	config := ConfigFrom(nil, 0.1)
	require.Equal(t, 240*time.Second, config.Policy(OperationOCR).TotalTimeout)
	require.Equal(t, DefaultFirstTokenTimeout, config.Policy(OperationOCR).FirstTokenTimeout)
}
