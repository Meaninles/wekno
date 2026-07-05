package generalagent

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	appservice "github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	modelprovider "github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	generalClaudeModelNameEnv = "CUSTOM_GENERAL_AGENT_CLAUDE_MODEL_NAME"
	generalClaudeBaseURLEnv   = "CUSTOM_GENERAL_AGENT_CLAUDE_BASE_URL"
	generalClaudeProviderEnv  = "CUSTOM_GENERAL_AGENT_CLAUDE_PROVIDER"
	deepSeekAnthropicBaseURL  = "https://api.deepseek.com/anthropic"
	zhipuAnthropicBaseURL     = "https://open.bigmodel.cn/api/anthropic"
	generalClaudeAuthAPIKey   = "api_key"
	generalClaudeAuthHelper   = "api_key_helper"
	generalClaudeNoAuthKey    = "weknora-no-auth"
)

type Service struct {
	db               *gorm.DB
	sessionService   interfaces.SessionService
	agentService     interfaces.AgentService
	messageService   interfaces.MessageService
	modelService     interfaces.ModelService
	knowledgeService interfaces.KnowledgeService
	fileService      interfaces.FileService
	client           *Client
	documentClient   *Client
	artifactRoot     string
}

func NewService(
	db *gorm.DB,
	sessionService interfaces.SessionService,
	agentService interfaces.AgentService,
	messageService interfaces.MessageService,
	modelService interfaces.ModelService,
	knowledgeService interfaces.KnowledgeService,
	fileService interfaces.FileService,
) *Service {
	root := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ARTIFACT_ROOT"))
	if root == "" {
		root = filepath.Join("custom", "general-agent-artifacts")
	}
	return &Service{
		db:               db,
		sessionService:   sessionService,
		agentService:     agentService,
		messageService:   messageService,
		modelService:     modelService,
		knowledgeService: knowledgeService,
		fileService:      fileService,
		client:           NewClientFromEnv(),
		documentClient:   NewDocumentProcessingClientFromEnv(),
		artifactRoot:     root,
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	if err := applyGeneralAgentMigrations(ctx, s.db); err != nil {
		return err
	}
	return nil
}

func (s *Service) clientForAgentType(agentType string) *Client {
	if agentType == types.AgentTypeDocumentProcessingAgent && s.documentClient != nil {
		return s.documentClient
	}
	return s.client
}

func (s *Service) Run(ctx context.Context, req *types.QARequest, eventBus *event.EventBus) error {
	if req == nil || req.CustomAgent == nil {
		return errors.New("通用智能体需要有效的智能体配置")
	}
	if !types.IsClaudeSDKAgentType(req.CustomAgent.Config.AgentType) {
		return fmt.Errorf("invalid agent_type for general-agent runner: %s", req.CustomAgent.Config.AgentType)
	}
	if s.sessionService == nil || s.agentService == nil || s.modelService == nil || s.client == nil {
		return errors.New("通用智能体后端依赖未初始化")
	}

	start := time.Now()
	sessionID := req.Session.ID
	agentConfig, err := s.sessionService.BuildAgentRuntimeConfig(ctx, req)
	if err != nil {
		return err
	}
	sidecarClient := s.clientForAgentType(agentConfig.AgentType)
	chatModel, err := s.modelService.GetChatModel(ctx, agentConfig.RuntimeModelID)
	if err != nil {
		return fmt.Errorf("通用智能体无法加载对话模型: %w", err)
	}
	rerankModel, err := s.resolveRerankModel(ctx, req, agentConfig)
	if err != nil {
		return err
	}
	registry, err := s.agentService.CreateToolRegistry(ctx, agentConfig, chatModel, rerankModel, sessionID)
	if err != nil {
		return err
	}
	defer registry.Cleanup(ctx)

	llm, err := s.resolveLLMConfig(ctx, agentConfig)
	if err != nil {
		return err
	}
	query := s.buildEffectiveQuery(ctx, req)
	lightMode, lightNames := configuredLightweightSkillSelection(req.CustomAgent)
	selectedSkillContext := appservice.LightweightSkillContext(ctx, lightMode, lightNames, req.SkillNames)
	professionalSkills, err := s.professionalSkillSpecs(ctx, req.CustomAgent)
	if err != nil {
		return err
	}
	agentConfig.ProfessionalSkillsEnabled = len(professionalSkills) > 0
	agentConfig.AllowedProfessionalSkills = professionalSkillNames(professionalSkills)
	runID := uuid.New().String()
	originalInputFiles := s.originalInputFileSpecs(ctx, req, runID)
	userID, _ := types.UserIDFromContext(ctx)
	active := &activeRun{
		runID:              runID,
		ctx:                ctx,
		eventBus:           eventBus,
		registry:           registry,
		sessionID:          sessionID,
		assistantMessageID: req.AssistantMessageID,
		requestID:          req.RequestID,
		userID:             userID,
		originalUserQuery:  query,
		toolExecTimeout:    generalAgentToolExecTimeout(agentConfig),
	}
	unregister := registerActiveRun(active)
	defer unregister()

	payload := ChatPayload{
		RunID:                   runID,
		TenantID:                tenantIDFromContext(ctx),
		UserID:                  userID,
		SessionID:               sessionID,
		RequestID:               req.RequestID,
		AssistantMessageID:      req.AssistantMessageID,
		Query:                   query,
		SystemPrompt:            renderSystemPrompt(ctx, agentConfig.ResolveSystemPrompt(agentConfig.WebSearchEnabled), agentConfig.WebSearchEnabled),
		History:                 s.buildHistory(ctx, req, agentConfig),
		ImageURLs:               cloneStringSlice(req.ImageURLs),
		ImageDescription:        req.ImageDescription,
		QuotedContext:           req.QuotedContext,
		SelectedSkillContext:    selectedSkillContext,
		Attachments:             attachmentSpecs(req.Attachments),
		OriginalInputFiles:      originalInputFiles,
		DocumentTemplateContext: documentTemplateContextSpec(ctx, agentConfig),
		VisibleContext:          s.buildVisibleContext(ctx, req, agentConfig, selectedSkillContext),
		ProfessionalSkills:      professionalSkills,
		Tools:                   runtimeToolSpecs(registry),
		RuntimeConfig:           runtimeConfigSpec(agentConfig),
		LLM:                     llm,
		ToolCallbackURL:         toolCallbackURL(),
		ToolCallbackAPIKey:      strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_API_KEY")),
		EnableArtifacts:         agentConfig.EnableArtifacts,
	}

	fallbackAnswerID := "general-answer-" + runID
	lastAnswerID := ""
	lastAnswerDone := false
	var streamed strings.Builder
	result, err := sidecarClient.ChatStream(ctx, payload, func(evt StreamEvent) {
		s.emitSidecarEvent(ctx, eventBus, sessionID, fallbackAnswerID, evt, &streamed, &lastAnswerID, &lastAnswerDone, active)
	})
	if err != nil {
		return err
	}
	finalAnswer := strings.TrimSpace(result.Answer)
	if finalAnswer == "" {
		finalAnswer = strings.TrimSpace(streamed.String())
	}
	if streamed.Len() == 0 && finalAnswer != "" {
		lastAnswerID = fallbackAnswerID
		lastAnswerDone = false
		eventBus.Emit(ctx, event.Event{
			ID:        fallbackAnswerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			RequestID: req.RequestID,
			Data: event.AgentFinalAnswerData{
				Content: finalAnswer,
				Done:    false,
			},
		})
	}
	if lastAnswerID != "" && !lastAnswerDone {
		eventBus.Emit(ctx, event.Event{
			ID:        lastAnswerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			RequestID: req.RequestID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})
	}

	artifactResults, err := s.persistArtifacts(ctx, sidecarClient, result.RunID, req, result.Artifacts)
	if err != nil {
		logger.Warnf(ctx, "general-agent persist artifacts failed: %v", err)
	}
	artifactData, artifactOutput, emitArtifactResult := buildArtifactToolResult(result, artifactResults, err)
	if emitArtifactResult {
		artifactToolCallID := "general-artifacts-" + runID
		artifactArgs := map[string]any{
			"run_id":                  result.RunID,
			"artifact_count":          len(artifactResults),
			"artifact_original_count": result.ArtifactOriginalCount,
			"artifact_dropped_count":  result.ArtifactDroppedCount,
		}
		if err != nil {
			artifactArgs["persist_failed"] = true
		}
		artifactIteration := active.allocateIteration()
		artifactToolResult := &types.ToolResult{
			Success: true,
			Output:  artifactOutput,
			Data:    artifactData,
		}
		active.recordToolCall(artifactIteration, artifactToolCallID, "create_artifact", artifactArgs, artifactToolResult, 0)
		eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolCall,
			SessionID: sessionID,
			RequestID: req.RequestID,
			Data: event.AgentToolCallData{
				ToolCallID:     artifactToolCallID,
				ToolName:       "create_artifact",
				Arguments:      artifactArgs,
				Iteration:      artifactIteration,
				PreserveAnswer: true,
			},
		})
		eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentToolResult,
			SessionID: sessionID,
			RequestID: req.RequestID,
			Data: event.AgentToolResultData{
				ToolCallID: artifactToolCallID,
				ToolName:   "create_artifact",
				Output:     artifactToolResult.Output,
				Success:    true,
				Iteration:  artifactIteration,
				Data:       artifactData,
			},
		})
	}

	steps := active.snapshotSteps(finalAnswer)
	eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		RequestID: req.RequestID,
		Data: event.AgentCompleteData{
			SessionID:       sessionID,
			FinalAnswer:     finalAnswer,
			AgentSteps:      steps,
			TotalSteps:      len(steps),
			TotalDurationMs: time.Since(start).Milliseconds(),
			MessageID:       req.AssistantMessageID,
			RequestID:       req.RequestID,
		},
	})
	return nil
}

