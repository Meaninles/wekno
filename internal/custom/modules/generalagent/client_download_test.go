package generalagent

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDownloadWithNameUsesTrustedContentDisposition(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs/run-1/files/token-1" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Disposition", `attachment; filename="replacement.md"; filename*=UTF-8''%E6%9B%BF%E6%8D%A2%E6%96%87%E6%A1%A3.md`)
		_, _ = w.Write([]byte("content"))
	}))
	defer server.Close()

	client := NewClient(server.URL)
	data, fileName, err := client.DownloadWithName(context.Background(), "run-1", "token-1")
	if err != nil {
		t.Fatalf("DownloadWithName() error = %v", err)
	}
	if string(data) != "content" {
		t.Fatalf("data = %q", data)
	}
	if fileName != "替换文档.md" {
		t.Fatalf("fileName = %q, want 替换文档.md", fileName)
	}
}
