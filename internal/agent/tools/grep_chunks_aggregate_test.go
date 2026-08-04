package tools

import (
	"regexp"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAggregateByKnowledge_mergesChunksFromSameDocument(t *testing.T) {
	tool := &GrepChunksTool{}
	compiled := []*regexp.Regexp{regexp.MustCompile(`(?i)sample`)}
	results := []chunkWithTitle{
		{
			Chunk: types.Chunk{
				ID:              "chunk-a",
				KnowledgeID:     "doc-1",
				KnowledgeBaseID: "kb-1",
				Content:         "sample hit one",
			},
			KnowledgeTitle: "report-a.pdf",
		},
		{
			Chunk: types.Chunk{
				ID:              "chunk-b",
				KnowledgeID:     "doc-1",
				KnowledgeBaseID: "kb-1",
				Content:         "sample hit two",
			},
			KnowledgeTitle: "report-a.pdf",
		},
		{
			Chunk: types.Chunk{
				ID:              "chunk-c",
				KnowledgeID:     "doc-2",
				KnowledgeBaseID: "kb-1",
				Content:         "sample other doc",
			},
			KnowledgeTitle: "report-b.pdf",
		},
	}

	aggregated := tool.aggregateByKnowledge(results, []string{"sample"}, compiled)
	if len(aggregated) != 2 {
		t.Fatalf("want 2 documents, got %d", len(aggregated))
	}

	byID := make(map[string]knowledgeAggregation, len(aggregated))
	for _, row := range aggregated {
		byID[row.KnowledgeID] = row
	}
	if byID["doc-1"].ChunkHitCount != 2 {
		t.Fatalf("doc-1 chunk hits = %d, want 2", byID["doc-1"].ChunkHitCount)
	}
	if byID["doc-2"].ChunkHitCount != 1 {
		t.Fatalf("doc-2 chunk hits = %d, want 1", byID["doc-2"].ChunkHitCount)
	}
}

func TestBuildGrepSourceReferencesUsesPhysicalChunkBody(t *testing.T) {
	compiled := []*regexp.Regexp{regexp.MustCompile(`第三十二条`)}
	results := []chunkWithTitle{{
		Chunk: types.Chunk{
			ID: "chunk-32", TenantID: 42, KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1",
			ChunkType: types.ChunkTypeText, ChunkIndex: 32, StartAt: 100, EndAt: 180,
			Content: "第三十一条……\n第三十二条 六种采购方式……\n第三十三条……",
		},
		KnowledgeTitle: "采购管理办法.docx",
	}}
	refs := buildGrepSourceReferences(results, compiled)
	if len(refs) != 1 || refs[0].ID != "chunk-32" || refs[0].EvidenceContent != results[0].Content {
		t.Fatalf("grep source did not retain the exact DB chunk: %#v", refs)
	}
	if refs[0].MatchedContent != "" || refs[0].SourceTenantID != 42 || refs[0].ChunkType != string(types.ChunkTypeText) {
		t.Fatalf("grep source identity/provenance is invalid: %#v", refs[0])
	}
}
