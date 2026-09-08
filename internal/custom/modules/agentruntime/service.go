package agentruntime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/artifactstore"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/custom/modules/dbanalytics"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct {
	admission          *modeladmission.Manager
	db                 *gorm.DB
	sessionService     interfaces.SessionService
	agentService       interfaces.AgentService
	messageService     interfaces.MessageService
	modelService       interfaces.ModelService
	knowledgeService   interfaces.KnowledgeService
	fileService        interfaces.FileService
	dbAnalytics        *dbanalytics.Service
	apiKey             string
	artifactStore      *artifactstore.Store
	artifactStoreErr   error
	housekeepingMu     sync.Mutex
	housekeepingCancel context.CancelFunc
	housekeepingDone   chan struct{}
	professionalSkills professionalSkillProvider
}

type professionalSkillProvider interface {
	ProfessionalPackages(
		context.Context,
		[]string,
		bool,
	) ([]skillhub.ProfessionalSkillPackage, error)
}

func NewService(
	db *gorm.DB,
	sessionService interfaces.SessionService,
	agentService interfaces.AgentService,
	messageService interfaces.MessageService,
	modelService interfaces.ModelService,
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
	dbAnalytics *dbanalytics.Service,
) *Service {
	privateArtifactStore, artifactStoreErr := artifactstore.NewFromEnv()
	return &Service{
		db:               db,
		sessionService:   sessionService,
		agentService:     agentService,
		messageService:   messageService,
		modelService:     modelService,
		knowledgeService: knowledgeService,
		fileService:      fileService,
		dbAnalytics:      dbAnalytics,
		apiKey:           strings.TrimSpace(os.Getenv("AGENT_RUNTIME_API_KEY")),
		artifactStore:    privateArtifactStore,
		artifactStoreErr: artifactStoreErr,
	}
}