func (s *Service) emitSidecarEvent(ctx context.Context, eventBus *event.EventBus, sessionID, fallbackAnswerID string, evt StreamEvent, streamed *strings.Builder, lastAnswerID *string, lastAnswerDone *bool, active *activeRun) {
	switch evt.Type {
	case "answer_delta":
		answerID := strings.TrimSpace(evt.ID)
		if answerID == "" {
			answerID = fallbackAnswerID
		}
		if evt.Content != "" {
			streamed.WriteString(evt.Content)
		}
		if lastAnswerID != nil {
			*lastAnswerID = answerID
		}
		if lastAnswerDone != nil {
			*lastAnswerDone = evt.Done
		}
		eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: evt.Content,
				Done:    evt.Done,
			},
		})
	case "thinking":
		content := evt.Content
		if content == "" {
			content = evt.Message
		}
		if content == "" {
			return
		}
		eventBus.Emit(ctx, event.Event{
			Type:      event.EventAgentThought,
			SessionID: sessionID,
			Data: event.AgentThoughtData{
				Content:   content,
				Iteration: evt.Iteration,
				Done:      evt.Done,
			},
		})
	case "progress":
		progress := sidecarProgressDataFromEvent(evt)
		if progress.Content == "" {
			return
		}
		if active != nil {
			active.recordProgressStatus(progress.ToolCallID, progress.ToolName, progress.Phase, progress.Content)
		}
		eventBus.Emit(ctx, event.Event{
			ID:        progress.ToolCallID,
			Type:      event.EventAgentProgress,
			SessionID: sessionID,
			Data: event.AgentProgressData{
				Content:    progress.Content,
				ToolName:   progress.ToolName,
				ToolCallID: progress.ToolCallID,
				Phase:      progress.Phase,
				Transient:  progress.Transient,
				Iteration:  evt.Iteration,
				Done:       progress.Done,
			},
		})
	}
}

