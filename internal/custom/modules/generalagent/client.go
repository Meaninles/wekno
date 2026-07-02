package generalagent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

const defaultHTTPTimeout = 2 * time.Hour

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

func NewClientFromEnv() *Client {
	baseURL := strings.TrimRight(os.Getenv("CUSTOM_GENERAL_AGENT_URL"), "/")
	if baseURL == "" {
		baseURL = defaultSidecarURL
	}
	return NewClient(baseURL)
}

func NewDocumentProcessingClientFromEnv() *Client {
	baseURL := strings.TrimRight(os.Getenv("CUSTOM_DOCUMENT_PROCESSING_AGENT_URL"), "/")
	if baseURL == "" {
		baseURL = defaultDocumentProcessingSidecarURL
	}
	return NewClient(baseURL)
}

func NewClient(baseURL string) *Client {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		baseURL = defaultSidecarURL
	}
	return &Client{
		baseURL: baseURL,
		apiKey:  os.Getenv("CUSTOM_GENERAL_AGENT_API_KEY"),
		httpClient: &http.Client{
			Timeout: envDurationSeconds("CUSTOM_GENERAL_AGENT_HTTP_TIMEOUT_SEC", defaultHTTPTimeout),
		},
	}
}

func envDurationSeconds(name string, fallback time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func (c *Client) ChatStream(ctx context.Context, payload ChatPayload, onEvent func(StreamEvent)) (*ChatResult, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/chat/stream", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("general agent stream failed: status=%d body=%s", resp.StatusCode, string(data))
	}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var result *ChatResult
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var evt StreamEvent
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			return nil, fmt.Errorf("decode general agent stream event: %w", err)
		}
		switch evt.Type {
		case "result":
			var out ChatResult
			if err := json.Unmarshal(evt.Data, &out); err != nil {
				return nil, fmt.Errorf("decode general agent result: %w", err)
			}
			result = &out
		case "error":
			if evt.Message == "" {
				evt.Message = evt.Content
			}
			if evt.Message == "" {
				evt.Message = "general agent failed"
			}
			return nil, errors.New(evt.Message)
		default:
			if onEvent != nil {
				onEvent(evt)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("general agent stream ended without result")
	}
	return result, nil
}

func (c *Client) Download(ctx context.Context, runID, token string) ([]byte, error) {
	url := fmt.Sprintf("%s/v1/runs/%s/files/%s", c.baseURL, runID, token)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("general agent download failed: status=%d body=%s", resp.StatusCode, string(data))
	}
	return io.ReadAll(resp.Body)
}
