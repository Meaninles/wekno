package scheduledchat

import (
	"context"
	stderrors "errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/event"
	sessionhandler "github.com/Tencent/WeKnora/internal/handler/session"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	schedulerInterval = 30 * time.Second
	runTimeout        = 6 * time.Hour

	legacySessionTitleRunning = "（定时任务）新会话"
)

type scheduledSessionTitleBackfillRow struct {
	SessionID   string
	Title       string
	CreatedAt   time.Time
	StartedAt   *time.Time
	ScheduledAt *time.Time
	Timezone    string
	TaskName    string
}

type Service struct {
	db                  *gorm.DB
	sessionService      interfaces.SessionService
	messageService      interfaces.MessageService
	customAgentService  interfaces.CustomAgentService
	agentShareService   interfaces.AgentShareService
	tenantService       interfaces.TenantService
	tenantMemberService interfaces.TenantMemberService
	userService         interfaces.UserService
	streamManager       interfaces.StreamManager

	schedulerMu     sync.Mutex
	schedulerCancel context.CancelFunc
}

func NewService(
	db *gorm.DB,
	sessionService interfaces.SessionService,
	messageService interfaces.MessageService,
	customAgentService interfaces.CustomAgentService,
	agentShareService interfaces.AgentShareService,
	tenantService interfaces.TenantService,
	tenantMemberService interfaces.TenantMemberService,
	userService interfaces.UserService,
	streamManager interfaces.StreamManager,
) *Service {
	return &Service{
		db:                  db,
		sessionService:      sessionService,
		messageService:      messageService,
		customAgentService:  customAgentService,
		agentShareService:   agentShareService,
		tenantService:       tenantService,
		tenantMemberService: tenantMemberService,
		userService:         userService,
		streamManager:       streamManager,
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
	if err := db.WithContext(ctx).AutoMigrate(&Task{}, &Run{}); err != nil {
		return err
	}
	if err := s.backfillScheduledSessionTitles(ctx, db); err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] backfill session titles failed: %v", err)
	}
	return nil
}

func (s *Service) backfillScheduledSessionTitles(ctx context.Context, db *gorm.DB) error {
	var rows []scheduledSessionTitleBackfillRow
	if err := db.WithContext(ctx).
		Table("sessions AS s").
		Select("s.id AS session_id, s.title, s.created_at, runs.started_at, runs.scheduled_at, COALESCE(tasks.timezone, '') AS timezone, COALESCE(tasks.name, '') AS task_name").
		Joins("LEFT JOIN custom_scheduled_chat_runs AS runs ON runs.session_id = s.id").
		Joins("LEFT JOIN custom_scheduled_chat_tasks AS tasks ON tasks.id = runs.task_id").
		Where("s.deleted_at IS NULL").
		Where("s.description LIKE ?", SessionMarkerPrefix+"%").
		Where("(s.title = ? OR s.title LIKE ? OR s.title LIKE ?)", legacySessionTitleRunning, "（定时任务）%", "% 定时任务").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		titleTime := row.CreatedAt
		if row.StartedAt != nil && !row.StartedAt.IsZero() {
			titleTime = *row.StartedAt
		} else if row.ScheduledAt != nil && !row.ScheduledAt.IsZero() {
			titleTime = *row.ScheduledAt
		}
		title := scheduledSessionTitleAt(titleTime, row.Timezone, row.TaskName)
		if row.Title == title {
			continue
		}
		if err := db.WithContext(ctx).
			Table("sessions").
			Where("id = ?", row.SessionID).
			Update("title", title).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) StartScheduler(ctx context.Context) error {
	if s == nil || s.db == nil {
		return nil
	}
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerCancel != nil {
		return nil
	}
	runCtx, cancel := context.WithCancel(context.Background())
	s.schedulerCancel = cancel
	go s.schedulerLoop(runCtx)
	return nil
}

func (s *Service) StopScheduler() {
	s.schedulerMu.Lock()
	defer s.schedulerMu.Unlock()
	if s.schedulerCancel != nil {
		s.schedulerCancel()
		s.schedulerCancel = nil
	}
}

func (s *Service) schedulerLoop(ctx context.Context) {
	s.pollDueTasks(ctx)
	ticker := time.NewTicker(schedulerInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollDueTasks(ctx)
		}
	}
}

func (s *Service) pollDueTasks(ctx context.Context) {
	now := time.Now().UTC()
	for {
		var tasks []Task
		if err := s.db.WithContext(ctx).
			Where("enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", true, now).
			Order("next_run_at ASC").
			Limit(20).
			Find(&tasks).Error; err != nil {
			logger.Warnf(ctx, "[custom scheduledchat] query due tasks failed: %v", err)
			return
		}
		if len(tasks) == 0 {
			return
		}
		for _, task := range tasks {
			if err := s.claimDueTask(ctx, task.ID, now); err != nil {
				logger.Warnf(ctx, "[custom scheduledchat] claim task %s failed: %v", task.ID, err)
			}
		}
		if len(tasks) < 20 {
			return
		}
	}
}

func (s *Service) ListTasks(ctx context.Context) ([]Task, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant context is required")
	}
	userID, _ := types.UserIDFromContext(ctx)
	db := s.db.WithContext(ctx).Where("tenant_id = ?", tenantID)
	if !types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin) {
		db = db.Where("created_by = ?", userID)
	}
	var tasks []Task
	if err := db.Order("created_at DESC").Find(&tasks).Error; err != nil {
		return nil, err
	}
	return tasks, nil
}

