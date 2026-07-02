package answerfeedback

import (
	"context"
	"fmt"
	"math/rand"
	"strings"
	"sync"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

type Service struct {
	db  *gorm.DB
	cfg Config

	startOnce sync.Once

	mu               sync.Mutex
	feedbackQueue    chan string
	snapshotQueue    chan string
	pendingFeedback  map[string]feedbackTask
	pendingSnapshots map[string]snapshotTask
}

type feedbackTask struct {
	TenantID           uint64
	UserID             string
	ActorKey           string
	SessionID          string
	RequestID          string
	AssistantMessageID string
	Feedback           string
	Channel            string
	CreatedAt          time.Time
}

type snapshotTask struct {
	Snapshot sessionhandler.AssistantRunSnapshot
	DueAt    time.Time
	QueuedAt time.Time
	TenantID uint64
	UserID   string
}

func NewService(db *gorm.DB, cfg Config) *Service {
	cfg = cfg.normalize()
	return &Service{
		db:               db,
		cfg:              cfg,
		feedbackQueue:    make(chan string, cfg.QueueSize),
		snapshotQueue:    make(chan string, cfg.QueueSize),
		pendingFeedback:  make(map[string]feedbackTask),
		pendingSnapshots: make(map[string]snapshotTask),
	}
}

func (s *Service) Migrate(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	db := s.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	return db.WithContext(ctx).AutoMigrate(&Feedback{}, &RunSnapshot{})
}

func (s *Service) Start() {
	if s == nil {
		return
	}
	s.startOnce.Do(func() {
		go s.keepFeedbackWorkerAlive()
		go s.keepSnapshotWorkerAlive()
	})
}

func (s *Service) SetMessageFeedback(ctx context.Context, message *types.Message, feedback string) bool {
	if s == nil || message == nil {
		return false
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	if tenantID == 0 {
		logger.Warnf(ctx, "[answerfeedback] missing tenant id while setting feedback for message %s", message.ID)
		return false
	}
	userID, actorKey := actorKeyFromContext(ctx, message.SessionID)
	task := feedbackTask{
		TenantID:           tenantID,
		UserID:             userID,
		ActorKey:           actorKey,
		SessionID:          message.SessionID,
		RequestID:          message.RequestID,
		AssistantMessageID: message.ID,
		Feedback:           feedback,
		Channel:            message.Channel,
		CreatedAt:          time.Now(),
	}
	if ok := s.enqueueFeedback(task); !ok {
		logger.Warnf(ctx, "[answerfeedback] feedback queue full, dropped feedback task for message %s", message.ID)
		return false
	}
	return true
}

func (s *Service) EnrichMessagesForClient(ctx context.Context, messages []*types.Message) {
	if s == nil || len(messages) == 0 {
		return
	}
	ids := make([]string, 0, len(messages))
	for _, message := range messages {
		if message != nil && message.Role == "assistant" && message.ID != "" {
			ids = append(ids, message.ID)
		}
	}
	if len(ids) == 0 {
		return
	}
	feedback, err := s.ListFeedback(ctx, "", ids)
	if err != nil {
		logger.Warnf(ctx, "[answerfeedback] failed to enrich message feedback: %v", err)
		return
	}
	for _, message := range messages {
		if message == nil {
			continue
		}
		message.AnswerFeedback = feedback[message.ID]
	}
}

func (s *Service) ListFeedback(ctx context.Context, sessionID string, messageIDs []string) (map[string]string, error) {
	result := make(map[string]string, len(messageIDs))
	if s == nil || s.db == nil || len(messageIDs) == 0 {
		return result, nil
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	if tenantID == 0 {
		return result, nil
	}
	_, actorKey := actorKeyFromContext(ctx, sessionID)
	query := s.db.WithContext(ctx).
		Where("tenant_id = ? AND actor_key = ? AND assistant_message_id IN ?", tenantID, actorKey, messageIDs)
	if sessionID != "" {
		query = query.Where("session_id = ?", sessionID)
	}
	var rows []Feedback
	if err := query.Find(&rows).Error; err != nil {
		return result, err
	}
	for _, row := range rows {
		result[row.AssistantMessageID] = row.Feedback
	}
	return result, nil
}

func (s *Service) HandleAssistantRunSnapshot(ctx context.Context, snapshot sessionhandler.AssistantRunSnapshot) {
	if s == nil || snapshot.AssistantMessage == nil || snapshot.AssistantMessage.ID == "" {
		return
	}
	tenantID, _ := types.TenantIDFromContext(ctx)
	if tenantID == 0 && snapshot.Session != nil {
		tenantID = snapshot.Session.TenantID
	}
	userID, _ := types.UserIDFromContext(ctx)
	if userID == "" && snapshot.Session != nil {
		userID = snapshot.Session.UserID
	}
	task := snapshotTask{
		Snapshot: snapshot,
		DueAt:    s.nextSnapshotDue(time.Now()),
		QueuedAt: time.Now(),
		TenantID: tenantID,
		UserID:   userID,
	}
	if ok := s.enqueueSnapshot(task); !ok {
		logger.Warnf(ctx, "[answerfeedback] snapshot queue full, dropped snapshot task for message %s", snapshot.AssistantMessage.ID)
	}
}

func (s *Service) enqueueFeedback(task feedbackTask) bool {
	if task.AssistantMessageID == "" || task.ActorKey == "" {
		return false
	}
	key := task.ActorKey + ":" + task.AssistantMessageID
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pendingFeedback[key]; exists {
		s.pendingFeedback[key] = task
		return true
	}
	if len(s.pendingFeedback) >= s.cfg.QueueSize {
		return false
	}
	s.pendingFeedback[key] = task
	select {
	case s.feedbackQueue <- key:
		return true
	default:
		delete(s.pendingFeedback, key)
		return false
	}
}

func (s *Service) enqueueSnapshot(task snapshotTask) bool {
	if task.Snapshot.AssistantMessage == nil || task.Snapshot.AssistantMessage.ID == "" {
		return false
	}
	key := task.Snapshot.AssistantMessage.ID
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.pendingSnapshots[key]; exists {
		return true
	}
	if len(s.pendingSnapshots) >= s.cfg.QueueSize {
		return false
	}
	s.pendingSnapshots[key] = task
	select {
	case s.snapshotQueue <- key:
		return true
	default:
		delete(s.pendingSnapshots, key)
		return false
	}
}

func (s *Service) keepFeedbackWorkerAlive() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf(context.Background(), "[answerfeedback] feedback worker recovered from panic: %v", r)
				}
			}()
			for key := range s.feedbackQueue {
				task, ok := s.takeFeedbackTask(key)
				if !ok {
					continue
				}
				s.withRetries(context.Background(), "feedback", task.AssistantMessageID, func(ctx context.Context) error {
					return s.persistFeedback(ctx, task)
				})
			}
		}()
		time.Sleep(time.Second)
	}
}

