package session

import (
	"context"
	"encoding/json"
	"fmt"
	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/custom/modules/usererrors"
	"github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/gin-gonic/gin"
)

// qaRequestContext holds all the common data needed for QA requests
type qaRequestContext struct {
	ctx                       context.Context
	c                         *gin.Context
	sessionID                 string
	requestID                 string
	receivedAt                time.Time // Wall-clock time the handler started processing the request
	query                     string
	session                   *types.Session
	customAgent               *types.CustomAgent
	assistantMessage          *types.Message
	knowledgeBaseIDs          []string
	knowledgeIDs              []string
	sessionUploadKnowledgeIDs []string
	tagScopes                 []types.TagScope
	tagIDs                    []string
	mcpServiceIDs             []string
	skillNames                []string
	professionalSkillNames    []string
	summaryModelID            string
	webSearchEnabled          bool
	enableMemory              bool // Whether memory feature is enabled
	mentionedItems            types.MentionedItems
	effectiveTenantID         uint64                    // when using shared agent, tenant ID for model/KB/MCP resolution; 0 = use context tenant
	images                    []ImageAttachment         // Uploaded images with analysis text
	userMessageID             string                    // Created user message ID (populated after createUserMessage)
	channel                   string                    // Source channel: "web", "api", "im", etc.
	attachments               types.MessageAttachments  // Processed file attachments
	originalInputFiles        []types.OriginalInputFile // Agent originals; never parsed or vectorized
	chatQueueTicket           ChatQueueTicket           // Conversation-level model-pool admission lease

	// Snapshot of the request fields needed to persist the input-bar state
	// for session restoration. Kept verbatim from the request so we record
	// what the user had selected on the UI (not server-side resolutions).
	reqAgentEnabled bool
	reqAgentID      string
}

func attachmentFileTypeAllowed(fileName string, supportedFileTypes []string) bool {
	if len(supportedFileTypes) == 0 {
		return true
	}
	lastDot := strings.LastIndex(fileName, ".")
	if lastDot < 0 || lastDot == len(fileName)-1 {
		return false
	}
	ext := strings.ToLower(strings.TrimSpace(fileName[lastDot+1:]))
	if ext == "" {
		return false
	}
	for _, supported := range supportedFileTypes {
		normalized := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(supported), "."))
		if normalized != "" && normalized == ext {
			return true
		}
	}
	return false
}

func appendUniqueMessageAttachments(dst, src types.MessageAttachments) types.MessageAttachments {
	seen := make(map[string]bool, len(dst)+len(src))
	for _, item := range dst {
		key := item.UploadID + "\x00" + item.KnowledgeID + "\x00" + item.FileName
		seen[key] = true
	}
	for _, item := range src {
		key := item.UploadID + "\x00" + item.KnowledgeID + "\x00" + item.FileName
		if !seen[key] {
			dst = append(dst, item)
			seen[key] = true
		}
	}
	return dst
}

func appendUniqueOriginalInputs(dst, src []types.OriginalInputFile) []types.OriginalInputFile {
	seen := make(map[string]bool, len(dst)+len(src))
	for _, item := range dst {
		seen[item.ID] = true
	}
	for _, item := range src {
		if item.ID == "" || seen[item.ID] {
			continue
		}
		dst = append(dst, item)
		seen[item.ID] = true
	}
	return dst
}

// buildQARequest converts the qaRequestContext into a types.QARequest for service invocation.
func (rc *qaRequestContext) buildQARequest() *types.QARequest {
	imageURLs, imageDescription := extractImageURLsAndOCRText(rc.images)
	return &types.QARequest{
		Session:                   rc.session,
		RequestID:                 rc.requestID,
		Query:                     rc.query,
		AssistantMessageID:        rc.assistantMessage.ID,
		SummaryModelID:            effectiveRequestModelID(rc),
		CustomAgent:               rc.customAgent,
		KnowledgeBaseIDs:          rc.knowledgeBaseIDs,
		KnowledgeIDs:              rc.knowledgeIDs,
		SessionUploadKnowledgeIDs: append([]string(nil), rc.sessionUploadKnowledgeIDs...),
		TagScopes:                 rc.tagScopes,
		MCPServiceIDs:             rc.mcpServiceIDs,
		SkillNames:                rc.skillNames,
		ProfessionalSkillNames:    append([]string(nil), rc.professionalSkillNames...),
		ImageURLs:                 imageURLs,
		ImageDescription:          imageDescription,
		UserMessageID:             rc.userMessageID,
		WebSearchEnabled:          rc.webSearchEnabled,
		EnableMemory:              rc.enableMemory,
		Attachments:               rc.attachments,
		OriginalInputFiles:        append([]types.OriginalInputFile(nil), rc.originalInputFiles...),
	}
}

