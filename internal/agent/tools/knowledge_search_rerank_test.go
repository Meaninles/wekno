package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestGetEnrichedPassageUsesRequestIndependentSearchMetadata(t *testing.T) {
	t.Parallel()
	tool := &KnowledgeSearchTool{}
	faqMetadata, err := json.Marshal(types.FAQChunkMetadata{
		StandardQuestion:  "How is a temporary access badge requested?",
		SimilarQuestions:  []string{"What is the visitor badge workflow?"},
		NegativeQuestions: []string{"How do I disable every badge?"},
		Answers:           []string{"Submit the host and visit window through the access portal."},
	})
	if err != nil {
		t.Fatal(err)
	}
	passage := tool.getEnrichedPassage(context.Background(), &types.SearchResult{
		Content:        "Access service FAQ entry.",
		MatchedContent: "Need a short-term badge for a guest",
		ChunkMetadata:  types.JSON(faqMetadata),
	})
	for _, want := range []string{
		"Need a short-term badge for a guest",
		"How is a temporary access badge requested?",
		"What is the visitor badge workflow?",
		"Submit the host and visit window through the access portal.",
	} {
		if !strings.Contains(passage, want) {
			t.Fatalf("enriched passage omitted %q: %s", want, passage)
		}
	}
	if strings.Contains(passage, "disable every badge") {
		t.Fatalf("negative FAQ examples must not become positive rerank evidence: %s", passage)
	}

	documentMetadata, err := json.Marshal(types.DocumentChunkMetadata{
		GeneratedQuestions: []types.GeneratedQuestion{{Question: "Where is the rollback owner recorded?"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	documentPassage := tool.getEnrichedPassage(context.Background(), &types.SearchResult{
		Content:       "Release checklist section.",
		ChunkMetadata: types.JSON(documentMetadata),
	})
	if !strings.Contains(documentPassage, "Where is the rollback owner recorded?") {
		t.Fatalf("generated document question was omitted: %s", documentPassage)
	}
}

func TestApplyModelRerankScoresUsesConfiguredCutoffAndPreservesInput(t *testing.T) {
	tool := &KnowledgeSearchTool{}
	originals := []*searchResultWithMeta{{SearchResult: &types.SearchResult{ID: "a", Score: .01}}, {SearchResult: &types.SearchResult{ID: "b", Score: .02}}}
	out := tool.applyModelRerankScores(originals, []rerank.RankResult{{Index: 0, RelevanceScore: .1}, {Index: 1, RelevanceScore: .9}}, .5)
	if len(out) != 1 || out[0].ID != "b" || out[0].Score != .9 {
		t.Fatalf("wrong relevance filter: %#v", out)
	}
	if originals[1].Score != .02 {
		t.Fatal("reranking mutated retrieval scores")
	}
}
func TestFilterKnowledgeSearchScores(t *testing.T) {
	input := []rerank.RankResult{{Index: 4, RelevanceScore: 1}, {Index: 1, RelevanceScore: .28}, {Index: 1, RelevanceScore: .27}, {Index: 0, RelevanceScore: .18}, {Index: -1, RelevanceScore: 1}}
	if got := filterKnowledgeSearchScores(input, 2, .3); len(got) != 0 {
		t.Fatal(got)
	}
	if got := filterKnowledgeSearchScores(input, 2, 0); len(got) != 2 || got[0].Index != 1 || got[1].Index != 0 {
		t.Fatal(got)
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
