package wikiqueue

import (
	"errors"
	"strings"
)

const ingestGenerationSeparator = ":"

// IngestDedupKey isolates durable Wiki ingest work by document processing
// generation. A late producer from an older parse can therefore coexist with
// (and be rejected independently from) the authoritative generation instead
// of replacing it during batch de-duplication.
func IngestDedupKey(knowledgeID, processingGeneration string) (string, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	processingGeneration = strings.TrimSpace(processingGeneration)
	if knowledgeID == "" || processingGeneration == "" {
		return "", errors.New("wiki ingest dedup key requires knowledge ID and processing generation")
	}
	if strings.Contains(knowledgeID, ingestGenerationSeparator) {
		return "", errors.New("wiki ingest knowledge ID contains reserved separator")
	}
	return knowledgeID + ingestGenerationSeparator + processingGeneration, nil
}

// IngestDedupPrefix identifies every generation-scoped ingest row for one
// document. Callers use it to scrub old generations on cancel/reparse/delete
// and to prove queue liveness without knowing the current generation.
func IngestDedupPrefix(knowledgeID string) (string, error) {
	knowledgeID = strings.TrimSpace(knowledgeID)
	if knowledgeID == "" {
		return "", errors.New("wiki ingest dedup prefix requires knowledge ID")
	}
	if strings.Contains(knowledgeID, ingestGenerationSeparator) {
		return "", errors.New("wiki ingest knowledge ID contains reserved separator")
	}
	return knowledgeID + ingestGenerationSeparator, nil
}
