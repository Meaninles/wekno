package tools

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestRenderKnowledgeSearchExactEvidenceSplitsAggregateParent(t *testing.T) {
	result := &searchResultWithMeta{SearchResult: &types.SearchResult{
		ID: "child-2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType: string(types.ChunkTypeText), ParentChunkID: "parent-1",
		Content: "第一段第二段第三段", SubChunkID: []string{"child-2"},
	}}
	refs := []*types.SearchResult{
		{ID: "child-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ParentChunkID: "parent-1", ChunkType: string(types.ChunkTypeText), EvidenceContent: "第一段"},
		{ID: "child-2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ParentChunkID: "parent-1", ChunkType: string(types.ChunkTypeText), EvidenceContent: "第二段"},
		{ID: "child-3", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ParentChunkID: "parent-1", ChunkType: string(types.ChunkTypeText), EvidenceContent: "第三段"},
	}
	output := renderKnowledgeSearchExactEvidence(result, refs)
	for _, id := range []string{"child-1", "child-2", "child-3"} {
		if !strings.Contains(output, `[EXACT_FRAGMENT chunk_id="`+id+`"]`) {
			t.Fatalf("exact child %s is not model-visible: %s", id, output)
		}
	}
	if strings.Contains(output, result.Content) {
		t.Fatalf("aggregate parent survived exact evidence rendering: %s", output)
	}
}

func TestRenderKnowledgeSearchExactEvidenceMapsSummaryToExactParent(t *testing.T) {
	result := &searchResultWithMeta{SearchResult: &types.SearchResult{
		ID: "summary-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
		ChunkType: string(types.ChunkTypeSummary), ParentChunkID: "text-1",
		Content: "generated summary must not be used as evidence",
	}}
	refs := []*types.SearchResult{
		{ID: "other", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), EvidenceContent: "other text"},
		{ID: "text-1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1", ChunkType: string(types.ChunkTypeText), EvidenceContent: "authoritative parent text"},
	}

	output := renderKnowledgeSearchExactEvidence(result, refs)
	if !strings.Contains(output, `[EXACT_FRAGMENT chunk_id="text-1"]`) ||
		!strings.Contains(output, "authoritative parent text") {
		t.Fatalf("summary parent evidence is not model-visible: %s", output)
	}
	if strings.Contains(output, "generated summary") || strings.Contains(output, "other text") {
		t.Fatalf("non-parent text leaked into exact evidence: %s", output)
	}
}
