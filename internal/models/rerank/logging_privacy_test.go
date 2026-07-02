package rerank

import (
	"strings"
	"testing"
)

func TestBuildRerankRequestDebugDoesNotLeakText(t *testing.T) {
	query := "secret query with private customer name"
	doc := "confidential document body should not be logged"

	got := buildRerankRequestDebug("model", "https://example.test/rerank", query, []string{doc})
	if strings.Contains(got, "secret query") || strings.Contains(got, "confidential document") {
		t.Fatalf("debug log leaked raw text: %s", got)
	}
	if !strings.Contains(got, "query_sha256=") || !strings.Contains(got, "doc_metrics=") {
		t.Fatalf("debug log should retain non-sensitive metrics, got: %s", got)
	}
}

func TestLangfuseRerankSummariesDoNotLeakText(t *testing.T) {
	docs := []string{"private doc text"}

	metrics := documentMetrics(docs, 1)
	if got := metrics[0]; got["sha256"] == "" || got["length"] == 0 {
		t.Fatalf("document metrics missing hash/length: %#v", got)
	}
	if _, ok := metrics[0]["preview"]; ok {
		t.Fatalf("document metrics must not include raw preview: %#v", metrics[0])
	}

	results := summarizeResults([]RankResult{{Index: 0, RelevanceScore: 0.9}}, docs, 1)
	if _, ok := results[0]["preview"]; ok {
		t.Fatalf("rerank result summary must not include raw preview: %#v", results[0])
	}
	if results[0]["document_sha256"] == "" || results[0]["document_length"] == 0 {
		t.Fatalf("rerank result summary missing hash/length: %#v", results[0])
	}
}