// parseQARequest parses and validates a QA request, returns the request context
func (h *Handler) parseQARequest(c *gin.Context, logPrefix string) (*qaRequestContext, *CreateKnowledgeQARequest, error) {
	receivedAt := time.Now()
	ctx := logger.CloneContext(c.Request.Context())
	requestID := secutils.SanitizeForLog(c.GetString(types.RequestIDContextKey.String()))
	logger.Infof(ctx, "[%s] TTFB:start request_id=%s received_at=%d",
		logPrefix, requestID, receivedAt.UnixMilli())

	// Get session ID from URL parameter
	sessionID := secutils.SanitizeForLog(c.Param("session_id"))
	if sessionID == "" {
		logger.Error(ctx, "Session ID is empty")
		return nil, nil, errors.NewBadRequestError(errors.ErrInvalidSessionID.Error())
	}

	// Parse request body
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	var request CreateKnowledgeQARequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		return nil, nil, errors.NewBadRequestError(err.Error())
	}

	// Validate query content
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		return nil, nil, errors.NewBadRequestError("Query content cannot be empty")
	}
	if len(request.Images) > 0 || len(request.AttachmentUploads) > 0 {
		return nil, nil, errors.NewBadRequestError("Upload files and images through the session upload endpoint, then submit upload_ids")
	}

	// Log request details
	if requestJSON, err := json.Marshal(request); err == nil {
		logger.Infof(ctx, "[%s] Request: session_id=%s, request=%s",
			logPrefix, sessionID, secutils.SanitizeForLog(secutils.CompactImageDataURLForLog(string(requestJSON))))
	}

	// Get session
	session, err := h.sessionService.GetSession(ctx, sessionID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get session, session ID: %s, error: %v", sessionID, err)
		return nil, nil, errors.NewNotFoundError("Session not found")
	}

	// Get custom agent if agent_id is provided. Backend resolves shared agent from share relation (no client-provided tenant).
	customAgent, effectiveTenantID := h.resolveAgent(ctx, c, request.AgentID)

	// Merge @mentioned items into knowledge_base_ids and knowledge_ids
	kbIDs, knowledgeIDs := mergeKnowledgeTargets(request.KnowledgeBaseIDs, request.KnowledgeIds, request.MentionedItems)

	// The built-in wiki fixer is invoked from a KB page, not from a tenant's
	// regular agent picker. When the KB is shared, run it in the source tenant
	// only if the caller has edit permission, so KB-scoped models/tools resolve
	// without granting viewers write capability.
	if customAgent != nil && customAgent.ID == types.BuiltinWikiFixerID {
		if scopedAgent, scopedTenantID := h.resolveWikiFixerTenantScope(
			ctx,
			customAgent,
			c.GetUint64(types.TenantIDContextKey.String()),
			types.TenantRoleFromContext(ctx),
			kbIDs,
		); scopedTenantID != 0 {
			customAgent = scopedAgent
			effectiveTenantID = scopedTenantID
		}
	}

	// Log merge results for debugging
	logger.Infof(ctx, "[%s] @mention merge: request.KnowledgeBaseIDs=%v, request.MentionedItems=%d, merged kbIDs=%v, merged knowledgeIDs=%v",
		logPrefix, request.KnowledgeBaseIDs, len(request.MentionedItems), kbIDs, knowledgeIDs)

	// Manual originals are accepted by the session upload endpoint before chat.
	// The request contains stable handles, never a synchronous document parse.
	var processedAttachments types.MessageAttachments
	var originalInputFiles []types.OriginalInputFile
	var sessionUploadKnowledgeIDs []string
	// Staged sources use the same retrieval and paged tools as knowledge files.
	if len(request.UploadIDs) > 0 {
		if h.uploadResolver == nil {
			return nil, nil, errors.NewInternalServerError("chat uploads are unavailable")
		}
		attachments, targets, err := h.uploadResolver.Resolve(ctx, sessionID, request.UploadIDs)
		if err != nil {
			return nil, nil, errors.NewBadRequestError(err.Error())
		}
		processedAttachments = append(processedAttachments, attachments...)
		sessionUploadKnowledgeIDs = append(sessionUploadKnowledgeIDs, targets...)
		knowledgeIDs = dedupRequestStrings(append(knowledgeIDs, targets...))
	}
	if len(request.InputFileIDs) > 0 {
		if !request.AgentEnabled {
			return nil, nil, errors.NewBadRequestError("input_file_ids are available only for Agent conversations")
		}
		if h.uploadResolver == nil {
			return nil, nil, errors.NewInternalServerError("chat uploads are unavailable")
		}
		attachments, originals, err := h.uploadResolver.ResolveOriginalInputs(ctx, sessionID, request.InputFileIDs)
		if err != nil {
			return nil, nil, errors.NewBadRequestError(err.Error())
		}
		processedAttachments = append(processedAttachments, attachments...)
		originalInputFiles = append(originalInputFiles, originals...)
	}

	if h.uploadResolver != nil && customAgent != nil && customAgent.Config.MultiTurnEnabled {
		historyTargets, err := h.uploadResolver.HistoryTargets(ctx, sessionID)
		if err != nil {
			return nil, nil, errors.NewBadRequestError(err.Error())
		}
		knowledgeIDs = dedupRequestStrings(append(knowledgeIDs, historyTargets...))
		sessionUploadKnowledgeIDs = append(sessionUploadKnowledgeIDs, historyTargets...)
		_, historyOriginals, err := h.uploadResolver.HistoryOriginalInputs(ctx, sessionID)
		if err != nil {
			return nil, nil, errors.NewBadRequestError(err.Error())
		}
		originalInputFiles = appendUniqueOriginalInputs(originalInputFiles, historyOriginals)
	}

	// Resolve enable_memory:
	//   1. Explicit value in request → honour it. Used by embedded mode
	//      (force false) and by older clients still sending the literal bool.
	//   2. Not set → fall back to the calling user's stored preference.
	//      The toggle is persisted server-side per user (see PUT
	//      /auth/me/preferences); this is the canonical path for the
	//      normal logged-in web UI now that it no longer sends the field.
	//   3. No user / no preference → false. API-key-only callers never
	//      had memory enabled in practice, keep that behaviour.
	enableMemory := h.resolveEnableMemory(ctx, request.EnableMemory)

	tagScopes := mergeTagScopesFromRequestIDs(
		tagScopesFromMentionedItems(request.MentionedItems),
		dedupRequestStrings(request.TagIDs),
		secutils.SanitizeForLogArray(kbIDs),
	)
	tagIDs := dedupRequestStrings(append(request.TagIDs, mentionedIDsByType(request.MentionedItems, "tag")...))
	mcpServiceIDs := dedupRequestStrings(append(request.MCPServiceIDs, mentionedIDsByType(request.MentionedItems, "mcp")...))
	skillNames := dedupRequestStrings(append(request.SkillNames, mentionedIDsByType(request.MentionedItems, "skill")...))
	professionalSkillNames := dedupRequestStrings(request.ProfessionalSkillNames)

	// Build request context
	reqCtx := &qaRequestContext{
		ctx:         ctx,
		c:           c,
		sessionID:   sessionID,
		requestID:   requestID,
		receivedAt:  receivedAt,
		query:       request.Query,
		session:     session,
		customAgent: customAgent,
		assistantMessage: &types.Message{
			SessionID:   sessionID,
			Role:        "assistant",
			RequestID:   c.GetString(types.RequestIDContextKey.String()),
			IsCompleted: false,
			AgentMode:   request.AgentEnabled,
			Channel:     request.Channel,
		},
		knowledgeBaseIDs:          secutils.SanitizeForLogArray(kbIDs),
		knowledgeIDs:              secutils.SanitizeForLogArray(knowledgeIDs),
		tagScopes:                 tagScopes,
		tagIDs:                    secutils.SanitizeForLogArray(tagIDs),
		mcpServiceIDs:             secutils.SanitizeForLogArray(mcpServiceIDs),
		skillNames:                secutils.SanitizeForLogArray(skillNames),
		professionalSkillNames:    secutils.SanitizeForLogArray(professionalSkillNames),
		summaryModelID:            secutils.SanitizeForLog(request.SummaryModelID),
		webSearchEnabled:          request.WebSearchEnabled,
		enableMemory:              enableMemory,
		mentionedItems:            convertMentionedItems(request.MentionedItems),
		effectiveTenantID:         effectiveTenantID,
		images:                    request.Images,
		channel:                   request.Channel,
		attachments:               processedAttachments,
		originalInputFiles:        originalInputFiles,
		sessionUploadKnowledgeIDs: dedupRequestStrings(sessionUploadKnowledgeIDs),
		reqAgentEnabled:           request.AgentEnabled,
		reqAgentID:                request.AgentID,
	}

	return reqCtx, &request, nil
}

