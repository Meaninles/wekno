package builtinagentpolicy

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const policyTestTenantID uint64 = 42

func openBuiltinAgentPolicyTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&types.Model{}, &BuiltinAgentModelPolicy{}, &BuiltinAgentChatVisibility{}); err != nil {
		t.Fatalf("auto migrate: %v", err)
	}
	return db
}

func seedBuiltinAgentPolicyModels(t *testing.T, db *gorm.DB) {
	t.Helper()
	models := []*types.Model{
		{
			ID:            "tenant-qwen-chat",
			TenantID:      policyTestTenantID,
			Name:          "qwen-plus",
			Type:          types.ModelTypeKnowledgeQA,
			Status:        types.ModelStatusActive,
			WorkloadScope: types.ModelWorkloadInteractive,
			IsDefault:     true,
		},
		{
			ID:            "tenant-deepseek-chat",
			TenantID:      policyTestTenantID,
			Name:          "deepseek-v3",
			Type:          types.ModelTypeKnowledgeQA,
			Status:        types.ModelStatusActive,
			WorkloadScope: types.ModelWorkloadInteractive,
		},
		{
			ID:       "tenant-rerank",
			TenantID: policyTestTenantID,
			Name:     "bge-reranker",
			Type:     types.ModelTypeRerank,
			Status:   types.ModelStatusActive,
		},
	}
	for _, model := range models {
		if err := db.Create(model).Error; err != nil {
			t.Fatalf("seed model %s: %v", model.ID, err)
		}
	}
}

func policyTestContext(role types.TenantRole) context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, policyTestTenantID)
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, role)
	ctx = context.WithValue(ctx, types.UserIDContextKey, "policy-admin")
	return types.WithPrincipal(ctx, types.Principal{Type: types.PrincipalWebUser, ID: "policy-admin"})
}

func TestApplyUsesModelGroupAndDefaultChatVisibility(t *testing.T) {
	db := openBuiltinAgentPolicyTestDB(t)
	seedBuiltinAgentPolicyModels(t, db)
	svc := NewService(db, nil)

	cases := []struct {
		name       string
		agentID    string
		wantModel  string
		wantGroup  string
		wantInChat bool
	}{
		{
			name:       "knowledge qa uses qwen",
			agentID:    types.BuiltinKnowledgeQAID,
			wantModel:  "tenant-qwen-chat",
			wantGroup:  modelGroupQwen,
			wantInChat: true,
		},
		{
			name:       "data analyst uses deepseek",
			agentID:    types.BuiltinDataAnalystID,
			wantModel:  "tenant-deepseek-chat",
			wantGroup:  modelGroupDeepSeek,
			wantInChat: false,
		},
		{
			name:       "document processing is visible by default",
			agentID:    types.BuiltinDocumentProcessingID,
			wantModel:  "tenant-deepseek-chat",
			wantGroup:  modelGroupDeepSeek,
			wantInChat: true,
		},
		{
			name:       "general agent is visible by default",
			agentID:    types.BuiltinGeneralAgentID,
			wantModel:  "tenant-deepseek-chat",
			wantGroup:  modelGroupDeepSeek,
			wantInChat: true,
		},
		{
			name:       "wiki fixer is hidden by default",
			agentID:    types.BuiltinWikiFixerID,
			wantModel:  "tenant-deepseek-chat",
			wantGroup:  modelGroupDeepSeek,
			wantInChat: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := &types.CustomAgent{
				ID:        tc.agentID,
				TenantID:  policyTestTenantID,
				IsBuiltin: true,
				Config: types.CustomAgentConfig{
					ModelID: "tenant-deepseek-chat",
				},
			}
			got, err := svc.Apply(context.Background(), agent, policyTestTenantID)
			if err != nil {
				t.Fatalf("Apply() error = %v", err)
			}
			if got.Config.ModelID != tc.wantModel {
				t.Fatalf("model_id = %q, want %q", got.Config.ModelID, tc.wantModel)
			}
			if got.ModelGroup != tc.wantGroup || !got.ModelFieldsLocked {
				t.Fatalf("effective model metadata = group %q locked %v", got.ModelGroup, got.ModelFieldsLocked)
			}
			if got.VisibleInChat == nil || *got.VisibleInChat != tc.wantInChat {
				t.Fatalf("visible_in_chat = %v, want %v", got.VisibleInChat, tc.wantInChat)
			}
		})
	}
}

