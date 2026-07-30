package vlmguard

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	openai "github.com/sashabaranov/go-openai"
	"github.com/stretchr/testify/require"
)

func TestStreamingCompletionReturnsProgressAndUsage(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStreamChunk(t, w, "识别", "")
		time.Sleep(10 * time.Millisecond)
		writeStreamChunk(t, w, "完成", "stop")
		writeUsageChunk(t, w, 1408, 263)
		writeStreamDone(t, w)
	})
	defer server.Close()

	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		testPolicy(),
	)
	require.NoError(t, err)
	require.Equal(t, "识别完成", result.Content)
	require.Equal(t, "stop", result.FinishReason)
	require.Equal(t, 1408, result.PromptTokens)
	require.Equal(t, 263, result.CompletionTokens)
	require.True(t, result.Streamed)
	require.Greater(t, result.TTFT, time.Duration(0))
}

func TestFinishedGenerationDoesNotIdleTimeoutWhileWaitingForTrailer(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStreamChunk(t, w, "完成", "stop")
		time.Sleep(50 * time.Millisecond)
		writeUsageChunk(t, w, 10, 2)
		writeStreamDone(t, w)
	})
	defer server.Close()

	policy := testPolicy()
	policy.IdleTimeout = 20 * time.Millisecond
	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		policy,
	)
	require.NoError(t, err)
	require.Equal(t, "完成", result.Content)
	require.Equal(t, 2, result.CompletionTokens)
}

func TestFirstTokenTimeoutCancelsUpstreamRequest(t *testing.T) {
	cancelled := make(chan struct{})
	server := newStreamingServer(t, func(w http.ResponseWriter, request *http.Request) {
		<-request.Context().Done()
		close(cancelled)
	})
	defer server.Close()

	policy := testPolicy()
	policy.FirstTokenTimeout = 25 * time.Millisecond
	_, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		policy,
	)
	requireFailureKind(t, err, FailureFirstTokenTimeout, true)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not cancelled")
	}
}

func TestIdleTimeoutAfterProgressCancelsUpstreamRequest(t *testing.T) {
	cancelled := make(chan struct{})
	server := newStreamingServer(t, func(w http.ResponseWriter, request *http.Request) {
		writeStreamChunk(t, w, "已有进展", "")
		<-request.Context().Done()
		close(cancelled)
	})
	defer server.Close()

	policy := testPolicy()
	policy.IdleTimeout = 25 * time.Millisecond
	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		policy,
	)
	require.Equal(t, "已有进展", result.Content)
	requireFailureKind(t, err, FailureIdleTimeout, true)
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("upstream request context was not cancelled")
	}
}

func TestProgressingRequestUsesTotalBudgetWithoutOpeningCircuit(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, request *http.Request) {
		for {
			select {
			case <-request.Context().Done():
				return
			case <-time.After(5 * time.Millisecond):
				writeStreamChunk(t, w, "进展", "")
			}
		}
	})
	defer server.Close()

	policy := testPolicy()
	policy.TotalTimeout = 45 * time.Millisecond
	policy.FirstTokenTimeout = 30 * time.Millisecond
	policy.IdleTimeout = 20 * time.Millisecond
	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		policy,
	)
	require.NotEmpty(t, result.Content)
	requireFailureKind(t, err, FailureTotalBudget, false)
}

func TestRunawayOutputIsCancelledBeforeTokenLimit(t *testing.T) {
	prefix := strings.Repeat("正常识别文字", 400)
	block := strings.Repeat("模型开始重复这一完整段落", 16)
	runaway := prefix + strings.Repeat(block, runawayRepeats)
	server := newStreamingServer(t, func(w http.ResponseWriter, request *http.Request) {
		writeStreamChunk(t, w, runaway, "")
		<-request.Context().Done()
	})
	defer server.Close()

	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		testPolicy(),
	)
	require.NotEmpty(t, result.Content)
	requireFailureKind(t, err, FailureRunaway, false)
}

