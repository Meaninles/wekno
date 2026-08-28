package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestMergeOverlappingChunksPreservesDistinctParentEvidenceWindows(t *testing.T) {
	p := &PluginMerge{}
	first := &types.SearchResult{
		ID: "child-a", ParentChunkID: "parent-a", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeText),
		Content: "parent A", StartAt: 0, EndAt: 100, Score: 0.9,
	}
	second := &types.SearchResult{
		ID: "child-b", ParentChunkID: "parent-b", KnowledgeID: "doc", ChunkType: string(types.ChunkTypeText),
		Content: "parent B", StartAt: 90, EndAt: 200, Score: 0.8,
	}
	got := p.mergeOverlappingChunks(context.Background(), "doc", []*types.SearchResult{first, second})
	if len(got) != 2 || got[0].ParentChunkID == got[1].ParentChunkID {
		t.Fatalf("distinct evidence windows were collapsed: %#v", got)
	}
}