func (s *Service) GetTask(ctx context.Context, id string) (*Task, error) {
	task, err := s.getTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !canManageTask(ctx, task) && !isTaskOwner(ctx, task) {
		return nil, fmt.Errorf("permission denied")
	}
	return task, nil
}

func (s *Service) CreateTask(ctx context.Context, req TaskRequest) (*Task, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant context is required")
	}
	userID, ok := types.UserIDFromContext(ctx)
	if !ok || userID == "" {
		return nil, fmt.Errorf("user context is required")
	}
	agent, err := s.resolveAgentForContext(ctx, strings.TrimSpace(req.AgentID))
	if err != nil {
		return nil, err
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	task := &Task{
		TenantID:          tenantID,
		CreatedBy:         userID,
		RunAsUserID:       userID,
		Name:              strings.TrimSpace(req.Name),
		Description:       strings.TrimSpace(req.Description),
		Enabled:           enabled,
		AgentID:           strings.TrimSpace(req.AgentID),
		AgentNameSnapshot: agent.Name,
		ScheduleType:      strings.TrimSpace(req.ScheduleType),
		Timezone:          strings.TrimSpace(req.Timezone),
		Minute:            req.Minute,
		Hour:              req.Hour,
		Weekday:           req.Weekday,
		DayOfMonth:        req.DayOfMonth,
		PromptTemplate:    strings.TrimSpace(req.PromptTemplate),
		WebSearchEnabled:  req.WebSearchEnabled,
		ConcurrencyPolicy: ConcurrencySkipIfRunning,
	}
	if err := normalizeAndValidateTask(task); err != nil {
		return nil, err
	}
	if task.Enabled {
		next, err := NextRunAfter(task, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		task.NextRunAt = &next
	}
	if err := s.db.WithContext(ctx).Create(task).Error; err != nil {
		return nil, err
	}
	return task, nil
}

func (s *Service) UpdateTask(ctx context.Context, id string, req TaskRequest) (*Task, error) {
	task, err := s.getTask(ctx, id)
	if err != nil {
		return nil, err
	}
	if !canManageTask(ctx, task) {
		return nil, fmt.Errorf("permission denied")
	}
	agent, err := s.resolveAgentForContext(ctx, strings.TrimSpace(req.AgentID))
	if err != nil {
		return nil, err
	}
	if req.Enabled != nil {
		task.Enabled = *req.Enabled
	}
	task.Name = strings.TrimSpace(req.Name)
	task.Description = strings.TrimSpace(req.Description)
	task.AgentID = strings.TrimSpace(req.AgentID)
	task.AgentNameSnapshot = agent.Name
	task.ScheduleType = strings.TrimSpace(req.ScheduleType)
	task.Timezone = strings.TrimSpace(req.Timezone)
	task.Minute = req.Minute
	task.Hour = req.Hour
	task.Weekday = req.Weekday
	task.DayOfMonth = req.DayOfMonth
	task.PromptTemplate = strings.TrimSpace(req.PromptTemplate)
	task.WebSearchEnabled = req.WebSearchEnabled
	task.ConcurrencyPolicy = ConcurrencySkipIfRunning
	task.LastMessage = ""
	if err := normalizeAndValidateTask(task); err != nil {
		return nil, err
	}
	if task.Enabled {
		next, err := NextRunAfter(task, time.Now().UTC())
		if err != nil {
			return nil, err
		}
		task.NextRunAt = &next
	} else {
		task.NextRunAt = nil
	}
	if err := s.db.WithContext(ctx).Save(task).Error; err != nil {
		return nil, err
	}
	return task, nil
}

func (s *Service) DeleteTask(ctx context.Context, id string) error {
	task, err := s.getTask(ctx, id)
	if err != nil {
		return err
	}
	if !canManageTask(ctx, task) {
		return fmt.Errorf("permission denied")
	}
	return s.db.WithContext(ctx).Delete(task).Error
}

func (s *Service) ListRuns(ctx context.Context, taskID string, limit int) ([]Run, error) {
	task, err := s.getTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !canManageTask(ctx, task) && !isTaskOwner(ctx, task) {
		return nil, fmt.Errorf("permission denied")
	}
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	var runs []Run
	err = s.db.WithContext(ctx).
		Where("task_id = ?", taskID).
		Order("scheduled_at DESC, created_at DESC").
		Limit(limit).
		Find(&runs).Error
	return runs, err
}

func (s *Service) RunTaskNow(ctx context.Context, taskID string) (*Run, error) {
	task, err := s.getTask(ctx, taskID)
	if err != nil {
		return nil, err
	}
	if !canManageTask(ctx, task) {
		return nil, fmt.Errorf("permission denied")
	}
	now := time.Now().UTC()
	if running, err := s.hasActiveRun(ctx, task.ID, now); err != nil {
		return nil, err
	} else if running {
		return nil, fmt.Errorf("task is already running")
	}
	run := &Run{
		TaskID:      task.ID,
		TenantID:    task.TenantID,
		RunAsUserID: task.RunAsUserID,
		ScheduledAt: now,
		TriggeredBy: TriggerManual,
		Status:      RunStatusRunning,
		StartedAt:   &now,
	}
	if err := s.db.WithContext(ctx).Create(run).Error; err != nil {
		return nil, err
	}
	if err := s.db.WithContext(ctx).Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
		"last_run_at":  now,
		"last_status":  RunStatusRunning,
		"last_message": "running",
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] update task running status failed: %v", err)
	}
	go s.executeRun(run.ID)
	return run, nil
}

