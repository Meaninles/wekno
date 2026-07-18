package kbmanager

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

type kbManagerTestKBService struct {
	interfaces.KnowledgeBaseService
	kbs map[string]*types.KnowledgeBase
}

func (s *kbManagerTestKBService) GetKnowledgeBaseByIDOnly(_ context.Context, id string) (*types.KnowledgeBase, error) {
	return s.kbs[id], nil
}

func (s *kbManagerTestKBService) GetKnowledgeBasesByIDsOnly(_ context.Context, ids []string) ([]*types.KnowledgeBase, error) {
	out := make([]*types.KnowledgeBase, 0, len(ids))
	for _, id := range ids {
		if kb := s.kbs[id]; kb != nil {
			out = append(out, kb)
		}
	}
	return out, nil
}

type kbManagerTestKnowledgeService struct {
	interfaces.KnowledgeService
	documents map[string]*types.Knowledge
}

func (s *kbManagerTestKnowledgeService) GetKnowledgeByIDOnly(_ context.Context, id string) (*types.Knowledge, error) {
	return s.documents[id], nil
}

type kbManagerTestShareService struct {
	interfaces.KBShareService
	allowed bool
}

func (s *kbManagerTestShareService) HasTenantKBPermission(
	_ context.Context,
	_ string,
	_ uint64,
	_ types.TenantRole,
	required types.OrgMemberRole,
) (bool, error) {
	return s.allowed && required == types.OrgRoleEditor, nil
}

func kbManagerTestContext(tenantID uint64) context.Context {
	ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tenantID)
	ctx = context.WithValue(ctx, types.UserIDContextKey, "user-1")
	return context.WithValue(ctx, types.TenantRoleContextKey, types.TenantRoleOwner)
}

func validManagerAgent(kbIDs ...string) *types.CustomAgent {
	overrides := map[string]types.KnowledgeManagementPermissionSet{}
	for _, kbID := range kbIDs {
		if kbID == "kb-b" {
			overrides["kb-b"] = types.KnowledgeManagementPermissionSet{Delete: true}
		}
	}
	return &types.CustomAgent{
		Config: types.CustomAgentConfig{
			AgentMode:       types.AgentModeSmartReasoning,
			AgentType:       types.AgentTypeKnowledgeBaseManager,
			RerankModelID:   "rerank-1",
			KBSelectionMode: "selected",
			KnowledgeBases:  kbIDs,
			AllowedTools:    []string{"thinking", ToolReplaceDocument},
			KnowledgeManagement: &types.KnowledgeManagementConfig{
				DefaultPermissions:     types.KnowledgeManagementPermissionSet{Add: true},
				KnowledgeBaseOverrides: overrides,
			},
		},
	}
}

func TestNormalizeAgentConfigAppliesUniformAndPerKBPermissions(t *testing.T) {
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", Name: "A", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
		"kb-b": {ID: "kb-b", Name: "B", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
	}}
	configurator := NewConfigurator(kbService, nil, nil)
	agent := validManagerAgent("kb-a", "kb-b", "kb-a")

	if err := configurator.NormalizeAgentConfig(kbManagerTestContext(1), agent); err != nil {
		t.Fatalf("NormalizeAgentConfig() error = %v", err)
	}
	if got := strings.Join(agent.Config.KnowledgeBases, ","); got != "kb-a,kb-b" {
		t.Fatalf("normalized KBs = %q, want kb-a,kb-b", got)
	}
	if !agent.Config.EnableArtifacts || agent.Config.KBSelectionMode != "selected" {
		t.Fatalf("manager invariants were not forced: %+v", agent.Config)
	}
	if agent.Config.KnowledgeManagement.PermissionsFor("kb-a").Add != true {
		t.Fatal("kb-a should inherit add permission")
	}
	if agent.Config.KnowledgeManagement.PermissionsFor("kb-b").Delete != true {
		t.Fatal("kb-b should use delete override")
	}
	tools := strings.Join(agent.Config.AllowedTools, ",")
	for _, want := range []string{ToolListDocuments, ToolMutationStatus, ToolAddDocument, ToolDeleteDocument} {
		if !strings.Contains(tools, want) {
			t.Fatalf("allowed tools %q missing %q", tools, want)
		}
	}
	if strings.Contains(tools, ToolReplaceDocument) {
		t.Fatalf("replace tool must be removed when no KB has composite modify permission: %q", tools)
	}
}