// resolveEnableMemory decides whether the memory pipeline runs for this
// request. See the call-site comment in parseQARequest for the resolution
// order. Lookup errors are logged but never propagate — a failure to read
// the user's preference shouldn't break the chat request itself, we just
// fall back to false (the safe default).
func (h *Handler) resolveEnableMemory(ctx context.Context, override *bool) bool {
	if override != nil {
		return *override
	}
	if h.userService == nil {
		return false
	}
	user, err := h.userService.GetCurrentUser(ctx)
	if err != nil {
		// API-key-only callers or revoked sessions land here; the chat
		// request itself stays authorised via the middleware that already
		// ran, we just have nobody to look preferences up for.
		logger.Debugf(ctx, "enable_memory: no user in context, defaulting to false: %v", err)
		return false
	}
	if user.Preferences.EnableMemory != nil {
		return *user.Preferences.EnableMemory
	}
	return false
}

// resolveAgent resolves the custom agent by ID, trying shared agent first, then own agent.
// Returns (nil, 0) if agentID is empty or not found.
func (h *Handler) resolveAgent(ctx context.Context, c *gin.Context, agentID string) (*types.CustomAgent, uint64) {
	if agentID == "" {
		return nil, 0
	}

	logger.Infof(ctx, "Resolving agent, agent ID: %s", secutils.SanitizeForLog(agentID))

	// Try shared agent first
	var customAgent *types.CustomAgent
	var effectiveTenantID uint64
	userIDVal, _ := c.Get(types.UserIDContextKey.String())
	currentTenantID := c.GetUint64(types.TenantIDContextKey.String())
	if h.agentShareService != nil && userIDVal != nil && currentTenantID != 0 {
		callerTenantRole := types.TenantRoleFromContext(ctx)
		agent, err := h.agentShareService.GetSharedAgentForTenant(ctx, currentTenantID, callerTenantRole, agentID)
		if err == nil && agent != nil {
			effectiveTenantID = agent.TenantID
			customAgent = agent
			logger.Infof(ctx, "Using shared agent: ID=%s, Name=%s, effectiveTenantID=%d (retrieval scope)",
				customAgent.ID, customAgent.Name, effectiveTenantID)
		}
	}

	// Fall back to own agent
	if customAgent == nil {
		agent, err := h.customAgentService.GetAgentByID(ctx, agentID)
		if err == nil {
			customAgent = agent
			logger.Infof(ctx, "Using own agent: ID=%s, Name=%s, AgentMode=%s",
				customAgent.ID, customAgent.Name, customAgent.Config.AgentMode)
		} else {
			logger.Warnf(ctx, "Failed to get custom agent, agent ID: %s, error: %v, using default config",
				secutils.SanitizeForLog(agentID), err)
		}
	} else {
		logger.Infof(ctx, "Using custom agent: ID=%s, Name=%s, IsBuiltin=%v, AgentMode=%s, effectiveTenantID=%d",
			customAgent.ID, customAgent.Name, customAgent.IsBuiltin, customAgent.Config.AgentMode, effectiveTenantID)
	}

	return customAgent, effectiveTenantID
}

