package embedding

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	secutils "github.com/Tencent/WeKnora/internal/utils"
)

// OpenAIEmbedder implements text vectorization functionality using OpenAI API
type OpenAIEmbedder struct {
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

// OpenAIEmbedRequest represents an OpenAI embedding request
type OpenAIEmbedRequest struct {
	Model                string   `json:"model"`
	Input                []string `json:"input"`
	EncodingFormat       string   `json:"encoding_format,omitempty"`
	Dimensions           int      `json:"dimensions,omitempty"`
	TruncatePromptTokens int      `json:"truncate_prompt_tokens,omitempty"`
}

// OpenAIEmbedResponse represents an OpenAI embedding response
type OpenAIEmbedResponse struct {
	Data []struct {
		Embedding []float32 `json:"embedding"`
		Index     int       `json:"index"`
	} `json:"data"`
}

// NewOpenAIEmbedder creates a new OpenAI embedder
func NewOpenAIEmbedder(apiKey, baseURL, modelName string,
	truncatePromptTokens int, dimensions int, modelID string, pooler EmbedderPooler,
) (*OpenAIEmbedder, error) {
	if baseURL == "" {
		baseURL = "https://api.openai.com/v1"
	}

	if modelName == "" {
		return nil, fmt.Errorf("model name is required")
	}

	if truncatePromptTokens == 0 {
		truncatePromptTokens = 511
	}

	timeout := 60 * time.Second

	if err := validateEmbeddingBaseURL(baseURL); err != nil {
		return nil, err
	}

	return &OpenAIEmbedder{
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

// SetCustomHeaders 设置用户自定义 HTTP 请求头（类似 OpenAI Python SDK 的 extra_headers）。
// 保留头（Authorization、Content-Type 等）会在发送时被自动跳过。
func (e *OpenAIEmbedder) SetCustomHeaders(headers map[string]string) {
	e.customHeaders = headers
}

func (e *OpenAIEmbedder) SetSupportsDimensionOverride(supported bool) {
	e.supportsDimensionOverride = supported
}

// Embed converts text to vector
func (e *OpenAIEmbedder) Embed(ctx context.Context, text string) ([]float32, error) {
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

func (e *OpenAIEmbedder) doRequestWithRetry(ctx context.Context, jsonData []byte) (*http.Response, error) {
	var resp *http.Response
	var err error
	url := e.baseURL + "/embeddings"

	for i := 0; i <= e.maxRetries; i++ {
		if i > 0 {
			backoffTime := time.Duration(1<<uint(i-1)) * time.Second
			if backoffTime > 10*time.Second {
				backoffTime = 10 * time.Second
			}
			logger.GetLogger(ctx).
				Infof("OpenAIEmbedder retrying request (%d/%d), waiting %v", i, e.maxRetries, backoffTime)

			select {
			case <-time.After(backoffTime):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}

		// Rebuild request each time to ensure Body is valid.
		// IMPORTANT: declare `req` separately (var) so the assignment to `err`
		// below uses the outer-scope variable, not a fresh loop-local one.
		// Previously this read `req, err := http.NewRequestWithContext(...)`,
		// where `:=` introduced a new `err` shadowing the outer one. The
		// `resp, err = httpClient.Do(req)` line then wrote to the shadowed
		// `err` only, so when all retries failed with connection errors the
		// outer `err` stayed nil. The function returned `(nil, nil)`, and
		// callers (BatchEmbed line 195) blindly dereferenced `resp.Body` →
		// SIGSEGV nil-pointer panic that took down the whole process.
		// Reproduce: stop the embedding upstream (e.g. localhost:3130), make
		// any RAG query → backend SIGSEGV instead of returning HTTP 500.
		var req *http.Request
		req, err = http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(jsonData))
		if err != nil {
			logger.GetLogger(ctx).Errorf("OpenAIEmbedder failed to create request: %v", err)
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+e.apiKey)
		secutils.ApplyCustomHeaders(req, e.customHeaders)

		resp, err = e.httpClient.Do(req)
		if err == nil {
			return resp, nil
		}

		logger.GetLogger(ctx).Errorf("OpenAIEmbedder request failed (attempt %d/%d): %v", i+1, e.maxRetries+1, err)
	}

	return nil, err
}

func (e *OpenAIEmbedder) BatchEmbed(ctx context.Context, texts []string) ([][]float32, error) {
	// Create request body
	reqBody := OpenAIEmbedRequest{
		Model:                e.modelName,
		Input:                texts,
		EncodingFormat:       "float",
		TruncatePromptTokens: e.truncatePromptTokens,
	}
	// 不向服务端发送 dimensions 参数：自建推理后端（vLLM/sglang）多数不支持服务端 matryoshka
	// 降维（返回 400 "does not support matryoshka representation"）。改为始终请求原始维度，
	// 由下方 applyDimension 在客户端做 MRL 截断——对支持 MRL 的模型，客户端截断与服务端降维
	// 数学等价。supportsDimensionOverride 仍作为 UI 开关（允许配置 dimension），但不再透传服务端。


	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder EmbedBatch marshal request error: %v", err)
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	// Log request details for debugging
	logger.GetLogger(ctx).Debugf("OpenAIEmbedder BatchEmbed: model=%s, input_count=%d, truncate_tokens=%d",
		e.modelName, len(texts), e.truncatePromptTokens)

	// Check for invalid input lengths and log details
	hasInvalidLength := false
	for i, text := range texts {
		textLen := len(text)
		textPreview := text
		if len(textPreview) > 200 {
			textPreview = textPreview[:200] + "..."
		}

		// Log warning if length is outside valid range [1, 8192]
		if textLen == 0 || textLen > 8192 {
			hasInvalidLength = true
			logger.GetLogger(ctx).Errorf("OpenAIEmbedder BatchEmbed input[%d]: INVALID length=%d (must be [1, 8192]), preview=%s",
				i, textLen, textPreview)
		} else {
			logger.GetLogger(ctx).Debugf("OpenAIEmbedder BatchEmbed input[%d]: length=%d, preview=%s",
				i, textLen, textPreview)
		}
	}

	if hasInvalidLength {
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder BatchEmbed: Found invalid input lengths, this will likely cause API error")
	}

	// Send request (passing jsonData instead of constructing http.Request)
	resp, err := e.doRequestWithRetry(ctx, jsonData)
	if err != nil {
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder EmbedBatch send request error: %v", err)
		return nil, fmt.Errorf("send request: %w", err)
	}
	if resp.Body != nil {
		defer resp.Body.Close()
	}

	// Read response
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder EmbedBatch read response error: %v", err)
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Log detailed error response from OpenAI API
		bodyStr := string(body)
		if len(bodyStr) > 1000 {
			bodyStr = bodyStr[:1000] + "... (truncated)"
		}
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder EmbedBatch API error: Http Status %s, Response Body: %s", resp.Status, bodyStr)
		return nil, fmt.Errorf("EmbedBatch API error: Http Status %s, Response: %s", resp.Status, bodyStr)
	}

	// Parse response
	var response OpenAIEmbedResponse
	if err := json.Unmarshal(body, &response); err != nil {
		logger.GetLogger(ctx).Errorf("OpenAIEmbedder EmbedBatch unmarshal response error: %v", err)
		return nil, fmt.Errorf("unmarshal response: %w", err)
	}

	// Extract embedding vectors (apply client-side MRL truncation if configured)
	embeddings := make([][]float32, 0, len(response.Data))
	for _, data := range response.Data {
		embeddings = append(embeddings, e.applyDimension(data.Embedding))
	}

	return embeddings, nil
}

// applyDimension 在配置了 dimensions 且服务端返回更高维度向量时，做客户端 MRL 截断 + L2 重归一化。
// 仅当 dimensions>0 时生效；dimensions==0（默认）完全不影响原行为，保持本地/原逻辑一致。
// 适用场景：Qwen3-Embedding 等原生支持 MRL 的模型，但服务端（如 vLLM/sglang）不支持请求级
// dimensions 参数、固定返回原始维度（如 4096）时，按配置降维到 2048 等以节省向量库存储与检索开销。
func (e *OpenAIEmbedder) applyDimension(vec []float32) []float32 {
	if e.dimensions <= 0 || len(vec) <= e.dimensions {
		return vec
	}
	out := make([]float32, e.dimensions)
	var sum float64
	for i := 0; i < e.dimensions; i++ {
		out[i] = vec[i]
		sum += float64(vec[i]) * float64(vec[i])
	}
	if sum > 0 {
		norm := float32(1.0 / math.Sqrt(sum))
		for i := range out {
			out[i] *= norm
		}
	}
	return out
}

// GetModelName returns the model name
func (e *OpenAIEmbedder) GetModelName() string {
	return e.modelName
}

func (e *OpenAIEmbedder) supportsDimensionsParam() bool {
	return e.supportsDimensionOverride && e.dimensions > 0
}

// GetDimensions returns the vector dimensions
func (e *OpenAIEmbedder) GetDimensions() int {
	return e.dimensions
}

// GetModelID returns the model ID
func (e *OpenAIEmbedder) GetModelID() string {
	return e.modelID
}
