package audiochatasr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestClientTranscribeUsesAudioURLChatCompletion(t *testing.T) {
	var received requestPayload
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"真实转写结果"}}]}`))
	}))
	defer server.Close()

	client, err := New(Config{
		BaseURL:    server.URL + "/v1",
		Model:      "/models/Qwen2.5-Omni-7B",
		APIKey:     "secret",
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result, err := client.Transcribe(context.Background(), []byte("RIFF-audio"), "sample.wav")
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if result != "真实转写结果" {
		t.Fatalf("result = %q", result)
	}
	if received.Model != "/models/Qwen2.5-Omni-7B" || len(received.Messages) != 1 {
		t.Fatalf("request identity mismatch: %+v", received)
	}
	parts := received.Messages[0].Content
	if len(parts) != 2 || parts[0].AudioURL == nil || parts[1].Text == "" {
		t.Fatalf("content parts = %+v", parts)
	}
	const prefix = "data:audio/wav;base64,"
	if !strings.HasPrefix(parts[0].AudioURL.URL, prefix) {
		t.Fatalf("audio URL prefix = %q", parts[0].AudioURL.URL)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(parts[0].AudioURL.URL, prefix))
	if err != nil || string(decoded) != "RIFF-audio" {
		t.Fatalf("audio payload mismatch: %q, %v", decoded, err)
	}
}

func TestClientTranscribeRejectsRemoteFailureAndEmptyChoice(t *testing.T) {
	for _, test := range []struct {
		name string
		code int
		body string
		want string
	}{
		{"remote failure", http.StatusBadGateway, `{"error":"upstream failed"}`, "HTTP 502"},
		{"empty choice", http.StatusOK, `{"choices":[]}`, "no choices"},
		{"empty text", http.StatusOK, `{"choices":[{"message":{"content":" "}}]}`, "empty transcription"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(test.code)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			client, err := New(Config{
				BaseURL:    server.URL,
				Model:      "model",
				HTTPClient: server.Client(),
			})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = client.Transcribe(context.Background(), []byte("audio"), "sample.mp3")
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