func (s *Service) SetProfessionalSkillProvider(provider professionalSkillProvider) {
	if s != nil {
		s.professionalSkills = provider
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := s.migrateRuns(ctx); err != nil {
		return err
	}
	if err := applyAgentRuntimeMigrations(ctx, s.db); err != nil {
		return err
	}
	if s.artifactStoreErr != nil {
		return s.artifactStoreErr
	}
	if s.artifactStore == nil {
		return errors.New("private artifact object storage is unavailable")
	}
	if err := s.artifactStore.CheckConnectivity(ctx); err != nil {
		return fmt.Errorf("private artifact object storage connectivity: %w", err)
	}
	return nil
}

func (s *Service) ArtifactStore() *artifactstore.Store {
	if s == nil {
		return nil
	}
	return s.artifactStore
}

// Run creates a durable job and projects its outbox. It never drives model turns.
func (s *Service) Run(ctx context.Context, req *types.QARequest, eventBus *event.EventBus) error {
	if req == nil || req.Session == nil || req.CustomAgent == nil {
		return errors.New("invalid QA runtime configuration")
	}
	if s.sessionService == nil || s.agentService == nil || s.modelService == nil || s.apiKey == "" {
		return errors.New("通用智能体后端依赖未初始化")
	}

	sessionID := req.Session.ID
	runID := uuid.New().String()
	agentConfig, err := s.sessionService.BuildAgentRuntimeConfig(ctx, req)
	if err != nil {
		return err
	}
	rerankModel, err := s.resolveRerankModel(ctx, req, agentConfig)
	if err != nil {
		return err
	}
	registry, err := s.agentService.CreateToolRegistry(ctx, agentConfig, rerankModel, sessionID)
	if err != nil {
		return err
	}
	defer registry.Cleanup(ctx)

	llm, err := s.resolveLLMConfig(ctx, agentConfig)
	if err != nil {
		return err
	}
	query := s.buildEffectiveQuery(ctx, req)
	lightweightSkills := make([]LightweightSkillSpec, 0, len(agentConfig.RuntimeLightweightSkills))
	for _, skill := range agentConfig.RuntimeLightweightSkills {
		lightweightSkills = append(lightweightSkills, LightweightSkillSpec{Key: skill.Key, Name: skill.Name, Description: skill.Description, SelectedByUser: skill.SelectedByUser, Instructions: skill.Instructions})
	}
	professionalSkills, err := s.professionalSkillSpecs(
		ctx,
		req.CustomAgent,
		req.ProfessionalSkillNames,
	)
	if err != nil {
		return err
	}
	agentConfig.ProfessionalSkillsEnabled = len(professionalSkills) > 0
	agentConfig.AllowedProfessionalSkills = professionalSkillNames(professionalSkills)
	originalInputFiles, err := s.originalInputFileSpecs(ctx, req, runID)
	if err != nil {
		return err
	}
	userID := types.SessionOwnerIDFromContext(ctx)
	history, durableUserContext, err := s.buildHistory(ctx, req, agentConfig)
	if err != nil {
		return err
	}
	payload := ChatPayload{
		RunID:              runID,
		TenantID:           tenantIDFromContext(ctx),
		UserID:             userID,
		SessionID:          sessionID,
		RequestID:          req.RequestID,
		AssistantMessageID: req.AssistantMessageID,
		UserMessageID:      req.UserMessageID,
		// The sidecar's <user_request> is documented as verbatim user text. Keep
		// platform continuity rules in the system prompt instead of appending
		// hidden protocol text to the highest-priority user request.
		Query: query,
		SystemPrompt: conversationmemory.AppendUserSourceLedger(
			renderSystemPrompt(ctx, answerStylePrompt(agentConfig.ResolveSystemPrompt(agentConfig.WebSearchEnabled), req.CustomAgent), agentConfig.WebSearchEnabled),
			durableUserContext,
		),
		History:                 history,
		ImageURLs:               cloneStringSlice(req.ImageURLs),
		ImageDescription:        req.ImageDescription,
		QuotedContext:           req.QuotedContext,
		LightweightSkillPolicy:  appservice.LightweightSkillExecutionContract(),
		LightweightSkills:       lightweightSkills,
		Attachments:             attachmentSpecs(req.Attachments),
		OriginalInputFiles:      originalInputFiles,
		DocumentTemplateContext: documentTemplateContextSpec(ctx, agentConfig),
		VisibleContext:          s.buildVisibleContext(ctx, req, agentConfig),
		ProfessionalSkills:      professionalSkills,
		Tools:                   runtimeToolSpecs(registry),
		RuntimeConfig:           runtimeConfigSpec(agentConfig),
		LLM:                     llm,
		ToolCallbackURL:         toolCallbackURL(),
		ToolCallbackAPIKey:      strings.TrimSpace(os.Getenv("AGENT_RUNTIME_API_KEY")),
		ArtifactUploadURL:       artifactUploadURL(),
		EnableArtifacts:         agentConfig.EnableArtifacts,
	}

	payload.RuntimeConfig.RerankModelID = req.CustomAgent.Config.RerankModelID
	payload.RuntimeConfig.MaxContextTokens = agentConfig.MaxContextTokens
	if payload.RuntimeConfig.MaxContextTokens < 4096 {
		payload.RuntimeConfig.MaxContextTokens = 128000
	}

	payload.LLM = nil
	payload.ToolCallbackAPIKey = ""
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	role, _ := ctx.Value(types.TenantRoleContextKey).(types.TenantRole)
	scope := RunScope{TenantRole: role, Config: agentConfig, ModelID: agentConfig.RuntimeModelID, ModelTenantID: agentConfig.AgentTenantID, VLMModelID: agentConfig.VLMModelID, SearchTargets: agentConfig.SearchTargets, PinnedMCPServiceIDs: agentConfig.PinnedMCPServiceIDs, PinnedSkillNames: agentConfig.PinnedSkillNames, RuntimeAttachments: agentConfig.RuntimeAttachments, NonInteractiveOAuth: types.IsMCPOAuthNonInteractive(ctx)}
	scope.LightweightSkills = lightweightSkills
	scopeJSON, err := json.Marshal(scope)
	if err != nil {
		return err
	}
	principal, _ := types.PrincipalFromContext(ctx)
	accountID, _ := types.UserIDFromContext(ctx)
	timeout := 2 * time.Hour
	if payload.EnableArtifacts && payload.RuntimeConfig.AgentType != types.AgentTypeKnowledgeQA {
		timeout = 4 * time.Hour
	}
	row := &RunRecord{ID: runID, TenantID: payload.TenantID, UserID: userID, AccountID: accountID, Principal: principal, SessionID: sessionID, MessageID: req.AssistantMessageID, Status: "queued", Deadline: time.Now().Add(timeout), Payload: encoded, Scope: scopeJSON}
	if err = s.db.WithContext(ctx).Create(row).Error; err != nil {
		return err
	}
	return s.followRun(ctx, row, eventBus)
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case fmt.Stringer:
		return v.String()
	default:
		if value == nil {
			return ""
		}
		return fmt.Sprint(value)
	}
}

