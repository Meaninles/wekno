package chat

import (
	"context"
	"encoding/json"
	"github.com/stretchr/testify/require"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestGenerationControlsActualHTTP(t *testing.T) {
	t.Setenv("SSRF_WHITELIST", "127.0.0.1")
	var sent map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&sent))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fixture","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer server.Close()
	c, err := NewRemoteAPIChat(&ChatConfig{ModelName: "route", Provider: "generic", BaseURL: server.URL + "/v1", APIKey: "fixture", ExtraConfig: map[string]string{"thinking_control": "thinking_type", "reasoning_effort": "xhigh"}})
	require.NoError(t, err)
	for _, enabled := range []bool{false, true} {
		_, err = c.Chat(context.Background(), []Message{{Role: "user", Content: "fixture"}}, &ChatOptions{Temperature: 0, Thinking: &enabled})
		require.NoError(t, err)
		require.Contains(t, sent, "temperature")
		require.Equal(t, 0.0, sent["temperature"])
		if enabled {
			require.Equal(t, "xhigh", sent["reasoning_effort"])
		} else {
			require.NotContains(t, sent, "reasoning_effort")
		}
	}
}

func TestAnthropicGenerationControls(t *testing.T) {
	c := &AnthropicChat{reasoningEffort: "xhigh"}
	for _, enabled := range []bool{false, true} {
		body := c.buildRequest([]Message{{Role: "user", Content: "fixture"}}, &ChatOptions{Temperature: 0, Thinking: &enabled})
		if enabled {
			require.Nil(t, body.Temperature)
			require.Equal(t, "adaptive", body.Thinking["type"])
			require.Equal(t, "xhigh", body.OutputConfig["effort"])
		} else {
			require.NotNil(t, body.Temperature)
			require.Equal(t, 0.0, *body.Temperature)
			require.Equal(t, "disabled", body.Thinking["type"])
			require.Empty(t, body.OutputConfig)
		}
	}
}
