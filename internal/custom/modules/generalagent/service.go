package generalagent

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
	"github.com/Tencent/WeKnora/internal/custom/modules/skillhub"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	modelprovider "github.com/Tencent/WeKnora/internal/models/provider"
	"github.com/Tencent/WeKnora/internal/models/rerank"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
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
	db                 *gorm.DB
	sessionService     interfaces.SessionService
	agentService       interfaces.AgentService
	messageService     interfaces.MessageService
	modelService       interfaces.ModelService
	knowledgeService   interfaces.KnowledgeService
	fileService        interfaces.FileService
	dbAnalytics        *dbanalytics.Service
	client             *Client
	documentClient     *Client
	artifactRoot       string
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
	root := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ARTIFACT_ROOT"))
	if root == "" {
		root = filepath.Join("custom", "general-agent-artifacts")
	}
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
		client:           NewClientFromEnv(),
		documentClient:   NewDocumentProcessingClientFromEnv(),
		artifactRoot:     root,
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
	if err := applyGeneralAgentMigrations(ctx, s.db); err != nil {
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
	if err := s.migrateLegacyArtifacts(ctx); err != nil {
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

func (s *Service) ArtifactStore() *artifactstore.Store {
	if s == nil {
		return nil
	}
	return s.artifactStore
}

func (s *Service) Run(ctx context.Context, req *types.QARequest, eventBus *event.EventBus) (runErr error) {
	if req == nil || req.CustomAgent == nil {
		return errors.New("通用智能体需要有效的智能体配置")
	}
	if !types.IsClaudeSDKAgentType(req.CustomAgent.Config.AgentType) {
		return fmt.Errorf("invalid agent_type for general-agent runner: %s", req.CustomAgent.Config.AgentType)
	}
	originalInputStorageURLs := make(map[string]struct{}, len(req.OriginalInputFiles)+len(req.KnowledgeIDs))
	for _, item := range req.OriginalInputFiles {
		if storageURL := strings.TrimSpace(item.StorageURL); storageURL != "" {
			originalInputStorageURLs[storageURL] = struct{}{}
		}
	}
	defer s.cleanupOriginalInputTransferObjects(ctx, originalInputStorageURLs)
	if s.sessionService == nil || s.agentService == nil || s.modelService == nil || s.client == nil {
		return errors.New("通用智能体后端依赖未初始化")
	}

	start := time.Now()
	sessionID := req.Session.ID
	runID := uuid.New().String()
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
	if agentConfig.EnableArtifacts && strings.TrimSpace(agentConfig.VLMModelID) != "" {
		registry = &visionRegistry{AgentToolRegistry: registry, models: s.modelService, modelID: agentConfig.VLMModelID}
	}

	llm, err := s.resolveLLMConfig(ctx, agentConfig)
	if err != nil {
		return err
	}
	query := s.buildEffectiveQuery(ctx, req)
	lightweightSkills := make([]LightweightSkillSpec, 0, len(agentConfig.RuntimeLightweightSkills))
	for _, skill := range agentConfig.RuntimeLightweightSkills {
		lightweightSkills = append(lightweightSkills, LightweightSkillSpec{Key: skill.Key, Name: skill.Name, Description: skill.Description, SelectedByUser: skill.SelectedByUser})
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
	for _, item := range originalInputFiles {
		if storageURL := strings.TrimSpace(item.StorageURL); storageURL != "" {
			originalInputStorageURLs[storageURL] = struct{}{}
		}
	}
	userID := types.SessionOwnerIDFromContext(ctx)
	active := &activeRun{
		chatModel: chatModel, agentConfig: agentConfig,
		runID:              runID,
		client:             sidecarClient,
		originalInputFiles: originalInputFilesByID(originalInputFiles),
		ctx:                conversationmemory.WithLiveTools(ctx),
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
	defer func() {
		if runErr != nil {
			runErr = &executionError{cause: runErr, steps: active.snapshotSteps("")}
		}
	}()

	history, durableUserContext := s.buildHistory(ctx, req, agentConfig)
	evalObservability := false
	if manager := langfuse.GetManager(); manager != nil {
		evalObservability = manager.CaptureContent() && manager.EnabledFor(ctx)
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
			renderSystemPrompt(ctx, agentConfig.ResolveSystemPrompt(agentConfig.WebSearchEnabled), agentConfig.WebSearchEnabled),
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
		ToolCallbackAPIKey:      strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_API_KEY")),
		ArtifactUploadURL:       artifactUploadURL(),
		EnableArtifacts:         agentConfig.EnableArtifacts,
		EvalObservability:       evalObservability,
	}

	var promptLayoutSpan *langfuse.Span
	if evalObservability {
		historyRoleCounts := map[string]int{}
		historyRoleChars := map[string]int{}
		for _, message := range history {
			historyRoleCounts[message.Role]++
			historyRoleChars[message.Role] += len([]rune(message.Content))
		}
		ctx, promptLayoutSpan = langfuse.GetManager().StartSpan(ctx, langfuse.SpanOptions{
			Name: "general_agent.prompt_layout",
			Input: map[string]interface{}{
				"configured_history_rounds": agentConfig.HistoryTurns,
				"actual_history_messages":   len(history),
				"history_count_by_role":     historyRoleCounts,
				"history_chars_by_role":     historyRoleChars,
				"archive_chars":             len([]rune(durableUserContext)),
				"current_query_chars":       len([]rune(query)),
				"system_prompt_chars":       len([]rune(payload.SystemPrompt)),
				"tool_count":                len(payload.Tools),
			},
			Metadata: map[string]interface{}{"eval_only": true},
		})
	}

	fallbackAnswerID := "general-answer-" + runID
	lastAnswerID := ""
	lastAnswerDone := false
	var streamed strings.Builder
	projection := &answerStream{refs: active.snapshotSourceReferences, emit: func(text string) {
		eventBus.Emit(ctx, event.Event{ID: fallbackAnswerID, Type: event.EventAgentFinalAnswer, SessionID: sessionID, RequestID: req.RequestID, Data: event.AgentFinalAnswerData{Content: text}})
	}}
	result, err := sidecarClient.ChatStream(ctx, payload, func(evt StreamEvent) {
		if evt.Type == "answer_delta" {
			captureSidecarAnswerEvent(fallbackAnswerID, evt, &streamed, &lastAnswerID, &lastAnswerDone)
			projection.push(evt.Content, evt.Done)
			return
		}
		s.emitSidecarEvent(ctx, eventBus, sessionID, fallbackAnswerID, evt, &streamed, &lastAnswerID, &lastAnswerDone, active)
	})
	allRefs := active.snapshotSourceReferences()
	if err != nil {
		if promptLayoutSpan != nil {
			promptLayoutSpan.Finish(nil, map[string]interface{}{"eval_only": true}, err)
		}
		return err
	}
	if result == nil {
		err = fmt.Errorf("智能体最终结果为空")
		if promptLayoutSpan != nil {
			promptLayoutSpan.Finish(nil, map[string]interface{}{"eval_only": true}, err)
		}
		return err
	}
	if promptLayoutSpan != nil {
		promptLayoutSpan.Finish(
			result.PromptObservation,
			map[string]interface{}{"eval_only": true},
			nil,
		)
	}
	if streamed.Len() == 0 {
		projection.push(result.Answer, true)
	} else {
		projection.push("", true)
	}
	finalAnswer := projection.output.String()
	_, citedRefs, citationReport := sourcerefs.FilterAnswerCitations(finalAnswer, allRefs)
	if citationReport.EvidenceAvailableUncited {
		logger.Warnf(ctx, "general-agent answer omitted all current source handles: %d available", len(allRefs))
	}
	eventBus.Emit(ctx, event.Event{ID: fallbackAnswerID, Type: event.EventAgentFinalAnswer, SessionID: sessionID, RequestID: req.RequestID, Data: event.AgentFinalAnswerData{Done: true}})

	_, err = s.persistArtifacts(ctx, sidecarClient, result.RunID, req, result.Artifacts)
	if err != nil {
		logger.Warnf(ctx, "general-agent persist artifacts failed: %v", err)
		return fmt.Errorf("智能体产物未能安全写入对象存储，本次运行不能标记完成: %w", err)
	}

	// finalAnswer and citedRefs are the exact pair already emitted above.
	steps := active.snapshotSteps(finalAnswer)
	eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		RequestID: req.RequestID,
		Data: event.AgentCompleteData{
			SessionID:                  sessionID,
			FinalAnswer:                finalAnswer,
			KnowledgeRefs:              citedRefs,
			KnowledgeRefsAuthoritative: true,
			AgentSteps:                 steps,
			TotalSteps:                 len(steps),
			TotalDurationMs:            time.Since(start).Milliseconds(),
			MessageID:                  req.AssistantMessageID,
			RequestID:                  req.RequestID,
		},
	})
	return nil
}

func captureSidecarAnswerEvent(
	fallbackAnswerID string,
	evt StreamEvent,
	streamed *strings.Builder,
	lastAnswerID *string,
	lastAnswerDone *bool,
) {
	answerID := strings.TrimSpace(evt.ID)
	if answerID == "" {
		answerID = fallbackAnswerID
	}
	if streamed != nil && evt.Content != "" {
		streamed.WriteString(evt.Content)
	}
	if lastAnswerID != nil {
		*lastAnswerID = answerID
	}
	if lastAnswerDone != nil {
		*lastAnswerDone = evt.Done
	}
}

func (s *Service) emitSidecarEvent(ctx context.Context, eventBus *event.EventBus, sessionID, fallbackAnswerID string, evt StreamEvent, streamed *strings.Builder, lastAnswerID *string, lastAnswerDone *bool, active *activeRun) {
	switch evt.Type {
	case "answer_delta":
		captureSidecarAnswerEvent(fallbackAnswerID, evt, streamed, lastAnswerID, lastAnswerDone)
		answerID := fallbackAnswerID
		if lastAnswerID != nil && strings.TrimSpace(*lastAnswerID) != "" {
			answerID = *lastAnswerID
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
	case "runtime_tool":
		if active != nil {
			active.recordRuntimeToolEvent(ctx, eventBus, sidecarProgressDataFromEvent(evt))
		}
	case "progress":
		progress := sidecarProgressDataFromEvent(evt)
		if progress.Content == "" {
			return
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
				Metadata:   progress.Metadata,
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
	Metadata   map[string]any
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
			out.Metadata = data
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
	out, err := runtimeLLMConfigFromModel(model)
	if err != nil {
		return nil, err
	}
	if err := secutils.ValidateURLForSSRF(out.BaseURL); err != nil {
		return nil, fmt.Errorf("通用智能体接口地址未通过 SSRF 校验: %w", err)
	}
	return out, nil
}

func generalClaudeLLMConfigFromModel(model *types.Model) (*LLMConfig, error) {
	if !model.IsInteractiveChatModel() {
		return nil, errors.New("通用智能体需要对话模型")
	}
	out := &LLMConfig{
		ReasoningEffort: strings.TrimSpace(model.Parameters.ExtraConfig["reasoning_effort"]),
		SupportsVision:  model.Parameters.SupportsVision,
		ModelName:       strings.TrimSpace(model.Name),
		BaseURL:         strings.TrimSpace(model.Parameters.BaseURL),
		APIKey:          strings.TrimSpace(model.Parameters.APIKey),
		Provider:        strings.TrimSpace(model.Parameters.Provider),
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

func (s *Service) buildHistory(
	ctx context.Context,
	req *types.QARequest,
	config *types.AgentConfig,
) ([]ChatHistoryMessage, string) {
	if s.messageService == nil || !config.MultiTurnEnabled {
		return nil, ""
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
		logger.Warnf(ctx, "general-agent load history failed: %v", err)
		return nil, ""
	}
	return buildGeneralAgentHistory(msgs, turns, req.UserMessageID, req.AssistantMessageID)
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
		if pair.Assistant == nil {
			continue
		}
		answer := conversationmemory.AssistantRecord(pair.Assistant)
		if answer != "" {
			out = append(out, ChatHistoryMessage{
				Role:     "assistant",
				Content:  answer,
				SourceID: conversationmemory.AssistantSourceID(pair.Assistant.ID),
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

func (s *Service) buildVisibleContext(ctx context.Context, req *types.QARequest, config *types.AgentConfig) map[string]any {
	out := map[string]any{}
	if req == nil {
		return out
	}
	if req.CustomAgent != nil {
		out["agent"] = map[string]any{
			"id":                            req.CustomAgent.ID,
			"name":                          req.CustomAgent.Name,
			"description":                   req.CustomAgent.Description,
			"avatar":                        req.CustomAgent.Avatar,
			"is_builtin":                    req.CustomAgent.IsBuiltin,
			"agent_mode":                    req.CustomAgent.Config.AgentMode,
			"agent_type":                    req.CustomAgent.Config.AgentType,
			"system_prompt_template_id":     req.CustomAgent.Config.SystemPromptID,
			"model_id":                      req.CustomAgent.Config.ModelID,
			"rerank_model_id":               req.CustomAgent.Config.RerankModelID,
			"kb_selection_mode":             req.CustomAgent.Config.KBSelectionMode,
			"configured_knowledge_base_ids": cloneStringSlice(req.CustomAgent.Config.KnowledgeBases),
			"configured_db_data_source_ids": cloneStringSlice(req.CustomAgent.Config.DBDataSources),
			"mcp_selection_mode":            req.CustomAgent.Config.MCPSelectionMode,
			"configured_mcp_service_ids":    cloneStringSlice(req.CustomAgent.Config.MCPServices),
			"image_upload_enabled":          req.CustomAgent.Config.ImageUploadEnabled,
			"audio_upload_enabled":          req.CustomAgent.Config.AudioUploadEnabled,
			"supported_file_types":          cloneStringSlice(req.CustomAgent.Config.SupportedFileTypes),
			"data_analysis_enabled":         req.CustomAgent.Config.DataAnalysisEnabled,
		}
	}
	out["current_turn"] = map[string]any{
		"quoted_context":               req.QuotedContext,
		"image_urls":                   cloneStringSlice(req.ImageURLs),
		"image_description":            req.ImageDescription,
		"attachments":                  attachmentSpecsWithoutContent(req.Attachments),
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
			"artifacts_enabled":                config.EnableArtifacts,
			"artifact_return_policy":           artifactReturnPolicy(),
		}
		out["visible_resources"] = s.visibleResources(ctx, config)
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
		ClaudeSDKWebSearchEnabled:   c.ClaudeSDKWebSearchEnabled,
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

func toolCallbackURL() string {
	if v := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_TOOL_CALLBACK_URL")); v != "" {
		return v
	}
	return defaultToolCallbackURL
}

func artifactUploadURL() string {
	if v := strings.TrimSpace(os.Getenv("CUSTOM_GENERAL_AGENT_ARTIFACT_UPLOAD_URL")); v != "" {
		return v
	}
	return defaultArtifactUploadURL
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

func (s *Service) persistArtifacts(ctx context.Context, _ *Client, runID string, req *types.QARequest, artifacts []SidecarArtifact) ([]ArtifactResult, error) {
	artifacts = dedupeSidecarArtifactsByFilenameKeepLast(artifacts)
	if len(artifacts) == 0 {
		return nil, nil
	}
	if s.db == nil || s.artifactStore == nil {
		return nil, errors.New("private artifact persistence is not initialized")
	}
	userID := types.SessionOwnerIDFromContext(ctx)
	out := make([]ArtifactResult, 0, len(artifacts))
	for _, item := range artifacts {
		if !item.Persisted || strings.TrimSpace(item.ArtifactID) == "" {
			return out, fmt.Errorf("sidecar returned an artifact before private object persistence completed")
		}
		var row Artifact
		err := s.db.WithContext(ctx).
			Where(
				"id = ? AND tenant_id = ? AND user_id = ? AND session_id = ? AND message_id = ? AND run_id = ? AND file_token = ? AND storage_state = ?",
				item.ArtifactID,
				tenantIDFromContext(ctx),
				userID,
				req.Session.ID,
				req.AssistantMessageID,
				runID,
				item.FileToken,
				artifactStorageStateReady,
			).
			First(&row).Error
		if err != nil {
			return out, fmt.Errorf("load privately persisted artifact %s: %w", item.ArtifactID, err)
		}
		if !s.artifactStore.Owns(row.FilePath) {
			return out, fmt.Errorf("artifact %s is outside the private object namespace", row.ID)
		}
		if row.FileName != item.FileName || row.FileSize != item.FileSize ||
			!strings.EqualFold(row.SHA256, item.SHA256) {
			return out, fmt.Errorf("artifact %s result metadata does not match the committed object", row.ID)
		}
		out = append(out, ArtifactResult{
			ArtifactID:  row.ID,
			FileName:    row.FileName,
			FileType:    row.FileType,
			FileSize:    row.FileSize,
			SHA256:      row.SHA256,
			DownloadURL: item.DownloadURL,
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

func safeFileName(name string) string {
	name = strings.TrimSpace(filepath.Base(name))
	name = strings.ReplaceAll(name, "\\", "_")
	name = strings.ReplaceAll(name, "/", "_")
	return name
}