func (s *Service) resolveRerankModel(ctx context.Context, req *types.QARequest, agentConfig *types.AgentConfig) (rerank.Reranker, error) {
	for _, tool := range agentConfig.AllowedTools {
		if tool != agenttools.ToolKnowledgeSearch {
			continue
		}
		modelID := strings.TrimSpace(req.CustomAgent.Config.RerankModelID)
		if modelID == "" {
			logger.Infof(ctx, "knowledge_search has no rerank model; using retrieval scores without an additional model call")
			return nil, nil
		}
		model, err := s.modelService.GetRerankModel(ctx, modelID)
		if err != nil {
			return nil, fmt.Errorf("通用智能体无法加载 rerank 模型: %w", err)
		}
		return model, nil
	}
	return nil, nil
}

func (s *Service) resolveLLMConfig(ctx context.Context, config *types.AgentConfig) (*LLMConfig, error) {
	modelCtx := ctx
	if config.AgentTenantID != 0 {
		modelCtx = context.WithValue(modelCtx, types.TenantIDContextKey, config.AgentTenantID)
	}
	model, err := s.modelService.GetModelByID(modelCtx, config.RuntimeModelID)
	if err != nil {
		return nil, fmt.Errorf("通用智能体无法读取模型配置: %w", err)
	}
	out, err := runtimeLLMConfigFromModel(model)
	if err != nil {
		return nil, err
	}
	if err := secutils.ValidateURLForSSRF(out.BaseURL); err != nil {
		return nil, fmt.Errorf("通用智能体接口地址未通过 SSRF 校验: %w", err)
	}
	return out, nil
}

func (s *Service) buildEffectiveQuery(ctx context.Context, req *types.QARequest) string {
	if req == nil {
		return ""
	}
	return req.Query
}

func (s *Service) buildHistory(
	ctx context.Context,
	req *types.QARequest,
	config *types.AgentConfig,
) ([]ChatHistoryMessage, string, error) {
	if s.messageService == nil || !config.MultiTurnEnabled {
		return nil, "", nil
	}
	turns := config.HistoryTurns
	if turns <= 0 {
		turns = 5
	}
	msgs, err := s.messageService.GetRecentMessagesBySession(
		ctx,
		req.Session.ID,
		conversationmemory.FetchMessageLimit(turns),
	)
	if err != nil {
		return nil, "", fmt.Errorf("load agent history: %w", err)
	}
	msgs, err = conversationmemory.WithArchivedEvidence(ctx, s.db, req.Session.ID, msgs, config.SearchTargets)
	if err != nil {
		return nil, "", fmt.Errorf("load conversation evidence: %w", err)
	}
	if err := s.attachMessageArtifacts(ctx, msgs); err != nil {
		return nil, "", err
	}
	history, archive := buildGeneralAgentHistory(msgs, turns, req.UserMessageID, req.AssistantMessageID)
	return history, archive, nil
}

func buildGeneralAgentHistory(
	msgs []*types.Message,
	turns int,
	excludedMessageIDs ...string,
) ([]ChatHistoryMessage, string) {
	turnsInContext, archive := conversationmemory.BuildHistory(msgs, turns, excludedMessageIDs...)
	out := make([]ChatHistoryMessage, 0, len(turnsInContext)*2)
	for _, pair := range turnsInContext {
		userSourceID := pair.SourceID
		out = append(out, ChatHistoryMessage{
			Role:           "user",
			Content:        strings.TrimSpace(pair.User.Content),
			SourceID:       userSourceID,
			MentionedItems: append([]types.MentionedItem(nil), pair.User.MentionedItems...),
			Images:         imageSpecs(pair.User.Images),
			Attachments:    attachmentSpecsWithoutContent(pair.User.Attachments),
		})
		if pair.UserArchived {
			out[len(out)-1].Content = ""
			out[len(out)-1].ContextMetadata = json.RawMessage(`{"read_required":true}`)
		}
		if pair.Assistant == nil {
			continue
		}
		answer := conversationmemory.AssistantContent(pair.Assistant)
		metadata := conversationmemory.AssistantMetadata(pair.Assistant)
		if pair.AssistantArchived && pair.Assistant.ErrorCode == "" {
			answer = ""
			metadata, _ = json.Marshal(map[string]any{"source_id": conversationmemory.AssistantSourceID(pair.Assistant.ID), "read_required": true})
		}
		if answer != "" || pair.Assistant.ErrorCode != "" || pair.AssistantArchived {
			out = append(out, ChatHistoryMessage{
				Role:            "assistant",
				Content:         answer,
				SourceID:        conversationmemory.AssistantSourceID(pair.Assistant.ID),
				ContextMetadata: metadata,
			})
		}
	}
	return out, archive
}

