package kbmanager

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

// ConfigureRuntime enforces config ∩ explicit-turn-selection ∩ live platform
// RBAC. Explicit out-of-scope targets fail the whole turn instead of silently
// falling back to the agent's broader configured scope.
func (c *Configurator) ConfigureRuntime(ctx context.Context, req *types.QARequest, config *types.AgentConfig) error {
	if req == nil || req.CustomAgent == nil || config == nil || config.AgentType != types.AgentTypeKnowledgeBaseManager {
		return nil
	}
	manager := req.CustomAgent.Config.KnowledgeManagement
	if manager == nil {
		return fmt.Errorf("知识库管理智能体缺少权限配置")
	}
	configured := compactUnique(req.CustomAgent.Config.KnowledgeBases)
	configuredSet := make(map[string]bool, len(configured))
	for _, id := range configured {
		configuredSet[id] = true
	}

	explicit := len(req.KnowledgeBaseIDs) > 0 || len(req.KnowledgeIDs) > 0 || len(req.TagScopes) > 0
	whole := configured
	documents := make(map[string]string)
	parents := make(map[string]bool)
	if explicit {
		whole = nil
		for _, kbID := range compactUnique(req.KnowledgeBaseIDs) {
			if !configuredSet[kbID] {
				return fmt.Errorf("本轮选择的知识库不在该智能体配置范围内：%s", kbID)
			}
			whole = append(whole, kbID)
			parents[kbID] = true
		}
		for _, scope := range req.TagScopes {
			kbID := strings.TrimSpace(scope.KnowledgeBaseID)
			if kbID == "" || !configuredSet[kbID] {
				return fmt.Errorf("本轮选择的标签不在该智能体配置的知识库范围内：%s", kbID)
			}
		}
		if c == nil || c.knowledgeService == nil {
			return fmt.Errorf("无法校验本轮选择的文档：知识库服务未初始化")
		}
		for _, knowledgeID := range compactUnique(req.KnowledgeIDs) {
			knowledge, err := c.knowledgeService.GetKnowledgeByIDOnly(ctx, knowledgeID)
			if err != nil || knowledge == nil {
				return fmt.Errorf("本轮选择的文档不存在或已删除：%s", knowledgeID)
			}
			if !configuredSet[knowledge.KnowledgeBaseID] {
				return fmt.Errorf("本轮选择的文档不在该智能体配置范围内：%s", knowledgeID)
			}
			documents[knowledge.ID] = knowledge.KnowledgeBaseID
			parents[knowledge.KnowledgeBaseID] = true
		}
	}
	for _, kbID := range whole {
		parents[kbID] = true
	}

	effective := make(map[string]types.KnowledgeManagementPermissionSet, len(parents))
	for kbID := range parents {
		configuredPermission := manager.PermissionsFor(kbID)
		platformPermission := c.platformMutationPermissions(ctx, kbID)
		effective[kbID] = configuredPermission.Intersect(platformPermission)
	}

	config.KnowledgeBases = compactUnique(whole)
	config.KnowledgeIDs = sortedKeys(documents)
	config.KnowledgeManagement = &types.KnowledgeManagementRuntimeScope{
		ExplicitSelection:     explicit,
		WholeKnowledgeBaseIDs: compactUnique(whole),
		Documents:             documents,
		EffectivePermissions:  effective,
		ReadOnlyTagScope:      len(req.TagScopes) > 0,
	}
	return nil
}

// platformMutationPermissions mirrors the native knowledge handlers: a caller
// inside the source tenant may mutate that tenant's KB, while a cross-tenant
// shared KB requires effective Editor (or stronger) permission. Missing
// identity/permission fails closed. Read access is deliberately separate.
func (c *Configurator) platformMutationPermissions(ctx context.Context, kbID string) types.KnowledgeManagementPermissionSet {
	deny := types.KnowledgeManagementPermissionSet{}
	if c == nil || c.kbService == nil {
		return deny
	}
	kb, err := c.kbService.GetKnowledgeBaseByIDOnly(ctx, kbID)
	if err != nil || kb == nil || kb.Type == types.KnowledgeBaseTypeFAQ {
		return deny
	}
	callerTenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || callerTenantID == 0 {
		return deny
	}
	role := types.TenantRoleFromContext(ctx)
	if kb.TenantID != callerTenantID {
		if c.kbShareService == nil {
			return deny
		}
		allowed, shareErr := c.kbShareService.HasTenantKBPermission(ctx, kbID, callerTenantID, role, types.OrgRoleEditor)
		if shareErr != nil || !allowed {
			return deny
		}
	}
	return types.KnowledgeManagementPermissionSet{Add: true, Modify: true, Delete: true}.Normalize()
}