func (s *Service) claimDueTask(ctx context.Context, taskID string, now time.Time) error {
	var runID string
	err := s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var task Task
		err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			First(&task, "id = ? AND enabled = ? AND next_run_at IS NOT NULL AND next_run_at <= ?", taskID, true, now).Error
		if stderrors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		s.expireStaleRunsTx(tx, task.ID, now)
		scheduledAt := task.NextRunAt
		if scheduledAt == nil {
			return nil
		}
		next, err := NextRunAfter(&task, now)
		if err != nil {
			return err
		}
		running, err := s.hasActiveRunTx(tx, task.ID, now)
		if err != nil {
			return err
		}
		if running {
			finishedAt := now
			run := &Run{
				TaskID:       task.ID,
				TenantID:     task.TenantID,
				RunAsUserID:  task.RunAsUserID,
				ScheduledAt:  scheduledAt.UTC(),
				TriggeredBy:  TriggerSchedule,
				Status:       RunStatusSkipped,
				ErrorMessage: "previous run is still running",
				StartedAt:    &now,
				FinishedAt:   &finishedAt,
			}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(run).Error; err != nil {
				return err
			}
			return tx.Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
				"next_run_at":  next,
				"last_run_at":  now,
				"last_status":  RunStatusSkipped,
				"last_message": "previous run is still running",
			}).Error
		}
		run := &Run{
			TaskID:      task.ID,
			TenantID:    task.TenantID,
			RunAsUserID: task.RunAsUserID,
			ScheduledAt: scheduledAt.UTC(),
			TriggeredBy: TriggerSchedule,
			Status:      RunStatusRunning,
			StartedAt:   &now,
		}
		res := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(run)
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return nil
		}
		runID = run.ID
		return tx.Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
			"next_run_at":  next,
			"last_run_at":  now,
			"last_status":  RunStatusRunning,
			"last_message": "running",
		}).Error
	})
	if err != nil {
		return err
	}
	if runID != "" {
		go s.executeRun(runID)
	}
	return nil
}

func (s *Service) executeRun(runID string) {
	ctx, cancel := context.WithTimeout(context.Background(), runTimeout)
	defer cancel()

	var run Run
	var task Task
	if err := s.db.WithContext(ctx).First(&run, "id = ?", runID).Error; err != nil {
		logger.Errorf(ctx, "[custom scheduledchat] load run %s failed: %v", runID, err)
		return
	}
	if err := s.db.WithContext(ctx).First(&task, "id = ?", run.TaskID).Error; err != nil {
		s.finishRun(ctx, &run, nil, RunStatusFailed, fmt.Sprintf("load task failed: %v", err))
		return
	}

	defer func() {
		if recovered := recover(); recovered != nil {
			buf := make([]byte, 10240)
			runtime.Stack(buf, true)
			s.finishRun(ctx, &run, &task, RunStatusFailed, fmt.Sprintf("panic: %v\n%s", recovered, string(buf)))
		}
	}()

	runCtx, user, tenant, err := s.buildRunContext(ctx, &task, &run)
	if err != nil {
		s.disableTaskAfterAuthFailure(ctx, &task, &run, err)
		return
	}
	agent, err := s.resolveAgentForContext(runCtx, task.AgentID)
	if err != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, err.Error())
		return
	}

	renderedPrompt, err := s.renderPrompt(runCtx, &task, &run, agent, tenant, user)
	if err != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, err.Error())
		return
	}
	run.RenderedPrompt = renderedPrompt
	if err := s.db.WithContext(runCtx).Model(&Run{}).Where("id = ?", run.ID).
		Update("rendered_prompt", renderedPrompt).Error; err != nil {
		logger.Warnf(runCtx, "[custom scheduledchat] update rendered prompt failed: %v", err)
	}

	session, userMsg, assistantMsg, err := s.createConversation(runCtx, &task, &run, renderedPrompt)
	if err != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, err.Error())
		return
	}
	run.SessionID = session.ID
	run.UserMessageID = userMsg.ID
	run.AssistantMessageID = assistantMsg.ID

	eventBus := event.NewEventBus()
	execCtx, execCancel := context.WithCancel(runCtx)
	defer execCancel()
	sessionhandler.NewAgentStreamHandler(
		execCtx,
		session.ID,
		assistantMsg.ID,
		userMsg.RequestID,
		time.Now(),
		assistantMsg,
		s.streamManager,
		eventBus,
	).Subscribe()
	s.writeAgentQueryEvent(execCtx, session.ID, assistantMsg.ID)
	s.startStopWatcher(execCtx, session.ID, assistantMsg.ID, eventBus)

	var once sync.Once
	done := make(chan struct{})
	var eventErr error
	complete := func() {
		once.Do(func() {
			close(done)
		})
	}
	eventBus.On(event.EventStop, func(ctx context.Context, evt event.Event) error {
		execCancel()
		s.completeAssistantMessage(runCtx, assistantMsg, "")
		complete()
		return nil
	})
	eventBus.On(event.EventError, func(ctx context.Context, evt event.Event) error {
		if data, ok := evt.Data.(event.ErrorData); ok && data.Error != "" {
			eventErr = fmt.Errorf("%s", data.Error)
		}
		complete()
		return nil
	})

	agentMode := agent.IsAgentMode()
	if !agentMode {
		var completionHandled bool
		eventBus.On(event.EventAgentThought, func(ctx context.Context, evt event.Event) error {
			data, ok := evt.Data.(event.AgentThoughtData)
			if ok && data.Content != "" {
				appendQuickAnswerReasoning(assistantMsg, data.Content)
			}
			return nil
		})
		eventBus.On(event.EventAgentFinalAnswer, func(ctx context.Context, evt event.Event) error {
			data, ok := evt.Data.(event.AgentFinalAnswerData)
			if !ok {
				return nil
			}
			assistantMsg.Content += data.Content
			if data.IsFallback {
				assistantMsg.IsFallback = true
			}
			if data.Done && !completionHandled {
				completionHandled = true
				s.completeAssistantMessage(runCtx, assistantMsg, renderedPrompt)
				_ = eventBus.Emit(runCtx, event.Event{
					Type:      event.EventAgentComplete,
					SessionID: session.ID,
					Data:      event.AgentCompleteData{FinalAnswer: assistantMsg.Content},
				})
				complete()
			}
			return nil
		})
	} else {
		eventBus.On(event.EventAgentComplete, func(ctx context.Context, evt event.Event) error {
			complete()
			return nil
		})
	}

	qaReq := &types.QARequest{
		Session:            session,
		Query:              renderedPrompt,
		AssistantMessageID: assistantMsg.ID,
		CustomAgent:        agent,
		UserMessageID:      userMsg.ID,
		WebSearchEnabled:   task.WebSearchEnabled,
		EnableMemory:       false,
	}

	var serviceErr error
	if agentMode {
		serviceErr = s.sessionService.AgentQA(execCtx, qaReq, eventBus)
		s.completeAssistantMessage(runCtx, assistantMsg, renderedPrompt)
		complete()
	} else {
		serviceErr = s.sessionService.KnowledgeQA(execCtx, qaReq, eventBus)
	}
	if serviceErr != nil {
		if execCtx.Err() != nil {
			serviceErr = execCtx.Err()
		}
		eventErr = serviceErr
		_ = eventBus.Emit(runCtx, event.Event{
			Type:      event.EventError,
			SessionID: session.ID,
			Data: event.ErrorData{
				Error:     serviceErr.Error(),
				Stage:     "scheduled_chat_execution",
				SessionID: session.ID,
			},
		})
		s.completeAssistantMessage(runCtx, assistantMsg, "")
		complete()
	}

	select {
	case <-done:
	case <-ctx.Done():
		eventErr = ctx.Err()
		s.completeAssistantMessage(runCtx, assistantMsg, "")
	}
	if eventErr != nil && !stderrors.Is(eventErr, context.Canceled) {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, eventErr.Error())
		return
	}
	if eventErr != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, "cancelled")
		return
	}
	s.finishRun(runCtx, &run, &task, RunStatusSuccess, "ok")
}