type sidecarProgressData struct {
	Content    string
	ToolName   string
	ToolCallID string
	Phase      string
	Transient  bool
	Done       bool
}

func sidecarProgressDataFromEvent(evt StreamEvent) sidecarProgressData {
	content := strings.TrimSpace(evt.Content)
	if content == "" {
		content = strings.TrimSpace(evt.Message)
	}
	out := sidecarProgressData{
		Content: content,
		Done:    evt.Done,
	}
	if len(evt.Data) > 0 {
		var data map[string]any
		if err := json.Unmarshal(evt.Data, &data); err == nil {
			out.ToolName = strings.TrimSpace(stringFromAny(data["tool_name"]))
			out.ToolCallID = strings.TrimSpace(stringFromAny(data["tool_call_id"]))
			out.Phase = strings.TrimSpace(stringFromAny(data["phase"]))
			if message := strings.TrimSpace(stringFromAny(data["message"])); message != "" {
				out.Content = message
			}
			if transient, ok := data["transient"].(bool); ok {
				out.Transient = transient
			}
		}
	}
	if out.ToolCallID == "" {
		out.ToolCallID = strings.TrimSpace(evt.ID)
	}
	if out.ToolCallID == "" {
		out.ToolCallID = fmt.Sprintf("agent-progress-%d", time.Now().UnixNano())
	}
	if out.ToolName == "" {
		out.ToolName = "general_agent_progress"
	}
	if out.Phase == "" {
		out.Phase = "start"
	}
	if out.Phase == "success" || out.Phase == "error" {
		out.Done = true
	}
	return out
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
			logger.Infof(ctx, "general-agent knowledge_search enabled without rerank_model_id; using chat-model rerank/fallback")
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
	out, err := generalClaudeLLMConfigFromModel(model)
	if err != nil {
		return nil, err
	}
	if err := secutils.ValidateURLForSSRF(out.BaseURL); err != nil {
		return nil, fmt.Errorf("通用智能体接口地址未通过 SSRF 校验: %w", err)
	}
	return out, nil
}