func TestNormalizeAgentConfigRejectsUnsafeScopes(t *testing.T) {
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"faq":    {ID: "faq", Name: "FAQ", Type: types.KnowledgeBaseTypeFAQ, TenantID: 1},
		"shared": {ID: "shared", Name: "Shared", Type: types.KnowledgeBaseTypeDocument, TenantID: 2},
	}}

	tests := []struct {
		name  string
		agent *types.CustomAgent
		share *kbManagerTestShareService
		want  string
	}{
		{name: "all knowledge bases", agent: func() *types.CustomAgent {
			a := validManagerAgent("faq")
			a.Config.KBSelectionMode = "all"
			return a
		}(), want: "必须选择指定知识库"},
		{name: "empty selection", agent: validManagerAgent(), want: "至少需要指定一个知识库"},
		{name: "missing rerank model", agent: func() *types.CustomAgent {
			a := validManagerAgent("faq")
			a.Config.RerankModelID = ""
			return a
		}(), want: "必须配置 ReRank 模型"},
		{name: "faq", agent: validManagerAgent("faq"), want: "FAQ 知识库不支持"},
		{name: "shared viewer", agent: validManagerAgent("shared"), share: &kbManagerTestShareService{allowed: false}, want: "可编辑权限"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configurator := NewConfigurator(kbService, nil, test.share)
			err := configurator.NormalizeAgentConfig(kbManagerTestContext(1), test.agent)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestConfigureRuntimeIntersectsConfigTurnScopeAndLiveRBAC(t *testing.T) {
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
		"kb-b": {ID: "kb-b", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
	}}
	knowledgeService := &kbManagerTestKnowledgeService{documents: map[string]*types.Knowledge{
		"doc-b": {ID: "doc-b", KnowledgeBaseID: "kb-b", TenantID: 1},
	}}
	configurator := NewConfigurator(kbService, knowledgeService, nil)
	agent := validManagerAgent("kb-a", "kb-b")
	agent.Config.KnowledgeManagement = &types.KnowledgeManagementConfig{
		DefaultPermissions: types.KnowledgeManagementPermissionSet{Add: true, Delete: true},
	}
	req := &types.QARequest{
		CustomAgent:      agent,
		KnowledgeBaseIDs: []string{"kb-a"},
		KnowledgeIDs:     []string{"doc-b"},
	}
	runtime := &types.AgentConfig{AgentType: types.AgentTypeKnowledgeBaseManager}

	if err := configurator.ConfigureRuntime(kbManagerTestContext(1), req, runtime); err != nil {
		t.Fatalf("ConfigureRuntime() error = %v", err)
	}
	scope := runtime.KnowledgeManagement
	if scope == nil || !scope.ExplicitSelection {
		t.Fatal("expected explicit runtime scope")
	}
	if !scope.HasWholeKnowledgeBase("kb-a") {
		t.Fatal("explicit KB should grant whole-KB scope")
	}
	if scope.HasWholeKnowledgeBase("kb-b") || !scope.ContainsDocument("doc-b", "kb-b") {
		t.Fatal("selected document should narrow kb-b to doc-b")
	}
	if scope.PermissionsFor("kb-a").Modify != true || scope.PermissionsFor("kb-b").Modify != true {
		t.Fatalf("configured add+delete should intersect to composite modify: %+v", scope.EffectivePermissions)
	}
	if got := strings.Join(runtime.KnowledgeBases, ","); got != "kb-a" {
		t.Fatalf("runtime KBs = %q, want kb-a", got)
	}
	if got := strings.Join(runtime.KnowledgeIDs, ","); got != "doc-b" {
		t.Fatalf("runtime documents = %q, want doc-b", got)
	}
}

func TestConfigureRuntimeFailsClosedForOutOfScopeDocumentAndTagOnly(t *testing.T) {
	kbService := &kbManagerTestKBService{kbs: map[string]*types.KnowledgeBase{
		"kb-a": {ID: "kb-a", Type: types.KnowledgeBaseTypeDocument, TenantID: 1},
	}}
	knowledgeService := &kbManagerTestKnowledgeService{documents: map[string]*types.Knowledge{
		"doc-x": {ID: "doc-x", KnowledgeBaseID: "kb-x", TenantID: 1},
	}}
	configurator := NewConfigurator(kbService, knowledgeService, nil)
	agent := validManagerAgent("kb-a")

	err := configurator.ConfigureRuntime(kbManagerTestContext(1), &types.QARequest{
		CustomAgent:  agent,
		KnowledgeIDs: []string{"doc-x"},
	}, &types.AgentConfig{AgentType: types.AgentTypeKnowledgeBaseManager})
	if err == nil || !strings.Contains(err.Error(), "不在该智能体配置范围") {
		t.Fatalf("out-of-scope document error = %v", err)
	}

	runtime := &types.AgentConfig{AgentType: types.AgentTypeKnowledgeBaseManager}
	err = configurator.ConfigureRuntime(kbManagerTestContext(1), &types.QARequest{
		CustomAgent: agent,
		TagScopes:   []types.TagScope{{KnowledgeBaseID: "kb-a", TagIDs: []string{"tag-1"}}},
	}, runtime)
	if err != nil {
		t.Fatalf("tag-only ConfigureRuntime() error = %v", err)
	}
	if runtime.KnowledgeManagement == nil || !runtime.KnowledgeManagement.ReadOnlyTagScope {
		t.Fatal("tag-only selection must be recorded as read-only")
	}
	if len(runtime.KnowledgeManagement.EffectivePermissions) != 0 || len(runtime.KnowledgeBases) != 0 {
		t.Fatalf("tag-only selection must not grant mutation scope: %+v", runtime.KnowledgeManagement)
	}
}
