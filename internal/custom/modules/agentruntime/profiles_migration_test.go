package agentruntime

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestPostgresProfileCutoverPreservesDistinctCapabilities(t *testing.T) {
	db := testsupport.Postgres(t, &types.CustomAgent{})
	profiles := []string{types.AgentTypeKnowledgeQA, types.AgentTypeWikiQA,
		types.AgentTypeHybridRAGWiki, types.AgentTypeDataAnalysis, types.AgentTypeTableAnalysis,
		types.AgentTypeDocumentProcessingAgent, types.AgentTypeGeneralAgent,
		types.AgentTypeKnowledgeBaseManager, types.AgentTypeCustom}
	for _, profile := range profiles {
		row := types.CustomAgent{ID: profile, Name: profile, TenantID: 7,
			Config: types.CustomAgentConfig{AgentType: profile, MaxIterations: 3,
				SystemPrompt: "Retain this profile's behavior", KnowledgeBases: []string{"kb-1"},
				DBDataSources: []string{"db-1"}, MCPServices: []string{"mcp-1"}}}
		require.NoError(t, db.Create(&row).Error)
	}
	for range 2 {
		require.NoError(t, migrateAgentProfiles(context.Background(), db))
	}
	var rows []types.CustomAgent
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, len(profiles))
	for _, row := range rows {
		require.Equal(t, row.ID, row.Config.AgentType)
		require.Equal(t, row.ID, row.Name)
		require.Equal(t, types.AgentIterationBudget(row.ID), row.Config.MaxIterations)
		require.Equal(t, types.AgentModeUnified, row.Config.AgentMode)
		require.Equal(t, "Retain this profile's behavior", row.Config.SystemPrompt)
		require.Equal(t, []string{"kb-1"}, row.Config.KnowledgeBases)
		require.Equal(t, []string{"db-1"}, row.Config.DBDataSources)
		require.Equal(t, []string{"mcp-1"}, row.Config.MCPServices)
	}
}