func (s *Service) keepSnapshotWorkerAlive() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					logger.Errorf(context.Background(), "[answerfeedback] snapshot worker recovered from panic: %v", r)
				}
			}()
			for key := range s.snapshotQueue {
				task, ok := s.takeSnapshotTask(key)
				if !ok {
					continue
				}
				if delay := time.Until(task.DueAt); delay > 0 {
					time.Sleep(delay)
				}
				assistantID := ""
				if task.Snapshot.AssistantMessage != nil {
					assistantID = task.Snapshot.AssistantMessage.ID
				}
				s.withRetries(context.Background(), "snapshot", assistantID, func(ctx context.Context) error {
					return s.persistRunSnapshot(ctx, task)
				})
			}
		}()
		time.Sleep(time.Second)
	}
}

func (s *Service) takeFeedbackTask(key string) (feedbackTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.pendingFeedback[key]
	if ok {
		delete(s.pendingFeedback, key)
	}
	return task, ok
}

func (s *Service) takeSnapshotTask(key string) (snapshotTask, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.pendingSnapshots[key]
	if ok {
		delete(s.pendingSnapshots, key)
	}
	return task, ok
}

func (s *Service) withRetries(ctx context.Context, taskType, messageID string, fn func(context.Context) error) {
	var err error
	for attempt := 0; attempt <= s.cfg.MaxRetries; attempt++ {
		err = fn(ctx)
		if err == nil {
			return
		}
		if attempt < s.cfg.MaxRetries {
			time.Sleep(time.Duration(attempt+1) * 250 * time.Millisecond)
		}
	}
	logger.Warnf(ctx, "[answerfeedback] %s task failed after retries, message=%s err=%v", taskType, messageID, err)
}

