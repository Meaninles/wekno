package rerank

import (
	"context"
	"testing"
)

func TestZhipuRerankerRejectsEmptyQueryAndDocument(t *testing.T) {
	r := &ZhipuReranker{modelName: "rerank", baseURL: "https://example.com/rerank"}
	if _, err := r.Rerank(context.Background(), "   ", []string{"doc"}); err == nil {
		t.Fatalf("expected empty query to be rejected")
	}
	if _, err := r.Rerank(context.Background(), "query", []string{"doc", "   "}); err == nil {
		t.Fatalf("expected empty document to be rejected")
	}
}

func TestZhipuRerankerEmptyDocumentsNoops(t *testing.T) {
	r := &ZhipuReranker{modelName: "rerank", baseURL: "https://example.com/rerank"}
	got, err := r.Rerank(context.Background(), "query", nil)
	if err != nil {
		t.Fatalf("empty documents should not call provider: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len(results) = %d, want 0", len(got))
	}
}