func (s *Service) createConversation(
	ctx context.Context,
	task *Task,
	run *Run,
	renderedPrompt string,
) (*types.Session, *types.Message, *types.Message, error) {
	session, err := s.sessionService.CreateSession(ctx, &types.Session{
		Title:       sessionTitleForRun(task, run),
		Description: fmt.Sprintf("%stask=%s;run=%s", SessionMarkerPrefix, task.ID, run.ID),
		TenantID:    task.TenantID,
		UserID:      task.RunAsUserID,
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Updates(map[string]any{
		"session_id": session.ID,
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] update run session id failed: %v", err)
	}
	if err := s.db.WithContext(ctx).Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
		"last_session_id": session.ID,
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] update task session id failed: %v", err)
	}
	requestID := uuid.NewString()
	userMsg, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "user",
		Content:     renderedPrompt,
		RequestID:   requestID,
		IsCompleted: true,
		Channel:     MessageChannel,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	assistantMsg, err := s.messageService.CreateMessage(ctx, &types.Message{
		SessionID:   session.ID,
		Role:        "assistant",
		RequestID:   requestID,
		IsCompleted: false,
		Channel:     MessageChannel,
		CreatedAt:   time.Now(),
	})
	if err != nil {
		return nil, nil, nil, err
	}
	if err := s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Updates(map[string]any{
		"user_message_id":      userMsg.ID,
		"assistant_message_id": assistantMsg.ID,
	}).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] update run message ids failed: %v", err)
	}
	return session, userMsg, assistantMsg, nil
}

func sessionTitleForRun(task *Task, run *Run) string {
	titleTime := time.Now().UTC()
	timezone := ""
	taskName := ""
	if task != nil {
		timezone = task.Timezone
		taskName = task.Name
	}
	if run != nil {
		if run.StartedAt != nil && !run.StartedAt.IsZero() {
			titleTime = *run.StartedAt
		} else if !run.ScheduledAt.IsZero() {
			titleTime = run.ScheduledAt
		}
	}
	return scheduledSessionTitleAt(titleTime, timezone, taskName)
}

func scheduledSessionTitleAt(value time.Time, timezone, taskName string) string {
	if value.IsZero() {
		value = time.Now().UTC()
	}
	loc, err := loadTaskLocation(timezone)
	if err != nil {
		loc = time.UTC
	}
	t := value.In(loc)
	title := fmt.Sprintf("%d.%d.%d 定时任务", t.Year(), int(t.Month()), t.Day())
	if name := strings.TrimSpace(taskName); name != "" {
		title += "-" + name
	}
	return title
}

