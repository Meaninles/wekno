package builtinagentdefaults

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMergeResetConfigPreservesRuntimeBindings(t *testing.T) {
	disabled := false
	defaultConfig := types.CustomAgentConfig{
		AgentType:           types.AgentTypeGeneralAgent,
		ModelID:             "default-model",
		RerankModelID:       "default-rerank",
		WebSearchProviderID: "default-provider",
		Thinking:            &disabled,
		DBDataSources:       []string{},
		MCPSelectionMode:    "none",
	}
	currentConfig := types.CustomAgentConfig{
		ModelID:       "current-model",
		RerankModelID: "current-rerank",

		VLMModelID:                 "current-vlm",
		ASRModelID:                 "current-asr",
		ImageStorageProvider:       "cos",
		DBDataSources:              []string{"db-1", "db-2"},
		MCPSelectionMode:           "selected",
		MCPServices:                []string{"mcp-1"},
		MCPAuthWaitTimeout:         123,
		WebSearchProviderID:        "current-provider",
		SelectedProfessionalSkills: []string{"tenant-skill"},
	}

	got := mergeResetConfig(defaultConfig, currentConfig)

	if got.Thinking == nil || *got.Thinking != false {
		t.Fatalf("thinking should follow the builtin default, got %#v", got.Thinking)
	}
	if got.ModelID != "current-model" || got.RerankModelID != "current-rerank" {
		t.Fatalf("model bindings were not preserved: %#v", got)
	}
	if got.VLMModelID != "current-vlm" || got.ASRModelID != "current-asr" {
		t.Fatalf("auxiliary model bindings were not preserved: %#v", got)
	}
	if got.ImageStorageProvider != "cos" {
		t.Fatalf("image storage provider should be preserved, got %q", got.ImageStorageProvider)
	}
	if len(got.DBDataSources) != 2 || got.DBDataSources[0] != "db-1" || got.DBDataSources[1] != "db-2" {
		t.Fatalf("data-analysis data sources were not preserved: %#v", got.DBDataSources)
	}
	if got.MCPSelectionMode != "selected" || len(got.MCPServices) != 1 || got.MCPServices[0] != "mcp-1" || got.MCPAuthWaitTimeout != 123 {
		t.Fatalf("MCP settings were not preserved: %#v", got)
	}
	if got.WebSearchProviderID != "default-provider" {
		t.Fatalf("web search provider should come from defaults, got %q", got.WebSearchProviderID)
	}
	if len(got.SelectedProfessionalSkills) != 0 {
		t.Fatalf("tenant skill selections should not be preserved: %#v", got.SelectedProfessionalSkills)
	}
}

func TestApplyReferenceModelDefaultsAddsReservedProfessionalSkills(t *testing.T) {
	requireBuiltinAgentConfig(t)
	svc := NewService(nil, nil)
	agent := &types.CustomAgent{
		ID:        types.BuiltinGeneralAgentID,
		IsBuiltin: true,
		TenantID:  10002,
		Config: types.CustomAgentConfig{
			AgentMode:                       types.AgentModeUnified,
			AgentType:                       types.AgentTypeGeneralAgent,
			ProfessionalSkillsSelectionMode: "none",
			SelectedProfessionalSkills:      []string{"tenant-skill"},
		},
	}

	got, err := svc.ApplyReferenceModelDefaults(context.Background(), agent, 10002)
	if err != nil {
		t.Fatalf("ApplyReferenceModelDefaults returned error: %v", err)
	}
	if got.Config.ProfessionalSkillsSelectionMode != "selected" {
		t.Fatalf("professional mode = %q, want selected", got.Config.ProfessionalSkillsSelectionMode)
	}
	for _, name := range []string{"tenant-skill", "anysearch-skill", "find-skill-skillhub"} {
		if !stringSliceContains(got.Config.SelectedProfessionalSkills, name) {
			t.Fatalf("selected professional skills = %#v, want %s", got.Config.SelectedProfessionalSkills, name)
		}
	}
}

