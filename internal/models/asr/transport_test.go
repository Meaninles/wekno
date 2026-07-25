package asr

import (
	"testing"
	"time"
)

func TestASRHTTPClientsReuseConnectionPool(t *testing.T) {
	first := newASRHTTPClient(time.Second)
	second := newASRHTTPClient(time.Minute)
	if first.Transport != second.Transport {
		t.Fatal("ASR clients must share their connection pool")
	}
}
