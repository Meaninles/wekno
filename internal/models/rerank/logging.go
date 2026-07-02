package rerank

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"unicode/utf8"
)

const (
	maxLogDocuments = 3
)

func buildRerankRequestDebug(model, endpoint, query string, documents []string) string {
	docMetrics := make([]string, 0, maxLogDocuments)
	for i, doc := range documents {
		if i >= maxLogDocuments {
			break
		}
		docMetrics = append(docMetrics, fmt.Sprintf("{index:%d,runes:%d,sha256:%s}", i, utf8.RuneCountInString(doc), textDigest(doc)))
	}

	return fmt.Sprintf(
		"rerank request endpoint=%s model=%s query_runes=%d query_sha256=%s documents=%d doc_metrics=%v",
		endpoint,
		model,
		utf8.RuneCountInString(query),
		textDigest(query),
		len(documents),
		docMetrics,
	)
}

func textDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:8])
}
