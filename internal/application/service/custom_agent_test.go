package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	require "github.com/stretchr/testify/require"
)

func TestRefreshBuiltinAgentMetadataUsesCurrentRegistry(t *testing.T) {
	originalRegistry := types.BuiltinAgentRegistry
	types.BuiltinAgentRegistry = map[string]func(uint64) *types.CustomAgent{
		types.BuiltinGeneralAgentID: func(tenantID uint64) *types.CustomAgent {
			return &types.CustomAgent{
				ID:          types.BuiltinGeneralAgentID,
				Name:        "通用智能体",
				Description: "通用智能体最新描述",
				Avatar:      "🧠",
				IsBuiltin:   true,
				TenantID:    tenantID,
				Config: types.CustomAgentConfig{
					AgentMode:      types.AgentModeSmartReasoning,
					AgentType:      types.AgentTypeGeneralAgent,
					SystemPrompt:   "最新系统提示词",
					SystemPromptID: "general_agent",
				},
			}
		},
	}
	t.Cleanup(func() {
		types.BuiltinAgentRegistry = originalRegistry
	})

	stale := &types.CustomAgent{
		ID:          types.BuiltinGeneralAgentID,
		Name:        "旧名称",
		Description: "旧描述",
		Avatar:      "old",
		IsBuiltin:   true,
		TenantID:    10002,
		Config: types.CustomAgentConfig{
			AgentMode:      types.AgentModeSmartReasoning,
			AgentType:      types.AgentTypeGeneralAgent,
			SystemPrompt:   "旧系统提示词",
			SystemPromptID: "legacy_general_agent",
		},
	}

	refreshed := refreshBuiltinAgentMetadata(context.Background(), stale, 10002)

	require.NotNil(t, refreshed)
	assert.Equal(t, "通用智能体", refreshed.Name)
	assert.Equal(t, "通用智能体最新描述", refreshed.Description)
	assert.Equal(t, "🧠", refreshed.Avatar)
	assert.Equal(t, types.AgentTypeGeneralAgent, refreshed.Config.AgentType)
	assert.Equal(t, "general_agent", refreshed.Config.SystemPromptID)
	assert.Equal(t, "最新系统提示词", refreshed.Config.SystemPrompt)
	assert.Equal(t, "旧名称", stale.Name)
	assert.Equal(t, "旧系统提示词", stale.Config.SystemPrompt)
}

func TestNormalizeCustomAgentDataSourceConfig_GeneralAgentPreservesDBSourcesAndTools(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentType:     types.AgentTypeGeneralAgent,
			DBDataSources: []string{" source-a ", "", "source-b", "source-a"},
			AllowedTools: []string{
				tools.ToolKnowledgeSearch,
				tools.ToolDBCatalog,
				tools.ToolDBSchema,
				tools.ToolDBQuery,
			},
		},
	}

	require.NoError(t, normalizeCustomAgentDataSourceConfig(agent))

	assert.Equal(t, []string{"source-a", "source-b"}, agent.Config.DBDataSources)
	assert.Equal(t, []string{
		tools.ToolKnowledgeSearch,
		tools.ToolDBCatalog,
		tools.ToolDBSchema,
		tools.ToolDBQuery,
	}, agent.Config.AllowedTools)
}

func TestNormalizeCustomAgentDataSourceConfig_LegacyDataAnalysisStillRequiresSource(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentType:     types.AgentTypeDataAnalysis,
			DBDataSources: []string{"", "   "},
			AllowedTools:  []string{tools.ToolDBCatalog, tools.ToolDBSchema, tools.ToolDBQuery},
		},
	}

	assert.ErrorIs(t, normalizeCustomAgentDataSourceConfig(agent), ErrAgentDatabaseSourcesRequired)
}

func TestNormalizeCustomAgentDataSourceConfig_NonDBAgentStripsDBSourcesAndTools(t *testing.T) {
	agent := &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentType:     types.AgentTypeRAGQA,
			DBDataSources: []string{"source-a"},
			AllowedTools: []string{
				tools.ToolKnowledgeSearch,
				tools.ToolDBCatalog,
				tools.ToolDBSchema,
				tools.ToolWebSearch,
			},
		},
	}

	require.NoError(t, normalizeCustomAgentDataSourceConfig(agent))

	assert.Empty(t, agent.Config.DBDataSources)
	assert.Equal(t, []string{tools.ToolKnowledgeSearch, tools.ToolWebSearch}, agent.Config.AllowedTools)
}
