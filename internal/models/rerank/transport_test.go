package rerank

import (
	"testing"
	"time"
)

func TestRerankHTTPClientsReuseConnectionPool(t *testing.T) {
	first := newRerankHTTPClient(time.Second)
	second := newRerankHTTPClient(time.Minute)
	if first.Transport != second.Transport {
		t.Fatal("rerank clients must share their connection pool")
	}
	if first.Timeout == second.Timeout {
		t.Fatal("per-client timeouts must remain independent")
	}
}