// mergeKnowledgeTargets merges request KB/knowledge IDs with @mentioned items into deduplicated slices.
func mergeKnowledgeTargets(requestKBIDs []string, requestKnowledgeIDs []string, mentionedItems []MentionedItemRequest) (kbIDs []string, knowledgeIDs []string) {
	kbIDSet := make(map[string]bool)
	kbIDs = make([]string, 0, len(requestKBIDs)+len(mentionedItems))
	for _, id := range requestKBIDs {
		if id != "" && !kbIDSet[id] {
			kbIDs = append(kbIDs, id)
			kbIDSet[id] = true
		}
	}

	knowledgeIDSet := make(map[string]bool)
	knowledgeIDs = make([]string, 0, len(requestKnowledgeIDs)+len(mentionedItems))
	for _, id := range requestKnowledgeIDs {
		if id != "" && !knowledgeIDSet[id] {
			knowledgeIDs = append(knowledgeIDs, id)
			knowledgeIDSet[id] = true
		}
	}

	for _, item := range mentionedItems {
		if item.ID == "" {
			continue
		}
		switch item.Type {
		case "kb":
			if !kbIDSet[item.ID] {
				kbIDs = append(kbIDs, item.ID)
				kbIDSet[item.ID] = true
			}
		case "file":
			if !knowledgeIDSet[item.ID] {
				knowledgeIDs = append(knowledgeIDs, item.ID)
				knowledgeIDSet[item.ID] = true
			}
		}
	}
	return kbIDs, knowledgeIDs
}

// sseStreamContext holds the context for SSE streaming
type sseStreamContext struct {
	eventBus         *event.EventBus
	asyncCtx         context.Context
	cancel           context.CancelFunc
	assistantMessage *types.Message
}

// setupSSEStream sets up the SSE streaming context
func (h *Handler) setupSSEStream(reqCtx *qaRequestContext) *sseStreamContext {
	// Set SSE headers
	setSSEHeaders(reqCtx.c)

	// Write initial agent_query event
	h.writeAgentQueryEvent(reqCtx.ctx, reqCtx.sessionID, reqCtx.assistantMessage.ID)

	// Base context for async work: when using shared agent, use source tenant for model/KB/MCP resolution
	baseCtx := h.effectiveQAContext(reqCtx)

	// Create EventBus and cancellable context
	eventBus := event.NewEventBus()
	asyncCtx, cancel := context.WithCancel(logger.CloneContext(baseCtx))

	streamCtx := &sseStreamContext{
		eventBus:         eventBus,
		asyncCtx:         asyncCtx,
		cancel:           cancel,
		assistantMessage: reqCtx.assistantMessage,
	}

	// Subscribe the canonical stream accumulator before wiring stop handling so
	// an interrupted request can persist the exact answer bytes already emitted.
	streamHandler := h.setupStreamHandler(
		asyncCtx,
		reqCtx.sessionID,
		reqCtx.assistantMessage.ID,
		reqCtx.requestID,
		reqCtx.receivedAt,
		reqCtx.assistantMessage,
		eventBus,
	)

	// Setup stop event handler
	h.setupStopEventHandler(
		eventBus,
		reqCtx.sessionID,
		reqCtx.session.TenantID,
		reqCtx.assistantMessage,
		reqCtx.receivedAt,
		cancel,
		streamHandler,
	)

	// Watch for stop events independently of the client SSE connection so a
	// user-requested stop reliably cancels generation even when the client
	// has already disconnected (e.g. API-Key callers that close the stream
	// before POSTing /stop). The watcher self-terminates on a terminal stream
	// event, so its lifetime is decoupled from when the QA service call
	// returns (KnowledgeQA returns immediately while streaming continues in a
	// background goroutine, whereas AgentQA blocks until done). Use a
	// connection-independent context derived from baseCtx so it survives the
	// client disconnect.
	h.startStopWatcher(logger.CloneContext(baseCtx), reqCtx.sessionID, reqCtx.assistantMessage.ID, eventBus)

	return streamCtx
}

func (h *Handler) effectiveQAContext(reqCtx *qaRequestContext) context.Context {
	baseCtx := reqCtx.ctx
	if reqCtx.effectiveTenantID != 0 && h.tenantService != nil {
		if tenant, err := h.tenantService.GetTenantByID(
			reqCtx.ctx, reqCtx.effectiveTenantID,
		); err == nil && tenant != nil {
			baseCtx = context.WithValue(
				context.WithValue(
					reqCtx.ctx, types.TenantIDContextKey, reqCtx.effectiveTenantID,
				),
				types.TenantInfoContextKey,
				tenant,
			)
			logger.Infof(
				reqCtx.ctx,
				"Using effective tenant %d for shared agent (model/KB/MCP)",
				reqCtx.effectiveTenantID,
			)
		}
	}
	return baseCtx
}

func (h *Handler) startTitleGeneration(
	reqCtx *qaRequestContext,
	streamCtx *sseStreamContext,
	generateTitle bool,
) {
	if !generateTitle || reqCtx.session.Title != "" {
		return
	}
	modelID := effectiveRequestModelID(reqCtx)
	logger.Infof(
		reqCtx.ctx,
		"Session has no title, starting async title generation, session ID: %s, model: %s",
		reqCtx.sessionID,
		modelID,
	)
	h.sessionService.GenerateTitleAsync(
		streamCtx.asyncCtx, reqCtx.session, reqCtx.query, modelID, streamCtx.eventBus,
	)
}