func TestOutputLimitIsSemanticFailureNotProviderOutage(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStreamChunk(t, w, "被截断的识别结果", "length")
	})
	defer server.Close()

	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		testPolicy(),
	)
	require.Equal(t, "被截断的识别结果", result.Content)
	requireFailureKind(t, err, FailureOutputLimit, false)
}

func TestPrematureStreamEOFWithoutFinishReasonIsProviderFailure(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStreamChunk(t, w, "只有部分 OCR 内容", "")
	})
	defer server.Close()

	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		testPolicy(),
	)
	require.Equal(t, "只有部分 OCR 内容", result.Content)
	require.ErrorIs(t, err, io.ErrUnexpectedEOF)
	requireFailureKind(t, err, FailureStreamTruncated, true)
}

func TestTerminalFinishReasonAllowsEOFWithoutDoneMarker(t *testing.T) {
	server := newStreamingServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeStreamChunk(t, w, "完整 OCR 内容", "stop")
	})
	defer server.Close()

	result, err := Complete(
		context.Background(),
		testOpenAIClient(server),
		openai.ChatCompletionRequest{Model: "test-vlm"},
		testPolicy(),
	)
	require.NoError(t, err)
	require.Equal(t, "完整 OCR 内容", result.Content)
	require.Equal(t, "stop", result.FinishReason)
}

func testPolicy() Policy {
	return Policy{
		Operation:         OperationOCR,
		Streaming:         true,
		FirstTokenTimeout: 200 * time.Millisecond,
		IdleTimeout:       100 * time.Millisecond,
		TotalTimeout:      time.Second,
		MaxTokens:         DefaultMaxTokens,
		Temperature:       0.1,
		DetectRunaway:     true,
	}
}

func newStreamingServer(
	t *testing.T,
	handler func(http.ResponseWriter, *http.Request),
) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flushResponse(w)
		handler(w, request)
	}))
}

func testOpenAIClient(server *httptest.Server) *openai.Client {
	config := openai.DefaultConfig("test-key")
	config.BaseURL = server.URL + "/v1"
	config.HTTPClient = server.Client()
	return openai.NewClientWithConfig(config)
}

func writeStreamChunk(t *testing.T, w http.ResponseWriter, content, finishReason string) {
	t.Helper()
	payload := map[string]any{
		"choices": []map[string]any{{
			"delta":         map[string]any{"content": content},
			"finish_reason": finishReason,
		}},
	}
	writeStreamJSON(t, w, payload)
}

func writeUsageChunk(t *testing.T, w http.ResponseWriter, promptTokens, completionTokens int) {
	t.Helper()
	writeStreamJSON(t, w, map[string]any{
		"choices": []any{},
		"usage": map[string]any{
			"prompt_tokens":     promptTokens,
			"completion_tokens": completionTokens,
			"total_tokens":      promptTokens + completionTokens,
		},
	})
}

func writeStreamJSON(t *testing.T, w http.ResponseWriter, payload any) {
	t.Helper()
	encoded, err := json.Marshal(payload)
	require.NoError(t, err)
	_, err = fmt.Fprintf(w, "data: %s\n\n", encoded)
	require.NoError(t, err)
	flushResponse(w)
}

func writeStreamDone(t *testing.T, w http.ResponseWriter) {
	t.Helper()
	_, err := fmt.Fprint(w, "data: [DONE]\n\n")
	require.NoError(t, err)
	flushResponse(w)
}

func flushResponse(w http.ResponseWriter) {
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func requireFailureKind(
	t *testing.T,
	err error,
	expected FailureKind,
	providerFailure bool,
) {
	t.Helper()
	require.Error(t, err)
	kind, ok := FailureKindOf(err)
	require.True(t, ok)
	require.Equal(t, expected, kind)
	failure, classified := ProviderCircuitFailure(err)
	require.True(t, classified)
	require.Equal(t, providerFailure, failure)
}