func TestApplyReferenceModelDefaultsAddsReservedProfessionalSkillsToDataAnalysis(t *testing.T) {
	requireBuiltinAgentConfig(t)
	svc := NewService(nil, nil)
	agent := &types.CustomAgent{
		ID:        types.BuiltinDataAnalystID,
		IsBuiltin: true,
		TenantID:  10002,
		Config: types.CustomAgentConfig{
			AgentMode:                       types.AgentModeUnified,
			AgentType:                       types.AgentTypeDataAnalysis,
			ProfessionalSkillsSelectionMode: "none",
		},
	}

	got, err := svc.ApplyReferenceModelDefaults(context.Background(), agent, 10002)
	if err != nil {
		t.Fatalf("ApplyReferenceModelDefaults returned error: %v", err)
	}
	if got.Config.ProfessionalSkillsSelectionMode != "selected" {
		t.Fatalf("data-analysis professional mode = %q, want selected", got.Config.ProfessionalSkillsSelectionMode)
	}
	for _, name := range []string{"anysearch-skill", "find-skill-skillhub"} {
		if !stringSliceContains(got.Config.SelectedProfessionalSkills, name) {
			t.Fatalf("data-analysis selected professional skills = %#v, want %s", got.Config.SelectedProfessionalSkills, name)
		}
	}
}

func TestMergeResetConfigClearsDataSourcesForNonDataAnalysisAgents(t *testing.T) {
	defaultConfig := types.CustomAgentConfig{
		AgentType:     types.AgentTypeKnowledgeQA,
		DBDataSources: []string{},
	}
	currentConfig := types.CustomAgentConfig{
		DBDataSources: []string{"db-1"},
	}

	got := mergeResetConfig(defaultConfig, currentConfig)
	if len(got.DBDataSources) != 0 {
		t.Fatalf("non-data-analysis data sources should reset to defaults, got %#v", got.DBDataSources)
	}
}

func TestApplyReferenceModelDefaultsClonesModelsForPersonalTenant(t *testing.T) {
	requireBuiltinAgentConfig(t)
	db := openBuiltinAgentDefaultsTestDB(t)
	svc := NewService(db, nil)

	disabled := false
	enabled := true
	sourceTenantID := uint64(10000)
	targetTenantID := uint64(10002)
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, targetTenantID)

	requireCreate(t, db, &types.User{
		ID:           "target-user",
		Username:     "target@example.com",
		PasswordHash: "x",
		TenantID:     targetTenantID,
		IsActive:     true,
	})
	requireCreate(t, db, &types.Model{
		ID:          "source-chat-model",
		TenantID:    sourceTenantID,
		Name:        "glm-5-turbo",
		DisplayName: "GLM-5-Turbo",
		Type:        types.ModelTypeKnowledgeQA,
		Source:      types.ModelSourceRemote,
		Description: "source chat",
		Parameters: types.ModelParameters{
			BaseURL:       "https://model.example.com/v1",
			APIKey:        "secret",
			InterfaceType: "openai",
			Provider:      "generic",
			ExtraConfig:   map[string]string{"a": "b"},
		},
		Status: types.ModelStatusActive,
	})
	requireCreate(t, db, &types.Model{
		ID:          "source-rerank-model",
		TenantID:    sourceTenantID,
		Name:        "BAAI/bge-reranker-v2-m3",
		Type:        types.ModelTypeRerank,
		Source:      types.ModelSourceRemote,
		Description: "source rerank",
		Parameters: types.ModelParameters{
			BaseURL:       "https://rerank.example.com/v1",
			InterfaceType: "openai",
		},
		Status: types.ModelStatusActive,
	})
	requireCreate(t, db, &types.CustomAgent{
		ID:        types.BuiltinGeneralAgentID,
		Name:      "通用智能体",
		IsBuiltin: true,
		TenantID:  sourceTenantID,
		Config: types.CustomAgentConfig{
			AgentMode:           types.AgentModeUnified,
			AgentType:           types.AgentTypeGeneralAgent,
			ModelID:             "source-chat-model",
			RerankModelID:       "source-rerank-model",
			Temperature:         0.5,
			MaxCompletionTokens: 4096,
			Thinking:            &enabled,
		},
	})

	target := &types.CustomAgent{
		ID:        types.BuiltinGeneralAgentID,
		Name:      "通用智能体",
		IsBuiltin: true,
		TenantID:  targetTenantID,
		Config: types.CustomAgentConfig{
			AgentMode:           types.AgentModeUnified,
			AgentType:           types.AgentTypeGeneralAgent,
			ModelID:             "old-model",
			RerankModelID:       "old-rerank",
			Temperature:         0.25,
			MaxCompletionTokens: 2048,
			Thinking:            &disabled,
			MCPSelectionMode:    "selected",
			MCPServices:         []string{"mcp-1"},
			MCPAuthWaitTimeout:  42,
		},
	}

	got, err := svc.ApplyReferenceModelDefaults(ctx, target, targetTenantID)
	if err != nil {
		t.Fatalf("ApplyReferenceModelDefaults returned error: %v", err)
	}

	expectedChatID := deterministicModelCloneID(targetTenantID, "source-chat-model")
	expectedRerankID := deterministicModelCloneID(targetTenantID, "source-rerank-model")
	if got.Config.ModelID != expectedChatID {
		t.Fatalf("model_id = %q, want %q", got.Config.ModelID, expectedChatID)
	}
	if got.Config.RerankModelID != expectedRerankID {
		t.Fatalf("rerank_model_id = %q, want %q", got.Config.RerankModelID, expectedRerankID)
	}
	if got.Config.Temperature != 0.25 || got.Config.MaxCompletionTokens != 2048 {
		t.Fatalf("non-model call settings should remain target-local: %#v", got.Config)
	}
	if got.Config.Thinking == nil || *got.Config.Thinking {
		t.Fatalf("thinking should remain target-local: %#v", got.Config.Thinking)
	}
	if got.Config.MCPSelectionMode != "selected" || len(got.Config.MCPServices) != 1 || got.Config.MCPServices[0] != "mcp-1" || got.Config.MCPAuthWaitTimeout != 42 {
		t.Fatalf("MCP settings should remain target-local: %#v", got.Config)
	}

	var clonedChat types.Model
	if err := db.Where("id = ? AND tenant_id = ?", expectedChatID, targetTenantID).First(&clonedChat).Error; err != nil {
		t.Fatalf("expected cloned chat model: %v", err)
	}
	if clonedChat.Name != "glm-5-turbo" || clonedChat.ManagedBy != modelCloneManagedBy || clonedChat.Parameters.BaseURL != "https://model.example.com/v1" {
		t.Fatalf("unexpected cloned chat model: %#v", clonedChat)
	}
	if clonedChat.Parameters.ExtraConfig["a"] != "b" {
		t.Fatalf("extra config not cloned: %#v", clonedChat.Parameters.ExtraConfig)
	}
}

