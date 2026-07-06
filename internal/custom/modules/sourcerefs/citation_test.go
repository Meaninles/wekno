package sourcerefs

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAssignCitationIDsSeparatesKnowledgeChunksWithinDocument(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "chunk-1",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkIndex:      0,
			ChunkType:       "text",
		},
		{
			ID:              "chunk-2",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkIndex:      1,
			ChunkType:       "text",
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 2 {
		t.Fatalf("sources len = %d, want 2", len(sources))
	}
	if got := refs[0].Metadata[MetadataCitationID]; got != "S1" {
		t.Fatalf("first citation id = %q, want S1", got)
	}
	if got := refs[1].Metadata[MetadataCitationID]; got != "S2" {
		t.Fatalf("second citation id = %q, want S2", got)
	}
	if got := sources[0].KnowledgeID; got != "doc-1" {
		t.Fatalf("source knowledge id = %q, want doc-1", got)
	}
	if got := sources[0].ChunkID; got != "chunk-1" {
		t.Fatalf("first source chunk id = %q, want chunk-1", got)
	}
	if got := sources[0].Granularity; got != "document_fragment" {
		t.Fatalf("first source granularity = %q, want document_fragment", got)
	}
	if got := refs[1].Metadata[MetadataChunkID]; got != "chunk-2" {
		t.Fatalf("second metadata chunk id = %q, want chunk-2", got)
	}
	if catalog := RenderCitationCatalog(refs); !strings.Contains(catalog, `granularity="document_fragment"`) || !strings.Contains(catalog, `chunk_id="chunk-1"`) {
		t.Fatalf("catalog should mark knowledge sources as document fragments with chunk ids, got %s", catalog)
	}
	if attrs := ContextCitationAttrs(refs[0]); !strings.Contains(attrs, `source_granularity="document_fragment"`) || !strings.Contains(attrs, `chunk_id="chunk-1"`) {
		t.Fatalf("context attrs should mark citation as document fragment, got %s", attrs)
	}
}

func TestAssignCitationIDsFallsBackToDocumentWhenNoChunkID(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "doc-1",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "text",
		},
		{
			ID:              "doc-1",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "text",
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(sources))
	}
	if got := refs[1].Metadata[MetadataCitationID]; got != "S1" {
		t.Fatalf("second citation id = %q, want S1", got)
	}
	if got := refs[0].Metadata[MetadataChunkID]; got != "" {
		t.Fatalf("metadata chunk id = %q, want empty", got)
	}
}

func TestAssignCitationIDsUsesDistinctWikiSlug(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "wiki:kb-1:ops/bastion",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "wiki_page",
			Metadata: map[string]string{
				"source_type": SourceTypeWiki,
				"slug":        "ops/bastion",
			},
		},
	}

	sources := AssignCitationIDs(refs)
	if len(sources) != 1 {
		t.Fatalf("sources len = %d, want 1", len(sources))
	}
	if sources[0].Slug != "ops/bastion" {
		t.Fatalf("slug = %q, want ops/bastion", sources[0].Slug)
	}
}
