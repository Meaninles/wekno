package kbmanager

import (
	"context"
	"fmt"
	"sort"
	"strings"

	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const (
	ToolListDocuments   = "kb_list_documents"
	ToolAddDocument     = "kb_add_document"
	ToolReplaceDocument = "kb_replace_document"
	ToolDeleteDocument  = "kb_delete_document"
	ToolMutationStatus  = "kb_mutation_status"
)

var managementToolNames = map[string]bool{
	ToolListDocuments:   true,
	ToolAddDocument:     true,
	ToolReplaceDocument: true,
	ToolDeleteDocument:  true,
	ToolMutationStatus:  true,
}

type Configurator struct {
	kbService        interfaces.KnowledgeBaseService
	knowledgeService interfaces.KnowledgeService
	kbShareService   interfaces.KBShareService
}

func NewConfigurator(
	kbService interfaces.KnowledgeBaseService,
	knowledgeService interfaces.KnowledgeService,
	kbShareService interfaces.KBShareService,
) *Configurator {
	return &Configurator{kbService: kbService, knowledgeService: knowledgeService, kbShareService: kbShareService}
}

func IsManagementTool(name string) bool { return managementToolNames[strings.TrimSpace(name)] }

func (c *Configurator) NormalizeAgentConfig(ctx context.Context, agent *types.CustomAgent) error {
	if agent == nil {
		return nil
	}
	if agent.Config.AgentType != types.AgentTypeKnowledgeBaseManager {
		agent.Config.KnowledgeManagement = nil
		agent.Config.AllowedTools = withoutManagementTools(agent.Config.AllowedTools)
		return nil
	}

	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", appservice.ErrAgentCustomConfigInvalid, fmt.Sprintf(format, args...))
	}
	if agent.Config.AgentMode != types.AgentModeSmartReasoning {
		return fail("知识库管理智能体必须使用智能推理模式")
	}
	if agent.Config.KBSelectionMode != "selected" {
		return fail("知识库管理智能体必须选择指定知识库，不能选择全部或不使用知识库")
	}
	selected := compactUnique(agent.Config.KnowledgeBases)
	if len(selected) == 0 {
		return fail("知识库管理智能体至少需要指定一个知识库")
	}
	if agent.Config.KnowledgeManagement == nil {
		return fail("知识库管理智能体必须配置新增、修改或删除权限")
	}
	if strings.TrimSpace(agent.Config.RerankModelID) == "" {
		return fail("知识库管理智能体必须配置 ReRank 模型")
	}

	manager := agent.Config.KnowledgeManagement
	manager.DefaultPermissions = manager.DefaultPermissions.Normalize()
	selectedSet := make(map[string]bool, len(selected))
	for _, id := range selected {
		selectedSet[id] = true
	}
	cleanOverrides := make(map[string]types.KnowledgeManagementPermissionSet)
	for kbID, permission := range manager.KnowledgeBaseOverrides {
		kbID = strings.TrimSpace(kbID)
		if kbID == "" || !selectedSet[kbID] {
			return fail("知识库权限覆盖只能引用已选择的知识库：%s", kbID)
		}
		cleanOverrides[kbID] = permission.Normalize()
	}
	manager.KnowledgeBaseOverrides = cleanOverrides
	for _, kbID := range selected {
		if !manager.PermissionsFor(kbID).Any() {
			return fail("知识库 %s 至少需要启用新增、修改或删除中的一项权限", kbID)
		}
	}

	if c == nil || c.kbService == nil {
		return fail("知识库服务未初始化")
	}
	kbs, err := c.kbService.GetKnowledgeBasesByIDsOnly(ctx, selected)
	if err != nil {
		return fail("无法校验所选知识库：%v", err)
	}
	byID := make(map[string]*types.KnowledgeBase, len(kbs))
	for _, kb := range kbs {
		if kb != nil {
			byID[kb.ID] = kb
		}
	}
	callerTenantID, _ := types.TenantIDFromContext(ctx)
	callerRole := types.TenantRoleFromContext(ctx)
	for _, id := range selected {
		kb := byID[id]
		if kb == nil {
			return fail("所选知识库不存在或已删除：%s", id)
		}
		if kb.Type == types.KnowledgeBaseTypeFAQ {
			return fail("FAQ 知识库不支持文档级新增、替换或删除：%s", kb.Name)
		}
		if kb.TenantID != callerTenantID {
			if c.kbShareService == nil {
				return fail("无权使用所选共享知识库：%s", kb.Name)
			}
			ok, accessErr := c.kbShareService.HasTenantKBPermission(ctx, id, callerTenantID, callerRole, types.OrgRoleEditor)
			if accessErr != nil || !ok {
				return fail("所选共享知识库仅允许具备可编辑权限的用户用于知识库管理：%s", kb.Name)
			}
		}
	}

	agent.Config.KnowledgeBases = selected
	agent.Config.RetrieveKBOnlyWhenMentioned = false
	agent.Config.EnableArtifacts = true
	agent.Config.SupportedFileTypes = nil // use the native KB parser's complete allowlist
	agent.Config.AllowedTools = normalizedManagementTools(agent.Config.AllowedTools, manager, selected)
	return nil
}

func normalizedManagementTools(current []string, manager *types.KnowledgeManagementConfig, kbIDs []string) []string {
	out := withoutManagementTools(current)
	out = append(out, ToolListDocuments, ToolMutationStatus)
	var add, modify, deletePermission bool
	for _, kbID := range kbIDs {
		p := manager.PermissionsFor(kbID)
		add = add || p.Add
		modify = modify || p.Modify
		deletePermission = deletePermission || p.Delete
	}
	if add {
		out = append(out, ToolAddDocument)
	}
	if modify {
		out = append(out, ToolReplaceDocument)
	}
	if deletePermission {
		out = append(out, ToolDeleteDocument)
	}
	return compactUnique(out)
}

func withoutManagementTools(values []string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if !IsManagementTool(value) {
			out = append(out, value)
		}
	}
	return compactUnique(out)
}

func compactUnique(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	return out
}

func sortedKeys[T any](values map[string]T) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
