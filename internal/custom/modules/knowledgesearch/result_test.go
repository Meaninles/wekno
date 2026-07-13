package knowledgesearch

import (
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestResultsReturnsOnlySearchDisplayFields(t *testing.T) {
	payload := Results([]*types.Knowledge{{
		ID:                "knowledge-1",
		KnowledgeBaseID:   "kb-1",
		Type:              "file",
		Title:             "guide",
		FileName:          "guide.md",
		FileType:          "md",
		KnowledgeBaseName: "Guides",
		Description:       "large content that must not be returned",
		Metadata:          types.JSON(`{"large":"metadata"}`),
	}})

	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal compact results: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(encoded, &rows); err != nil {
		t.Fatalf("unmarshal compact results: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("expected one result, got %d", len(rows))
	}
	if len(rows[0]) != 7 {
		t.Fatalf("expected seven compact fields, got %d: %v", len(rows[0]), rows[0])
	}
	if _, exists := rows[0]["description"]; exists {
		t.Fatal("description must not be present in a search result")
	}
	if _, exists := rows[0]["metadata"]; exists {
		t.Fatal("metadata must not be present in a search result")
	}
}
