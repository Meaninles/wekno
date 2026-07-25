package vlm

import (
	"testing"
	"time"
)

func TestVLMHTTPClientsReuseConnectionPool(t *testing.T) {
	first := newVLMHTTPClient(time.Second)
	second := newVLMHTTPClient(time.Minute)
	if first.Transport != second.Transport {
		t.Fatal("VLM clients must share their connection pool")
	}
}
