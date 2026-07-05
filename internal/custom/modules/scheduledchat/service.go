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
	secutils "github.com/Tencent/WeKnora/internal/utils"
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

type scheduledSessionStateBackfillRow struct {
	SessionID        string
	TenantID         uint64
	AgentID          string
	WebSearchEnabled bool
	RequestContext   RequestContext
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
	fileService         interfaces.FileService
	modelService        interfaces.ModelService
	attachmentProcessor *sessionhandler.AttachmentProcessor

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
	fileService interfaces.FileService,
	modelService interfaces.ModelService,
	attachmentProcessor *sessionhandler.AttachmentProcessor,
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
		fileService:         fileService,
		modelService:        modelService,
		attachmentProcessor: attachmentProcessor,
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
	if err := s.backfillScheduledSessionLastRequestStates(ctx, db); err != nil {
		logger.Warnf(ctx, "[custom scheduledchat] backfill session request state failed: %v", err)
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

func (s *Service) backfillScheduledSessionLastRequestStates(ctx context.Context, db *gorm.DB) error {
	var rows []scheduledSessionStateBackfillRow
	if err := db.WithContext(ctx).
		Table("sessions AS s").
		Select("s.id AS session_id, tasks.tenant_id AS tenant_id, COALESCE(tasks.agent_id, '') AS agent_id, COALESCE(tasks.web_search_enabled, false) AS web_search_enabled, tasks.request_context AS request_context").
		Joins("JOIN custom_scheduled_chat_runs AS runs ON runs.session_id = s.id AND runs.deleted_at IS NULL").
		Joins("JOIN custom_scheduled_chat_tasks AS tasks ON tasks.id = runs.task_id").
		Where("s.deleted_at IS NULL").
		Where("s.description LIKE ?", SessionMarkerPrefix+"%").
		Where("s.agent_config IS NULL").
		Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		agentID := strings.TrimSpace(row.AgentID)
		if agentID == "" {
			continue
		}
		agent := s.lookupAgentForSessionStateBackfill(ctx, db, row.TenantID, agentID)
		task := &Task{
			TenantID:         row.TenantID,
			AgentID:          agentID,
			WebSearchEnabled: row.WebSearchEnabled,
		}
		state := scheduledLastRequestState(task, agent, row.RequestContext)
		if state == nil {
			continue
		}
		stateValue, err := state.Value()
		if err != nil {
			return err
		}
		if err := db.WithContext(ctx).
			Table("sessions").
			Where("id = ? AND agent_config IS NULL", row.SessionID).
			UpdateColumn("agent_config", stateValue).Error; err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) lookupAgentForSessionStateBackfill(ctx context.Context, db *gorm.DB, tenantID uint64, agentID string) *types.CustomAgent {
	if strings.TrimSpace(agentID) == "" || tenantID == 0 {
		return nil
	}
	lookupCtx := context.WithValue(ctx, types.TenantIDContextKey, tenantID)
	lookupCtx = context.WithValue(lookupCtx, types.TenantRoleContextKey, types.TenantRoleAdmin)
	if s != nil && s.agentShareService != nil {
		if agent, err := s.agentShareService.GetSharedAgentForTenant(lookupCtx, tenantID, types.TenantRoleAdmin, agentID); err == nil && agent != nil {
			return agent
		}
	}
	if s != nil && s.customAgentService != nil {
		if agent, err := s.customAgentService.GetAgentByID(lookupCtx, agentID); err == nil && agent != nil {
			return agent
		}
	}
	if agent := types.GetBuiltinAgentWithContext(lookupCtx, agentID, tenantID); agent != nil {
		return agent
	}
	if db == nil {
		return nil
	}
	var own types.CustomAgent
	if err := db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", agentID, tenantID).
		First(&own).Error; err == nil {
		return &own
	}
	var shared types.CustomAgent
	if err := db.WithContext(ctx).
		Table("custom_agents AS agents").
		Select("agents.*").
		Joins("JOIN agent_shares AS shares ON shares.agent_id = agents.id AND shares.source_tenant_id = agents.tenant_id AND shares.deleted_at IS NULL").
		Joins("JOIN organization_tenant_members AS otm ON otm.organization_id = shares.organization_id").
		Where("agents.id = ? AND otm.tenant_id = ? AND shares.source_tenant_id != ? AND agents.deleted_at IS NULL", agentID, tenantID, tenantID).
		First(&shared).Error; err == nil {
		return &shared
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
		RequestContext:    normalizeRequestContext(req.RequestContext),
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
	task.RequestContext = normalizeRequestContext(req.RequestContext)
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
	requestContext := effectiveRequestContextForAgent(task.RequestContext, agent)

	renderedPrompt, err := s.renderPrompt(runCtx, &task, &run, agent, tenant, user)
	if err != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, err.Error())
		return
	}
	renderedPrompt = applyProfessionalSkillPrefix(requestContext.ProfessionalSkillNames, renderedPrompt)
	run.RenderedPrompt = renderedPrompt
	if err := s.db.WithContext(runCtx).Model(&Run{}).Where("id = ?", run.ID).
		Update("rendered_prompt", renderedPrompt).Error; err != nil {
		logger.Warnf(runCtx, "[custom scheduledchat] update rendered prompt failed: %v", err)
	}
	images, imageURLs, imageDescription, attachments, err := s.prepareRequestMedia(runCtx, agent, renderedPrompt, requestContext)
	if err != nil {
		s.finishRun(runCtx, &run, &task, RunStatusFailed, err.Error())
		return
	}

	session, userMsg, assistantMsg, err := s.createConversation(runCtx, &task, &run, agent, renderedPrompt, requestContext, images, attachments)
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
		KnowledgeBaseIDs:   cloneStringSlice(requestContext.KnowledgeBaseIDs),
		KnowledgeIDs:       cloneStringSlice(requestContext.KnowledgeIDs),
		TagScopes:          cloneTagScopes(requestContext.TagScopes),
		MCPServiceIDs:      cloneStringSlice(requestContext.MCPServiceIDs),
		SkillNames:         cloneStringSlice(requestContext.SkillNames),
		ImageURLs:          imageURLs,
		ImageDescription:   imageDescription,
		SummaryModelID:     requestContext.SummaryModelID,
		UserMessageID:      userMsg.ID,
		WebSearchEnabled:   task.WebSearchEnabled,
		EnableMemory:       false,
		Attachments:        attachments,
	}

	var serviceErr error
	if agentMode {
		serviceErr = sessionhandler.RunAgentQA(execCtx, s.sessionService, qaReq, eventBus)
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
	agent *types.CustomAgent,
	renderedPrompt string,
	requestContext RequestContext,
	images []sessionhandler.ImageAttachment,
	attachments types.MessageAttachments,
) (*types.Session, *types.Message, *types.Message, error) {
	session, err := s.sessionService.CreateSession(ctx, &types.Session{
		Title:            sessionTitleForRun(task, run),
		Description:      fmt.Sprintf("%stask=%s;run=%s", SessionMarkerPrefix, task.ID, run.ID),
		TenantID:         task.TenantID,
		UserID:           task.RunAsUserID,
		LastRequestState: scheduledLastRequestState(task, agent, requestContext),
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
		SessionID:      session.ID,
		Role:           "user",
		Content:        renderedPrompt,
		RequestID:      requestID,
		MentionedItems: cloneMentionedItems(requestContext.MentionedItems),
		Images:         sessionhandler.ConvertImageAttachments(images),
		Attachments:    append(types.MessageAttachments(nil), attachments...),
		IsCompleted:    true,
		Channel:        MessageChannel,
		CreatedAt:      time.Now(),
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

func scheduledLastRequestState(task *Task, agent *types.CustomAgent, requestContext RequestContext) *types.SessionLastRequestState {
	if task == nil {
		return nil
	}
	agentEnabled := false
	if agent != nil {
		agentEnabled = agent.IsAgentMode()
	}
	state := &types.SessionLastRequestState{
		AgentID:                task.AgentID,
		AgentEnabled:           agentEnabled,
		ModelID:                requestContext.SummaryModelID,
		KnowledgeBaseIDs:       cloneStringSlice(requestContext.KnowledgeBaseIDs),
		KnowledgeIDs:           cloneStringSlice(requestContext.KnowledgeIDs),
		TagIDs:                 cloneStringSlice(requestContext.TagIDs),
		MCPServiceIDs:          cloneStringSlice(requestContext.MCPServiceIDs),
		SkillNames:             cloneStringSlice(requestContext.SkillNames),
		ProfessionalSkillNames: cloneStringSlice(requestContext.ProfessionalSkillNames),
		MentionedItems:         append(types.MentionedItems(nil), requestContext.MentionedItems...),
		WebSearchEnabled:       task.WebSearchEnabled,
	}
	if agent != nil && agent.TenantID != 0 && task.TenantID != 0 && agent.TenantID != task.TenantID {
		state.AgentSourceTenantID = strconv.FormatUint(agent.TenantID, 10)
	}
	return state
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
	var selectedAgent *types.CustomAgent
	if req.AgentID != "" {
		if agent, err := s.resolveAgentForContext(ctx, req.AgentID); err == nil && agent != nil {
			agentName = agent.Name
			selectedAgent = agent
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
	rendered, err := s.renderPrompt(ctx, task, run, agent, tenant, user)
	if err != nil {
		return "", err
	}
	requestContext := effectiveRequestContextForAgent(req.RequestContext, selectedAgent)
	return applyProfessionalSkillPrefix(requestContext.ProfessionalSkillNames, rendered), nil
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

func (s *Service) prepareRequestMedia(
	ctx context.Context,
	agent *types.CustomAgent,
	query string,
	requestContext RequestContext,
) ([]sessionhandler.ImageAttachment, []string, string, types.MessageAttachments, error) {
	images := cloneImageAttachments(requestContext.Images)
	if len(images) > 0 {
		if agent == nil || !agent.Config.ImageUploadEnabled {
			return nil, nil, "", nil, fmt.Errorf("image upload is not enabled for this agent")
		}
		if types.IsClaudeSDKAgentType(agent.Config.AgentType) && strings.TrimSpace(agent.Config.VLMModelID) == "" {
			return nil, nil, "", nil, fmt.Errorf("general agent image upload requires a configured VLM model")
		}
		if s.fileService == nil {
			return nil, nil, "", nil, fmt.Errorf("image storage service is not configured")
		}
		tenantID, _ := types.TenantIDFromContext(ctx)
		if err := sessionhandler.SaveImageAttachments(ctx, s.fileService, images, tenantID, agent.Config.ImageStorageProvider); err != nil {
			return nil, nil, "", nil, err
		}
		if strings.TrimSpace(agent.Config.VLMModelID) != "" {
			sessionhandler.AnalyzeImageAttachments(ctx, s.modelService, images, agent.Config.VLMModelID, query)
		}
	}
	imageURLs, imageDescription := sessionhandler.ExtractImageURLsAndOCRText(images)

	uploads := requestContext.AttachmentUploads
	if len(uploads) == 0 {
		return images, imageURLs, imageDescription, nil, nil
	}
	if s.attachmentProcessor == nil {
		return nil, nil, "", nil, fmt.Errorf("attachment processor is not configured")
	}
	maxSizeMB := secutils.GetMaxFileSizeMB()
	maxSize := int64(maxSizeMB) * 1024 * 1024
	for i, upload := range uploads {
		if upload.FileSize > maxSize {
			return nil, nil, "", nil, fmt.Errorf("attachment %d exceeds size limit of %dMB", i+1, maxSizeMB)
		}
	}

	tenantID, _ := types.TenantIDFromContext(ctx)
	asrModelID := ""
	if agent != nil && agent.Config.AudioUploadEnabled && agent.Config.ASRModelID != "" {
		asrModelID = agent.Config.ASRModelID
	}
	attachments := make(types.MessageAttachments, 0, len(uploads))
	for i, upload := range uploads {
		data, err := sessionhandler.DecodeBase64Attachment(upload.Data)
		if err != nil {
			return nil, nil, "", nil, fmt.Errorf("attachment %d decode failed: %w", i+1, err)
		}
		fileSize := upload.FileSize
		if fileSize <= 0 {
			fileSize = int64(len(data))
		}
		if fileSize > maxSize {
			return nil, nil, "", nil, fmt.Errorf("attachment %d exceeds size limit of %dMB", i+1, maxSizeMB)
		}
		processed, err := s.attachmentProcessor.ProcessAttachment(
			ctx,
			data,
			upload.FileName,
			fileSize,
			tenantID,
			asrModelID,
		)
		if err != nil {
			return nil, nil, "", nil, fmt.Errorf("attachment %d processing failed: %w", i+1, err)
		}
		attachments = append(attachments, *processed)
	}
	return images, imageURLs, imageDescription, attachments, nil
}

func normalizeRequestContext(ctx RequestContext) RequestContext {
	normalized := RequestContext{
		KnowledgeBaseIDs:       uniqueTrimmedStrings(ctx.KnowledgeBaseIDs),
		KnowledgeIDs:           uniqueTrimmedStrings(ctx.KnowledgeIDs),
		TagIDs:                 uniqueTrimmedStrings(ctx.TagIDs),
		TagScopes:              normalizeTagScopes(ctx.TagScopes),
		MCPServiceIDs:          uniqueTrimmedStrings(ctx.MCPServiceIDs),
		SkillNames:             uniqueTrimmedStrings(ctx.SkillNames),
		ProfessionalSkillNames: uniqueTrimmedStrings(ctx.ProfessionalSkillNames),
		SummaryModelID:         strings.TrimSpace(ctx.SummaryModelID),
		Images:                 normalizeImageAttachments(ctx.Images),
		AttachmentUploads:      normalizeAttachmentUploads(ctx.AttachmentUploads),
	}
	for _, item := range ctx.MentionedItems {
		item.ID = strings.TrimSpace(item.ID)
		item.Name = strings.TrimSpace(item.Name)
		item.Type = strings.TrimSpace(item.Type)
		item.KBType = strings.TrimSpace(item.KBType)
		item.KBID = strings.TrimSpace(item.KBID)
		item.KBName = strings.TrimSpace(item.KBName)
		item.ServiceID = strings.TrimSpace(item.ServiceID)
		item.SkillName = strings.TrimSpace(item.SkillName)
		if item.ID == "" || item.Type == "" {
			continue
		}
		normalized.MentionedItems = append(normalized.MentionedItems, item)
		switch item.Type {
		case "kb":
			normalized.KnowledgeBaseIDs = appendUniqueString(normalized.KnowledgeBaseIDs, item.ID)
		case "file":
			normalized.KnowledgeIDs = appendUniqueString(normalized.KnowledgeIDs, item.ID)
		case "tag":
			normalized.TagIDs = appendUniqueString(normalized.TagIDs, item.ID)
			normalized.KnowledgeBaseIDs = appendUniqueString(normalized.KnowledgeBaseIDs, item.KBID)
		case "mcp":
			normalized.MCPServiceIDs = appendUniqueString(normalized.MCPServiceIDs, item.ID)
		case "skill":
			if item.SkillName != "" {
				normalized.SkillNames = appendUniqueString(normalized.SkillNames, item.SkillName)
			} else {
				normalized.SkillNames = appendUniqueString(normalized.SkillNames, item.ID)
			}
		}
	}
	return normalized
}

func effectiveRequestContextForAgent(ctx RequestContext, agent *types.CustomAgent) RequestContext {
	normalized := normalizeRequestContext(ctx)
	normalized.ProfessionalSkillNames = professionalSkillNamesAllowedByAgent(
		normalized.ProfessionalSkillNames,
		agent,
	)
	return normalized
}

func professionalSkillNamesAllowedByAgent(requested []string, agent *types.CustomAgent) []string {
	requested = uniqueTrimmedStrings(requested)
	if len(requested) == 0 || agent == nil {
		return nil
	}
	mode := strings.TrimSpace(agent.Config.ProfessionalSkillsSelectionMode)
	if mode == "" || mode == "none" {
		return nil
	}
	if mode == "all" {
		return requested
	}
	if mode != "selected" {
		return nil
	}
	allowed := map[string]bool{}
	for _, name := range uniqueTrimmedStrings(agent.Config.SelectedProfessionalSkills) {
		allowed[name] = true
	}
	out := make([]string, 0, len(requested))
	for _, name := range requested {
		if allowed[name] {
			out = append(out, name)
		}
	}
	return out
}

func normalizeTagScopes(scopes []types.TagScope) []types.TagScope {
	out := make([]types.TagScope, 0, len(scopes))
	seen := map[string]map[string]bool{}
	for _, scope := range scopes {
		kbID := strings.TrimSpace(scope.KnowledgeBaseID)
		if kbID == "" {
			continue
		}
		tagIDs := uniqueTrimmedStrings(scope.TagIDs)
		if len(tagIDs) == 0 {
			continue
		}
		if seen[kbID] == nil {
			seen[kbID] = map[string]bool{}
			out = append(out, types.TagScope{KnowledgeBaseID: kbID})
		}
		for _, tagID := range tagIDs {
			if seen[kbID][tagID] {
				continue
			}
			seen[kbID][tagID] = true
			for i := range out {
				if out[i].KnowledgeBaseID == kbID {
					out[i].TagIDs = append(out[i].TagIDs, tagID)
					break
				}
			}
		}
	}
	return out
}

func applyProfessionalSkillPrefix(skillNames []string, query string) string {
	names := uniqueTrimmedStrings(skillNames)
	if len(names) == 0 {
		return query
	}
	parts := make([]string, 0, len(names))
	for _, name := range names {
		parts = append(parts, fmt.Sprintf("%s技能", name))
	}
	return fmt.Sprintf("使用%s完成以下工作\n%s", strings.Join(parts, "、"), query)
}

func uniqueTrimmedStrings(values []string) []string {
	out := make([]string, 0, len(values))
	seen := map[string]bool{}
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

func appendUniqueString(values []string, value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return values
	}
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func cloneStringSlice(values []string) []string {
	return append([]string(nil), values...)
}

func cloneTagScopes(values []types.TagScope) []types.TagScope {
	out := make([]types.TagScope, 0, len(values))
	for _, value := range values {
		out = append(out, types.TagScope{
			KnowledgeBaseID: value.KnowledgeBaseID,
			TagIDs:          cloneStringSlice(value.TagIDs),
		})
	}
	return out
}

func normalizeImageAttachments(values []sessionhandler.ImageAttachment) []sessionhandler.ImageAttachment {
	out := make([]sessionhandler.ImageAttachment, 0, len(values))
	for _, value := range values {
		value.URL = strings.TrimSpace(value.URL)
		value.Caption = strings.TrimSpace(value.Caption)
		if strings.TrimSpace(value.Data) == "" && value.URL == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func cloneImageAttachments(values []sessionhandler.ImageAttachment) []sessionhandler.ImageAttachment {
	out := make([]sessionhandler.ImageAttachment, len(values))
	copy(out, values)
	return out
}

func normalizeAttachmentUploads(values []sessionhandler.AttachmentUpload) []sessionhandler.AttachmentUpload {
	out := make([]sessionhandler.AttachmentUpload, 0, len(values))
	for _, value := range values {
		value.FileName = strings.TrimSpace(value.FileName)
		if strings.TrimSpace(value.Data) == "" || value.FileName == "" {
			continue
		}
		out = append(out, value)
	}
	return out
}

func cloneMentionedItems(values types.MentionedItems) types.MentionedItems {
	return append(types.MentionedItems(nil), values...)
}

func normalizeAndValidateTask(task *Task) error {
	task.Name = strings.TrimSpace(task.Name)
	task.AgentID = strings.TrimSpace(task.AgentID)
	task.ScheduleType = strings.TrimSpace(task.ScheduleType)
	task.Timezone = strings.TrimSpace(task.Timezone)
	task.PromptTemplate = strings.TrimSpace(task.PromptTemplate)
	task.RequestContext = normalizeRequestContext(task.RequestContext)
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
		{
			ID:          "weekly-work-report",
			Name:        "周工作报告",
			Description: "生成面向团队或上级的周报",
			Content:     "请生成 {{week_start}} 至 {{week_end}} 的周工作报告，面向团队负责人阅读。请按以下结构输出：\n1. 本周完成事项：用结果导向表述，尽量给出可量化数据\n2. 关键进展与里程碑：说明影响和价值\n3. 问题与风险：区分已解决、处理中、需协助\n4. 下周计划：列出优先级、负责人建议和预期产出\n5. 需要决策或支持的事项\n请用 {{language}} 输出，语气专业、简洁。",
		},
		{
			ID:          "monthly-business-review",
			Name:        "月度经营分析",
			Description: "梳理核心指标、变化原因和行动建议",
			Content:     "请基于可访问的数据、知识库和工具，生成 {{month_start}} 至 {{month_end}} 的月度经营分析。输出结构：\n1. 本月核心结论：先给 3-5 条要点\n2. 核心指标变化：列出指标、环比/同比或阶段变化、异常点\n3. 变化原因分析：区分数据事实、合理推断和待验证假设\n4. 机会与风险：说明影响范围和优先级\n5. 下月行动建议：给出可执行动作、目标和跟踪指标\n请用 {{language}} 输出。",
		},
		{
			ID:          "industry-research-brief",
			Name:        "行业调研简报",
			Description: "定期整理行业动态、趋势和机会",
			Content:     "请围绕当前任务相关行业，生成截至 {{current_date}} 的行业调研简报。请使用可访问的网络搜索、知识库或文件信息，并标注信息来源类型。输出结构：\n1. 摘要：3-5 条最重要结论\n2. 市场/政策/技术/客户需求动态\n3. 重要公司或产品动作\n4. 对我们的机会、威胁和影响\n5. 后续需要深入验证的问题清单\n请用 {{language}} 输出。",
		},
		{
			ID:          "competitor-monitor",
			Name:        "竞品动态跟踪",
			Description: "跟踪竞品产品、市场和内容更新",
			Content:     "请跟踪截至 {{current_time}} 的竞品动态，优先使用网络搜索、知识库和已选择文件。请输出：\n1. 新增动态列表：竞品名称、动作、时间、来源线索\n2. 动态类型归类：产品功能、定价、市场活动、客户案例、融资/组织、内容发布\n3. 可能影响：对客户、销售、产品路线或市场定位的影响\n4. 建议跟进动作：需要谁关注、建议何时处理、预期产出\n如果信息不足，请明确缺口和建议补充的数据源。请用 {{language}} 输出。",
		},
		{
			ID:          "data-analysis-digest",
			Name:        "数据分析解读",
			Description: "把周期性数据转成结论和行动项",
			Content:     "请对当前任务涉及的数据进行分析，生成截至 {{current_time}} 的数据解读报告。请按以下结构输出：\n1. 数据范围与口径：说明时间范围、指标定义和数据来源\n2. 关键发现：列出最重要的变化、异常和趋势\n3. 分析过程：说明对比维度、分组、排序或筛选逻辑\n4. 可能原因：区分事实、推断和待验证假设\n5. 行动建议：给出优先级、预期收益和跟踪指标\n请用 {{language}} 输出。",
		},
		{
			ID:          "research-material-digest",
			Name:        "调研资料综述",
			Description: "汇总知识库/文件中的调研材料",
			Content:     "请基于已选择的知识库和文件，生成调研资料综述。请输出：\n1. 资料范围：列出涉及的主题、文件或知识库线索\n2. 核心观点：按主题归纳，不要简单罗列原文\n3. 证据与依据：概括关键数据、案例或论据\n4. 分歧与不确定性：指出材料之间的冲突或信息缺口\n5. 可直接用于报告的段落草稿\n请用 {{language}} 输出。",
		},
		{
			ID:          "meeting-action-summary",
			Name:        "会议纪要与行动项",
			Description: "把会议材料整理成纪要和待办",
			Content:     "请基于已选择的会议记录、文档或上下文，整理会议纪要。输出结构：\n1. 会议主题与时间：如无法确定请说明\n2. 关键结论：按议题归纳\n3. 决策事项：列出决策内容、影响和后续要求\n4. 行动项：用表格列出事项、负责人、截止时间、依赖和状态\n5. 待确认问题：列出需要补充信息的问题\n请用 {{language}} 输出。",
		},
		{
			ID:          "project-progress-risk",
			Name:        "项目进展与风险",
			Description: "定期输出项目状态、阻塞和风险",
			Content:     "请生成截至 {{current_date}} 的项目进展与风险报告。请输出：\n1. 项目总体状态：正常/需关注/高风险，并说明判断依据\n2. 本周期完成进展：按模块或里程碑列出\n3. 延期、阻塞与风险：包含影响、概率、责任方和缓解建议\n4. 下周期计划：明确交付物、优先级和依赖\n5. 需要管理层或跨团队协助的事项\n请用 {{language}} 输出。",
		},
		{
			ID:          "sales-customer-followup",
			Name:        "客户跟进摘要",
			Description: "整理客户沟通、需求和下一步动作",
			Content:     "请基于已选择的客户资料、沟通记录或知识库内容，生成客户跟进摘要。请输出：\n1. 客户背景与当前阶段\n2. 近期沟通要点：需求、异议、预算、决策链、时间表\n3. 风险与机会：说明可能影响成交或续约的因素\n4. 下一步跟进行动：事项、负责人建议、截止时间和话术要点\n5. 可同步给团队的简短摘要\n请用 {{language}} 输出。",
		},
		{
			ID:          "news-public-opinion-monitor",
			Name:        "新闻舆情监测",
			Description: "监控新闻、舆情和外部信号",
			Content:     "请监测截至 {{current_time}} 与任务主题相关的新闻、舆情和外部信号。请输出：\n1. 重要动态：按影响程度排序，说明来源线索和时间\n2. 情绪与立场：正面/中性/负面及代表性观点\n3. 潜在影响：对品牌、业务、客户或政策环境的影响\n4. 需要响应的事项：建议动作、优先级和责任方\n5. 信息不足或需要继续跟踪的方向\n请用 {{language}} 输出。",
		},
		{
			ID:          "executive-briefing",
			Name:        "管理层简报",
			Description: "把复杂信息压缩成管理层可读摘要",
			Content:     "请将本次任务相关信息整理成管理层简报。要求先给结论，再给依据。输出结构：\n1. 一页摘要：3-5 条最重要结论\n2. 背景：为什么现在需要关注\n3. 关键事实与数据：只保留影响判断的信息\n4. 决策选项：列出可选方案、收益、风险和资源需求\n5. 建议决策与下一步\n请用 {{language}} 输出，避免冗长背景铺垫。",
		},
		{
			ID:          "report-outline-draft",
			Name:        "报告大纲与初稿",
			Description: "从资料生成正式报告结构和初稿",
			Content:     "请基于已选择资料，为当前主题生成一份正式报告的大纲和初稿。请输出：\n1. 报告标题建议\n2. 适用读者与写作目标\n3. 详细大纲：每个章节说明要回答的问题和需要的数据\n4. 初稿正文：先完成摘要、背景、分析、结论与建议\n5. 待补充材料清单：列出缺失数据、图表或访谈信息\n请用 {{language}} 输出。",
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
