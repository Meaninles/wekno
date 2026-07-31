package retrievalfence

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestFilterKeepsOnlyAuthorizedCoreReadyCurrentGeneration(t *testing.T) {
	results := []*types.RetrieveResult{{Results: []*types.IndexWithScore{
		{ChunkID: "valid", KnowledgeID: "knowledge-current", KnowledgeBaseID: "kb-1"},
		{ChunkID: "stale", KnowledgeID: "knowledge-current", KnowledgeBaseID: "kb-1"},
		{ChunkID: "foreign", KnowledgeID: "knowledge-foreign", KnowledgeBaseID: "kb-2"},
		{ChunkID: "not-ready", KnowledgeID: "knowledge-waiting", KnowledgeBaseID: "kb-1"},
	}}}
	chunks := []*types.Chunk{
		{ID: "valid", TenantID: 7, KnowledgeID: "knowledge-current", KnowledgeBaseID: "kb-1", ProcessingGeneration: "gen-2", IsEnabled: true},
		{ID: "stale", TenantID: 7, KnowledgeID: "knowledge-current", KnowledgeBaseID: "kb-1", ProcessingGeneration: "gen-1", IsEnabled: true},
		{ID: "foreign", TenantID: 8, KnowledgeID: "knowledge-foreign", KnowledgeBaseID: "kb-2", ProcessingGeneration: "gen-x", IsEnabled: true},
		{ID: "not-ready", TenantID: 7, KnowledgeID: "knowledge-waiting", KnowledgeBaseID: "kb-1", ProcessingGeneration: "gen-3", IsEnabled: true},
	}
	knowledge := map[uint64][]*types.Knowledge{
		7: {
			{ID: "knowledge-current", TenantID: 7, KnowledgeBaseID: "kb-1", ProcessingGeneration: "gen-2", EnableStatus: "enabled", CoreStatus: types.CoreStatusReady},
			{ID: "knowledge-waiting", TenantID: 7, KnowledgeBaseID: "kb-1", ProcessingGeneration: "gen-3", EnableStatus: "enabled", CoreStatus: types.CoreStatusProcessing},
		},
	}
	filtered, err := Filter(
		context.Background(), results, []Scope{{TenantID: 7, KnowledgeBaseID: "kb-1"}},
		func(context.Context, []string) ([]*types.Chunk, error) { return chunks, nil },
		func(_ context.Context, tenant uint64, _ []string) ([]*types.Knowledge, error) {
			return knowledge[tenant], nil
		},
	)
	require.NoError(t, err)
	require.Len(t, filtered, 1)
	require.Len(t, filtered[0].Results, 1)
	require.Equal(t, "valid", filtered[0].Results[0].ChunkID)
}