func (s *Service) buildRunContext(
	parent context.Context,
	task *Task,
	run *Run,
) (context.Context, *types.User, *types.Tenant, error) {
	user, err := s.userService.GetUserByID(parent, task.RunAsUserID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("run user not found: %w", err)
	}
	if user == nil || !user.IsActive {
		return nil, nil, nil, fmt.Errorf("run user is inactive")
	}
	member, err := s.tenantMemberService.GetMembership(parent, task.RunAsUserID, task.TenantID)
	if err != nil {
		return nil, nil, nil, err
	}
	if member == nil || member.Status != types.TenantMemberStatusActive {
		return nil, nil, nil, fmt.Errorf("run user is no longer an active tenant member")
	}
	tenant, err := s.tenantService.GetTenantByID(parent, task.TenantID)
	if err != nil {
		return nil, nil, nil, err
	}
	ctx := context.Background()
	ctx = context.WithValue(ctx, types.TenantIDContextKey, task.TenantID)
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)
	ctx = context.WithValue(ctx, types.UserIDContextKey, task.RunAsUserID)
	ctx = context.WithValue(ctx, types.UserContextKey, user)
	ctx = context.WithValue(ctx, types.TenantRoleContextKey, member.Role)
	ctx = context.WithValue(ctx, types.LanguageContextKey, types.DefaultLanguage())
	ctx = context.WithValue(ctx, types.RequestIDContextKey, "scheduled-"+run.ID)
	return ctx, user, tenant, nil
}

func (s *Service) resolveAgentForContext(ctx context.Context, agentID string) (*types.CustomAgent, error) {
	agentID = strings.TrimSpace(agentID)
	if agentID == "" {
		return nil, fmt.Errorf("agent_id is required")
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant context is required")
	}
	role := types.TenantRoleFromContext(ctx)
	var agent *types.CustomAgent
	if s.agentShareService != nil {
		if shared, err := s.agentShareService.GetSharedAgentForTenant(ctx, tenantID, role, agentID); err == nil && shared != nil {
			agent = shared
		}
	}
	if agent == nil {
		own, err := s.customAgentService.GetAgentByID(ctx, agentID)
		if err != nil {
			return nil, fmt.Errorf("agent not found or inaccessible")
		}
		agent = own
	}
	if agent.IsAgentMode() && role == types.TenantRoleViewer && !agent.RunnableByViewer {
		return nil, fmt.Errorf("current user cannot run this agent")
	}
	return agent, nil
}

func (s *Service) renderPrompt(
	ctx context.Context,
	task *Task,
	run *Run,
	agent *types.CustomAgent,
	tenant *types.Tenant,
	user *types.User,
) (string, error) {
	loc, err := loadTaskLocation(task.Timezone)
	if err != nil {
		return "", err
	}
	now := time.Now().In(loc)
	scheduled := run.ScheduledAt.In(loc)
	lastRunAt := s.lastFinishedRunAt(ctx, task.ID, run.ID, loc)
	runCount := s.successRunCount(ctx, task.ID)
	vals := types.PlaceholderValues{
		"task_name":       task.Name,
		"agent_name":      agent.Name,
		"scheduled_at":    scheduled.Format("2006-01-02 15:04:05"),
		"triggered_at":    now.Format("2006-01-02 15:04:05"),
		"current_time":    now.Format("2006-01-02 15:04:05"),
		"current_date":    now.Format("2006-01-02"),
		"current_week":    chineseWeekday(now),
		"yesterday":       now.AddDate(0, 0, -1).Format("2006-01-02"),
		"week_start":      startOfWeek(now).Format("2006-01-02"),
		"week_end":        startOfWeek(now).AddDate(0, 0, 6).Format("2006-01-02"),
		"month_start":     time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc).Format("2006-01-02"),
		"month_end":       time.Date(now.Year(), now.Month()+1, 0, 0, 0, 0, 0, loc).Format("2006-01-02"),
		"last_run_at":     lastRunAt,
		"last_success_at": formatTimePtr(task.LastSuccessAt, loc),
		"run_count":       strconv.FormatInt(runCount, 10),
		"timezone":        task.Timezone,
		"tenant_name":     tenant.Name,
		"user_name":       user.Username,
		"language":        types.LanguageNameFromContext(ctx),
	}
	rendered := strings.TrimSpace(types.RenderPromptPlaceholders(task.PromptTemplate, vals))
	if rendered == "" {
		return "", fmt.Errorf("rendered prompt is empty")
	}
	return rendered, nil
}

func (s *Service) RenderPreview(ctx context.Context, req RenderPreviewRequest) (string, error) {
	tenantID, _ := types.TenantIDFromContext(ctx)
	userID, _ := types.UserIDFromContext(ctx)
	tenant, _ := s.tenantService.GetTenantByID(ctx, tenantID)
	user, _ := s.userService.GetUserByID(ctx, userID)
	agentName := ""
	if req.AgentID != "" {
		if agent, err := s.resolveAgentForContext(ctx, req.AgentID); err == nil && agent != nil {
			agentName = agent.Name
		}
	}
	if tenant == nil {
		tenant = &types.Tenant{ID: tenantID}
	}
	if user == nil {
		user = &types.User{ID: userID}
	}
	tz := strings.TrimSpace(req.Timezone)
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	task := &Task{Name: strings.TrimSpace(req.TaskName), Timezone: tz, PromptTemplate: req.PromptTemplate}
	agent := &types.CustomAgent{Name: agentName}
	run := &Run{ScheduledAt: time.Now().UTC()}
	return s.renderPrompt(ctx, task, run, agent, tenant, user)
}

