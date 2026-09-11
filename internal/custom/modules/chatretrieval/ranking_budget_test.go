package chatretrieval

import (
	"context"
	"strconv"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestResolveBudgetUsesUnifiedRetrievalMaximum(t *testing.T) {
	configured := types.RetrievalBudget{
		CandidateCount: types.MaxRetrievalTopK + 100,
		FusionCount:    types.MaxRetrievalTopK + 100,
	}

	budget := ResolveBudget(100, 100, configured)
	require.Equal(t, types.MaxRetrievalTopK, budget.Candidates)
	require.Equal(t, types.MaxRetrievalTopK, budget.Fusion)
	require.Equal(t, types.MaxRetrievalTopK, budget.EvidenceCount)

	automatic := ResolveBudget(100, 100)
	require.Equal(t, types.MaxRetrievalTopK, automatic.Candidates)
	require.Equal(t, types.MaxRetrievalTopK, automatic.Fusion)
}

func TestRankDefensivelyCapsDirectBudget(t *testing.T) {
	input := make([]*types.SearchResult, types.MaxRetrievalTopK+25)
	for i := range input {
		input[i] = &types.SearchResult{ID: "id-" + strconv.Itoa(i+1), Content: "content"}
	}

	out, err := Rank(context.Background(), []string{"query"}, input, nil, 0, Budget{
		Fusion:         types.MaxRetrievalTopK + 25,
		EvidenceCount:  types.MaxRetrievalTopK + 25,
		EvidenceTokens: 0,
	})
	require.NoError(t, err)
	require.Len(t, out, types.MaxRetrievalTopK)
}
