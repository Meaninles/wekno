package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/provider"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// ZhipuEmbedder implements text vectorization functionality using Zhipu AI API
type ZhipuEmbedder struct {
	apiKey                    string
	baseURL                   string
	modelName                 string
	truncatePromptTokens      int
	dimensions                int
	modelID                   string
	httpClient                *http.Client
	timeout                   time.Duration
	maxRetries                int
	customHeaders             map[string]string
	supportsDimensionOverride bool
	EmbedderPooler
}

// ZhipuEmbedRequest represents a Zhipu embedding request
type ZhipuEmbedRequest struct {
	Model                string   `json:"model"`
	Input                []string `json:"input"`
	Dimensions           int      `json:"dimensions,omitempty"`
	TruncatePromptTokens int      `json:"truncate_prompt_tokens,omitempty"`
}

// ZhipuEmbedResponse represents a Zhipu embedding response
type ZhipuEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
	Model string `json:"model"`
}

const maxZhipuEmbeddingInputRunes = 8192

// NewZhipuEmbedder creates a new Zhipu embedder
func NewZhipuEmbedder(apiKey, baseURL, modelName string,
	truncatePromptTokens int, dimensions int, modelID string, pooler EmbedderPooler,
) (*ZhipuEmbedder, error) {
	if baseURL == "" {
		baseURL = provider.ZhipuEmbeddingBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}

	if truncatePromptTokens == 0 {
		truncatePromptTokens = 511
	}

	timeout := 60 * time.Second

	if err := secutils.ValidateURLForSSRF(zhipuEmbeddingEndpoint(baseURL)); err != nil {
		return nil, fmt.Errorf("baseURL SSRF check failed: %w", err)
	}

	return &ZhipuEmbedder{
		apiKey:               apiKey,
		baseURL:              baseURL,
		modelName:            modelName,
		httpClient:           newEmbeddingHTTPClient(timeout),
		truncatePromptTokens: truncatePromptTokens,
		EmbedderPooler:       pooler,
		dimensions:           dimensions,
		modelID:              modelID,
		timeout:              timeout,
		maxRetries:           3, // Maximum retry count
	}, nil
}

// SetCustomHeaders sets custom HTTP headers for the embedder
func (e *ZhipuEmbedder) SetCustomHeaders(headers map[string]string) {
	e.customHeaders = headers
}

func (e *ZhipuEmbedder) SetSupportsDimensionOverride(supported bool) {
	e.supportsDimensionOverride = supported
}

// Embed converts text to vector
func (e *ZhipuEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
	for range 3 {
		embeddings, err := e.BatchEmbed(ctx, []string{text})
		if err != nil {
			return nil, err
		}
		if len(embeddings) > 0 {
			return embeddings[0], nil
		}
	}
	return nil, fmt.Errorf("no embedding returned")
}

