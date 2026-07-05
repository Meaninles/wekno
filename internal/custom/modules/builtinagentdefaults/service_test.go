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
		AgentType:           types.AgentTypeDataAnalysis,
		ModelID:             "default-model",
		RerankModelID:       "default-rerank",
		WebSearchProviderID: "default-provider",
		Thinking:            &disabled,
		DBDataSources:       []string{},
		MCPSelectionMode:    "none",
	}
	currentConfig := types.CustomAgentConfig{
		ModelID:                    "current-model",
		RerankModelID:              "current-rerank",
		QueryUnderstandModelID:     "current-query-model",
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

	if got.Thinking == nil || *got.Thinking != true {
		t.Fatalf("thinking should be forced to true, got %#v", got.Thinking)
	}
	if got.ModelID != "current-model" || got.RerankModelID != "current-rerank" {
		t.Fatalf("model bindings were not preserved: %#v", got)
	}
	if got.QueryUnderstandModelID != "current-query-model" || got.VLMModelID != "current-vlm" || got.ASRModelID != "current-asr" {
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
			AgentMode:                       types.AgentModeSmartReasoning,
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

func TestApplyReferenceModelDefaultsDoesNotAddReservedProfessionalSkillsToDataAnalysis(t *testing.T) {
	requireBuiltinAgentConfig(t)
	svc := NewService(nil, nil)
	agent := &types.CustomAgent{
		ID:        types.BuiltinDataAnalystID,
		IsBuiltin: true,
		TenantID:  10002,
		Config: types.CustomAgentConfig{
			AgentMode:                       types.AgentModeSmartReasoning,
			AgentType:                       types.AgentTypeDataAnalysis,
			ProfessionalSkillsSelectionMode: "none",
		},
	}

	got, err := svc.ApplyReferenceModelDefaults(context.Background(), agent, 10002)
	if err != nil {
		t.Fatalf("ApplyReferenceModelDefaults returned error: %v", err)
	}
	if got.Config.ProfessionalSkillsSelectionMode != "none" || len(got.Config.SelectedProfessionalSkills) != 0 {
		t.Fatalf("data-analysis professional skills = mode %q skills %#v, want unchanged none",
			got.Config.ProfessionalSkillsSelectionMode, got.Config.SelectedProfessionalSkills)
	}
}

func TestMergeResetConfigClearsDataSourcesForNonDataAnalysisAgents(t *testing.T) {
	defaultConfig := types.CustomAgentConfig{
		AgentType:     types.AgentTypeGeneralAgent,
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
			AgentMode:           types.AgentModeSmartReasoning,
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
			AgentMode:          types.AgentModeSmartReasoning,
			AgentType:          types.AgentTypeGeneralAgent,
			ModelID:            "old-model",
			RerankModelID:      "old-rerank",
			MCPSelectionMode:   "selected",
			MCPServices:        []string{"mcp-1"},
			MCPAuthWaitTimeout: 42,
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
	if got.Config.Temperature != 0.5 || got.Config.MaxCompletionTokens != 4096 {
		t.Fatalf("model call settings not synced: %#v", got.Config)
	}
	if got.Config.Thinking == nil || !*got.Config.Thinking {
		t.Fatalf("thinking should sync to true: %#v", got.Config.Thinking)
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

func TestApplyReferenceModelConfigSyncsPromptFields(t *testing.T) {
	svc := NewService(nil, nil)
	current := types.CustomAgentConfig{
		SystemPrompt:        "target system",
		SystemPromptID:      "target_system_id",
		ContextTemplate:     "target context",
		ContextTemplateID:   "target_context_id",
		RewritePromptSystem: "target rewrite system",
		RewritePromptUser:   "target rewrite user",
		FallbackPrompt:      "target fallback",
		IntentPrompts:       map[string]string{"chitchat": "target chitchat"},
		MCPSelectionMode:    "selected",
		MCPServices:         []string{"mcp-1"},
	}
	reference := types.CustomAgentConfig{
		SystemPrompt:        "reference system",
		SystemPromptID:      "reference_system_id",
		ContextTemplate:     "reference context",
		ContextTemplateID:   "reference_context_id",
		RewritePromptSystem: "reference rewrite system",
		RewritePromptUser:   "reference rewrite user",
		FallbackPrompt:      "reference fallback",
		IntentPrompts:       map[string]string{"chitchat": "reference chitchat"},
	}

	got, err := svc.applyReferenceModelConfig(context.Background(), 10002, current, reference)
	if err != nil {
		t.Fatalf("applyReferenceModelConfig returned error: %v", err)
	}

	if got.SystemPrompt != "reference system" || got.SystemPromptID != "reference_system_id" {
		t.Fatalf("system prompt fields were not synced: %#v", got)
	}
	if got.ContextTemplate != "reference context" || got.ContextTemplateID != "reference_context_id" {
		t.Fatalf("context template fields were not synced: %#v", got)
	}
	if got.RewritePromptSystem != "reference rewrite system" || got.RewritePromptUser != "reference rewrite user" {
		t.Fatalf("rewrite prompt fields were not synced: %#v", got)
	}
	if got.FallbackPrompt != "reference fallback" {
		t.Fatalf("fallback prompt was not synced: %#v", got)
	}
	if got.IntentPrompts["chitchat"] != "reference chitchat" {
		t.Fatalf("intent prompts were not synced: %#v", got.IntentPrompts)
	}
	reference.IntentPrompts["chitchat"] = "changed"
	if got.IntentPrompts["chitchat"] != "reference chitchat" {
		t.Fatalf("intent prompts should be cloned, got %#v", got.IntentPrompts)
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
