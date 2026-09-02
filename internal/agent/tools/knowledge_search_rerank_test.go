package tools

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestApplyModelRerankScores_usesBoundedRankRecallFloor(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{
		config: &config.Config{
			Conversation: &config.ConversationConfig{RerankThreshold: 0.3},
		},
	}
	originals := []*searchResultWithMeta{
		{
			SearchResult:      &types.SearchResult{ID: "faq-1", Content: "Q: WeKnora", Score: 0.011},
			KnowledgeBaseType: types.KnowledgeBaseTypeFAQ,
		},
		{
			SearchResult: &types.SearchResult{ID: "doc-1", Content: "swimming club", Score: 0.02},
		},
		{SearchResult: &types.SearchResult{ID: "doc-2", Content: "supporting passage", Score: 0.015}},
		{SearchResult: &types.SearchResult{ID: "doc-3", Content: "irrelevant tail", Score: 0.01}},
	}
	rankResults := []rerank.RankResult{
		{Index: 0, RelevanceScore: 0.05},
		{Index: 1, RelevanceScore: 0.9},
		{Index: 2, RelevanceScore: 0.2},
		{Index: 3, RelevanceScore: 0.01},
	}
	out := tool.applyModelRerankScores(originals, rankResults, 0.3)
	if len(out) != 3 {
		t.Fatalf("expected the bounded top-three recall floor, got %#v", out)
	}
	seen := map[string]bool{}
	for _, result := range out {
		seen[result.ID] = true
	}
	if !seen["doc-1"] || !seen["doc-2"] || !seen["faq-1"] || seen["doc-3"] {
		t.Fatalf("unexpected recall-floor membership: %#v", seen)
	}
	if out[0].Score <= 0.02 {
		t.Fatalf("composite score should exceed raw retrieval score, got %.4f", out[0].Score)
	}
}

func TestRetainKnowledgeSearchRecallFloorIsQueryIndependentAndValidatesIndices(t *testing.T) {
	t.Parallel()
	input := []rerank.RankResult{
		{Index: 4, RelevanceScore: 1},
		{Index: 1, RelevanceScore: 0.28},
		{Index: 1, RelevanceScore: 0.27},
		{Index: 0, RelevanceScore: 0.18},
		{Index: 2, RelevanceScore: 0.08},
		{Index: -1, RelevanceScore: 1},
		{Index: 3, RelevanceScore: 0.01},
	}
	got := retainKnowledgeSearchRecallFloor(input, 4, 0.3)
	if len(got) != 3 {
		t.Fatalf("got %d results, want bounded top-three: %#v", len(got), got)
	}
	for i, want := range []int{1, 0, 2} {
		if got[i].Index != want {
			t.Fatalf("rank %d index=%d, want %d", i, got[i].Index, want)
		}
	}
}

func TestRerankThreshold_default(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{}
	if got := tool.rerankThreshold(); got != 0.3 {
		t.Fatalf("default threshold = %v, want 0.3", got)
	}
}

func TestRerankThreshold_agentZeroDisablesAbsoluteCutoff(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{
		agentConfig: &types.AgentConfig{RerankThreshold: 0},
		config: &config.Config{
			Conversation: &config.ConversationConfig{RerankThreshold: 0.7},
		},
	}
	if got := tool.rerankThreshold(); got != 0 {
		t.Fatalf("agent threshold = %v, want explicit zero", got)
	}
}