func (s *Service) persistFeedback(ctx context.Context, task feedbackTask) error {
	if s == nil || s.db == nil {
		return nil
	}
	if task.Feedback == FeedbackNone {
		return s.db.WithContext(ctx).
			Where("tenant_id = ? AND actor_key = ? AND assistant_message_id = ?", task.TenantID, task.ActorKey, task.AssistantMessageID).
			Delete(&Feedback{}).Error
	}
	now := time.Now()
	row := Feedback{
		TenantID:           task.TenantID,
		UserID:             task.UserID,
		ActorKey:           task.ActorKey,
		SessionID:          task.SessionID,
		RequestID:          task.RequestID,
		AssistantMessageID: task.AssistantMessageID,
		Feedback:           task.Feedback,
		Channel:            task.Channel,
		Metadata: types.JSONMap{
			"source":     "chat_answer_toolbar",
			"queued_at":  task.CreatedAt,
			"written_at": now,
		},
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"},
			{Name: "actor_key"},
			{Name: "assistant_message_id"},
		},
		DoUpdates: clause.Assignments(map[string]any{
			"user_id":    row.UserID,
			"session_id": row.SessionID,
			"request_id": row.RequestID,
			"feedback":   row.Feedback,
			"channel":    row.Channel,
			"metadata":   row.Metadata,
			"updated_at": now,
		}),
	}).Create(&row).Error
}

func (s *Service) persistRunSnapshot(ctx context.Context, task snapshotTask) error {
	if s == nil || s.db == nil || task.Snapshot.AssistantMessage == nil {
		return nil
	}
	snapshot := task.Snapshot
	assistant := snapshot.AssistantMessage
	conversation, err := s.loadConversationForSnapshot(ctx, assistant)
	if err != nil {
		return err
	}
	if refreshed := findMessage(conversation, assistant.ID); refreshed != nil {
		assistant = refreshed
	}

	collectedAt := time.Now()
	agentID, agentTenantID, agentMode, agentType, modelID := snapshotAgentFields(snapshot)
	tenantID := task.TenantID
	if tenantID == 0 && snapshot.Session != nil {
		tenantID = snapshot.Session.TenantID
	}
	userID := task.UserID
	if userID == "" && snapshot.Session != nil {
		userID = snapshot.Session.UserID
	}

	row := RunSnapshot{
		TenantID:           tenantID,
		UserID:             userID,
		SessionID:          assistant.SessionID,
		RequestID:          assistant.RequestID,
		UserMessageID:      snapshot.UserMessageID,
		AssistantMessageID: assistant.ID,
		Channel:            firstNonEmpty(snapshot.Channel, assistant.Channel),
		AgentID:            agentID,
		AgentTenantID:      agentTenantID,
		AgentMode:          agentMode,
		AgentType:          agentType,
		ModelID:            modelID,
		UserQuery:          snapshot.UserQuery,
		AssistantAnswer:    assistant.Content,
		Snapshot:           buildSnapshotPayload(snapshot, assistant, conversation, task, collectedAt),
		CollectedAt:        collectedAt,
	}
	return s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "assistant_message_id"}},
		DoUpdates: clause.AssignmentColumns([]string{
			"tenant_id",
			"user_id",
			"session_id",
			"request_id",
			"user_message_id",
			"channel",
			"agent_id",
			"agent_tenant_id",
			"agent_mode",
			"agent_type",
			"model_id",
			"user_query",
			"assistant_answer",
			"snapshot",
			"collected_at",
			"updated_at",
		}),
	}).Create(&row).Error
}

func (s *Service) loadConversationForSnapshot(ctx context.Context, assistant *types.Message) ([]types.Message, error) {
	if assistant == nil || assistant.SessionID == "" {
		return nil, nil
	}
	query := s.db.WithContext(ctx).
		Where("session_id = ?", assistant.SessionID).
		Order("created_at ASC")
	if !assistant.CreatedAt.IsZero() {
		query = query.Where("created_at <= ? OR id = ?", assistant.CreatedAt, assistant.ID)
	}
	var messages []types.Message
	if err := query.Find(&messages).Error; err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *Service) nextSnapshotDue(now time.Time) time.Time {
	if start, end, ok := parseNightWindow(s.cfg.SnapshotNightWindow, now); ok {
		return randomTimeBetween(start, end)
	}
	return now.Add(s.cfg.SnapshotDelay).Add(randomDuration(s.cfg.SnapshotJitter))
}