func configuredProfessionalSkillSelection(agent *types.CustomAgent) (string, []string) {
	if agent == nil {
		return "none", nil
	}
	mode := strings.TrimSpace(agent.Config.ProfessionalSkillsSelectionMode)
	if mode == "" {
		mode = "none"
	}
	return mode, cloneStringSlice(agent.Config.SelectedProfessionalSkills)
}

func effectiveProfessionalSkillSelection(
	agent *types.CustomAgent,
	requested []string,
) ([]string, bool) {
	mode, configured := configuredProfessionalSkillSelection(agent)
	requested = compactStrings(requested)
	switch mode {
	case "all":
		if len(requested) > 0 {
			return requested, false
		}
		return nil, true
	case "selected":
		configured = compactStrings(configured)
		if len(configured) == 0 {
			return nil, false
		}
		if len(requested) == 0 {
			return configured, false
		}
		allowed := make(map[string]struct{}, len(configured))
		for _, name := range configured {
			allowed[name] = struct{}{}
		}
		names := make([]string, 0, len(requested))
		for _, name := range requested {
			if _, ok := allowed[name]; ok {
				names = append(names, name)
			}
		}
		return names, false
	default:
		return nil, false
	}
}

func (s *Service) professionalSkillSpecs(
	ctx context.Context,
	agent *types.CustomAgent,
	requested []string,
) ([]ProfessionalSkillSpec, error) {
	names, all := effectiveProfessionalSkillSelection(agent, requested)
	if !all && len(names) == 0 {
		return nil, nil
	}
	if s.professionalSkills == nil {
		return nil, fmt.Errorf("professional skill provider is unavailable")
	}
	packages, err := s.professionalSkills.ProfessionalPackages(ctx, names, all)
	if err != nil {
		return nil, fmt.Errorf("load professional skills: %w", err)
	}
	out := make([]ProfessionalSkillSpec, 0, len(packages))
	for _, pkg := range packages {
		spec := ProfessionalSkillSpec{
			Name:        pkg.Name,
			DisplayName: pkg.DisplayName,
			Description: pkg.Description,
			Files:       make([]ProfessionalSkillFileSpec, 0, len(pkg.Files)),
		}
		for _, file := range pkg.Files {
			spec.Files = append(spec.Files, ProfessionalSkillFileSpec{
				Path:          file.Path,
				ContentBase64: file.ContentBase64,
			})
		}
		out = append(out, spec)
	}
	return out, nil
}

func professionalSkillNames(skills []ProfessionalSkillSpec) []string {
	out := make([]string, 0, len(skills))
	for _, skill := range skills {
		if strings.TrimSpace(skill.Name) != "" {
			out = append(out, skill.Name)
		}
	}
	return out
}

func professionalSkillSpecsContain(skills []ProfessionalSkillSpec, name string) bool {
	for _, skill := range skills {
		if strings.TrimSpace(skill.Name) == name {
			return true
		}
	}
	return false
}

// Only task facts enter model context; runtime knobs and duplicate ID lists
// stay in the control record and tool schemas.
func (s *Service) buildVisibleContext(ctx context.Context, req *types.QARequest, config *types.AgentConfig) map[string]any {
	out := map[string]any{}
	if req == nil || config == nil {
		return out
	}
	if req.CustomAgent != nil {
		out["agent"] = map[string]any{"name": req.CustomAgent.Name, "description": req.CustomAgent.Description}
	}
	out["visible_resources"] = s.visibleResources(ctx, config)
	if config.EnableArtifacts {
		out["artifact_return_policy"] = artifactReturnPolicy()
	}
	if len(req.KnowledgeBaseIDs) > 0 {
		out["selected_knowledge_base_ids"] = req.KnowledgeBaseIDs
	}
	if len(req.KnowledgeIDs) > 0 {
		out["selected_knowledge_file_ids"] = req.KnowledgeIDs
	}
	return out
}

