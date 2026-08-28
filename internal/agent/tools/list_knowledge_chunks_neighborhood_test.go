package tools

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestMergeChunkNeighborhoodKeepsSparseLogicalOrderAndScope(t *testing.T) {
	target := &types.Chunk{
		ID: "target", KnowledgeID: "doc", ChunkType: types.ChunkTypeText, ChunkIndex: 100,
	}
	got := mergeChunkNeighborhood(target, []*types.Chunk{
		{ID: "after", KnowledgeID: "doc", ChunkType: types.ChunkTypeText, ChunkIndex: 900},
		{ID: "before", KnowledgeID: "doc", ChunkType: types.ChunkTypeText, ChunkIndex: 7},
		{ID: "other-document", KnowledgeID: "other", ChunkType: types.ChunkTypeText, ChunkIndex: 99},
		{ID: "faq", KnowledgeID: "doc", ChunkType: types.ChunkTypeFAQ, ChunkIndex: 101},
		target,
	})
	if len(got) != 3 {
		t.Fatalf("neighborhood size = %d, want 3", len(got))
	}
	if got[0].ID != "before" || got[1].ID != "target" || got[2].ID != "after" {
		t.Fatalf("unexpected neighborhood order: %s, %s, %s", got[0].ID, got[1].ID, got[2].ID)
	}
}