func TestEnsureUserProvisionedCreatesMissingBuiltinAgentsWithTenantModelDefaults(t *testing.T) {
	requireBuiltinAgentConfig(t)
	db := openBuiltinAgentDefaultsTestDB(t)
	svc := NewService(db, nil)

	enabled := true
	sourceTenantID := uint64(10000)
	targetTenantID := uint64(10002)
	targetUser := &types.User{
		ID:           "target-user",
		Username:     "target@example.com",
		PasswordHash: "x",
		TenantID:     targetTenantID,
		IsActive:     true,
	}

	requireCreate(t, db, targetUser)
	requireCreate(t, db, &types.Model{
		ID:          "source-deepseek-model",
		TenantID:    sourceTenantID,
		Name:        "deepseek-v4-flash-int8",
		DisplayName: "DeepSeek-V4-Flash-INT8 LiteLLM",
		Type:        types.ModelTypeKnowledgeQA,
		Source:      types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			BaseURL:       "https://model.example.com/v1",
			InterfaceType: "openai",
			Provider:      "generic",
		},
		Status: types.ModelStatusActive,
	})
	requireCreate(t, db, &types.Model{
		ID:       "source-rerank-model",
		TenantID: sourceTenantID,
		Name:     "BAAI/bge-reranker-v2-m3",
		Type:     types.ModelTypeRerank,
		Source:   types.ModelSourceRemote,
		Parameters: types.ModelParameters{
			BaseURL:       "https://rerank.example.com/v1",
			InterfaceType: "openai",
		},
		Status: types.ModelStatusActive,
	})
	requireCreate(t, db, &types.CustomAgent{
		ID:        types.BuiltinKnowledgeQAID,
		Name:      "知识问答",
		IsBuiltin: true,
		TenantID:  sourceTenantID,
		Config: types.CustomAgentConfig{
			AgentMode:           types.AgentModeUnified,
			ModelID:             "source-deepseek-model",
			RerankModelID:       "source-rerank-model",
			Temperature:         0.7,
			MaxCompletionTokens: 4096,
			Thinking:            &enabled,
		},
	})
	requireCreate(t, db, &types.CustomAgent{
		ID:        types.BuiltinGeneralAgentID,
		Name:      "通用智能体",
		IsBuiltin: true,
		TenantID:  targetTenantID,
		Config: types.CustomAgentConfig{
			AgentMode: types.AgentModeUnified,
			ModelID:   "tenant-custom-model",
		},
	})

	if err := svc.EnsureUserProvisioned(context.Background(), targetUser); err != nil {
		t.Fatalf("EnsureUserProvisioned returned error: %v", err)
	}

	expectedChatID := deterministicModelCloneID(targetTenantID, "source-deepseek-model")
	expectedRerankID := deterministicModelCloneID(targetTenantID, "source-rerank-model")

	var quickAnswer types.CustomAgent
	if err := db.Where("id = ? AND tenant_id = ?", types.BuiltinKnowledgeQAID, targetTenantID).
		First(&quickAnswer).Error; err != nil {
		t.Fatalf("expected tenant knowledge-qa agent: %v", err)
	}
	if !quickAnswer.IsBuiltin {
		t.Fatalf("knowledge-qa should be built-in")
	}
	if quickAnswer.Config.ModelID != expectedChatID {
		t.Fatalf("knowledge-qa model_id = %q, want %q", quickAnswer.Config.ModelID, expectedChatID)
	}
	if quickAnswer.Config.RerankModelID != expectedRerankID {
		t.Fatalf("knowledge-qa rerank_model_id = %q, want %q", quickAnswer.Config.RerankModelID, expectedRerankID)
	}

	var clonedChat types.Model
	if err := db.Where("id = ? AND tenant_id = ?", expectedChatID, targetTenantID).First(&clonedChat).Error; err != nil {
		t.Fatalf("expected cloned knowledge-qa chat model: %v", err)
	}
	if clonedChat.Name != "deepseek-v4-flash-int8" || clonedChat.ManagedBy != modelCloneManagedBy {
		t.Fatalf("unexpected cloned chat model: %#v", clonedChat)
	}

	var smartReasoning types.CustomAgent
	if err := db.Where("id = ? AND tenant_id = ?", types.BuiltinGeneralAgentID, targetTenantID).
		First(&smartReasoning).Error; err != nil {
		t.Fatalf("expected existing knowledge-qa agent: %v", err)
	}
	if smartReasoning.Config.ModelID != "tenant-custom-model" {
		t.Fatalf("existing built-in agent should not be overwritten, got %#v", smartReasoning.Config)
	}

	if err := svc.EnsureUserProvisioned(context.Background(), targetUser); err != nil {
		t.Fatalf("EnsureUserProvisioned second call returned error: %v", err)
	}
	var knowledgeQACount int64
	if err := db.Model(&types.CustomAgent{}).
		Where("id = ? AND tenant_id = ?", types.BuiltinKnowledgeQAID, targetTenantID).
		Count(&knowledgeQACount).Error; err != nil {
		t.Fatalf("count knowledge-qa: %v", err)
	}
	if knowledgeQACount != 1 {
		t.Fatalf("knowledge-qa row count = %d, want 1", knowledgeQACount)
	}
}

