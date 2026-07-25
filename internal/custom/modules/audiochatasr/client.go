package audiochatasr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	secutils "github.com/Tencent/WeKnora/internal/utils"
)

const (
	defaultTimeout       = 300 * time.Second
	defaultMaxTokens     = 4096
	maxResponseBodyBytes = 4 << 20
	defaultPrompt        = "请准确、完整地转写这段音频。只输出转写文本，不要解释，也不要添加音频中没有的内容。"
)

// Config describes an OpenAI-compatible multimodal chat endpoint that can
// transcribe audio_url content.
type Config struct {
	BaseURL    string
	Model      string
	APIKey     string
	Prompt     string
	Language   string
	Path       string
	MaxTokens  int
	Headers    map[string]string
	HTTPClient *http.Client
}

type Client struct {
	endpoint   string
	model      string
	apiKey     string
	prompt     string
	maxTokens  int
	httpClient *http.Client
}

type requestPayload struct {
	Model       string           `json:"model"`
	Messages    []requestMessage `json:"messages"`
	Temperature float64          `json:"temperature"`
	MaxTokens   int              `json:"max_tokens"`
}

type requestMessage struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type     string         `json:"type"`
	Text     string         `json:"text,omitempty"`
	AudioURL *audioURLValue `json:"audio_url,omitempty"`
}

type audioURLValue struct {
	URL string `json:"url"`
}

type responsePayload struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

func New(config Config) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(config.BaseURL), "/")
	model := strings.TrimSpace(config.Model)
	if baseURL == "" {
		return nil, errors.New("audio chat ASR base URL is required")
	}
	if model == "" {
		return nil, errors.New("audio chat ASR model is required")
	}
	path := strings.TrimSpace(config.Path)
	if path == "" {
		path = "/chat/completions"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	prompt := strings.TrimSpace(config.Prompt)
	if prompt == "" {
		prompt = defaultPrompt
		if language := strings.TrimSpace(config.Language); language != "" {
			prompt += " 识别语言：" + language + "。"
		}
	}
	maxTokens := config.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		httpConfig := secutils.DefaultSSRFSafeHTTPClientConfig()
		httpConfig.Timeout = defaultTimeout
		httpClient = secutils.NewSSRFSafeHTTPClient(httpConfig)
	}
	if len(config.Headers) > 0 {
		httpClient = secutils.WrapHTTPClientWithHeaders(httpClient, config.Headers)
	}
	return &Client{
		endpoint:   baseURL + path,
		model:      model,
		apiKey:     config.APIKey,
		prompt:     prompt,
		maxTokens:  maxTokens,
		httpClient: httpClient,
	}, nil
}

func (c *Client) Transcribe(
	ctx context.Context,
	audio []byte,
	fileName string,
) (string, error) {
	if len(audio) == 0 {
		return "", errors.New("audio bytes are empty")
	}
	mimeType := detectAudioMIME(audio, fileName)
	dataURI := "data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(audio)
	payload := requestPayload{
		Model: c.model,
		Messages: []requestMessage{{
			Role: "user",
			Content: []contentPart{
				{
					Type:     "audio_url",
					AudioURL: &audioURLValue{URL: dataURI},
				},
				{Type: "text", Text: c.prompt},
			},
		}},
		Temperature: 0,
		MaxTokens:   c.maxTokens,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal audio chat ASR request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("create audio chat ASR request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(c.apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("audio chat ASR request failed: %w", err)
	}
	defer resp.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBodyBytes+1))
	if err != nil {
		return "", fmt.Errorf("read audio chat ASR response: %w", err)
	}
	if len(responseBody) > maxResponseBodyBytes {
		return "", errors.New("audio chat ASR response exceeded size limit")
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return "", fmt.Errorf(
			"audio chat ASR returned HTTP %d: %s",
			resp.StatusCode,
			truncatedMessage(responseBody, 1024),
		)
	}
	var decoded responsePayload
	if err := json.Unmarshal(responseBody, &decoded); err != nil {
		return "", fmt.Errorf("decode audio chat ASR response: %w", err)
	}
	if len(decoded.Choices) == 0 {
		return "", errors.New("audio chat ASR returned no choices")
	}
	text := strings.TrimSpace(decoded.Choices[0].Message.Content)
	if text == "" {
		return "", errors.New("audio chat ASR returned empty transcription")
	}
	return text, nil
}

func ParseMaxTokens(value string) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || parsed <= 0 {
		return defaultMaxTokens
	}
	return parsed
}

func detectAudioMIME(data []byte, fileName string) string {
	if extension := strings.ToLower(filepath.Ext(strings.TrimSpace(fileName))); extension != "" {
		if value := mime.TypeByExtension(extension); strings.HasPrefix(value, "audio/") {
			return value
		}
		switch extension {
		case ".m4a":
			return "audio/mp4"
		case ".flac":
			return "audio/flac"
		case ".ogg":
			return "audio/ogg"
		case ".wav":
			return "audio/wav"
		case ".mp3":
			return "audio/mpeg"
		}
	}
	detected := http.DetectContentType(data)
	if strings.HasPrefix(detected, "audio/") {
		return detected
	}
	return "application/octet-stream"
}

func truncatedMessage(body []byte, limit int) string {
	value := strings.TrimSpace(string(body))
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}