func generalClaudeLLMConfigFromModel(model *types.Model) (*LLMConfig, error) {
	if model == nil || model.Type != types.ModelTypeKnowledgeQA {
		return nil, errors.New("通用智能体需要对话模型")
	}
	out := &LLMConfig{
		ModelName: strings.TrimSpace(model.Name),
		BaseURL:   strings.TrimSpace(model.Parameters.BaseURL),
		APIKey:    strings.TrimSpace(model.Parameters.APIKey),
		Provider:  strings.TrimSpace(model.Parameters.Provider),
	}
	if remoteModel := strings.TrimSpace(model.Parameters.ExtraConfig["remote_model_name"]); remoteModel != "" {
		out.ModelName = remoteModel
	}
	override := applyGeneralClaudeOverrides(out, model.Parameters.ExtraConfig)
	if out.ModelName == "" {
		return nil, errors.New("通用智能体模型缺少模型名称")
	}
	if !override.baseConfigured {
		deriveGeneralClaudeEndpoint(out)
	} else if out.Provider == "" {
		out.Provider = string(modelprovider.ProviderAnthropic)
	}
	if out.BaseURL == "" {
		return nil, errors.New("通用智能体无法为当前模型推导 Anthropic 兼容接口：请在模型 extra_config 配置通用智能体兼容接口 Base URL；API key 会复用当前模型")
	}
	if out.Provider == "" {
		out.Provider = string(modelprovider.ProviderAnthropic)
	}
	if out.APIKey == "" {
		if !override.baseConfigured {
			return nil, errors.New("通用智能体模型缺少 API key")
		}
		out.AuthType = generalClaudeAuthHelper
		out.APIKeyHelper = fmt.Sprintf("printf %s", generalClaudeNoAuthKey)
	} else {
		out.AuthType = generalClaudeAuthAPIKey
	}
	return out, nil
}

type overrideStatus struct {
	configured     bool
	baseConfigured bool
}

func applyGeneralClaudeOverrides(cfg *LLMConfig, extra map[string]string) overrideStatus {
	modelName := overrideValue(extra, generalClaudeModelNameEnv, "general_agent_claude_model_name", "general_agent_claude_model", "claude_model_name")
	baseURL := overrideValue(extra, generalClaudeBaseURLEnv, "general_agent_claude_base_url", "general_agent_claude_endpoint", "claude_base_url")
	provider := overrideValue(extra, generalClaudeProviderEnv, "general_agent_claude_provider", "claude_provider")
	configured := modelName != "" || baseURL != "" || provider != ""
	if modelName != "" {
		cfg.ModelName = modelName
	}
	if baseURL != "" {
		cfg.BaseURL = baseURL
		cfg.Provider = string(modelprovider.ProviderAnthropic)
	}
	if provider != "" {
		cfg.Provider = provider
	}
	if configured && cfg.Provider == "" {
		cfg.Provider = string(modelprovider.ProviderAnthropic)
	}
	return overrideStatus{configured: configured, baseConfigured: baseURL != ""}
}

func overrideValue(extra map[string]string, _ string, keys ...string) string {
	for _, key := range keys {
		if value := strings.TrimSpace(extra[key]); value != "" {
			return value
		}
	}
	return ""
}

