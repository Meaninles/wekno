package retrievalfence

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func publicationFixture() ([]Scope, ChunkLoader, KnowledgeLoader) {
	return []Scope{{TenantID: 7, KnowledgeBaseID: "kb"}},
		func(_ context.Context, ids []string) ([]*types.Chunk, error) {
			var chunks []*types.Chunk
			for _, id := range ids {
				generation := "visible"
				if id == "draft-1" || id == "draft-2" {
					generation = "next"
				}
				chunks = append(chunks, &types.Chunk{ID: id, TenantID: 7, KnowledgeID: "doc", KnowledgeBaseID: "kb", IsEnabled: true, ProcessingGeneration: generation})
			}
			return chunks, nil
		}, func(context.Context, uint64, []string) ([]*types.Knowledge, error) {
			return []*types.Knowledge{{ID: "doc", TenantID: 7, KnowledgeBaseID: "kb", EnableStatus: "enabled", PublishedGeneration: "visible", ProcessingGeneration: "next"}}, nil
		}
}

func candidate(id string) *types.IndexWithScore {
	return &types.IndexWithScore{ChunkID: id, KnowledgeID: "doc", KnowledgeBaseID: "kb"}
}

func TestRetrieveRefillsOnlyChannelsWhoseBudgetContainsUnpublishedChunks(t *testing.T) {
	scopes, chunks, knowledge := publicationFixture()
	params := []types.RetrieveParams{{TopK: 2, RetrieverType: types.VectorRetrieverType, Query: "unchanged"}, {TopK: 2, RetrieverType: types.KeywordsRetrieverType, Query: "unchanged"}}
	counts := make(map[types.RetrieverType]int)
	fetch := func(_ context.Context, request []types.RetrieveParams) ([]*types.RetrieveResult, error) {
		var results []*types.RetrieveResult
		for _, param := range request {
			require.Equal(t, "unchanged", param.Query)
			require.Equal(t, 2, param.TopK)
			counts[param.RetrieverType]++
			ids := []string{"live-1", "live-2"}
			if param.RetrieverType == types.VectorRetrieverType {
				ids = append([]string{"draft-1", "draft-2"}, ids...)
			}
			result := &types.RetrieveResult{RetrieverType: param.RetrieverType}
			for _, id := range ids {
				if !slices.Contains(param.ExcludeChunkIDs, id) && len(result.Results) < param.TopK {
					result.Results = append(result.Results, candidate(id))
				}
			}
			results = append(results, result)
		}
		return results, nil
	}
	results, err := Retrieve(context.Background(), params, scopes, fetch, chunks, knowledge)
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, result := range results {
		require.Len(t, result.Results, 2)
		require.Equal(t, "live-1", result.Results[0].ChunkID)
		require.Equal(t, "live-2", result.Results[1].ChunkID)
	}
	require.Equal(t, 2, counts[types.VectorRetrieverType])
	require.Equal(t, 1, counts[types.KeywordsRetrieverType])
	require.Empty(t, params[0].ExcludeChunkIDs, "caller parameters must stay immutable")
}

func TestRetrieveStopsOnExhaustionAndRejectsNoncompliantBackends(t *testing.T) {
	for _, topK := range []int{1, 2} {
		t.Run(fmt.Sprint(topK), func(t *testing.T) {
			scopes, chunks, knowledge := publicationFixture()
			calls := 0
			fetch := func(context.Context, []types.RetrieveParams) ([]*types.RetrieveResult, error) {
				calls++
				return []*types.RetrieveResult{{RetrieverType: types.VectorRetrieverType, Results: []*types.IndexWithScore{candidate("draft-1")}}}, nil
			}
			result, err := Retrieve(context.Background(), []types.RetrieveParams{{TopK: topK, RetrieverType: types.VectorRetrieverType}}, scopes, fetch, chunks, knowledge)
			if topK == 1 {
				require.ErrorContains(t, err, "did not honor")
				require.Equal(t, 2, calls)
			} else {
				require.NoError(t, err)
				require.Len(t, result, 1)
				require.Empty(t, result[0].Results)
				require.Equal(t, 1, calls)
			}
		})
	}
}

func TestRetrieveCancellationAndVisibilityFailureNeverReturnPartialEvidence(t *testing.T) {
	scopes, chunks, knowledge := publicationFixture()
	ctx, cancel := context.WithCancel(context.Background())
	fetch := func(context.Context, []types.RetrieveParams) ([]*types.RetrieveResult, error) {
		cancel()
		return []*types.RetrieveResult{{RetrieverType: types.VectorRetrieverType, Results: []*types.IndexWithScore{candidate("draft-1")}}}, nil
	}
	result, err := Retrieve(ctx, []types.RetrieveParams{{TopK: 1, RetrieverType: types.VectorRetrieverType}}, scopes, fetch, chunks, knowledge)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, result)
	_, err = Retrieve(context.Background(), []types.RetrieveParams{{TopK: 1}}, scopes, fetch,
		func(context.Context, []string) ([]*types.Chunk, error) {
			return nil, errors.New("database unavailable")
		}, knowledge)
	require.ErrorContains(t, err, "database unavailable")
}