// effectiveRequestModelID is the server-side model value used by auxiliary
// conversation paths (queue admission, title generation and persisted UI
// state). Once an agent is selected, its editor configuration is authoritative
// for both built-in and custom agents; the request payload is only a legacy
// fallback for agent-less callers.
func effectiveRequestModelID(reqCtx *qaRequestContext) string {
	if reqCtx == nil {
		return ""
	}
	if reqCtx.customAgent != nil {
		return strings.TrimSpace(reqCtx.customAgent.Config.ModelID)
	}
	return strings.TrimSpace(reqCtx.summaryModelID)
}

// SearchKnowledge godoc
// @Summary      知识搜索
// @Description  在知识库中搜索（不使用LLM总结）
// @Tags         问答
// @Accept       json
// @Produce      json
// @Param        request  body      SearchKnowledgeRequest  true  "搜索请求"
// @Success      200      {object}  map[string]interface{}  "搜索结果"
// @Failure      400      {object}  errors.AppError         "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/search [post]
func (h *Handler) SearchKnowledge(c *gin.Context) {
	ctx := logger.CloneContext(c.Request.Context())
	logger.Info(ctx, "Start processing knowledge search request")

	// Parse request body
	var request SearchKnowledgeRequest
	if err := c.ShouldBindJSON(&request); err != nil {
		logger.Error(ctx, "Failed to parse request data", err)
		c.Error(errors.NewBadRequestError(err.Error()))
		return
	}

	// Validate request parameters
	if request.Query == "" {
		logger.Error(ctx, "Query content is empty")
		c.Error(errors.NewBadRequestError("Query content cannot be empty"))
		return
	}

	// Merge single knowledge_base_id into knowledge_base_ids for backward compatibility
	knowledgeBaseIDs := request.KnowledgeBaseIDs
	if request.KnowledgeBaseID != "" {
		// Check if it's already in the list to avoid duplicates
		found := false
		for _, id := range knowledgeBaseIDs {
			if id == request.KnowledgeBaseID {
				found = true
				break
			}
		}
		if !found {
			knowledgeBaseIDs = append(knowledgeBaseIDs, request.KnowledgeBaseID)
		}
	}

	if len(knowledgeBaseIDs) == 0 && len(request.KnowledgeIDs) == 0 {
		logger.Error(ctx, "No knowledge base IDs or knowledge IDs provided")
		c.Error(errors.NewBadRequestError("At least one knowledge_base_id, knowledge_base_ids or knowledge_ids must be provided"))
		return
	}

	logger.Infof(
		ctx,
		"Knowledge search request, knowledge base IDs: %v, knowledge IDs: %v, query: %s",
		secutils.SanitizeForLogArray(knowledgeBaseIDs),
		secutils.SanitizeForLogArray(request.KnowledgeIDs),
		secutils.SanitizeForLog(request.Query),
	)

	// Directly call knowledge retrieval service without LLM summarization
	searchResults, err := h.sessionService.SearchKnowledge(ctx, knowledgeBaseIDs, request.KnowledgeIDs, request.Query)
	if err != nil {
		logger.ErrorWithFields(ctx, err, nil)
		c.Error(errors.NewInternalServerError(err.Error()))
		return
	}

	logger.Infof(ctx, "Knowledge search completed, found %d results", len(searchResults))
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"data":    searchResults,
	})
}

// KnowledgeQA godoc
// @Summary      知识问答
// @Description  基于知识库的问答（使用LLM总结），支持SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/knowledge-qa [post]
func (h *Handler) KnowledgeQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "KnowledgeQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Execute normal mode QA, generate title unless disabled
	h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
}

// AgentQA godoc
// @Summary      Agent问答
// @Description  基于Agent的智能问答，支持多轮对话和SSE流式响应
// @Tags         问答
// @Accept       json
// @Produce      text/event-stream
// @Param        session_id  path      string                   true  "会话ID"
// @Param        request     body      CreateKnowledgeQARequest true  "问答请求"
// @Success      200         {object}  map[string]interface{}   "问答结果（SSE流）"
// @Failure      400         {object}  errors.AppError          "请求参数错误"
// @Security     Bearer
// @Security     ApiKeyAuth
// @Router       /sessions/{session_id}/agent-qa [post]
func (h *Handler) AgentQA(c *gin.Context) {
	// Parse and validate request
	reqCtx, request, err := h.parseQARequest(c, "AgentQA")
	if err != nil {
		c.Error(err)
		return
	}

	// Determine if agent mode should be enabled
	// Priority: customAgent.IsAgentMode() > request.AgentEnabled
	agentModeEnabled := request.AgentEnabled
	if reqCtx.customAgent != nil {
		agentModeEnabled = reqCtx.customAgent.IsAgentMode()
		logger.Infof(reqCtx.ctx, "Agent mode determined by custom agent: %v (config.agent_mode=%s)",
			agentModeEnabled, reqCtx.customAgent.Config.AgentMode)
	}

	// Sanity gate: agent mode requires a resolved CustomAgent. If we got
	// here with agent_enabled=true but agent_id missing/unresolvable, the
	// AgentQA service will fail deep inside the async goroutine with a
	// generic "custom agent configuration is required" error and the user
	// just sees a broken stream. Reject early with a clear 400 so the
	// frontend can recover (e.g. fall back to quick-answer). Most likely
	// cause is a stale localStorage settings blob where selectedAgentId
	// got blanked but isAgentEnabled stayed true — usually after a
	// cross-tenant switch where the previously selected agent is no
	// longer visible.
	if agentModeEnabled && reqCtx.customAgent == nil {
		logger.Warnf(reqCtx.ctx,
			"Agent mode requested without a resolvable agent_id, rejecting; session=%s, request.AgentID=%q",
			reqCtx.sessionID, secutils.SanitizeForLog(request.AgentID))
		c.Error(errors.NewBadRequestError(
			"agent_id is required when agent mode is enabled"))
		return
	}

	// Route to appropriate handler based on agent mode
	if agentModeEnabled {
		h.executeQA(reqCtx, qaModeAgent, !request.DisableTitle)
	} else {
		logger.Infof(reqCtx.ctx, "Agent mode disabled, delegating to normal mode for session: %s", reqCtx.sessionID)
		h.executeQA(reqCtx, qaModeNormal, !request.DisableTitle)
	}
}