func (s *Service) finishRun(ctx context.Context, run *Run, task *Task, status, message string) {
	now := time.Now().UTC()
	updates := map[string]any{
		"status":        status,
		"error_message": "",
		"finished_at":   now,
	}
	if status != RunStatusSuccess {
		updates["error_message"] = trimMessage(message)
	}
	if err := s.db.WithContext(ctx).Model(&Run{}).Where("id = ?", run.ID).Updates(updates).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] finish run %s failed: %v", run.ID, err)
	}
	if task == nil {
		return
	}
	taskUpdates := map[string]any{
		"last_status":  status,
		"last_message": trimMessage(message),
	}
	if status == RunStatusSuccess {
		taskUpdates["last_success_at"] = now
		if run.SessionID != "" {
			taskUpdates["last_session_id"] = run.SessionID
		}
	}
	if err := s.db.WithContext(ctx).Model(&Task{}).Where("id = ?", task.ID).Updates(taskUpdates).Error; err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] finish task status failed: %v", err)
	}
}

func (s *Service) disableTaskAfterAuthFailure(ctx context.Context, task *Task, run *Run, err error) {
	msg := err.Error()
	if updateErr := s.db.WithContext(ctx).Model(&Task{}).Where("id = ?", task.ID).Updates(map[string]any{
		"enabled":      false,
		"next_run_at":  nil,
		"last_status":  RunStatusFailed,
		"last_message": trimMessage(msg),
	}).Error; updateErr != nil {
		logger.Warnf(ctx, "[custom scheduledchat] disable task after auth failure failed: %v", updateErr)
	}
	s.finishRun(ctx, run, task, RunStatusFailed, msg)
}

func (s *Service) completeAssistantMessage(ctx context.Context, assistantMessage *types.Message, userQuery string) {
	assistantMessage.UpdatedAt = time.Now()
	assistantMessage.IsCompleted = true
	if err := s.messageService.UpdateMessage(ctx, assistantMessage); err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] update assistant message failed: %v", err)
		return
	}
	if strings.TrimSpace(userQuery) != "" && strings.TrimSpace(assistantMessage.Content) != "" {
		bgCtx := context.WithoutCancel(ctx)
		go s.messageService.IndexMessageToKB(bgCtx, userQuery, assistantMessage.Content, assistantMessage.ID, assistantMessage.SessionID)
	}
}

func (s *Service) writeAgentQueryEvent(ctx context.Context, sessionID, assistantMessageID string) {
	if s.streamManager == nil {
		return
	}
	_ = s.streamManager.AppendEvent(ctx, sessionID, assistantMessageID, interfaces.StreamEvent{
		ID:        fmt.Sprintf("query-%d", time.Now().UnixNano()),
		Type:      types.ResponseTypeAgentQuery,
		Done:      true,
		Timestamp: time.Now(),
		Data: map[string]interface{}{
			"session_id":           sessionID,
			"assistant_message_id": assistantMessageID,
		},
	})
}

func (s *Service) startStopWatcher(ctx context.Context, sessionID, assistantMessageID string, eventBus *event.EventBus) {
	if s.streamManager == nil {
		return
	}
	go func() {
		watchCtx, cancel := context.WithTimeout(ctx, runTimeout)
		defer cancel()
		ticker := time.NewTicker(300 * time.Millisecond)
		defer ticker.Stop()
		offset := 0
		for {
			select {
			case <-watchCtx.Done():
				return
			case <-ticker.C:
				events, newOffset, err := s.streamManager.GetEvents(watchCtx, sessionID, assistantMessageID, offset)
				if err != nil {
					continue
				}
				offset = newOffset
				for _, evt := range events {
					switch {
					case evt.Type == types.ResponseType(event.EventStop):
						_ = eventBus.Emit(watchCtx, event.Event{
							Type:      event.EventStop,
							SessionID: sessionID,
							Data: event.StopData{
								SessionID: sessionID,
								MessageID: assistantMessageID,
								Reason:    "user_requested",
							},
						})
						return
					case evt.Type == types.ResponseTypeComplete:
						return
					case evt.Type == types.ResponseTypeError && evt.Done:
						return
					}
				}
			}
		}
	}()
}

func (s *Service) getTask(ctx context.Context, id string) (*Task, error) {
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant context is required")
	}
	var task Task
	if err := s.db.WithContext(ctx).First(&task, "id = ? AND tenant_id = ?", strings.TrimSpace(id), tenantID).Error; err != nil {
		return nil, err
	}
	return &task, nil
}

func (s *Service) hasActiveRun(ctx context.Context, taskID string, now time.Time) (bool, error) {
	s.expireStaleRuns(ctx, taskID, now)
	var count int64
	err := s.db.WithContext(ctx).Model(&Run{}).Where("task_id = ? AND status = ?", taskID, RunStatusRunning).Count(&count).Error
	return count > 0, err
}

func (s *Service) hasActiveRunTx(tx *gorm.DB, taskID string, now time.Time) (bool, error) {
	s.expireStaleRunsTx(tx, taskID, now)
	var count int64
	err := tx.Model(&Run{}).Where("task_id = ? AND status = ?", taskID, RunStatusRunning).Count(&count).Error
	return count > 0, err
}