func deriveGeneralClaudeEndpoint(cfg *LLMConfig) {
	if cfg == nil {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	baseURL := strings.ToLower(strings.TrimSpace(cfg.BaseURL))
	switch {
	case provider == string(modelprovider.ProviderAnthropic) || provider == "claude" ||
		strings.Contains(baseURL, "anthropic") || strings.Contains(baseURL, "claude"):
		cfg.Provider = string(modelprovider.ProviderAnthropic)
	default:
		// Do not treat an arbitrary OpenAI-compatible chat endpoint as
		// Claude-compatible. For unknown/custom providers, require an explicit
		// general_agent_claude_base_url / CUSTOM_GENERAL_AGENT_CLAUDE_BASE_URL
		// override so the failure happens before the sidecar starts a run.
		cfg.BaseURL = ""
	}
}

func (s *Service) buildEffectiveQuery(ctx context.Context, req *types.QARequest) string {
	if req == nil {
		return ""
	}
	return req.Query
}

func (s *Service) buildHistory(ctx context.Context, req *types.QARequest, config *types.AgentConfig) []ChatHistoryMessage {
	if s.messageService == nil || !config.MultiTurnEnabled {
		return nil
	}
	turns := config.HistoryTurns
	if turns <= 0 {
		turns = 5
	}
	msgs, err := s.messageService.GetRecentMessagesBySession(ctx, req.Session.ID, turns*2+4)
	if err != nil {
		logger.Warnf(ctx, "general-agent load history failed: %v", err)
		return nil
	}
	sort.Slice(msgs, func(i, j int) bool {
		return msgs[i].CreatedAt.Before(msgs[j].CreatedAt)
	})
	out := make([]ChatHistoryMessage, 0, len(msgs))
	for _, msg := range msgs {
		if msg == nil || msg.ID == req.AssistantMessageID || msg.ID == req.UserMessageID {
			continue
		}
		role := strings.TrimSpace(msg.Role)
		if role != "user" && role != "assistant" {
			continue
		}
		content := strings.TrimSpace(msg.Content)
		if content == "" {
			continue
		}
		out = append(out, ChatHistoryMessage{
			Role:           role,
			Content:        content,
			MentionedItems: append([]types.MentionedItem(nil), msg.MentionedItems...),
			Images:         imageSpecs(msg.Images),
			Attachments:    attachmentSpecsWithoutContent(msg.Attachments),
		})
	}
	return out
}

func configuredLightweightSkillSelection(agent *types.CustomAgent) (string, []string) {
	if agent == nil {
		return "none", nil
	}
	if agent.Config.LightweightSkillsSelectionMode != "" {
		return agent.Config.LightweightSkillsSelectionMode, cloneStringSlice(agent.Config.SelectedLightweightSkills)
	}
	if agent.Config.SkillsSelectionMode != "" {
		return agent.Config.SkillsSelectionMode, cloneStringSlice(agent.Config.SelectedSkills)
	}
	return "none", nil
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

func (s *Service) professionalSkillSpecs(ctx context.Context, agent *types.CustomAgent) ([]ProfessionalSkillSpec, error) {
	mode, names := configuredProfessionalSkillSelection(agent)
	if mode != "all" && (mode != "selected" || len(names) == 0) {
		return nil, nil
	}
	packages, err := skillhub.ProfessionalPackages(ctx, names, mode == "all")
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

func (s *Service) buildVisibleContext(ctx context.Context, req *types.QARequest, config *types.AgentConfig, selectedSkillContext string) map[string]any {
	out := map[string]any{}
	if req == nil {
		return out
	}
	if req.CustomAgent != nil {
		out["agent"] = map[string]any{
			"id":                                  req.CustomAgent.ID,
			"name":                                req.CustomAgent.Name,
			"description":                         req.CustomAgent.Description,
			"avatar":                              req.CustomAgent.Avatar,
			"is_builtin":                          req.CustomAgent.IsBuiltin,
			"agent_mode":                          req.CustomAgent.Config.AgentMode,
			"agent_type":                          req.CustomAgent.Config.AgentType,
			"system_prompt":                       req.CustomAgent.Config.SystemPrompt,
			"system_prompt_template_id":           req.CustomAgent.Config.SystemPromptID,
			"model_id":                            req.CustomAgent.Config.ModelID,
			"rerank_model_id":                     req.CustomAgent.Config.RerankModelID,
			"kb_selection_mode":                   req.CustomAgent.Config.KBSelectionMode,
			"configured_knowledge_base_ids":       cloneStringSlice(req.CustomAgent.Config.KnowledgeBases),
			"configured_db_data_source_ids":       cloneStringSlice(req.CustomAgent.Config.DBDataSources),
			"mcp_selection_mode":                  req.CustomAgent.Config.MCPSelectionMode,
			"configured_mcp_service_ids":          cloneStringSlice(req.CustomAgent.Config.MCPServices),
			"skills_selection_mode":               req.CustomAgent.Config.SkillsSelectionMode,
			"configured_selected_skill_names":     cloneStringSlice(req.CustomAgent.Config.SelectedSkills),
			"lightweight_skills_selection_mode":   req.CustomAgent.Config.LightweightSkillsSelectionMode,
			"configured_lightweight_skill_names":  cloneStringSlice(req.CustomAgent.Config.SelectedLightweightSkills),
			"professional_skills_selection_mode":  req.CustomAgent.Config.ProfessionalSkillsSelectionMode,
			"configured_professional_skill_names": cloneStringSlice(req.CustomAgent.Config.SelectedProfessionalSkills),
			"image_upload_enabled":                req.CustomAgent.Config.ImageUploadEnabled,
			"audio_upload_enabled":                req.CustomAgent.Config.AudioUploadEnabled,
			"supported_file_types":                cloneStringSlice(req.CustomAgent.Config.SupportedFileTypes),
			"data_analysis_enabled":               req.CustomAgent.Config.DataAnalysisEnabled,
		}
	}
	out["current_turn"] = map[string]any{
		"user_request_verbatim":        req.Query,
		"quoted_context":               req.QuotedContext,
		"image_urls":                   cloneStringSlice(req.ImageURLs),
		"image_description":            req.ImageDescription,
		"attachments":                  attachmentSpecsWithoutContent(req.Attachments),
		"selected_chat_skill_names":    cloneStringSlice(req.SkillNames),
		"selected_chat_skill_context":  selectedSkillContext,
		"selected_knowledge_base_ids":  cloneStringSlice(req.KnowledgeBaseIDs),
		"selected_knowledge_file_ids":  cloneStringSlice(req.KnowledgeIDs),
		"web_search_requested_in_chat": req.WebSearchEnabled,
	}
	if config != nil {
		out["effective_configuration"] = map[string]any{
			"runtime_model_id":                 config.RuntimeModelID,
			"vlm_model_id":                     config.VLMModelID,
			"max_iterations":                   config.MaxIterations,
			"temperature":                      config.Temperature,
			"thinking":                         config.Thinking,
			"allowed_tools":                    cloneStringSlice(config.AllowedTools),
			"knowledge_base_ids":               cloneStringSlice(config.KnowledgeBases),
			"knowledge_file_ids":               cloneStringSlice(config.KnowledgeIDs),
			"database_data_source_ids":         cloneStringSlice(config.DBDataSources),
			"web_search_enabled":               config.WebSearchEnabled,
			"web_search_provider_id":           config.WebSearchProviderID,
			"web_search_max_results":           config.WebSearchMaxResults,
			"claude_sdk_web_search_enabled":    config.ClaudeSDKWebSearchEnabled,
			"web_fetch_enabled":                config.WebFetchEnabled,
			"web_fetch_top_n":                  config.WebFetchTopN,
			"multi_turn_enabled":               config.MultiTurnEnabled,
			"history_turns":                    config.HistoryTurns,
			"mcp_selection_mode":               config.MCPSelectionMode,
			"mcp_service_ids":                  cloneStringSlice(config.MCPServices),
			"skills_enabled":                   config.SkillsEnabled,
			"allowed_skill_names":              cloneStringSlice(config.AllowedSkills),
			"professional_skills_enabled":      config.ProfessionalSkillsEnabled,
			"allowed_professional_skill_names": cloneStringSlice(config.AllowedProfessionalSkills),
			"retrieve_kb_only_when_mentioned":  config.RetrieveKBOnlyWhenMentioned,
			"retain_retrieval_history":         config.RetainRetrievalHistory,
			"llm_call_timeout_seconds":         config.LLMCallTimeout,
			"embedding_top_k":                  config.EmbeddingTopK,
			"keyword_threshold":                config.KeywordThreshold,
			"vector_threshold":                 config.VectorThreshold,
			"rerank_top_k":                     config.RerankTopK,
			"rerank_threshold":                 config.RerankThreshold,
			"faq_priority_enabled":             config.FAQPriorityEnabled,
			"faq_direct_answer_threshold":      config.FAQDirectAnswerThreshold,
			"faq_score_boost":                  config.FAQScoreBoost,
			"artifacts_enabled":                config.EnableArtifacts,
			"artifact_return_policy": map[string]any{
				"max_artifact_count":            5,
				"total_return_size_limit_bytes": int64(128 * 1024 * 1024),
				"order":                         "important files first",
			},
		}
		out["visible_resources"] = s.visibleResources(ctx, config)
	}
	return out
}

type visibleDBSource struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Type           string `json:"type"`
	Status         string `json:"status"`
	QueryMode      string `json:"query_mode"`
	MaxRows        int    `json:"max_rows"`
	MaxScanRows    int    `json:"max_scan_rows"`
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
	if names := compactStrings(config.AllowedSkills); len(names) > 0 {
		var rows []visibleSkill
		if err := s.db.WithContext(ctx).Where("name IN ? AND enabled = ?", names, true).Find(&rows).Error; err == nil {
			out["skills"] = orderVisibleSkills(names, rows)
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
			"max_scan_rows":   row.MaxScanRows,
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
		Thinking:                    c.Thinking,
		AllowedTools:                cloneStringSlice(c.AllowedTools),
		KnowledgeBases:              cloneStringSlice(c.KnowledgeBases),
		KnowledgeIDs:                cloneStringSlice(c.KnowledgeIDs),
		DBDataSources:               cloneStringSlice(c.DBDataSources),
		WebSearchEnabled:            c.WebSearchEnabled,
		WebSearchProviderID:         c.WebSearchProviderID,
		WebSearchMaxResults:         c.WebSearchMaxResults,
		ClaudeSDKWebSearchEnabled:   c.ClaudeSDKWebSearchEnabled,
		WebFetchEnabled:             c.WebFetchEnabled,
		WebFetchTopN:                c.WebFetchTopN,
		MultiTurnEnabled:            c.MultiTurnEnabled,
		HistoryTurns:                c.HistoryTurns,
		MCPSelectionMode:            c.MCPSelectionMode,
		MCPServices:                 cloneStringSlice(c.MCPServices),
		SkillsEnabled:               c.SkillsEnabled,
		AllowedSkills:               cloneStringSlice(c.AllowedSkills),
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
		FAQScoreBoost:               c.FAQScoreBoost,
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
	return prompt
}

func toolCallbackURL() string {
	if v := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_TOOL_CALLBACK_URL")); v != "" {
		return v
	}
	return defaultToolCallbackURL
}

func generalAgentToolExecTimeout(config *types.AgentConfig) time.Duration {
	timeout := envDurationSeconds("CUSTOM_GENERAL_AGENT_TOOL_EXEC_TIMEOUT_SEC", 15*time.Minute)
	if config != nil && config.LLMCallTimeout > 0 {
		configured := time.Duration(config.LLMCallTimeout) * time.Second
		if configured > timeout {
			timeout = configured
		}
	}
	return timeout
}

func (s *Service) persistArtifacts(ctx context.Context, client *Client, runID string, req *types.QARequest, artifacts []SidecarArtifact) ([]ArtifactResult, error) {
	artifacts = dedupeSidecarArtifactsByFilenameKeepLast(artifacts)
	if len(artifacts) == 0 {
		return nil, nil
	}
	if client == nil {
		client = s.client
	}
	if s.db == nil {
		return nil, errors.New("artifact database is not initialized")
	}
	userID, _ := types.UserIDFromContext(ctx)
	root := filepath.Join(s.artifactRoot, req.Session.ID, runID)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	out := make([]ArtifactResult, 0, len(artifacts))
	for _, item := range artifacts {
		if item.FileToken == "" {
			continue
		}
		data, err := client.Download(ctx, runID, item.FileToken)
		if err != nil {
			return out, err
		}
		name := safeFileName(item.FileName)
		if name == "" {
			name = item.FileToken
		}
		sum := sha256.Sum256(data)
		sha := hex.EncodeToString(sum[:])
		fileType := strings.TrimPrefix(strings.ToLower(item.FileType), ".")
		if fileType == "" {
			fileType = strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
		}
		tokenDir := filepath.Join(root, safeFileName(item.FileToken))
		if err := os.MkdirAll(tokenDir, 0o755); err != nil {
			return out, err
		}
		path := filepath.Join(tokenDir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return out, err
		}
		contentType := item.ContentType
		if contentType == "" {
			contentType = mime.TypeByExtension("." + fileType)
		}
		if contentType == "" {
			contentType = "application/octet-stream"
		}
		row := &Artifact{
			TenantID:    tenantIDFromContext(ctx),
			UserID:      userID,
			RunID:       runID,
			SessionID:   req.Session.ID,
			MessageID:   req.AssistantMessageID,
			FileToken:   item.FileToken,
			FilePath:    path,
			FileName:    name,
			FileType:    fileType,
			FileSize:    int64(len(data)),
			SHA256:      sha,
			ContentType: contentType,
		}
		if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
			return out, err
		}
		out = append(out, ArtifactResult{
			ArtifactID:  row.ID,
			FileName:    row.FileName,
			FileType:    row.FileType,
			FileSize:    row.FileSize,
			SHA256:      row.SHA256,
			DownloadURL: "/api/v1/custom/general-agent/artifacts/" + row.ID + "/download",
		})
	}
	return out, nil
}

func dedupeSidecarArtifactsByFilenameKeepLast(artifacts []SidecarArtifact) []SidecarArtifact {
	if len(artifacts) <= 1 {
		return artifacts
	}
	seen := make(map[string]bool, len(artifacts))
	out := make([]SidecarArtifact, 0, len(artifacts))
	for i := len(artifacts) - 1; i >= 0; i-- {
		item := artifacts[i]
		key := item.FileName
		if key == "" {
			key = item.FileToken
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func buildArtifactToolResult(result *ChatResult, artifacts []ArtifactResult, persistErr error) (map[string]interface{}, string, bool) {
	if result == nil {
		result = &ChatResult{}
	}
	notice := combinedArtifactNotice(result.ArtifactNotice, persistErr)
	if len(artifacts) == 0 && notice == "" {
		return nil, "", false
	}
	data := map[string]interface{}{
		"display_type":            displayTypeArtifacts,
		"artifacts":               artifactResultMaps(artifacts),
		"notice":                  notice,
		"artifact_original_count": result.ArtifactOriginalCount,
		"artifact_returned_count": result.ArtifactReturnedCount,
		"artifact_dropped_count":  result.ArtifactDroppedCount,
		"artifact_returned_size":  result.ArtifactReturnedSize,
		"artifact_limit_bytes":    result.ArtifactLimitBytes,
	}
	if persistErr != nil {
		data["persist_failed"] = true
		data["persist_error"] = sanitizeArtifactPersistError(persistErr)
	}
	return data, generalArtifactOutput(notice, len(artifacts)), true
}

func artifactResultMaps(artifacts []ArtifactResult) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(artifacts))
	for _, item := range artifacts {
		out = append(out, map[string]interface{}{
			"artifact_id":  item.ArtifactID,
			"filename":     item.FileName,
			"file_type":    item.FileType,
			"file_size":    item.FileSize,
			"sha256":       item.SHA256,
			"download_url": item.DownloadURL,
		})
	}
	return out
}

func combinedArtifactNotice(base string, persistErr error) string {
	notice := strings.TrimSpace(base)
	if persistErr == nil {
		return notice
	}
	persistNotice := "产物文件已生成，但保存下载记录失败，暂时无法提供下载链接。请稍后重试或联系管理员查看后端日志。"
	if msg := sanitizeArtifactPersistError(persistErr); msg != "" {
		persistNotice += "错误：" + msg
	}
	if notice == "" {
		return persistNotice
	}
	return notice + "\n" + persistNotice
}

func sanitizeArtifactPersistError(err error) string {
	if err == nil {
		return ""
	}
	msg := strings.TrimSpace(err.Error())
	msg = strings.Join(strings.Fields(msg), " ")
	const maxLen = 240
	runes := []rune(msg)
	if len(runes) > maxLen {
		msg = string(runes[:maxLen]) + "..."
	}
	return msg
}

func generalArtifactOutput(notice string, returned int) string {
	notice = strings.TrimSpace(notice)
	if notice != "" {
		if returned > 0 {
			return "已生成产物。" + notice
		}
		return notice
	}
	return "已生成产物"
}

func safeFileName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	return name
}
