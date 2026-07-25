package chatretrieval

import (
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestGraphChunkBudgetIsBounded(t *testing.T) {
	tests := []struct {
		topK int
		want int
	}{
		{topK: 0, want: 8},
		{topK: 4, want: 4},
		{topK: 30, want: 7},
		{topK: 100, want: 8},
	}
	for _, test := range tests {
		if got := GraphChunkBudget(test.topK); got != test.want {
			t.Fatalf("GraphChunkBudget(%d) = %d, want %d", test.topK, got, test.want)
		}
	}
}

func TestGraphSupplementScoreNeverOutranksPrimary(t *testing.T) {
	primary := []*types.SearchResult{
		{Score: 0.016},
		{Score: 0.009},
	}
	got := GraphSupplementScore(primary)
	if got <= 0 || got >= 0.009 {
		t.Fatalf("GraphSupplementScore() = %f, want 0 < score < 0.009", got)
	}
	if got := GraphSupplementScore(nil); got != 1 {
		t.Fatalf("GraphSupplementScore(nil) = %f, want 1", got)
	}
}

func TestRankGraphNodesPrefersExactAndSpecificMatches(t *testing.T) {
	nodes := []*types.GraphNode{
		{Name: "评审会议"},
		{Name: "会议"},
		{Name: "年度生产会议"},
	}
	got := RankGraphNodes(nodes, []string{"会议", "评审会议"})
	if got[0].Name != "评审会议" {
		t.Fatalf("first node = %q, want exact specific match", got[0].Name)
	}
	if got[1].Name != "会议" {
		t.Fatalf("second node = %q, want exact generic match", got[1].Name)
	}
}

func TestSortSearchResultsIsGloballyDeterministic(t *testing.T) {
	results := []*types.SearchResult{
		{ID: "graph", KnowledgeID: "b", Score: 0.004, MatchType: types.MatchTypeGraph},
		{ID: "vector-b", KnowledgeID: "b", ChunkIndex: 2, Score: 0.016, MatchType: types.MatchTypeEmbedding},
		{ID: "vector-a", KnowledgeID: "a", ChunkIndex: 1, Score: 0.016, MatchType: types.MatchTypeEmbedding},
	}
	SortSearchResults(results)
	got := []string{results[0].ID, results[1].ID, results[2].ID}
	want := []string{"vector-a", "vector-b", "graph"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sorted IDs = %v, want %v", got, want)
		}
	}
}

func TestPreserveBothForPartialOverlapKeepsRawEvidenceBesideSummary(t *testing.T) {
	summary := &types.SearchResult{ChunkType: types.ChunkTypeSummary}
	raw := &types.SearchResult{ChunkType: types.ChunkTypeText}
	if !PreserveBothForPartialOverlap(summary, raw) {
		t.Fatal("generated summary must not evict raw source evidence")
	}
	if !PreserveBothForPartialOverlap(raw, summary) {
		t.Fatal("policy must be symmetric")
	}
	if PreserveBothForPartialOverlap(
		raw,
		&types.SearchResult{ChunkType: types.ChunkTypeText},
	) {
		t.Fatal("two raw chunks should still use normal overlap deduplication")
	}
}

func TestSelectRerankModelIDHonorsConfigAndActiveDefault(t *testing.T) {
	now := time.Now()
	models := []*types.Model{
		{
			ID:        "inactive",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusDownloadFailed,
			CreatedAt: now.Add(-time.Hour),
		},
		{
			ID:        "old-active",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusActive,
			CreatedAt: now.Add(-2 * time.Hour),
		},
		{
			ID:        "default-active",
			Type:      types.ModelTypeRerank,
			Status:    types.ModelStatusActive,
			IsDefault: true,
			CreatedAt: now,
		},
	}
	if got := SelectRerankModelID("configured", models); got != "configured" {
		t.Fatalf("configured selection = %q", got)
	}
	if got := SelectRerankModelID("", models); got != "default-active" {
		t.Fatalf("fallback selection = %q, want default-active", got)
	}
}