func (s *Service) expireStaleRuns(ctx context.Context, taskID string, now time.Time) {
	s.expireStaleRunsTx(s.db.WithContext(ctx), taskID, now)
}

func (s *Service) expireStaleRunsTx(tx *gorm.DB, taskID string, now time.Time) {
	cutoff := now.Add(-runTimeout)
	_ = tx.Model(&Run{}).
		Where("task_id = ? AND status = ? AND started_at < ?", taskID, RunStatusRunning, cutoff).
		Updates(map[string]any{
			"status":        RunStatusFailed,
			"error_message": "run timed out",
			"finished_at":   now,
		}).Error
}

func (s *Service) lastFinishedRunAt(ctx context.Context, taskID, excludeRunID string, loc *time.Location) string {
	var run Run
	err := s.db.WithContext(ctx).
		Where("task_id = ? AND id <> ? AND finished_at IS NOT NULL", taskID, excludeRunID).
		Order("finished_at DESC").
		First(&run).Error
	if err != nil || run.FinishedAt == nil {
		return ""
	}
	return run.FinishedAt.In(loc).Format("2006-01-02 15:04:05")
}

func (s *Service) successRunCount(ctx context.Context, taskID string) int64 {
	var count int64
	_ = s.db.WithContext(ctx).Model(&Run{}).Where("task_id = ? AND status = ?", taskID, RunStatusSuccess).Count(&count).Error
	return count
}

func normalizeAndValidateTask(task *Task) error {
	task.Name = strings.TrimSpace(task.Name)
	task.AgentID = strings.TrimSpace(task.AgentID)
	task.ScheduleType = strings.TrimSpace(task.ScheduleType)
	task.Timezone = strings.TrimSpace(task.Timezone)
	task.PromptTemplate = strings.TrimSpace(task.PromptTemplate)
	if task.Name == "" {
		return fmt.Errorf("name is required")
	}
	if task.AgentID == "" {
		return fmt.Errorf("agent_id is required")
	}
	if task.Timezone == "" {
		task.Timezone = "Asia/Shanghai"
	}
	if _, err := loadTaskLocation(task.Timezone); err != nil {
		return err
	}
	if task.PromptTemplate == "" {
		return fmt.Errorf("prompt_template is required")
	}
	if task.Minute < 0 || task.Minute > 59 {
		return fmt.Errorf("minute must be between 0 and 59")
	}
	switch task.ScheduleType {
	case ScheduleTypeHourly:
		task.Hour = 0
		task.Weekday = 1
		task.DayOfMonth = 1
	case ScheduleTypeDaily:
		if task.Hour < 0 || task.Hour > 23 {
			return fmt.Errorf("hour must be between 0 and 23")
		}
		task.Weekday = 1
		task.DayOfMonth = 1
	case ScheduleTypeWeekly:
		if task.Hour < 0 || task.Hour > 23 {
			return fmt.Errorf("hour must be between 0 and 23")
		}
		if task.Weekday < 1 || task.Weekday > 7 {
			return fmt.Errorf("weekday must be between 1 and 7")
		}
		task.DayOfMonth = 1
	case ScheduleTypeMonthly:
		if task.Hour < 0 || task.Hour > 23 {
			return fmt.Errorf("hour must be between 0 and 23")
		}
		if task.DayOfMonth < 1 || task.DayOfMonth > 31 {
			return fmt.Errorf("day_of_month must be between 1 and 31")
		}
		task.Weekday = 1
	default:
		return fmt.Errorf("unsupported schedule_type")
	}
	if task.ConcurrencyPolicy == "" {
		task.ConcurrencyPolicy = ConcurrencySkipIfRunning
	}
	return nil
}

func NextRunAfter(task *Task, after time.Time) (time.Time, error) {
	loc, err := loadTaskLocation(task.Timezone)
	if err != nil {
		return time.Time{}, err
	}
	base := after.In(loc)
	switch task.ScheduleType {
	case ScheduleTypeHourly:
		candidate := time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), task.Minute, 0, 0, loc)
		if !candidate.After(base) {
			candidate = candidate.Add(time.Hour)
		}
		return candidate.UTC(), nil
	case ScheduleTypeDaily:
		candidate := time.Date(base.Year(), base.Month(), base.Day(), task.Hour, task.Minute, 0, 0, loc)
		if !candidate.After(base) {
			candidate = candidate.AddDate(0, 0, 1)
		}
		return candidate.UTC(), nil
	case ScheduleTypeWeekly:
		today := weekdayNumber(base.Weekday())
		delta := (task.Weekday - today + 7) % 7
		candidateDay := base.AddDate(0, 0, delta)
		candidate := time.Date(candidateDay.Year(), candidateDay.Month(), candidateDay.Day(), task.Hour, task.Minute, 0, 0, loc)
		if !candidate.After(base) {
			candidate = candidate.AddDate(0, 0, 7)
		}
		return candidate.UTC(), nil
	case ScheduleTypeMonthly:
		firstOfMonth := time.Date(base.Year(), base.Month(), 1, task.Hour, task.Minute, 0, 0, loc)
		for i := 0; i < 48; i++ {
			month := firstOfMonth.AddDate(0, i, 0)
			if task.DayOfMonth > daysInMonth(month.Year(), month.Month(), loc) {
				continue
			}
			candidate := time.Date(month.Year(), month.Month(), task.DayOfMonth, task.Hour, task.Minute, 0, 0, loc)
			if candidate.After(base) {
				return candidate.UTC(), nil
			}
		}
		return time.Time{}, fmt.Errorf("cannot calculate next monthly run")
	default:
		return time.Time{}, fmt.Errorf("unsupported schedule_type")
	}
}

