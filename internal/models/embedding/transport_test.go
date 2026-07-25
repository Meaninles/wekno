package embedding

import (
	"testing"
	"time"
)

func TestEmbeddingHTTPClientsReuseConnectionPool(t *testing.T) {
	short := newEmbeddingHTTPClient(time.Second)
	long := newEmbeddingHTTPClient(time.Minute)

	if short == long {
		t.Fatal("callers need independent client timeouts")
	}
	if short.Transport != long.Transport {
		t.Fatal("embedding clients must reuse one connection pool")
	}
	if short.Timeout != time.Second || long.Timeout != time.Minute {
		t.Fatalf("client timeouts were not kept independent: %s and %s", short.Timeout, long.Timeout)
	}
}