// qaMode determines which QA execution path to use.
type qaMode int

const (
	qaModeNormal qaMode = iota // KnowledgeQA pipeline (RAG / pure chat)
	qaModeAgent                // Agent engine with tool calling
)

// executeQA is the unified execution flow for both KnowledgeQA and AgentQA modes.
// It handles message creation, SSE setup, VLM analysis, service invocation, and error handling.
func (h *Handler) executeQA(reqCtx *qaRequestContext, mode qaMode, generateTitle bool) {
	ctx := reqCtx.ctx
	sessionID := reqCtx.sessionID

	agentModelID := ""
	if reqCtx.customAgent != nil {
		agentModelID = strings.TrimSpace(reqCtx.customAgent.Config.ModelID)
	}
	queueModelID := effectiveRequestModelID(reqCtx)
	principalID := types.SessionOwnerIDFromContext(ctx)
	if principalID == "" && reqCtx.session != nil {
		principalID = reqCtx.session.UserID
	}
	queueCtx := h.effectiveQAContext(reqCtx)
	queueTenantID := reqCtx.session.TenantID
	if reqCtx.effectiveTenantID != 0 {
		queueTenantID = reqCtx.effectiveTenantID
	}
	ticket, rejection, queueErr := reserveChatQueue(queueCtx, ChatQueueAdmissionRequest{
		TenantID:         queueTenantID,
		PrincipalID:      principalID,
		RequestID:        reqCtx.requestID,
		SessionID:        reqCtx.sessionID,
		SummaryModelID:   queueModelID,
		AgentModelID:     agentModelID,
		KnowledgeBaseIDs: append([]string(nil), reqCtx.knowledgeBaseIDs...),
		KnowledgeIDs:     append([]string(nil), reqCtx.knowledgeIDs...),
	})
	if rejection != nil || queueErr != nil {
		writeChatQueueRejection(reqCtx.c, rejection, queueErr)
		return
	}
	reqCtx.chatQueueTicket = ticket

	// Persist the input-bar state used for this request so reopening the
	// session can rehydrate agent / model / KB / web-search / MCP selections.
	// This is a pure UI memo (no behavioural effect) and runs in a goroutine
	// to avoid adding a DB round-trip to TTFB. Use WithoutCancel so a fast
	// client disconnect doesn't drop the write.
	go h.persistLastRequestState(ctx, reqCtx, mode)

	// Agent mode: emit agent query event before message creation
	if mode == qaModeAgent {
		if err := event.Emit(ctx, event.Event{
			Type:      event.EventAgentQuery,
			SessionID: sessionID,
			RequestID: reqCtx.requestID,
			Data: event.AgentQueryData{
				SessionID: sessionID,
				Query:     reqCtx.query,
				RequestID: reqCtx.requestID,
			},
		}); err != nil {
			logger.Errorf(ctx, "Failed to emit agent query event: %v", err)
			if ticket != nil {
				ticket.Cancel(context.WithoutCancel(ctx))
			}
			return
		}
	}

	// Create user message
	userMsg, err := h.createUserMessage(ctx, sessionID, reqCtx.query, reqCtx.requestID, reqCtx.mentionedItems, convertImageAttachments(reqCtx.images), reqCtx.attachments, reqCtx.channel)
	if err != nil {
		if ticket != nil {
			ticket.Cancel(context.WithoutCancel(ctx))
		}
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	reqCtx.userMessageID = userMsg.ID

	// Create assistant message
	assistantMessagePtr, err := h.createAssistantMessage(ctx, reqCtx.assistantMessage)
	if err != nil {
		if ticket != nil {
			ticket.Cancel(context.WithoutCancel(ctx))
		}
		reqCtx.c.Error(errors.NewInternalServerError(err.Error()))
		return
	}
	reqCtx.assistantMessage = assistantMessagePtr

	if mode == qaModeNormal {
		logger.Infof(ctx, "Using knowledge bases: %v", reqCtx.knowledgeBaseIDs)
	} else {
		logger.Infof(ctx, "Calling agent QA service, session ID: %s", sessionID)
	}

	// Setup SSE stream
	streamCtx := h.setupSSEStream(reqCtx)

	// A conversation slot belongs to the whole stream, not just the initial
	// service call. KnowledgeQA may return while its pipeline still streams,
	// therefore only terminal events release the cross-replica lease.
	if ticket != nil {
		var releaseOnce sync.Once
		release := func(_ context.Context, _ event.Event) error {
			releaseOnce.Do(func() {
				ticket.Release(context.WithoutCancel(streamCtx.asyncCtx))
			})
			return nil
		}
		streamCtx.eventBus.On(event.EventAgentComplete, release)
		streamCtx.eventBus.On(event.EventError, release)
		streamCtx.eventBus.On(event.EventStop, release)
	}

	// All profiles receive one committed completion from the same runtime.
	streamCtx.eventBus.On(event.EventAgentComplete, func(ctx context.Context, evt event.Event) error {
		updateCtx := context.WithValue(context.WithoutCancel(ctx), types.TenantIDContextKey, reqCtx.session.TenantID)
		h.completeAssistantMessage(updateCtx, streamCtx.assistantMessage, reqCtx.query, reqCtx)
		return nil
	})

	// Execute QA asynchronously
	go func() {
		defer func() {
			if r := recover(); r != nil {
				buf := make([]byte, 10240)
				runtime.Stack(buf, true)
				stageName := "Knowledge QA"
				if mode == qaModeAgent {
					stageName = "Agent QA"
				}
				logger.ErrorWithFields(streamCtx.asyncCtx,
					errors.NewInternalServerError(fmt.Sprintf("%s service panicked: %v\n%s", stageName, r, string(buf))),
					map[string]interface{}{"session_id": sessionID})
				streamCtx.eventBus.Emit(context.WithoutCancel(streamCtx.asyncCtx), event.Event{
					Type:      event.EventError,
					SessionID: sessionID,
					Data: event.ErrorData{
						Error:     "对话执行发生内部错误，请稍后重试",
						ErrorCode: "CHAT_EXECUTION_PANIC",
						Stage:     "chat_execution",
						SessionID: sessionID,
					},
				})
			}
		}()

		if ticket != nil {
			waitErr := ticket.Wait(streamCtx.asyncCtx, func(snapshot ChatQueueSnapshot) {
				streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
					Type:      event.EventChatQueueStatus,
					SessionID: sessionID,
					RequestID: reqCtx.requestID,
					Data: event.ChatQueueStatusData{
						State:          snapshot.State,
						ModelID:        snapshot.ModelID,
						ResourcePoolID: snapshot.ResourcePoolID,
						Position:       snapshot.Position,
						Waiting:        snapshot.Waiting,
						Active:         snapshot.Active,
						MaxConcurrent:  snapshot.MaxConcurrent,
						MaxWaiting:     snapshot.MaxWaiting,
						QueuedAtUnix:   snapshot.QueuedAtUnix,
					},
				})
			})
			if waitErr != nil {
				if streamCtx.asyncCtx.Err() != nil {
					logger.Infof(
						streamCtx.asyncCtx,
						"Queued QA cancelled before admission for session: %s",
						sessionID,
					)
					return
				}
				logger.Errorf(
					streamCtx.asyncCtx,
					"Chat queue wait failed for session %s: %v",
					sessionID,
					waitErr,
				)
				streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
					Type:      event.EventError,
					SessionID: sessionID,
					Data: event.ErrorData{
						Error:     "聊天排队等待失败，请稍后重试",
						ErrorCode: "CHAT_QUEUE_WAIT_FAILED",
						Stage:     "chat_queue_wait",
						SessionID: sessionID,
					},
				})
				return
			}
		}

		// Build QA request and invoke the appropriate service
		qaReq := reqCtx.buildQARequest()

		var serviceErr error
		var stageName string
		if mode == qaModeNormal {
			stageName = "knowledge_qa_execution"
			serviceErr = h.sessionService.KnowledgeQA(streamCtx.asyncCtx, qaReq, streamCtx.eventBus)
		} else {
			stageName = "agent_execution"

			serviceErr = RunAgentQA(streamCtx.asyncCtx, h.sessionService, qaReq, streamCtx.eventBus)
		}

		if serviceErr != nil {
			// An interrupted or failed alternate runtime still owns a complete
			// execution trace. Preserve it before the common message finalizer.
			if trace, ok := serviceErr.(interface{ AgentExecutionSteps() []types.AgentStep }); ok {
				steps := agenttools.SanitizeAgentStepsForStorage(trace.AgentExecutionSteps())
				streamCtx.assistantMessage.AgentSteps = types.AgentSteps(steps)
				streamCtx.assistantMessage.AgentToolCount = sourcerefs.AgentToolCallCount(streamCtx.assistantMessage.AgentSteps)
			}
			// A user-requested stop cancels asyncCtx, which surfaces here as a
			// context cancellation. That is an expected outcome, not a failure:
			// the stop event already notifies the client, so don't emit a
			// spurious error event (which would otherwise show an error toast).
			if streamCtx.asyncCtx.Err() != nil {
				logger.Infof(streamCtx.asyncCtx, "QA cancelled by user stop for session: %s", sessionID)
			} else {
				errorMessage := userFacingAgentErrorMessage(serviceErr)
				if mode == qaModeAgent {
					streamCtx.assistantMessage.Content = errorMessage
				}
				logger.ErrorWithFields(streamCtx.asyncCtx, serviceErr, nil)
				streamCtx.eventBus.Emit(streamCtx.asyncCtx, event.Event{
					Type:      event.EventError,
					SessionID: sessionID,
					Data: event.ErrorData{
						Error:     errorMessage,
						Stage:     stageName,
						SessionID: sessionID,
					},
				})
			}
		}

		// Title generation is auxiliary. Start it only after the answer path has
		// returned so it cannot compete with the user's answer for local-model
		// admission or inference capacity. The SSE handler keeps only its existing
		// short grace window; title generation never extends answer completion.
		h.startTitleGeneration(reqCtx, streamCtx, generateTitle)
	}()

	// Handle SSE events (blocking)
	shouldWaitForTitle := generateTitle && reqCtx.session.Title == ""
	h.handleAgentEventsForSSE(ctx, reqCtx.c, sessionID, reqCtx.assistantMessage.ID,
		reqCtx.requestID, streamCtx.eventBus, shouldWaitForTitle)
}