func artifactReturnPolicy() map[string]any {
	return map[string]any{
		"artifact_count_limited":        false,
		"total_return_size_limit_bytes": int64(128 * 1024 * 1024),
		"admission":                     "checked at registration; registered files are never silently discarded",
	}
}

type visibleDBSource struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	QueryMode      string `json:"query_mode"`
	MaxRows        int    `json:"max_rows"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

func (visibleDBSource) TableName() string { return "custom_db_sources" }

type visibleSkill struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
}

func (visibleSkill) TableName() string { return "custom_skills" }

func (s *Service) visibleResources(ctx context.Context, config *types.AgentConfig) map[string]any {
	out := map[string]any{}
	if config == nil {
		return out
	}
	modelCtx := ctx
	if config.AgentTenantID != 0 {
		modelCtx = context.WithValue(ctx, types.TenantIDContextKey, config.AgentTenantID)
	}
	if s.modelService != nil && config.RuntimeModelID != "" {
		if model, err := s.modelService.GetModelByID(modelCtx, config.RuntimeModelID); err == nil && model != nil {
			out["chat_model"] = visibleModel(model)
		} else {
			out["chat_model"] = map[string]any{"id": config.RuntimeModelID, "error": "model details unavailable"}
		}
	}
	if s.db == nil {
		return out
	}

	if ids := compactStrings(config.KnowledgeBases); len(ids) > 0 {
		var rows []types.KnowledgeBase
		if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err == nil {
			out["knowledge_bases"] = orderVisibleKnowledgeBases(ids, rows)
		}
	}
	if ids := compactStrings(config.KnowledgeIDs); len(ids) > 0 {
		var rows []types.Knowledge
		if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err == nil {
			out["knowledge_files"] = orderVisibleKnowledgeFiles(ids, rows)
		}
	}
	if ids := compactStrings(config.DBDataSources); len(ids) > 0 {
		var rows []visibleDBSource
		if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err == nil {
			out["database_data_sources"] = orderVisibleDBSources(ids, rows)
		}
	}
	if ids := compactStrings(config.MCPServices); len(ids) > 0 {
		var rows []types.MCPService
		if err := s.db.WithContext(ctx).Where("id IN ?", ids).Find(&rows).Error; err == nil {
			out["mcp_services"] = orderVisibleMCPServices(ids, rows)
		}
	}
	if config.WebSearchProviderID != "" {
		var provider types.WebSearchProviderEntity
		if err := s.db.WithContext(ctx).Where("id = ?", config.WebSearchProviderID).First(&provider).Error; err == nil {
			out["web_search_provider"] = map[string]any{
				"id":          provider.ID,
				"name":        provider.Name,
				"provider":    provider.Provider,
				"description": provider.Description,
				"is_default":  provider.IsDefault,
			}
		}
	}
	return out
}

func visibleModel(model *types.Model) map[string]any {
	if model == nil {
		return nil
	}
	return map[string]any{
		"id":              model.ID,
		"name":            model.Name,
		"display_name":    model.DisplayName,
		"type":            model.Type,
		"source":          model.Source,
		"description":     model.Description,
		"supports_vision": model.Parameters.SupportsVision,
		"is_default":      model.IsDefault,
		"is_builtin":      model.IsBuiltin,
		"status":          model.Status,
	}
}

func orderVisibleKnowledgeBases(ids []string, rows []types.KnowledgeBase) []map[string]any {
	byID := make(map[string]types.KnowledgeBase, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			out = append(out, map[string]any{"id": id, "missing": true})
			continue
		}
		out = append(out, map[string]any{
			"id":                row.ID,
			"name":              row.Name,
			"type":              row.Type,
			"description":       row.Description,
			"knowledge_count":   row.KnowledgeCount,
			"chunk_count":       row.ChunkCount,
			"creator_name":      row.CreatorName,
			"indexing_strategy": row.IndexingStrategy,
		})
	}
	return out
}

func orderVisibleKnowledgeFiles(ids []string, rows []types.Knowledge) []map[string]any {
	byID := make(map[string]types.Knowledge, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			out = append(out, map[string]any{"id": id, "missing": true})
			continue
		}
		out = append(out, map[string]any{
			"id":                  row.ID,
			"title":               row.Title,
			"description":         row.Description,
			"type":                row.Type,
			"source":              row.Source,
			"channel":             row.Channel,
			"file_name":           row.FileName,
			"file_type":           row.FileType,
			"file_size":           row.FileSize,
			"knowledge_base_id":   row.KnowledgeBaseID,
			"knowledge_base_name": row.KnowledgeBaseName,
			"parse_status":        row.ParseStatus,
			"summary_status":      row.SummaryStatus,
			"enable_status":       row.EnableStatus,
		})
	}
	return out
}

func orderVisibleDBSources(ids []string, rows []visibleDBSource) []map[string]any {
	byID := make(map[string]visibleDBSource, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			out = append(out, map[string]any{"id": id, "missing": true})
			continue
		}
		out = append(out, map[string]any{
			"id":              row.ID,
			"name":            row.Name,
			"description":     row.Description,
			"type":            row.Type,
			"status":          row.Status,
			"query_mode":      row.QueryMode,
			"max_rows":        row.MaxRows,
			"timeout_seconds": row.TimeoutSeconds,
		})
	}
	return out
}

func orderVisibleMCPServices(ids []string, rows []types.MCPService) []map[string]any {
	byID := make(map[string]types.MCPService, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	out := make([]map[string]any, 0, len(ids))
	for _, id := range ids {
		row, ok := byID[id]
		if !ok {
			out = append(out, map[string]any{"id": id, "missing": true})
			continue
		}
		out = append(out, map[string]any{
			"id":             row.ID,
			"name":           row.Name,
			"description":    row.Description,
			"enabled":        row.Enabled,
			"transport_type": row.TransportType,
			"is_builtin":     row.IsBuiltin,
		})
	}
	return out
}

func orderVisibleSkills(names []string, rows []visibleSkill) []map[string]any {
	byName := make(map[string]visibleSkill, len(rows))
	for _, row := range rows {
		byName[row.Name] = row
	}
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		row, ok := byName[name]
		if !ok {
			out = append(out, map[string]any{"name": name, "missing": true})
			continue
		}
		out = append(out, map[string]any{
			"name":        row.Name,
			"description": row.Description,
			"enabled":     row.Enabled,
		})
	}
	return out
}

func attachmentSpecs(in types.MessageAttachments) []AttachmentSpec {
	out := make([]AttachmentSpec, 0, len(in))
	for _, att := range in {
		out = append(out, AttachmentSpec{
			FileName:    att.FileName,
			FileType:    att.FileType,
			FileSize:    att.FileSize,
			Content:     att.Content,
			IsTruncated: att.IsTruncated,
		})
	}
	return out
}

func documentTemplateContextSpec(ctx context.Context, config *types.AgentConfig) DocumentTemplateContextSpec {
	if config == nil || config.AgentType != types.AgentTypeDocumentProcessingAgent || config.DocumentTemplate == nil {
		return DocumentTemplateContextSpec{}
	}
	files := make([]DocumentTemplateFileSpec, 0, 16)
	appendFormatFiles := func(format string, cfg types.DocumentTemplateFormatConfig) {
		if cfg.RequirementFile != nil {
			if spec, ok := documentTemplateFileSpec(ctx, format, "requirement", formatRequirementVariable(format), *cfg.RequirementFile); ok {
				files = append(files, spec)
			}
		}
		for i, file := range cfg.TemplateFiles {
			if spec, ok := documentTemplateFileSpec(ctx, format, "reference", formatReferenceVariable(format), file); ok {
				if spec.Variable != "" {
					spec.Variable = fmt.Sprintf("%s[%d]", spec.Variable, i+1)
				}
				files = append(files, spec)
			}
		}
	}
	appendFormatFiles(types.DocumentFormatWord, config.DocumentTemplate.Word)
	appendFormatFiles(types.DocumentFormatExcel, config.DocumentTemplate.Excel)
	appendFormatFiles(types.DocumentFormatPDF, config.DocumentTemplate.PDF)
	appendFormatFiles(types.DocumentFormatPPT, config.DocumentTemplate.PPT)
	return DocumentTemplateContextSpec{Files: files}
}

func documentTemplateFileSpec(ctx context.Context, format string, role string, variable string, file types.DocumentTemplateFile) (DocumentTemplateFileSpec, bool) {
	contentBase64 := strings.TrimSpace(file.ContentBase64)
	if file.Source == types.DocumentTemplateFileSourceBuiltin {
		info, ok := types.BuiltinDocumentTemplateFileInfoByID(file.BuiltinID)
		if !ok {
			logger.Warnf(ctx, "unknown builtin document template file: %s", file.BuiltinID)
			return DocumentTemplateFileSpec{}, false
		}
		data, err := os.ReadFile(info.RelativePath)
		if err != nil {
			logger.Warnf(ctx, "failed to read builtin document template file %s: %v", info.RelativePath, err)
			return DocumentTemplateFileSpec{}, false
		}
		contentBase64 = base64.StdEncoding.EncodeToString(data)
		file.FileName = info.FileName
		file.FileType = info.FileType
		file.FileSize = int64(len(data))
	}
	if contentBase64 == "" {
		return DocumentTemplateFileSpec{}, false
	}
	return DocumentTemplateFileSpec{
		Role:          role,
		Format:        format,
		Variable:      variable,
		Source:        file.Source,
		BuiltinID:     file.BuiltinID,
		FileName:      file.FileName,
		FileType:      strings.TrimPrefix(strings.ToLower(file.FileType), "."),
		FileSize:      file.FileSize,
		ContentBase64: contentBase64,
	}, true
}

func formatRequirementVariable(format string) string {
	switch format {
	case types.DocumentFormatWord:
		return "word_template_requirement"
	case types.DocumentFormatExcel:
		return "excel_template_requirement"
	case types.DocumentFormatPDF:
		return "pdf_template_requirement"
	case types.DocumentFormatPPT:
		return "ppt_template_requirement"
	default:
		return ""
	}
}

func formatReferenceVariable(format string) string {
	switch format {
	case types.DocumentFormatWord:
		return "word_template_files"
	case types.DocumentFormatExcel:
		return "excel_template_files"
	case types.DocumentFormatPDF:
		return "pdf_template_files"
	case types.DocumentFormatPPT:
		return "ppt_template_files"
	default:
		return ""
	}
}

func attachmentSpecsWithoutContent(in types.MessageAttachments) []AttachmentSpec {
	out := attachmentSpecs(in)
	for i := range out {
		out[i].Content = ""
		out[i].IsTruncated = false
	}
	return out
}

func imageSpecs(in types.MessageImages) []ImageSpec {
	out := make([]ImageSpec, 0, len(in))
	for _, img := range in {
		out = append(out, ImageSpec{
			URL:     img.URL,
			Caption: img.Caption,
		})
	}
	return out
}

func cloneStringSlice(in []string) []string {
	out := make([]string, 0, len(in))
	return append(out, in...)
}

func appendUniqueString(in []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return cloneStringSlice(in)
	}
	out := cloneStringSlice(in)
	for _, item := range out {
		if strings.TrimSpace(item) == value {
			return out
		}
	}
	return append(out, value)
}

func compactStrings(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]bool, len(in))
	for _, item := range in {
		item = strings.TrimSpace(item)
		if item == "" || seen[item] {
			continue
		}
		seen[item] = true
		out = append(out, item)
	}
	return out
}

func runtimeConfigSpec(c *types.AgentConfig) RuntimeConfigSpec {
	return RuntimeConfigSpec{
		AgentID:                     c.AgentID,
		AgentType:                   c.AgentType,
		MaxIterations:               c.MaxIterations,
		Temperature:                 c.Temperature,
		MaxCompletionTokens:         c.MaxCompletionTokens,
		Thinking:                    c.Thinking,
		AllowedTools:                cloneStringSlice(c.AllowedTools),
		KnowledgeBases:              cloneStringSlice(c.KnowledgeBases),
		KnowledgeIDs:                cloneStringSlice(c.KnowledgeIDs),
		DBDataSources:               cloneStringSlice(c.DBDataSources),
		WebSearchEnabled:            c.WebSearchEnabled,
		WebSearchProviderID:         c.WebSearchProviderID,
		WebSearchMaxResults:         c.WebSearchMaxResults,
		WebFetchEnabled:             c.WebFetchEnabled,
		WebFetchTopN:                c.WebFetchTopN,
		MultiTurnEnabled:            c.MultiTurnEnabled,
		HistoryTurns:                c.HistoryTurns,
		MCPSelectionMode:            c.MCPSelectionMode,
		MCPServices:                 cloneStringSlice(c.MCPServices),
		ProfessionalSkillsEnabled:   c.ProfessionalSkillsEnabled,
		AllowedProfessionalSkills:   cloneStringSlice(c.AllowedProfessionalSkills),
		RetrieveKBOnlyWhenMentioned: c.RetrieveKBOnlyWhenMentioned,
		RetainRetrievalHistory:      c.RetainRetrievalHistory,
		LLMCallTimeout:              c.LLMCallTimeout,
		EmbeddingTopK:               c.EmbeddingTopK,
		KeywordThreshold:            c.KeywordThreshold,
		VectorThreshold:             c.VectorThreshold,
		RerankTopK:                  c.RerankTopK,
		RerankThreshold:             c.RerankThreshold,
		FAQPriorityEnabled:          c.FAQPriorityEnabled,
		FAQDirectAnswerThreshold:    c.FAQDirectAnswerThreshold,
		KnowledgeManagement:         c.KnowledgeManagement,
	}
}

func tenantIDFromContext(ctx context.Context) uint64 {
	if tid, ok := types.TenantIDFromContext(ctx); ok {
		return tid
	}
	return 0
}

func renderSystemPrompt(ctx context.Context, prompt string, webSearchEnabled bool) string {
	status := "Disabled"
	if webSearchEnabled {
		status = "Enabled"
	}
	replacements := map[string]string{
		"web_search_status": status,
		"language":          types.LanguageNameFromContext(ctx),
		"current_time":      time.Now().Format(time.RFC3339),
	}
	for key, value := range replacements {
		prompt = strings.ReplaceAll(prompt, "{{"+key+"}}", value)
	}
	return sourcerefs.EnsureGenerationContract(prompt)
}

// Presentation is a profile choice; retrieval and generation budgets remain
// shared so short answers retain the same evidence and qualification standards.
func answerStylePrompt(prompt string, agent *types.CustomAgent) string {
	if agent == nil || agent.Config.AgentType != types.AgentTypeKnowledgeQA {
		return prompt
	}
	return prompt + `