func Variables() []Variable {
	return []Variable{
		{Name: "task_name", Label: "任务名称", Description: "当前定时任务名称"},
		{Name: "agent_name", Label: "智能体名称", Description: "任务选择的智能体名称"},
		{Name: "scheduled_at", Label: "计划触发时间", Description: "本次计划触发时间"},
		{Name: "triggered_at", Label: "实际触发时间", Description: "任务实际开始执行的时间"},
		{Name: "current_time", Label: "当前时间", Description: "当前时间，格式 2006-01-02 15:04:05"},
		{Name: "current_date", Label: "当前日期", Description: "当前日期，格式 2006-01-02"},
		{Name: "current_week", Label: "当前星期", Description: "当前星期几"},
		{Name: "yesterday", Label: "昨天日期", Description: "昨天日期"},
		{Name: "week_start", Label: "本周开始", Description: "本周周一日期"},
		{Name: "week_end", Label: "本周结束", Description: "本周周日日期"},
		{Name: "month_start", Label: "本月开始", Description: "本月第一天"},
		{Name: "month_end", Label: "本月结束", Description: "本月最后一天"},
		{Name: "last_run_at", Label: "上次运行时间", Description: "该任务上一次完成运行的时间"},
		{Name: "last_success_at", Label: "上次成功时间", Description: "该任务上一次成功完成的时间"},
		{Name: "run_count", Label: "成功次数", Description: "该任务历史成功运行次数"},
		{Name: "timezone", Label: "任务时区", Description: "该任务使用的时区"},
		{Name: "tenant_name", Label: "组织名称", Description: "当前租户/组织名称"},
		{Name: "user_name", Label: "执行用户", Description: "任务发起用户名称"},
		{Name: "language", Label: "用户语言", Description: "当前界面语言偏好"},
	}
}

func PromptTemplates() []PromptTemplate {
	return []PromptTemplate{
		{
			ID:          "daily-summary",
			Name:        "每日摘要",
			Description: "按天汇总重点信息、风险和待办",
			Content:     "请基于你能访问的知识库和工具，生成 {{current_date}} 的每日摘要，包含：\n1. 关键进展\n2. 风险与异常\n3. 后续待办\n请用 {{language}} 输出。",
		},
		{
			ID:          "weekly-review",
			Name:        "每周复盘",
			Description: "汇总本周情况并给出下周建议",
			Content:     "请总结 {{week_start}} 至 {{week_end}} 的重要事项，输出本周结论、未解决问题和下周建议。请用 {{language}} 输出。",
		},
		{
			ID:          "monthly-report",
			Name:        "月度报告",
			Description: "生成本月阶段性报告",
			Content:     "请生成 {{month_start}} 至 {{month_end}} 的月度报告，重点关注成果、数据变化、风险和下一步计划。请用 {{language}} 输出。",
		},
		{
			ID:          "risk-scan",
			Name:        "风险巡检",
			Description: "定期检查异常、风险和需要人工跟进的问题",
			Content:     "请检查截至 {{current_time}} 的风险、异常和待处理事项。若没有明显风险，请说明检查范围和结论。请用 {{language}} 输出。",
		},
	}
}

func isTaskOwner(ctx context.Context, task *Task) bool {
	userID, _ := types.UserIDFromContext(ctx)
	return userID != "" && task != nil && task.CreatedBy == userID
}

func canManageTask(ctx context.Context, task *Task) bool {
	if isTaskOwner(ctx, task) {
		return true
	}
	return types.TenantRoleFromContext(ctx).HasPermission(types.TenantRoleAdmin)
}

func loadTaskLocation(name string) (*time.Location, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("invalid timezone")
	}
	return loc, nil
}

func weekdayNumber(w time.Weekday) int {
	if w == time.Sunday {
		return 7
	}
	return int(w)
}

func daysInMonth(year int, month time.Month, loc *time.Location) int {
	return time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
}

func startOfWeek(t time.Time) time.Time {
	offset := weekdayNumber(t.Weekday()) - 1
	d := t.AddDate(0, 0, -offset)
	return time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, t.Location())
}

func chineseWeekday(t time.Time) string {
	switch t.Weekday() {
	case time.Monday:
		return "星期一"
	case time.Tuesday:
		return "星期二"
	case time.Wednesday:
		return "星期三"
	case time.Thursday:
		return "星期四"
	case time.Friday:
		return "星期五"
	case time.Saturday:
		return "星期六"
	default:
		return "星期日"
	}
}

func formatTimePtr(value *time.Time, loc *time.Location) string {
	if value == nil {
		return ""
	}
	return value.In(loc).Format("2006-01-02 15:04:05")
}

func appendQuickAnswerReasoning(msg *types.Message, content string) {
	if content == "" {
		return
	}
	if len(msg.AgentSteps) == 0 {
		msg.AgentSteps = types.AgentSteps{{
			Iteration: 0,
			Timestamp: time.Now(),
			ToolCalls: make([]types.ToolCall, 0),
		}}
	}
	msg.AgentSteps[0].ReasoningContent += content
}

func trimMessage(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 1000 {
		return value[:1000]
	}
	return value
}
