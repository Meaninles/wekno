package embedding

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnoseEmbeddingInputDoesNotTreatUTF8BytesAsTokens(t *testing.T) {
	text := strings.Repeat("制度", 2_000)
	diagnostic := diagnoseEmbeddingInput(text)

	if diagnostic.Bytes <= 8_192 {
		t.Fatalf("test input bytes = %d, want > 8192", diagnostic.Bytes)
	}
	if diagnostic.EstimatedTokens >= 8_192 {
		t.Fatalf("estimated tokens = %d, want < 8192", diagnostic.EstimatedTokens)
	}
	if diagnostic.Runes != 4_000 {
		t.Fatalf("runes = %d, want 4000", diagnostic.Runes)
	}
}

func TestEmbeddingInputPreviewPreservesValidUTF8(t *testing.T) {
	preview := embeddingInputPreview(strings.Repeat("列说明", 100), 17)
	if !strings.HasSuffix(preview, "...") {
		t.Fatalf("preview missing truncation marker: %q", preview)
	}
	if len([]rune(strings.TrimSuffix(preview, "..."))) != 17 {
		t.Fatalf("preview rune count = %d, want 17", len([]rune(strings.TrimSuffix(preview, "..."))))
	}
}

func TestOpenAIEmbedderBatchEmbedOmitsDimensionsByDefault(t *testing.T) {
	requestBody := captureOpenAIEmbeddingRequest(t, "text-embedding-3-small", 256, false)

	if _, ok := requestBody["dimensions"]; ok {
		t.Fatalf("expected request body to omit dimensions by default, got %v", requestBody)
	}
}

func TestOpenAIEmbedderBatchEmbedOmitsServerDimensionsWhenOverrideEnabled(t *testing.T) {
	requestBody := captureOpenAIEmbeddingRequest(t, "text-embedding-3-small", 256, true)

	if _, ok := requestBody["dimensions"]; ok {
		t.Fatalf("expected client-side dimension reduction, got server dimensions in %v", requestBody)
	}
}

func TestOpenAIEmbedderApplyDimensionTruncatesAndNormalizes(t *testing.T) {
	embedder := &OpenAIEmbedder{dimensions: 2, supportsDimensionOverride: true}
	got := embedder.applyDimension([]float32{3, 4, 12})
	if len(got) != 2 {
		t.Fatalf("dimension-reduced vector length = %d, want 2", len(got))
	}
	if got[0] != 0.6 || got[1] != 0.8 {
		t.Fatalf("dimension-reduced vector = %v, want [0.6 0.8]", got)
	}
}

func TestOpenAIEmbedderBatchEmbedOmitsDimensionsForOpenAICompatibleModels(t *testing.T) {
	requestBody := captureOpenAIEmbeddingRequest(t, "text-embedding-v3", 1024, false)

	if _, ok := requestBody["dimensions"]; ok {
		t.Fatalf("expected request body to omit dimensions for OpenAI-compatible model, got %v", requestBody)
	}
}

func TestOpenAIEmbedderBatchEmbedOmitsDimensionsForFixedSizeModels(t *testing.T) {
	requestBody := captureOpenAIEmbeddingRequest(t, "text-embedding-ada-002", 1536, false)

	if _, ok := requestBody["dimensions"]; ok {
		t.Fatalf("expected request body to omit dimensions for fixed-size model, got %v", requestBody)
	}
}

func captureOpenAIEmbeddingRequest(t *testing.T, modelName string, dimensions int, supportsDimensionOverride bool) map[string]any {
	t.Helper()
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")

	requestBody := map[string]any{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&requestBody); err != nil {
			t.Fatalf("decode request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"embedding":[0.1,0.2],"index":0}]}`))
	}))
	defer server.Close()

	embedder, err := NewOpenAIEmbedder(
		"test-key",
		server.URL,
		modelName,
		511,
		dimensions,
		"8f7d6082-5a15-4f84-ae55-88b2bdac4ba0",
		nil,
	)
	if err != nil {
		t.Fatalf("NewOpenAIEmbedder: %v", err)
	}
	embedder.SetSupportsDimensionOverride(supportsDimensionOverride)

	if _, err := embedder.BatchEmbed(context.Background(), []string{"hello"}); err != nil {
		t.Fatalf("BatchEmbed: %v", err)
	}

	return requestBody
}
