package types

import "testing"

func TestRetrievalBudgetUsesUnifiedMaximum(t *testing.T) {
	if err := (RetrievalBudget{
		CandidateCount: MaxRetrievalTopK,
		FusionCount:    MaxRetrievalTopK,
	}).Validate(); err != nil {
		t.Fatalf("maximum retrieval budget should be valid: %v", err)
	}

	for _, budget := range []RetrievalBudget{
		{CandidateCount: MaxRetrievalTopK + 1},
		{FusionCount: MaxRetrievalTopK + 1},
	} {
		if err := budget.Validate(); err == nil {
			t.Fatalf("retrieval budget above %d should be rejected: %+v", MaxRetrievalTopK, budget)
		}
	}
}

func TestRetrievalConfigClampsLegacyTopKValues(t *testing.T) {
	cfg := &RetrievalConfig{EmbeddingTopK: MaxRetrievalTopK + 1, RerankTopK: MaxRetrievalTopK + 1}
	if got := cfg.GetEffectiveEmbeddingTopK(); got != MaxRetrievalTopK {
		t.Fatalf("GetEffectiveEmbeddingTopK() = %d, want %d", got, MaxRetrievalTopK)
	}
	if got := cfg.GetEffectiveRerankTopK(); got != MaxRetrievalTopK {
		t.Fatalf("GetEffectiveRerankTopK() = %d, want %d", got, MaxRetrievalTopK)
	}
}