func (e *ZhipuEmbedder) doRequestWithRetry(ctx context.Context, jsonData []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	url := zhipuEmbeddingEndpoint(e.baseURL)

	for i := 0; i <= e.maxRetries; i++ {
		if i > 0 {
			backoffTime := time.Duration(1<<uint(i-1)) * time.Second
			if backoffTime > 10*time.Second {
				backoffTime = 10 * time.Second
			}
			logger.GetLogger(ctx).
				Infof("ZhipuEmbedder retrying request (%d/%d), waiting %v", i, e.maxRetries, backoffTime)

			select {
			case <-time.After(backoffTime):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
		if err != nil {
			logger.GetLogger(ctx).Errorf("ZhipuEmbedder failed to create request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		secutils.ApplyCustomHeaders(req, e.customHeaders)

		resp, err = e.httpClient.Do(req)
		if err == nil {
			if shouldRetryZhipuStatus(resp.StatusCode) && i < e.maxRetries {
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				_ = resp.Body.Close()
				logger.GetLogger(ctx).Warnf("ZhipuEmbedder retryable HTTP status (attempt %d/%d): %s",
					i+1, e.maxRetries+1, resp.Status)
				continue
			}
			return resp, nil
		}

		logger.GetLogger(ctx).Errorf("ZhipuEmbedder request failed (attempt %d/%d): %v", i+1, e.maxRetries+1, err)
	}

	return nil, err
}

func zhipuEmbeddingEndpoint(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if strings.HasSuffix(baseURL, "/embeddings") {
		return baseURL
	}
	return baseURL + "/embeddings"
}

func shouldRetryZhipuStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func (e *ZhipuEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	expandedTexts, chunkIndexes, err := prepareZhipuEmbeddingInputs(ctx, texts)
	if err != nil {
		return nil, err
	}

	// Create request body
	reqBody := ZhipuEmbedRequest{
		Model:                e.modelName,
		Input:                expandedTexts,
		TruncatePromptTokens: e.truncatePromptTokens,
	}
	if e.supportsDimensionsParam() {
		reqBody.Dimensions = e.dimensions
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		logger.GetLogger(ctx).Errorf("ZhipuEmbedder BatchEmbed marshal request error: %v", err)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Log request details for debugging
	logger.GetLogger(ctx).Debugf("ZhipuEmbedder BatchEmbed: model=%s, input_count=%d, request_chunks=%d, truncate_tokens=%d",
		e.modelName, len(texts), len(expandedTexts), e.truncatePromptTokens)

	// Send request (passing jsonData instead of constructing http.Request)
	resp, err := e.doRequestWithRetry(ctx, jsonData)
	if err != nil {
		logger.GetLogger(ctx).Errorf("ZhipuEmbedder BatchEmbed send request error: %v", err)
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.GetLogger(ctx).Errorf("ZhipuEmbedder BatchEmbed read response error: %v", err)
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Log detailed error response from OpenAI API
		bodyStr := string(body)
		if len(bodyStr) > 1000 {
			bodyStr = bodyStr[:1000] + "... (truncated)"
		}
		logger.GetLogger(ctx).Errorf("ZhipuEmbedder BatchEmbed API error: Http Status %s, Response Body: %s", resp.Status, bodyStr)
		return nil, fmt.Errorf("BatchEmbed API error: Http Status %s, Response: %s", resp.Status, bodyStr)
	}

	// Parse response
	var response ZhipuEmbedResponse
	if err := json.Unmarshal(body, &response); err != nil {
		logger.GetLogger(ctx).Errorf("ZhipuEmbedder BatchEmbed unmarshal response error: %v", err)
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Extract embedding vectors
	chunkEmbeddings := make([][]float32, len(expandedTexts))
	seen := make([]bool, len(expandedTexts))
	for i, data := range response.Data {
		idx := data.Index
		if idx < 0 || idx >= len(expandedTexts) {
			idx = i
		}
		if idx < 0 || idx >= len(expandedTexts) {
			return nil, fmt.Errorf("embedding response index out of range: %d", data.Index)
		}
		chunkEmbeddings[idx] = data.Embedding
		seen[idx] = true
	}
	for i, ok := range seen {
		if !ok {
			return nil, fmt.Errorf("embedding response missing vector for chunk %d", i)
		}
	}

	embeddings := make([][]float32, len(texts))
	for i, indexes := range chunkIndexes {
		vectors := make([][]float32, 0, len(indexes))
		weights := make([]int, 0, len(indexes))
		for _, idx := range indexes {
			vectors = append(vectors, chunkEmbeddings[idx])
			weights = append(weights, len([]rune(expandedTexts[idx])))
		}
		merged, err := averageEmbeddingVectorsWeighted(vectors, weights)
		if err != nil {
			return nil, fmt.Errorf("merge embedding chunks for input[%d]: %w", i, err)
		}
		embeddings[i] = merged
	}

	return embeddings, nil
}

func prepareZhipuEmbeddingInputs(ctx context.Context, texts []string) ([]string, [][]int, error) {
	expandedTexts := make([]string, 0, len(texts))
	chunkIndexes := make([][]int, len(texts))
	for i, text := range texts {
		if strings.TrimSpace(text) == "" {
			return nil, nil, fmt.Errorf("ZhipuEmbedder BatchEmbed input[%d] is empty", i)
		}
		textRunes := []rune(text)
		textLen := len(textRunes)
		if textLen <= maxZhipuEmbeddingInputRunes {
			chunkIndexes[i] = append(chunkIndexes[i], len(expandedTexts))
			expandedTexts = append(expandedTexts, text)
			logger.GetLogger(ctx).Debugf("ZhipuEmbedder BatchEmbed input[%d]: length=%d", i, textLen)
			continue
		}

		startChunkCount := len(expandedTexts)
		for start := 0; start < textLen; start += maxZhipuEmbeddingInputRunes {
			end := start + maxZhipuEmbeddingInputRunes
			if end > textLen {
				end = textLen
			}
			chunkIndexes[i] = append(chunkIndexes[i], len(expandedTexts))
			expandedTexts = append(expandedTexts, string(textRunes[start:end]))
		}
		logger.GetLogger(ctx).Warnf(
			"ZhipuEmbedder BatchEmbed input[%d]: length=%d exceeds provider limit=%d; split into %d chunks and will mean-pool embeddings",
			i, textLen, maxZhipuEmbeddingInputRunes, len(expandedTexts)-startChunkCount)
	}
	return expandedTexts, chunkIndexes, nil
}

func averageEmbeddingVectors(vectors [][]float32) ([]float32, error) {
	weights := make([]int, len(vectors))
	for i := range weights {
		weights[i] = 1
	}
	return averageEmbeddingVectorsWeighted(vectors, weights)
}

func averageEmbeddingVectorsWeighted(vectors [][]float32, weights []int) ([]float32, error) {
	if len(vectors) == 0 {
		return nil, fmt.Errorf("no vectors")
	}
	if len(weights) != len(vectors) {
		return nil, fmt.Errorf("weights length mismatch: got %d want %d", len(weights), len(vectors))
	}
	if len(vectors) == 1 {
		out := make([]float32, len(vectors[0]))
		copy(out, vectors[0])
		return out, nil
	}
	dim := len(vectors[0])
	if dim == 0 {
		return nil, fmt.Errorf("empty vector")
	}
	out := make([]float32, dim)
	totalWeight := 0
	for idx, vector := range vectors {
		if len(vector) != dim {
			return nil, fmt.Errorf("dimension mismatch: got %d want %d", len(vector), dim)
		}
		weight := weights[idx]
		if weight <= 0 {
			return nil, fmt.Errorf("invalid weight %d for vector %d", weight, idx)
		}
		totalWeight += weight
		for i, v := range vector {
			out[i] += v * float32(weight)
		}
	}
	scale := float32(totalWeight)
	for i := range out {
		out[i] /= scale
	}
	return out, nil
}

// GetModelName returns the model name
func (e *ZhipuEmbedder) GetModelName() string {
	return e.modelName
}

func (e *ZhipuEmbedder) supportsDimensionsParam() bool {
	return e.supportsDimensionOverride && e.dimensions > 0
}

// GetDimensions returns the vector dimensions
func (e *ZhipuEmbedder) GetDimensions() int {
	return e.dimensions
}

// GetModelID returns the model ID
func (e *ZhipuEmbedder) GetModelID() string {
	return e.modelID
}
