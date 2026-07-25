package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestFilterSeenChunkHonorsBudgetAndSkipsPrimaryResults(t *testing.T) {
	graph := &types.GraphData{
		Node: []*types.GraphNode{
			{Name: "exact", Chunks: []string{"seen", "graph-1", "graph-2"}},
			{Name: "related", Chunks: []string{"graph-3"}},
		},
	}
	primary := []*types.SearchResult{{ID: "seen"}}
	got := filterSeenChunk(context.Background(), graph, primary, 2)
	if len(got) != 2 || got[0] != "graph-1" || got[1] != "graph-2" {
		t.Fatalf("filterSeenChunk() = %v, want [graph-1 graph-2]", got)
	}
}

func TestChunk2SearchResultUsesSupplementScore(t *testing.T) {
	chunk := &types.Chunk{
		ID:              "chunk",
		KnowledgeID:     "knowledge",
		KnowledgeBaseID: "kb",
		Content:         "content",
		ChunkIndex:      3,
		ChunkType:       types.ChunkTypeText,
	}
	knowledge := &types.Knowledge{
		ID:              "knowledge",
		KnowledgeBaseID: "kb",
		Title:           "title",
	}
	got := chunk2SearchResult(chunk, knowledge, 0.004)
	if got.Score != 0.004 {
		t.Fatalf("Score = %f, want 0.004", got.Score)
	}
	if got.MatchType != types.MatchTypeGraph {
		t.Fatalf("MatchType = %v, want graph", got.MatchType)
	}
}