func actorKeyFromContext(ctx context.Context, sessionID string) (string, string) {
	userID, _ := types.UserIDFromContext(ctx)
	if userID != "" && !types.IsSyntheticUserID(userID) {
		return userID, "user:" + userID
	}
	if sessionID != "" {
		return userID, "session:" + sessionID
	}
	if userID != "" {
		return userID, "user:" + userID
	}
	return "", "anonymous"
}

func normalizeFeedback(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case FeedbackLike:
		return FeedbackLike, true
	case FeedbackDislike:
		return FeedbackDislike, true
	case "", "none", "clear", "cancel":
		return FeedbackNone, true
	default:
		return "", false
	}
}

func buildSnapshotPayload(snapshot sessionhandler.AssistantRunSnapshot, assistant *types.Message, conversation []types.Message, task snapshotTask, collectedAt time.Time) types.JSONMap {
	return types.JSONMap{
		"schema_version": 1,
		"collected_at":   collectedAt,
		"queue": types.JSONMap{
			"queued_at": task.QueuedAt,
			"due_at":    task.DueAt,
		},
		"session":              sessionPayload(snapshot.Session),
		"request":              requestPayload(snapshot, assistant),
		"agent":                customAgentPayload(snapshot.CustomAgent),
		"input_context":        inputContextPayload(snapshot),
		"assistant_message":    messagePayload(assistant),
		"conversation":         conversationPayload(conversation),
		"knowledge_references": assistant.KnowledgeReferences,
		"agent_steps":          assistant.AgentSteps,
		"tool_summary":         toolSummary(assistant.AgentSteps),
	}
}

func sessionPayload(session *types.Session) types.JSONMap {
	if session == nil {
		return nil
	}
	return types.JSONMap{
		"id":          session.ID,
		"title":       session.Title,
		"description": session.Description,
		"tenant_id":   session.TenantID,
		"user_id":     session.UserID,
		"created_at":  session.CreatedAt,
		"updated_at":  session.UpdatedAt,
	}
}

func requestPayload(snapshot sessionhandler.AssistantRunSnapshot, assistant *types.Message) types.JSONMap {
	messageID := ""
	sessionID := ""
	requestID := ""
	if assistant != nil {
		messageID = assistant.ID
		sessionID = assistant.SessionID
		requestID = assistant.RequestID
	}
	return types.JSONMap{
		"session_id":            sessionID,
		"request_id":            requestID,
		"user_message_id":       snapshot.UserMessageID,
		"assistant_message_id":  messageID,
		"user_query":            snapshot.UserQuery,
		"channel":               firstNonEmpty(snapshot.Channel, valueOrEmpty(func() string { return assistant.Channel })),
		"request_agent_id":      snapshot.RequestAgentID,
		"request_agent_enabled": snapshot.RequestAgentEnabled,
		"summary_model_id":      snapshot.SummaryModelID,
		"web_search_enabled":    snapshot.WebSearchEnabled,
		"enable_memory":         snapshot.EnableMemory,
		"effective_tenant_id":   snapshot.EffectiveTenantID,
		"knowledge_base_ids":    append([]string(nil), snapshot.KnowledgeBaseIDs...),
		"knowledge_ids":         append([]string(nil), snapshot.KnowledgeIDs...),
		"skill_names":           append([]string(nil), snapshot.SkillNames...),
	}
}

func inputContextPayload(snapshot sessionhandler.AssistantRunSnapshot) types.JSONMap {
	return types.JSONMap{
		"mentioned_items": snapshot.MentionedItems,
		"images":          imagePayload(snapshot.Images),
		"attachments":     snapshot.Attachments,
	}
}

func customAgentPayload(agent *types.CustomAgent) types.JSONMap {
	if agent == nil {
		return nil
	}
	return types.JSONMap{
		"id":                 agent.ID,
		"name":               agent.Name,
		"description":        agent.Description,
		"avatar":             agent.Avatar,
		"is_builtin":         agent.IsBuiltin,
		"tenant_id":          agent.TenantID,
		"created_by":         agent.CreatedBy,
		"runnable_by_viewer": agent.RunnableByViewer,
		"config":             agent.Config,
		"created_at":         agent.CreatedAt,
		"updated_at":         agent.UpdatedAt,
	}
}

