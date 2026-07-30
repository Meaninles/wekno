package vlm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/vlmguard"
	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestRemoteAPIVLMUsesOperationSpecificStreamingPolicy(t *testing.T) {
	tests := []struct {
		name          string
		operation     vlmguard.Operation
		wantMaxTokens int
		wantTempUpper float64
	}{
		{
			name:          "OCR is deterministic and retains dense-page budget",
			operation:     vlmguard.OperationOCR,
			wantMaxTokens: vlmguard.DefaultMaxTokens,
			wantTempUpper: 0.00001,
		},
		{
			name:          "caption has a bounded short-output budget",
			operation:     vlmguard.OperationCaption,
			wantMaxTokens: vlmguard.DefaultCaptionMaxTokens,
			wantTempUpper: 0.11,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requestBody map[string]any
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				require.NoError(t, json.NewDecoder(request.Body).Decode(&requestBody))
				w.Header().Set("Content-Type", "text/event-stream")
				_, err := fmt.Fprint(
					w,
					"data: {\"choices\":[{\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}]}\n\n"+
						"data: [DONE]\n\n",
				)
				require.NoError(t, err)
			}))
			defer server.Close()

			config := openai.DefaultConfig("test-key")
			config.BaseURL = server.URL + "/v1"
			config.HTTPClient = server.Client()
			model := &RemoteAPIVLM{
				modelName:   "test-vlm",
				modelID:     "test-id",
				client:      openai.NewClientWithConfig(config),
				baseURL:     server.URL,
				temperature: defaultTemp,
				guardConfig: vlmguard.ConfigFrom(nil, defaultTemp),
			}

			ctx := vlmguard.WithOperation(context.Background(), test.operation)
			content, err := model.Predict(ctx, [][]byte{{0x89, 'P', 'N', 'G'}}, "test prompt")
			require.NoError(t, err)
			require.Equal(t, "完成", content)
			require.Equal(t, true, requestBody["stream"])
			require.EqualValues(t, test.wantMaxTokens, requestBody["max_tokens"])
			temperature, ok := requestBody["temperature"].(float64)
			require.True(t, ok, "temperature must be sent explicitly")
			require.Greater(t, temperature, float64(0))
			require.LessOrEqual(t, temperature, test.wantTempUpper)
		})
	}
}

func TestRemoteAPIVLMPreservesPartialOCRAtOutputLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := fmt.Fprint(
			w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"有效 OCR 前缀\"},\"finish_reason\":\"length\"}]}\n\n",
		)
		require.NoError(t, err)
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	config.HTTPClient = server.Client()
	model := &RemoteAPIVLM{
		modelName:   "test-vlm",
		modelID:     "test-id",
		client:      openai.NewClientWithConfig(config),
		baseURL:     server.URL,
		temperature: defaultTemp,
		guardConfig: vlmguard.ConfigFrom(nil, defaultTemp),
	}

	content, err := model.Predict(
		vlmguard.WithOperation(context.Background(), vlmguard.OperationOCR),
		[][]byte{{0x89, 'P', 'N', 'G'}},
		"test prompt",
	)
	require.Error(t, err)
	require.Equal(t, "有效 OCR 前缀", content)
	kind, ok := vlmguard.FailureKindOf(err)
	require.True(t, ok)
	require.Equal(t, vlmguard.FailureOutputLimit, kind)
}

func TestRemoteAPIVLMRejectsPartialContentFromPrematureEOF(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, err := fmt.Fprint(
			w,
			"data: {\"choices\":[{\"delta\":{\"content\":\"不完整 OCR\"},\"finish_reason\":\"\"}]}\n\n",
		)
		require.NoError(t, err)
	}))
	defer server.Close()

	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	config.HTTPClient = server.Client()
	model := &RemoteAPIVLM{
		modelName:   "test-vlm",
		modelID:     "test-id",
		client:      openai.NewClientWithConfig(config),
		baseURL:     server.URL,
		temperature: defaultTemp,
		guardConfig: vlmguard.ConfigFrom(nil, defaultTemp),
	}

	content, err := model.Predict(
		vlmguard.WithOperation(context.Background(), vlmguard.OperationOCR),
		[][]byte{{0x89, 'P', 'N', 'G'}},
		"test prompt",
	)
	require.Error(t, err)
	require.Equal(t, "不完整 OCR", content)
	kind, ok := vlmguard.FailureKindOf(err)
	require.True(t, ok)
	require.Equal(t, vlmguard.FailureStreamTruncated, kind)
}
