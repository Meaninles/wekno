package vlm

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/vlmguard"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/provider"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	openai "github.com/sashabaranov/go-openai"
)

const (
	defaultTimeout = vlmguard.DefaultTotalTimeout
	defaultMaxToks = vlmguard.DefaultMaxTokens
	defaultTemp    = float32(0.1)
)

// vlmHTTPTimeout remains the compatibility timeout for non-OpenAI VLM
// transports. OpenAI-compatible calls use progress-aware vlmguard deadlines.
func vlmHTTPTimeout() time.Duration {
	for _, name := range []string{"VLM_TOTAL_TIMEOUT_SECONDS", "VLM_HTTP_TIMEOUT_SECONDS"} {
		if value := strings.TrimSpace(os.Getenv(name)); value != "" {
			if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
				return time.Duration(seconds) * time.Second
			}
		}
	}
	return defaultTimeout
}

func newVLMHTTPClient(timeout time.Duration) *http.Client {
	config := secutils.DefaultSSRFSafeHTTPClientConfig()
	config.Timeout = timeout
	return secutils.NewSSRFSafeHTTPClient(config)
}

// RemoteAPIVLM implements VLM via an OpenAI-compatible chat completions API.
type RemoteAPIVLM struct {
	modelName   string
	modelID     string
	client      *openai.Client
	baseURL     string
	temperature float32
	guardConfig vlmguard.Config
}

// NewRemoteAPIVLM creates a remote-API backed VLM instance.
func NewRemoteAPIVLM(config *Config) (*RemoteAPIVLM, error) {
	providerName := provider.ProviderName(config.Provider)
	if providerName == "" {
		providerName = provider.DetectProvider(config.BaseURL)
	}

	var apiCfg openai.ClientConfig
	if providerName == provider.ProviderAzureOpenAI {
		apiCfg = openai.DefaultAzureConfig(config.APIKey, config.BaseURL)
		apiCfg.AzureModelMapperFunc = func(model string) string {
			return model
		}
		if config.Extra != nil {
			if v, ok := config.Extra["api_version"]; ok {
				if vs, ok := v.(string); ok && vs != "" {
					apiCfg.APIVersion = vs
				}
			}
		}
	} else {
		apiCfg = openai.DefaultConfig(config.APIKey)
		if config.BaseURL != "" {
			apiCfg.BaseURL = config.BaseURL
		}
	}
	// Per-request progress deadlines are enforced by vlmguard. A client-wide
	// timeout cannot distinguish active long generation from a stalled call
	// and may release admission while the upstream is still generating.
	httpClient := newVLMHTTPClient(0)

	// 注入用户自定义 HTTP header（类似 OpenAI Python SDK 的 extra_headers）
	if len(config.CustomHeaders) > 0 {
		apiCfg.HTTPClient = secutils.WrapHTTPClientWithHeaders(httpClient, config.CustomHeaders)
	} else {
		apiCfg.HTTPClient = httpClient
	}

	temp := defaultTemp
	if config.Extra != nil {
		if v, ok := config.Extra["temperature"]; ok {
			if vs, ok := v.(string); ok {
				if f, err := strconv.ParseFloat(vs, 32); err == nil {
					temp = float32(f)
				}
			}
		}
	}

	return &RemoteAPIVLM{
		modelName:   config.ModelName,
		modelID:     config.ModelID,
		client:      openai.NewClientWithConfig(apiCfg),
		baseURL:     config.BaseURL,
		temperature: temp,
		guardConfig: vlmguard.ConfigFrom(config.Extra, temp),
	}, nil
}

// Predict sends an image with a text prompt to the OpenAI-compatible API.
func (v *RemoteAPIVLM) Predict(ctx context.Context, imgBytesList [][]byte, prompt string) (string, error) {
	operation := vlmguard.OperationFromContext(ctx)
	policy := v.guardConfig.Policy(operation)
	var parts []openai.ChatMessagePart

	// Add text prompt first
	parts = append(parts, openai.ChatMessagePart{
		Type: openai.ChatMessagePartTypeText,
		Text: prompt,
	})

	// Add images
	for _, imgBytes := range imgBytesList {
		if len(imgBytes) > 0 {
			mimeType := detectImageMIME(imgBytes)
			b64 := base64.StdEncoding.EncodeToString(imgBytes)
			dataURI := fmt.Sprintf("data:%s;base64,%s", mimeType, b64)
			parts = append(parts, openai.ChatMessagePart{
				Type: openai.ChatMessagePartTypeImageURL,
				ImageURL: &openai.ChatMessageImageURL{
					URL:    dataURI,
					Detail: openai.ImageURLDetailAuto,
				},
			})
		}
	}

	req := openai.ChatCompletionRequest{
		Model: v.modelName,
		Messages: []openai.ChatCompletionMessage{
			{
				Role:         openai.ChatMessageRoleUser,
				MultiContent: parts,
			},
		},
		MaxTokens:   policy.MaxTokens,
		Temperature: policy.Temperature,
	}

	totalImageSize := 0
	for _, img := range imgBytesList {
		totalImageSize += len(img)
	}
	logger.Infof(
		ctx,
		"[VLM] Calling OpenAI-compatible API, model=%s, baseURL=%s, operation=%s, "+
			"numImages=%d, totalImageSize=%d, stream=%t, maxTokens=%d, "+
			"firstTokenTimeout=%s, idleTimeout=%s, totalTimeout=%s",
		v.modelName,
		v.baseURL,
		operation,
		len(imgBytesList),
		totalImageSize,
		policy.Streaming,
		policy.MaxTokens,
		policy.FirstTokenTimeout,
		policy.IdleTimeout,
		policy.TotalTimeout,
	)

	result, err := vlmguard.Complete(ctx, v.client, req, policy)
	if err != nil {
		logger.Warnf(
			ctx,
			"[VLM] OpenAI request terminated, model=%s, operation=%s, streamed=%t, "+
				"ttft=%s, duration=%s, lastProgressAge=%s, completionTokens=%d, "+
				"responseChars=%d, finishReason=%s, error=%v",
			v.modelName,
			operation,
			result.Streamed,
			result.TTFT,
			result.Duration,
			result.LastProgressAge,
			result.CompletionTokens,
			len([]rune(result.Content)),
			result.FinishReason,
			err,
		)
		// Preserve partial streamed content for callers that can explicitly
		// handle a typed terminal condition (notably OCR output truncation).
		// Ordinary callers still treat the non-nil error as a failed request.
		return result.Content, fmt.Errorf("OpenAI VLM request: %w", err)
	}
	logger.Infof(
		ctx,
		"[VLM] OpenAI response received, model=%s, operation=%s, streamed=%t, "+
			"ttft=%s, duration=%s, completionTokens=%d, responseChars=%d, finishReason=%s",
		v.modelName,
		operation,
		result.Streamed,
		result.TTFT,
		result.Duration,
		result.CompletionTokens,
		len([]rune(result.Content)),
		result.FinishReason,
	)
	return result.Content, nil
}

func (v *RemoteAPIVLM) GetModelName() string { return v.modelName }
func (v *RemoteAPIVLM) GetModelID() string   { return v.modelID }

// detectImageMIME returns the MIME type for the given image bytes.
func detectImageMIME(data []byte) string {
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "image/png"
}