func TestMutateAllowsOnlyTenantAdminToChangeCentralModelPolicy(t *testing.T) {
	db := openBuiltinAgentPolicyTestDB(t)
	seedBuiltinAgentPolicyModels(t, db)
	svc := NewService(db, nil)
	base := &types.CustomAgent{
		ID:        types.BuiltinKnowledgeQAID,
		TenantID:  policyTestTenantID,
		IsBuiltin: true,
		Config: types.CustomAgentConfig{
			ModelID:       "tenant-qwen-chat",
			RerankModelID: "tenant-rerank",
		},
	}
	if _, err := svc.Apply(context.Background(), base, policyTestTenantID); err != nil {
		t.Fatalf("initial Apply() error = %v", err)
	}

	nonAdminRequest := *base
	nonAdminRequest.Config = base.Config
	nonAdminRequest.Config.ModelID = "tenant-deepseek-chat"
	nonAdminRequest.Config.RerankModelID = ""
	if err := svc.Mutate(policyTestContext(types.TenantRoleContributor), base, &nonAdminRequest, policyTestTenantID); err != nil {
		t.Fatalf("non-admin Mutate() error = %v", err)
	}
	if nonAdminRequest.Config.ModelID != "tenant-qwen-chat" || nonAdminRequest.Config.RerankModelID != "tenant-rerank" {
		t.Fatalf("non-admin changed model policy: %#v", nonAdminRequest.Config)
	}

	adminRequest := *base
	adminRequest.Config = base.Config
	adminRequest.Config.ModelID = "tenant-qwen-chat"
	adminRequest.Config.RerankModelID = ""
	if err := svc.Mutate(policyTestContext(types.TenantRoleAdmin), base, &adminRequest, policyTestTenantID); err != nil {
		t.Fatalf("admin Mutate() error = %v", err)
	}
	if adminRequest.Config.ModelID != "tenant-qwen-chat" || adminRequest.Config.RerankModelID != "" {
		t.Fatalf("admin update not persisted: %#v", adminRequest.Config)
	}
	var policy BuiltinAgentModelPolicy
	if err := db.Where("tenant_id = ? AND agent_id = ?", policyTestTenantID, types.BuiltinKnowledgeQAID).First(&policy).Error; err != nil {
		t.Fatalf("load policy: %v", err)
	}
	if policy.RerankModelID != "" || policy.UpdatedBy != "policy-admin" {
		t.Fatalf("unexpected persisted policy: %#v", policy)
	}
}

func TestSetVisibilityIsTenantScoped(t *testing.T) {
	db := openBuiltinAgentPolicyTestDB(t)
	svc := NewService(db, nil)
	ctx := policyTestContext(types.TenantRoleAdmin)
	if err := svc.SetVisibility(ctx, types.BuiltinKnowledgeQAID, policyTestTenantID, false); err != nil {
		t.Fatalf("SetVisibility() error = %v", err)
	}
	visible, err := svc.ResolveVisibility(context.Background(), types.BuiltinKnowledgeQAID, policyTestTenantID)
	if err != nil {
		t.Fatalf("ResolveVisibility() error = %v", err)
	}
	if visible {
		t.Fatal("visibility should be disabled after tenant update")
	}
	otherTenantVisible, err := svc.ResolveVisibility(context.Background(), types.BuiltinKnowledgeQAID, policyTestTenantID+1)
	if err != nil {
		t.Fatalf("ResolveVisibility(other tenant) error = %v", err)
	}
	if !otherTenantVisible {
		t.Fatal("visibility override leaked across tenants")
	}
}
