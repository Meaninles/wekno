package rerank

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

// ZhipuReranker implements a reranking system based on Zhipu AI models
type ZhipuReranker struct {
	modelName     string       // Name of the model used for reranking
	modelID       string       // Unique identifier of the model
	apiKey        string       // API key for authentication
	baseURL       string       // Base URL for API requests
	client        *http.Client // HTTP client for making API requests
	maxRetries    int
	customHeaders map[string]string
}

// SetCustomHeaders 设置用户自定义 HTTP 请求头（类似 OpenAI Python SDK 的 extra_headers）。
func (r *ZhipuReranker) SetCustomHeaders(headers map[string]string) {
	r.customHeaders = headers
}

// ZhipuRerankRequest represents a request to rerank documents using Zhipu AI API
type ZhipuRerankRequest struct {
	Model           string   `json:"model"`                       // Model to use for reranking
	Query           string   `json:"query"`                       // Query text to compare documents against
	Documents       []string `json:"documents"`                   // List of document texts to rerank
	TopN            int      `json:"top_n,omitempty"`             // Number of top results to return (0 = all)
	ReturnDocuments bool     `json:"return_documents,omitempty"`  // Whether to return documents in response
	ReturnRawScores bool     `json:"return_raw_scores,omitempty"` // Whether to return raw scores
}

// ZhipuRerankResponse represents the response from Zhipu AI reranking request
type ZhipuRerankResponse struct {
	RequestID string            `json:"request_id"` // Request ID from client or platform
	ID        string            `json:"id"`         // Task order ID from Zhipu platform
	Results   []ZhipuRankResult `json:"results"`    // Ranked results with relevance scores
	Usage     ZhipuUsage        `json:"usage"`      // Token usage information
}

// ZhipuRankResult represents a single reranking result from Zhipu AI
type ZhipuRankResult struct {
	Index          int     `json:"index"`              // Original index of the document
	RelevanceScore float64 `json:"relevance_score"`    // Relevance score
	Document       string  `json:"document,omitempty"` // Document text (optional)
}

// ZhipuUsage contains information about token usage in the Zhipu API request
type ZhipuUsage struct {
	TotalTokens  int `json:"total_tokens"`  // Total tokens consumed
	PromptTokens int `json:"prompt_tokens"` // Prompt tokens
}

// NewZhipuReranker creates a new instance of Zhipu reranker with the provided configuration
func NewZhipuReranker(config *RerankerConfig) (*ZhipuReranker, error) {
	apiKey := config.APIKey
	baseURL := provider.ZhipuRerankBaseURL
	if url := config.BaseURL; url != "" {
		baseURL = url
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if err := secutils.ValidateURLForSSRF(baseURL); err != nil {
		return nil, fmt.Errorf("baseURL SSRF check failed: %w", err)
	}

	return &ZhipuReranker{
		modelName:  config.ModelName,
		modelID:    config.ModelID,
		apiKey:     apiKey,
		baseURL:    baseURL,
		client:     &http.Client{Timeout: 60 * time.Second},
		maxRetries: 3,
	}, nil
}

// Rerank performs document reranking based on relevance to the query using Zhipu AI API
func (r *ZhipuReranker) Rerank(ctx context.Context, query string, documents []string) ([]RankResult, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("zhipu rerank query is empty")
	}
	if len(documents) == 0 {
		return []RankResult{}, nil
	}
	for i, doc := range documents {
		if strings.TrimSpace(doc) == "" {
			return nil, fmt.Errorf("zhipu rerank document[%d] is empty", i)
		}
	}

	// Build the request body
	requestBody := &ZhipuRerankRequest{
		Model:           r.modelName,
		Query:           query,
		Documents:       documents,
		TopN:            0, // Return all documents
		ReturnDocuments: true,
		ReturnRawScores: false,
	}

	jsonData, err := json.Marshal(requestBody)
	if err != nil {
		return nil, fmt.Errorf("marshal request body: %w", err)
	}

	logger.Debugf(ctx, "%s", buildRerankRequestDebug(r.modelName, r.baseURL, query, documents))
	resp, err := r.doRequestWithRetry(ctx, jsonData)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	// Read the response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		bodyStr := truncateZhipuErrorBody(body)
		return nil, fmt.Errorf("zhipu rerank API error: Http Status: %s, Body: %s", resp.Status, bodyStr)
	}

	var response ZhipuRerankResponse
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Convert Zhipu results to standard RankResult format
	results := make([]RankResult, len(response.Results))
	for i, zhipuResult := range response.Results {
		results[i] = RankResult{
			Index: zhipuResult.Index,
			Document: DocumentInfo{
				Text: zhipuResult.Document,
			},
			RelevanceScore: zhipuResult.RelevanceScore,
		}
	}

	return results, nil
}

func (r *ZhipuReranker) doRequestWithRetry(ctx context.Context, jsonData []byte) (*http.Response, error) {
	var lastErr error
	for i := 0; i <= r.maxRetries; i++ {
		if i > 0 {
			backoff := time.Duration(1<<uint(i-1)) * time.Second
			if backoff > 10*time.Second {
				backoff = 10 * time.Second
			}
			logger.Infof(ctx, "ZhipuReranker retrying request (%d/%d), waiting %v", i, r.maxRetries, backoff)
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		req, err := http.NewRequestWithContext(ctx, "POST", r.baseURL, bytes.NewReader(jsonData))
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", r.apiKey))
		secutils.ApplyCustomHeaders(req, r.customHeaders)

		resp, err := r.client.Do(req)
		if err != nil {
			lastErr = err
			logger.Warnf(ctx, "ZhipuReranker request failed (attempt %d/%d): %v", i+1, r.maxRetries+1, err)
			continue
		}
		if shouldRetryZhipuRerankStatus(resp.StatusCode) && i < r.maxRetries {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			logger.Warnf(ctx, "ZhipuReranker retryable HTTP status (attempt %d/%d): %s",
				i+1, r.maxRetries+1, resp.Status)
			continue
		}
		return resp, nil
	}
	return nil, lastErr
}

func shouldRetryZhipuRerankStatus(status int) bool {
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status >= 500
}

func truncateZhipuErrorBody(body []byte) string {
	bodyStr := string(body)
	if len(bodyStr) > 1000 {
		return bodyStr[:1000] + "... (truncated)"
	}
	return bodyStr
}

// GetModelName returns the name of the reranking model
func (r *ZhipuReranker) GetModelName() string {
	return r.modelName
}

// GetModelID returns the unique identifier of the reranking model
func (r *ZhipuReranker) GetModelID() string {
	return r.modelID
}
