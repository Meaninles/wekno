package rerank

import (
	"context"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/logger"
)

// debugReranker wraps a Reranker with LLM debug logging.
type debugReranker struct {
	inner Reranker
}

func (d *debugReranker) Rerank(ctx context.Context, query string, documents []string) ([]RankResult, error) {
	start := time.Now()
	result, err := d.inner.Rerank(ctx, query, documents)
	logRerankDebug(ctx, d.inner.GetModelName(), query, documents, result, err, time.Since(start))
	return result, err
}

func (d *debugReranker) GetModelName() string { return d.inner.GetModelName() }
func (d *debugReranker) GetModelID() string   { return d.inner.GetModelID() }

func logRerankDebug(ctx context.Context, model string, query string, documents []string, results []RankResult, callErr error, dur time.Duration) {
	if !logger.LLMDebugEnabled() {
		return
	}

	record := &logger.LLMCallRecord{
		CallType: "Rerank",
		Model:    model,
		Duration: dur,
	}

	// Query section
	record.Sections = append(record.Sections, logger.RecordSection{
		Title:   "Query",
		Content: fmt.Sprintf("runes=%d sha256=%s", utf8.RuneCountInString(query), textDigest(query)),
	})

	// Documents section
	var docBuf strings.Builder
	docBuf.WriteString(fmt.Sprintf("count=%d\n", len(documents)))
	for i, doc := range documents {
		docBuf.WriteString(fmt.Sprintf("[%d] runes=%d sha256=%s\n", i, utf8.RuneCountInString(doc), textDigest(doc)))
	}
	record.Sections = append(record.Sections, logger.RecordSection{Title: "Documents", Content: docBuf.String()})

	// Results section
	if results != nil {
		var resBuf strings.Builder
		resBuf.WriteString(fmt.Sprintf("count=%d\n", len(results)))
		for _, r := range results {
			resBuf.WriteString(fmt.Sprintf(
				"  [%d] score=%.6f doc_runes=%d doc_sha256=%s\n",
				r.Index,
				r.RelevanceScore,
				utf8.RuneCountInString(r.Document.Text),
				textDigest(r.Document.Text),
			))
		}
		record.Sections = append(record.Sections, logger.RecordSection{Title: "Results", Content: resBuf.String()})
	}

	if callErr != nil {
		record.Error = callErr.Error()
	}
	logger.LLMDebugLog(ctx, record)
}
