package wecom

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/im"
)

func TestWebhookDecryptAcceptsWeComPKCS7Block32(t *testing.T) {
	const (
		corpID = "wx5823bf96d3bd56c7"
		aesKey = "jWmYm7qr5nMoAUwZRjGtBxmz3KA1tkAj3ykkR6q2B2C"
		// Official WeCom encryption-protocol example. Its valid PKCS#7
		// padding length is 30 bytes, which must not be rejected as AES-16.
		encrypted = "RypEvHKD8QQKFhvQ6QleEB4J58tiPdvo+rtK1I9qca6aM/wvqnLSV5zEPeusUiX5L5X/0lWfrf0QADHHhGd3QczcdCUpj911L3vg3W/sYYvuJTs3TUUkSUXxaccAS0qhxchrRYt66wiSpGLYL42aM6A8dTT+6k4aSknmPj48kzJs8qLjvd4Xgpue06DOdnLxAUHzM6+kDZ+HMZfJYuR+LtwGc2hgf5gsijff0ekUNXZiqATP7PF5mZxZ3Izoun1s4zG4LUMnvw2r+KqCKIw+3IQH03v+BCA9nMELNqbSf6tiWSrXJB3LAVGUcallcrw8V2t9EL4EhzJWrQUax5wLVMNS0+rUPA3k22Ncx4XXZS9o0MBH27Bo6BpNelZpS+/uh9KsNlY6bHCmJU9p8g7m3fVKn28H3KDYA5Pl/T8Z1ptDAVe0lXdQ2YoyyH2uyPIGHBZZIs2pDBS8R07+qN+E7Q=="
	)

	adapter, err := NewWebhookAdapter(corpID, "secret", "token", aesKey, 218, "")
	if err != nil {
		t.Fatalf("NewWebhookAdapter() error = %v", err)
	}

	decrypted, err := adapter.decrypt(encrypted)
	if err != nil {
		t.Fatalf("decrypt() error = %v", err)
	}
	if !strings.Contains(string(decrypted), "<Content><![CDATA[hello]]></Content>") {
		t.Fatalf("decrypt() returned unexpected message: %s", decrypted)
	}
}

func TestSplitWeComMarkdownUsesUTF8ByteLimit(t *testing.T) {
	content := strings.Repeat("这是一个用于验证企业微信分条发送的中文段落。\n", 180)
	chunks := splitWeComMarkdown(content)
	if len(chunks) < 2 {
		t.Fatalf("splitWeComMarkdown() returned %d chunk, want multiple", len(chunks))
	}

	var reconstructed strings.Builder
	for i, chunk := range chunks {
		if !utf8.ValidString(chunk) {
			t.Fatalf("chunk %d is not valid UTF-8", i+1)
		}
		if len(chunk) > wecomMarkdownMaxBytes {
			t.Fatalf("chunk %d is %d bytes, limit is %d", i+1, len(chunk), wecomMarkdownMaxBytes)
		}
		prefix := wecomMarkdownPartPrefix(i+1, len(chunks))
		if !strings.HasPrefix(chunk, prefix) {
			t.Fatalf("chunk %d missing prefix %q", i+1, prefix)
		}
		reconstructed.WriteString(strings.TrimPrefix(chunk, prefix))
	}
	if reconstructed.String() != content {
		t.Fatal("split chunks do not reconstruct the original content")
	}
}

func TestWebhookSendReplySplitsLongMarkdown(t *testing.T) {
	var (
		mu       sync.Mutex
		received []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			ToUser   string `json:"touser"`
			MsgType  string `json:"msgtype"`
			Markdown struct {
				Content string `json:"content"`
			} `json:"markdown"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if payload.ToUser != "user-1" || payload.MsgType != "markdown" {
			http.Error(w, "unexpected payload", http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, payload.Markdown.Content)
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"errcode":0,"errmsg":"ok"}`))
	}))
	defer server.Close()

	previousClient := httpClient
	httpClient = server.Client()
	defer func() { httpClient = previousClient }()

	adapter := &WebhookAdapter{
		corpAgentID: 1000001,
		apiBaseURL:  server.URL,
		tokenCache:  "cached-token",
		tokenExpAt:  time.Now().Add(time.Hour),
	}
	content := strings.Repeat("预算执行应当遵循制度要求，并完成必要的审批流程。\n", 180)
	err := adapter.SendReply(
		context.Background(),
		&im.IncomingMessage{UserID: "user-1", ChatType: im.ChatTypeDirect},
		&im.ReplyMessage{Content: content, IsFinal: true},
	)
	if err != nil {
		t.Fatalf("SendReply() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) < 2 {
		t.Fatalf("SendReply() made %d request, want multiple", len(received))
	}
	var reconstructed strings.Builder
	for i, chunk := range received {
		if len(chunk) > wecomMarkdownMaxBytes {
			t.Fatalf("request %d content is %d bytes, limit is %d", i+1, len(chunk), wecomMarkdownMaxBytes)
		}
		reconstructed.WriteString(strings.TrimPrefix(chunk, wecomMarkdownPartPrefix(i+1, len(received))))
	}
	if reconstructed.String() != content {
		t.Fatal("sent chunks do not reconstruct the original reply")
	}
}