func TestApplyReferenceModelConfigPreservesNonModelFields(t *testing.T) {
	svc := NewService(nil, nil)
	current := types.CustomAgentConfig{
		SystemPrompt:   "target system",
		SystemPromptID: "target_system_id",

		MCPSelectionMode: "selected",
		MCPServices:      []string{"mcp-1"},
	}
	reference := types.CustomAgentConfig{
		SystemPrompt:   "reference system",
		SystemPromptID: "reference_system_id",
	}

	got, err := svc.applyReferenceModelConfig(context.Background(), 10002, current, reference)
	if err != nil {
		t.Fatalf("applyReferenceModelConfig returned error: %v", err)
	}

	if got.SystemPrompt != "target system" || got.SystemPromptID != "target_system_id" {
		t.Fatalf("system prompt fields should remain target-local: %#v", got)
	}

	if got.MCPSelectionMode != "selected" || len(got.MCPServices) != 1 || got.MCPServices[0] != "mcp-1" {
		t.Fatalf("MCP settings should remain target-local: %#v", got)
	}
}

func openBuiltinAgentDefaultsTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&types.User{}, &types.Model{}, &types.CustomAgent{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func requireBuiltinAgentConfig(t *testing.T) {
	t.Helper()
	if err := types.LoadBuiltinAgentsConfig("../../../../config"); err != nil {
		t.Fatalf("load builtin agent config: %v", err)
	}
}

func requireCreate(t *testing.T, db *gorm.DB, value any) {
	t.Helper()
	if err := db.Create(value).Error; err != nil {
		t.Fatalf("create %T: %v", value, err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