func messagePayload(message *types.Message) types.JSONMap {
	if message == nil {
		return nil
	}
	return types.JSONMap{
		"id":                   message.ID,
		"session_id":           message.SessionID,
		"request_id":           message.RequestID,
		"role":                 message.Role,
		"content":              message.Content,
		"knowledge_references": message.KnowledgeReferences,
		"agent_steps":          message.AgentSteps,
		"mentioned_items":      message.MentionedItems,
		"images":               message.Images,
		"attachments":          message.Attachments,
		"is_completed":         message.IsCompleted,
		"is_fallback":          message.IsFallback,
		"agent_duration_ms":    message.AgentDurationMs,
		"channel":              message.Channel,
		"knowledge_id":         message.KnowledgeID,
		"created_at":           message.CreatedAt,
		"updated_at":           message.UpdatedAt,
	}
}

func conversationPayload(messages []types.Message) []types.JSONMap {
	out := make([]types.JSONMap, 0, len(messages))
	for i := range messages {
		out = append(out, messagePayload(&messages[i]))
	}
	return out
}

func imagePayload(images []sessionhandler.ImageAttachment) []types.JSONMap {
	out := make([]types.JSONMap, 0, len(images))
	for _, img := range images {
		out = append(out, types.JSONMap{
			"url":             img.URL,
			"caption":         img.Caption,
			"had_inline_data": img.Data != "",
		})
	}
	return out
}

func toolSummary(steps types.AgentSteps) []types.JSONMap {
	var out []types.JSONMap
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			success := false
			hasResult := call.Result != nil
			if call.Result != nil {
				success = call.Result.Success
			}
			out = append(out, types.JSONMap{
				"iteration":    step.Iteration,
				"tool_call_id": call.ID,
				"tool_name":    call.Name,
				"success":      success,
				"has_result":   hasResult,
				"duration_ms":  call.Duration,
			})
		}
	}
	if out == nil {
		return []types.JSONMap{}
	}
	return out
}

func snapshotAgentFields(snapshot sessionhandler.AssistantRunSnapshot) (string, uint64, string, string, string) {
	if snapshot.CustomAgent == nil {
		return snapshot.RequestAgentID, snapshot.EffectiveTenantID, "", "", snapshot.SummaryModelID
	}
	agent := snapshot.CustomAgent
	modelID := firstNonEmpty(snapshot.SummaryModelID, agent.Config.ModelID)
	return agent.ID, agent.TenantID, agent.Config.AgentMode, agent.Config.AgentType, modelID
}

func findMessage(messages []types.Message, id string) *types.Message {
	for i := range messages {
		if messages[i].ID == id {
			return &messages[i]
		}
	}
	return nil
}

func parseNightWindow(raw string, now time.Time) (time.Time, time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, time.Time{}, false
	}
	parts := strings.Split(raw, "-")
	if len(parts) != 2 {
		return time.Time{}, time.Time{}, false
	}
	startMin, ok := parseClockMinute(parts[0])
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	endMin, ok := parseClockMinute(parts[1])
	if !ok {
		return time.Time{}, time.Time{}, false
	}
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := dayStart.Add(time.Duration(startMin) * time.Minute)
	end := dayStart.Add(time.Duration(endMin) * time.Minute)
	if !end.After(start) {
		end = end.Add(24 * time.Hour)
	}
	if now.After(end) {
		start = start.Add(24 * time.Hour)
		end = end.Add(24 * time.Hour)
	} else if now.After(start) {
		start = now
	}
	return start, end, true
}

func parseClockMinute(raw string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) != 2 {
		return 0, false
	}
	var hour, minute int
	if _, err := fmt.Sscanf(parts[0], "%d", &hour); err != nil {
		return 0, false
	}
	if _, err := fmt.Sscanf(parts[1], "%d", &minute); err != nil {
		return 0, false
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, false
	}
	return hour*60 + minute, true
}

func randomDuration(max time.Duration) time.Duration {
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(int64(max)))
}

func randomTimeBetween(start, end time.Time) time.Time {
	if !end.After(start) {
		return start
	}
	return start.Add(randomDuration(end.Sub(start)))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func valueOrEmpty(fn func() string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	return fn()
}