[KNOWLEDGE_QA_STYLE]
Accuracy is the first priority. Resolve pronouns and omitted subjects or scope from the preceding topic; switch topics when the user asks to. Preserve that scope when planning retrieval; do not expand a contextual follow-up into an exhaustive cross-topic inventory unless requested. Answer the actual question directly: conclusion first, then only necessary evidence, conditions, exceptions and citations. A simple question usually needs one short paragraph or a few bullets. Do not narrate searches, recap the question or conversation, repeat conclusions, or offer unrequested further work. Retrieve only for unresolved claims: reuse still-applicable verified conversation evidence with read_conversation, batch independent searches and reads, and stop once evidence supports the requested claims. Do not repeat an equivalent search without identifying a specific evidence gap. Cross-check contradictory, ambiguous or insufficient evidence; explicitly distinguish direct evidence from inference. Preserve qualifications and uncertainty instead of guessing. If the user asks for detailed analysis or a complete procedure, cover it fully. Never sacrifice correctness for brevity or speed.
[/KNOWLEDGE_QA_STYLE]`
}

func toolCallbackURL() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_TOOL_CALLBACK_URL")); v != "" {
		return v
	}
	return defaultToolCallbackURL
}

func artifactUploadURL() string {
	if v := strings.TrimSpace(os.Getenv("AGENT_RUNTIME_ARTIFACT_UPLOAD_URL")); v != "" {
		return v
	}
	return defaultArtifactUploadURL
}

func generalAgentToolExecTimeout(config *types.AgentConfig) time.Duration {
	timeout := envDurationSeconds("AGENT_RUNTIME_TOOL_EXEC_TIMEOUT_SEC", 15*time.Minute)
	if config != nil && config.LLMCallTimeout > 0 {
		configured := time.Duration(config.LLMCallTimeout) * time.Second
		if configured > timeout {
			timeout = configured
		}
	}
	return timeout
}

func safeFileName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	return name
}

func (s *Service) SetAdmission(manager *modeladmission.Manager) { s.admission = manager }