// persistLastRequestState records the input-bar state the user just sent so
// that reopening this session restores agent/model/KB/web-search/MCP picks.
// Pure UI memo — failures are logged but never bubble up; the caller runs
// this in a goroutine and is safe to discard the returned context.
func (h *Handler) persistLastRequestState(parentCtx context.Context, reqCtx *qaRequestContext, mode qaMode) {
	// Detach from the HTTP request lifetime: this write must survive both
	// SSE disconnects and the parent gin context being released after the
	// handler returns.
	ctx := logger.CloneContext(context.WithoutCancel(parentCtx))

	agentEnabled := reqCtx.reqAgentEnabled
	// Mirror the resolution rule used in AgentQA: a resolved custom agent's
	// agent_mode wins over the request flag. For KnowledgeQA the request
	// itself carries agent_enabled=false, so this collapses correctly.
	if mode == qaModeAgent && reqCtx.customAgent != nil {
		agentEnabled = reqCtx.customAgent.IsAgentMode()
	}

	state := &types.SessionLastRequestState{
		AgentID:                reqCtx.reqAgentID,
		AgentEnabled:           agentEnabled,
		ModelID:                effectiveRequestModelID(reqCtx),
		KnowledgeBaseIDs:       append([]string(nil), reqCtx.knowledgeBaseIDs...),
		KnowledgeIDs:           append([]string(nil), reqCtx.knowledgeIDs...),
		TagIDs:                 append([]string(nil), reqCtx.tagIDs...),
		MCPServiceIDs:          append([]string(nil), reqCtx.mcpServiceIDs...),
		SkillNames:             append([]string(nil), reqCtx.skillNames...),
		ProfessionalSkillNames: append([]string(nil), reqCtx.professionalSkillNames...),
		MentionedItems:         append(types.MentionedItems(nil), reqCtx.mentionedItems...),
		WebSearchEnabled:       reqCtx.webSearchEnabled,
	}
	if reqCtx.effectiveTenantID != 0 && reqCtx.session != nil && reqCtx.session.TenantID != reqCtx.effectiveTenantID {
		state.AgentSourceTenantID = fmt.Sprintf("%d", reqCtx.effectiveTenantID)
	}

	if err := h.sessionService.UpdateSessionLastRequestState(ctx, reqCtx.sessionID, state); err != nil {
		logger.Warnf(ctx, "persist last_request_state failed for session %s: %v", reqCtx.sessionID, err)
	}
}

func userFacingAgentErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	return usererrors.Message(err.Error())
}

// completeAssistantMessage marks an assistant message as complete, updates it,
// and asynchronously indexes the Q&A pair into the chat history knowledge base.
func (h *Handler) completeAssistantMessage(ctx context.Context, assistantMessage *types.Message, userQuery string, reqCtxs ...*qaRequestContext) {
	assistantMessage.UpdatedAt = time.Now()
	assistantMessage.IsCompleted = true
	_ = h.messageService.UpdateMessage(ctx, assistantMessage)

	if len(reqCtxs) > 0 && reqCtxs[0] != nil {
		reqCtx := reqCtxs[0]
		emitAssistantRunSnapshot(context.WithoutCancel(ctx), AssistantRunSnapshot{
			Session:             cloneFeedbackSession(reqCtx.session),
			AssistantMessage:    cloneFeedbackMessage(assistantMessage),
			UserMessageID:       reqCtx.userMessageID,
			UserQuery:           userQuery,
			CustomAgent:         cloneFeedbackCustomAgent(reqCtx.customAgent),
			KnowledgeBaseIDs:    append([]string(nil), reqCtx.knowledgeBaseIDs...),
			KnowledgeIDs:        append([]string(nil), reqCtx.knowledgeIDs...),
			SkillNames:          append([]string(nil), reqCtx.skillNames...),
			SummaryModelID:      effectiveRequestModelID(reqCtx),
			WebSearchEnabled:    reqCtx.webSearchEnabled,
			EnableMemory:        reqCtx.enableMemory,
			EffectiveTenantID:   reqCtx.effectiveTenantID,
			Channel:             reqCtx.channel,
			MentionedItems:      append(types.MentionedItems(nil), reqCtx.mentionedItems...),
			Images:              append([]ImageAttachment(nil), reqCtx.images...),
			Attachments:         append(types.MessageAttachments(nil), reqCtx.attachments...),
			RequestAgentID:      reqCtx.reqAgentID,
			RequestAgentEnabled: reqCtx.reqAgentEnabled,
		})
	}

	// Asynchronously index the Q&A pair into the chat history knowledge base for vector search.
	// Use WithoutCancel so the goroutine survives after the HTTP request context is done.
	bgCtx := context.WithoutCancel(ctx)
	go h.messageService.IndexMessageToKB(bgCtx, userQuery, assistantMessage.Content, assistantMessage.ID, assistantMessage.SessionID)
}

func cloneFeedbackSession(session *types.Session) *types.Session {
	if session == nil {
		return nil
	}
	cloned := *session
	cloned.Messages = nil
	return &cloned
}

func cloneFeedbackMessage(message *types.Message) *types.Message {
	if message == nil {
		return nil
	}
	cloned := *message
	cloned.KnowledgeReferences = append(types.References(nil), message.KnowledgeReferences...)
	cloned.AgentSteps = append(types.AgentSteps(nil), message.AgentSteps...)
	cloned.MentionedItems = append(types.MentionedItems(nil), message.MentionedItems...)
	cloned.Images = append(types.MessageImages(nil), message.Images...)
	cloned.Attachments = append(types.MessageAttachments(nil), message.Attachments...)
	return &cloned
}

func cloneFeedbackCustomAgent(agent *types.CustomAgent) *types.CustomAgent {
	if agent == nil {
		return nil
	}
	cloned := *agent
	return &cloned
}
