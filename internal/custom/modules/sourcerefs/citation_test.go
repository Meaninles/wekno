package sourcerefs

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestAssignCitationIDsGroupsKnowledgeChunksByDocument(t *testing.T) {
	refs := []*types.SearchResult{
		{
			ID:              "chunk-1",
			KnowledgeID:     "doc-1",
			KnowledgeBaseID: "kb-1",
			KnowledgeTitle:  "堡垒机",
			ChunkType:       "text",
		},
		{
			ID:              "chunk-2",
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
	if got := refs[0].Metadata[MetadataCitationID]; got != "S1" {
		t.Fatalf("first citation id = %q, want S1", got)
	}
	if got := refs[1].Metadata[MetadataCitationID]; got != "S1" {
		t.Fatalf("second citation id = %q, want S1", got)
	}
	if got := sources[0].KnowledgeID; got != "doc-1" {
		t.Fatalf("source knowledge id = %q, want doc-1", got)
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
