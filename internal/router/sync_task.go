package router

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/taskretry"
	"github.com/Tencent/WeKnora/internal/custom/modules/terminalrepair"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
)

// SyncTaskExecutor executes tasks synchronously (in a goroutine) without Redis.
// Used in Lite mode as a drop-in replacement for *asynq.Client.
type SyncTaskExecutor struct {
	mu       sync.RWMutex
	handlers map[string]func(context.Context, *asynq.Task) error
	tasks    map[string]*syncTaskRecord
	retained map[string]time.Time
	archived map[string]time.Time
	unique   map[string]syncUniqueLease
	now      func() time.Time
	shutdown bool
	// retryDelay is a test seam; production uses bounded linear backoff.
	retryDelay func(int) time.Duration
}

type syncTaskRecord struct {
	taskType        string
	payload         []byte
	cancel          context.CancelFunc
	cancelWithCause context.CancelCauseFunc
	active          bool
	workers         int
	uniqueKey       string
	attempt         int
	lastError       string
	timedOut        bool
}

type syncUniqueLease struct {
	taskID    string
	expiresAt time.Time
}

type syncTaskOptions struct {
	maxRetry  int
	queue     string
	taskID    string
	timeout   time.Duration
	deadline  time.Time
	processAt time.Time
	retention time.Duration
	uniqueTTL time.Duration
	group     string
}

type syncTaskOutcome int

const (
	syncTaskCancelled syncTaskOutcome = iota
	syncTaskSucceeded
	syncTaskExhausted
	syncTaskRevoked

	syncDefaultTimeout   = 30 * time.Minute
	syncArchiveRetention = 90 * 24 * time.Hour
)

var (
	ErrSyncTaskExecutorShutdown = errors.New("sync task executor is shut down")
	ErrSyncTaskNotFound         = errors.New("sync task not found")
)

type syncTaskSnapshot struct {
	taskType string
	payload  []byte
}

func NewSyncTaskExecutor() *SyncTaskExecutor {
	return &SyncTaskExecutor{
		handlers: make(map[string]func(context.Context, *asynq.Task) error),
		tasks:    make(map[string]*syncTaskRecord),
		retained: make(map[string]time.Time),
		archived: make(map[string]time.Time),
		unique:   make(map[string]syncUniqueLease),
		now:      time.Now,
		retryDelay: func(attempt int) time.Duration {
			delay := time.Duration(attempt) * 5 * time.Second
			if delay > 30*time.Second {
				return 30 * time.Second
			}
			return delay
		},
	}
}

// RegisterHandler registers a handler for a given task type pattern.
func (e *SyncTaskExecutor) RegisterHandler(pattern string, handler func(context.Context, *asynq.Task) error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.handlers[pattern] = handler
}

// Enqueue satisfies interfaces.TaskEnqueuer. Lite mode still executes in local
// goroutines, but option composition and lifecycle semantics intentionally
// mirror asynq.Client: the last scheduling option wins, deadlines fence every
// attempt, successful Unique leases are released immediately, and stable IDs
// remain reserved while completed/archived records are retained.
func (e *SyncTaskExecutor) Enqueue(task *asynq.Task, opts ...asynq.Option) (*asynq.TaskInfo, error) {
	if task == nil {
		return nil, errors.New("task cannot be nil")
	}
	if strings.TrimSpace(task.Type()) == "" {
		return nil, errors.New("task typename cannot be empty")
	}
	now := e.clockNow()
	options, err := composeSyncTaskOptions(now, opts)
	if err != nil {
		return nil, err
	}

	e.mu.RLock()
	handler, ok := e.handlers[task.Type()]
	e.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("sync task executor: no handler registered for type %q", task.Type())
	}

	taskCtx, cancelTask := context.WithCancelCause(context.Background())
	uniqueKey := syncUniqueKey(options.queue, task.Type(), task.Payload(), options.uniqueTTL)
	e.mu.Lock()
	e.ensureLifecycleMapsLocked()
	e.pruneLifecycleRecordsLocked(now)
	if e.shutdown {
		e.mu.Unlock()
		cancelTask(ErrSyncTaskExecutorShutdown)
		return nil, ErrSyncTaskExecutorShutdown
	}
	if _, exists := e.tasks[options.taskID]; exists ||
		recordLiveAt(e.retained[options.taskID], now) ||
		recordLiveAt(e.archived[options.taskID], now) {
		e.mu.Unlock()
		cancelTask(context.Canceled)
		return nil, asynq.ErrTaskIDConflict
	}
	if _, exists := e.unique[uniqueKey]; uniqueKey != "" && exists {
		e.mu.Unlock()
		cancelTask(context.Canceled)
		return nil, asynq.ErrDuplicateTask
	}
	if uniqueKey != "" {
		e.unique[uniqueKey] = syncUniqueLease{taskID: options.taskID, expiresAt: now.Add(options.uniqueTTL)}
	}
	e.tasks[options.taskID] = &syncTaskRecord{
		taskType: task.Type(), payload: append([]byte(nil), task.Payload()...),
		cancel: func() { cancelTask(context.Canceled) }, cancelWithCause: cancelTask,
		uniqueKey: uniqueKey,
	}
	e.mu.Unlock()

	info := &asynq.TaskInfo{
		ID: options.taskID, Queue: options.queue, Type: task.Type(),
		Payload: append([]byte(nil), task.Payload()...), MaxRetry: options.maxRetry,
		Timeout: options.timeout, Deadline: options.deadline, Group: options.group,
		NextProcessAt: options.processAt, Retention: options.retention,
	}
	if info.Timeout == 0 && info.Deadline.IsZero() {
		info.Timeout = syncDefaultTimeout
	}

	go e.executeTask(taskCtx, cancelTask, task, handler, options, uniqueKey)
	return info, nil
}

func composeSyncTaskOptions(now time.Time, opts []asynq.Option) (syncTaskOptions, error) {
	result := syncTaskOptions{
		maxRetry: 25, queue: "default", taskID: uuid.NewString(), processAt: now,
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		switch opt.Type() {
		case asynq.MaxRetryOpt:
			result.maxRetry, _ = opt.Value().(int)
			if result.maxRetry < 0 {
				result.maxRetry = 0
			}
		case asynq.QueueOpt:
			result.queue, _ = opt.Value().(string)
			if strings.TrimSpace(result.queue) == "" {
				return syncTaskOptions{}, errors.New("queue name must contain one or more characters")
			}
		case asynq.TaskIDOpt:
			result.taskID, _ = opt.Value().(string)
			if strings.TrimSpace(result.taskID) == "" {
				return syncTaskOptions{}, errors.New("task ID cannot be empty")
			}
		case asynq.TimeoutOpt:
			result.timeout, _ = opt.Value().(time.Duration)
		case asynq.DeadlineOpt:
			result.deadline, _ = opt.Value().(time.Time)
		case asynq.UniqueOpt:
			result.uniqueTTL, _ = opt.Value().(time.Duration)
			if result.uniqueTTL < time.Second {
				return syncTaskOptions{}, errors.New("Unique TTL cannot be less than 1s")
			}
		case asynq.ProcessAtOpt:
			result.processAt, _ = opt.Value().(time.Time)
		case asynq.ProcessInOpt:
			delay, _ := opt.Value().(time.Duration)
			result.processAt = now.Add(delay)
		case asynq.RetentionOpt:
			result.retention, _ = opt.Value().(time.Duration)
		case asynq.GroupOpt:
			result.group, _ = opt.Value().(string)
			if strings.TrimSpace(result.group) == "" {
				return syncTaskOptions{}, errors.New("group key cannot be empty")
			}
		}
	}
	return result, nil
}

func (e *SyncTaskExecutor) executeTask(
	taskCtx context.Context,
	cancelTask context.CancelCauseFunc,
	task *asynq.Task,
	handler func(context.Context, *asynq.Task) error,
	options syncTaskOptions,
	uniqueKey string,
) {
	outcome := syncTaskCancelled
	var workers sync.WaitGroup
	defer func() {
		// An attempt is not terminal merely because its context expired. Wait for
		// the handler to return before publishing an outcome or releasing Unique.
		cancelTask(context.Canceled)
		workers.Wait()
		e.finishTaskOutcome(options, uniqueKey, outcome)
		e.removeLiveTask(options.taskID)
	}()

	delay := options.processAt.Sub(e.clockNow())
	if delay > 0 && !waitForSyncTask(taskCtx, delay) {
		outcome = syncTaskCancellationOutcome(taskCtx)
		return
	}
	start := e.clockNow()
	logger.Infof(taskCtx, "[SyncTask] Executing task type=%s id=%s queue=%s", task.Type(), options.taskID, options.queue)

	var lastErr error
	for attempt := 0; attempt <= options.maxRetry; attempt++ {
		if taskCtx.Err() != nil {
			outcome = syncTaskCancellationOutcome(taskCtx)
			return
		}
		if !options.deadline.IsZero() && !e.clockNow().Before(options.deadline) {
			lastErr = context.DeadlineExceeded
			break
		}
		if attempt > 0 {
			backoff := e.retryDelay(attempt)
			logger.Infof(taskCtx, "[SyncTask] Retrying task type=%s id=%s attempt=%d/%d backoff=%s",
				task.Type(), options.taskID, attempt, options.maxRetry, backoff)
			if !waitForSyncTask(taskCtx, backoff) {
				outcome = syncTaskCancellationOutcome(taskCtx)
				return
			}
			if !options.deadline.IsZero() && !e.clockNow().Before(options.deadline) {
				lastErr = context.DeadlineExceeded
				break
			}
		}

		e.beginTaskAttempt(options.taskID, attempt)
		attemptCtx, cancelAttempt := context.WithDeadline(
			taskretry.WithMetadata(taskCtx, attempt, options.maxRetry),
			syncAttemptDeadline(e.clockNow(), options),
		)
		lastErr = e.performAttempt(attemptCtx, options.taskID, task, handler, &workers)
		cancelAttempt()
		if lastErr == nil {
			outcome = syncTaskSucceeded
			logger.Infof(taskCtx, "[SyncTask] Task completed type=%s id=%s elapsed=%v",
				task.Type(), options.taskID, e.clockNow().Sub(start))
			return
		}
		if taskCtx.Err() != nil {
			outcome = syncTaskCancellationOutcome(taskCtx)
			return
		}
		e.markTaskAttemptError(options.taskID, lastErr)
		if errors.Is(lastErr, asynq.RevokeTask) {
			outcome = syncTaskRevoked
			return
		}
		if errors.Is(lastErr, asynq.SkipRetry) {
			break
		}
	}

	outcome = syncTaskExhausted
	logger.Errorf(taskCtx, "[SyncTask] Task failed (exhausted retries) type=%s id=%s elapsed=%v err=%v",
		task.Type(), options.taskID, e.clockNow().Sub(start), lastErr)
	if task.Type() != types.TypeKnowledgeTerminalRepair && terminalrepair.RepairableTaskType(task.Type()) {
		if err := terminalrepair.Enqueue(e, task, lastErr); err != nil {
			logger.Errorf(taskCtx,
				"[SyncTask] Failed to persist terminal repair for exhausted task type=%s id=%s: %v",
				task.Type(), options.taskID, err)
		}
	}
}

func (e *SyncTaskExecutor) performAttempt(
	ctx context.Context,
	taskID string,
	task *asynq.Task,
	handler func(context.Context, *asynq.Task) error,
	workers *sync.WaitGroup,
) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	result := make(chan error, 1)
	workers.Add(1)
	e.changeTaskWorkers(taskID, 1)
	go func() {
		var handlerErr error
		defer func() {
			if recovered := recover(); recovered != nil {
				handlerErr = fmt.Errorf("sync task handler panic: %v", recovered)
			}
			e.changeTaskWorkers(taskID, -1)
			workers.Done()
			result <- handlerErr
		}()
		handlerErr = handler(ctx, task)
	}()
	select {
	case err := <-result:
		if terminalErr := ctx.Err(); terminalErr != nil {
			e.markTaskAttemptError(taskID, terminalErr)
			return terminalErr
		}
		return err
	case <-ctx.Done():
		terminalErr := ctx.Err()
		e.markTaskAttemptError(taskID, terminalErr)
		// Do not overlap attempts. A handler which ignores cancellation keeps
		// this task live, active, and Unique-owned until it actually exits.
		<-result
		return terminalErr
	}
}

func syncTaskCancellationOutcome(ctx context.Context) syncTaskOutcome {
	if errors.Is(context.Cause(ctx), asynq.RevokeTask) {
		return syncTaskRevoked
	}
	return syncTaskCancelled
}

func syncAttemptDeadline(now time.Time, options syncTaskOptions) time.Time {
	deadline := options.deadline
	if options.timeout != 0 {
		timeoutDeadline := now.Add(options.timeout)
		if deadline.IsZero() || timeoutDeadline.Before(deadline) {
			deadline = timeoutDeadline
		}
	}
	if deadline.IsZero() {
		deadline = now.Add(syncDefaultTimeout)
	}
	return deadline
}

func syncUniqueKey(queue, taskType string, payload []byte, ttl time.Duration) string {
	if ttl <= 0 {
		return ""
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(queue))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write([]byte(taskType))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(payload)
	return hex.EncodeToString(digest.Sum(nil))
}

func (e *SyncTaskExecutor) clockNow() time.Time {
	if e != nil && e.now != nil {
		return e.now()
	}
	return time.Now()
}

func (e *SyncTaskExecutor) ensureLifecycleMapsLocked() {
	if e.tasks == nil {
		e.tasks = make(map[string]*syncTaskRecord)
	}
	if e.retained == nil {
		e.retained = make(map[string]time.Time)
	}
	if e.archived == nil {
		e.archived = make(map[string]time.Time)
	}
	if e.unique == nil {
		e.unique = make(map[string]syncUniqueLease)
	}
}

func (e *SyncTaskExecutor) pruneLifecycleRecordsLocked(now time.Time) {
	for id, expiresAt := range e.retained {
		if !recordLiveAt(expiresAt, now) {
			delete(e.retained, id)
		}
	}
	for id, expiresAt := range e.archived {
		if !recordLiveAt(expiresAt, now) {
			delete(e.archived, id)
		}
	}
	for key, lease := range e.unique {
		_, ownerLive := e.tasks[lease.taskID]
		if !lease.expiresAt.After(now) && !ownerLive {
			delete(e.unique, key)
		}
	}
}

func recordLiveAt(expiresAt, now time.Time) bool {
	return !expiresAt.IsZero() && expiresAt.After(now)
}

func (e *SyncTaskExecutor) finishTaskOutcome(options syncTaskOptions, uniqueKey string, outcome syncTaskOutcome) {
	now := e.clockNow()
	e.mu.Lock()
	defer e.mu.Unlock()
	e.ensureLifecycleMapsLocked()
	switch outcome {
	case syncTaskSucceeded:
		if options.retention > 0 {
			e.retained[options.taskID] = now.Add(options.retention)
		}
	case syncTaskExhausted:
		e.archived[options.taskID] = now.Add(syncArchiveRetention)
	}
	if uniqueKey == "" {
		return
	}
	lease, ok := e.unique[uniqueKey]
	if !ok || lease.taskID != options.taskID {
		return
	}
	// Failed/SkipRetry tasks retain the original enqueue-time lease for only
	// its remaining TTL. Success, revoke, and local cancellation release it.
	if outcome != syncTaskExhausted {
		delete(e.unique, uniqueKey)
	}
}

func (e *SyncTaskExecutor) removeLiveTask(taskID string) {
	e.mu.Lock()
	delete(e.tasks, taskID)
	e.mu.Unlock()
}

func waitForSyncTask(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (e *SyncTaskExecutor) changeTaskWorkers(taskID string, delta int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if task := e.tasks[taskID]; task != nil {
		task.workers += delta
		if task.workers < 0 {
			task.workers = 0
		}
		task.active = task.workers > 0
	}
}

func (e *SyncTaskExecutor) beginTaskAttempt(taskID string, attempt int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if task := e.tasks[taskID]; task != nil {
		task.attempt = attempt
		task.lastError = ""
		task.timedOut = false
	}
}

func (e *SyncTaskExecutor) markTaskAttemptError(taskID string, err error) {
	if err == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if task := e.tasks[taskID]; task != nil {
		task.lastError = err.Error()
		task.timedOut = errors.Is(err, context.DeadlineExceeded)
	}
}

func cancelSyncTaskRecord(task *syncTaskRecord, cause error) {
	if task == nil {
		return
	}
	if task.cancelWithCause != nil {
		task.cancelWithCause(cause)
		return
	}
	if task.cancel != nil {
		task.cancel()
	}
}

// Revoke cancels a Lite task without declaring it quiescent. If its handler
// ignores cancellation the task remains live and continues to own its Unique
// lease until that handler returns.
func (e *SyncTaskExecutor) Revoke(taskID string) error {
	e.mu.RLock()
	task := e.tasks[taskID]
	e.mu.RUnlock()
	if task == nil {
		return ErrSyncTaskNotFound
	}
	cancelSyncTaskRecord(task, asynq.RevokeTask)
	return nil
}

// Shutdown prevents new work, cancels all queued/running tasks, and waits for
// handler quiescence. A context deadline never pretends a non-cooperative
// handler stopped: the task remains visible and is cleaned up when it exits.
func (e *SyncTaskExecutor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		return errors.New("sync task executor shutdown context is nil")
	}
	e.mu.Lock()
	e.shutdown = true
	tasks := make([]*syncTaskRecord, 0, len(e.tasks))
	for _, task := range e.tasks {
		if task != nil {
			tasks = append(tasks, task)
		}
	}
	e.mu.Unlock()
	for _, task := range tasks {
		cancelSyncTaskRecord(task, ErrSyncTaskExecutorShutdown)
	}

	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		e.mu.RLock()
		remaining := len(e.tasks)
		e.mu.RUnlock()
		if remaining == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// liveTaskSnapshots returns a stable copy of every pending, delayed, retrying,
// or active Lite task. A task remains visible after cancellation until its
// handler actually returns, so a deletion barrier cannot mistake a worker that
// ignores context cancellation for quiescence.
func (e *SyncTaskExecutor) liveTaskSnapshots() []syncTaskSnapshot {
	if e == nil {
		return nil
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	tasks := make([]syncTaskSnapshot, 0, len(e.tasks))
	for _, task := range e.tasks {
		if task == nil {
			continue
		}
		tasks = append(tasks, syncTaskSnapshot{
			taskType: task.taskType,
			payload:  append([]byte(nil), task.payload...),
		})
	}
	return tasks
}

func (e *SyncTaskExecutor) cancelTasksForKnowledge(knowledgeID string) (deleted, cancelled int) {
	if e == nil || knowledgeID == "" {
		return 0, 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, task := range e.tasks {
		if task == nil || !matchesKnowledge(task.taskType, task.payload, knowledgeID) {
			continue
		}
		if task.active {
			cancelled++
		} else {
			deleted++
		}
		cancelSyncTaskRecord(task, context.Canceled)
	}
	return deleted, cancelled
}

func (e *SyncTaskExecutor) cancelWikiTasksForKnowledgeBase(
	knowledgeBaseID string,
) (deleted, cancelled int, retErr error) {
	if e == nil || knowledgeBaseID == "" {
		return 0, 0, fmt.Errorf("sync task executor: Wiki KB cancellation requires an identity")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, task := range e.tasks {
		if task == nil {
			continue
		}
		kbID, tracked, err := wikiTaskKnowledgeBaseID(task.taskType, task.payload)
		if err != nil {
			return deleted, cancelled, err
		}
		if !tracked || kbID != knowledgeBaseID {
			continue
		}
		if task.active {
			cancelled++
		} else {
			deleted++
		}
		cancelSyncTaskRecord(task, context.Canceled)
	}
	return deleted, cancelled, nil
}

func (e *SyncTaskExecutor) hasWikiTasksForKnowledgeBase(knowledgeBaseID string) (bool, error) {
	if e == nil || knowledgeBaseID == "" {
		return false, fmt.Errorf("sync task executor: Wiki KB liveness requires an identity")
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	for _, task := range e.tasks {
		if task == nil {
			continue
		}
		kbID, tracked, err := wikiTaskKnowledgeBaseID(task.taskType, task.payload)
		if err != nil {
			return false, err
		}
		if tracked && kbID == knowledgeBaseID {
			return true, nil
		}
	}
	return false, nil
}

type SyncTaskParams struct {
	dig.In

	Executor             *SyncTaskExecutor
	KnowledgeService     interfaces.KnowledgeService
	KnowledgeBaseService interfaces.KnowledgeBaseService
	TagService           interfaces.KnowledgeTagService
	DataSourceService    interfaces.DataSourceService
	ChunkExtractor       interfaces.TaskHandler `name:"chunkExtractor"`
	DataTableSummary     interfaces.TaskHandler `name:"dataTableSummary"`
	ImageMultimodal      interfaces.TaskHandler `name:"imageMultimodal"`
	KnowledgePostProcess interfaces.TaskHandler `name:"knowledgePostProcess"`
	WikiIngest           interfaces.TaskHandler `name:"wikiIngest"`
}

// RegisterSyncHandlers registers all task handlers on the SyncTaskExecutor.
// Used in Lite mode instead of RunAsynqServer.
func RegisterSyncHandlers(params SyncTaskParams) {
	repairer := terminalrepair.New(params.KnowledgeService.GetRepository(), params.Executor, nil)
	repairer.SetKnowledgeMoveRepairer(params.KnowledgeService)
	background := modeladmission.BackgroundTaskHandler
	params.Executor.RegisterHandler(types.TypeChunkExtract, background(params.ChunkExtractor.Handle))
	params.Executor.RegisterHandler(types.TypeDataTableSummary, background(params.DataTableSummary.Handle))
	params.Executor.RegisterHandler(types.TypeDocumentProcess, background(params.KnowledgeService.ProcessDocument))
	params.Executor.RegisterHandler(documentsplit.TypePartProcess, background(params.KnowledgeService.ProcessDocumentSplitPart))
	params.Executor.RegisterHandler(documentsplit.TypeFinalize, background(params.KnowledgeService.ProcessDocumentSplitFinalize))
	params.Executor.RegisterHandler(types.TypeManualProcess, background(params.KnowledgeService.ProcessManualUpdate))
	params.Executor.RegisterHandler(types.TypeFAQImport, background(params.KnowledgeService.ProcessFAQImport))
	params.Executor.RegisterHandler(types.TypeQuestionGeneration, background(params.KnowledgeService.ProcessQuestionGeneration))
	params.Executor.RegisterHandler(types.TypeSummaryGeneration, background(params.KnowledgeService.ProcessSummaryGeneration))
	params.Executor.RegisterHandler(types.TypeKBClone, background(params.KnowledgeService.ProcessKBClone))
	params.Executor.RegisterHandler(types.TypeKnowledgeMove, background(params.KnowledgeService.ProcessKnowledgeMove))
	params.Executor.RegisterHandler(types.TypeKnowledgeListDelete, background(params.KnowledgeService.ProcessKnowledgeListDelete))
	params.Executor.RegisterHandler(types.TypeKnowledgeListReparse, background(params.KnowledgeService.ProcessKnowledgeListReparse))
	params.Executor.RegisterHandler(types.TypeIndexDelete, background(params.TagService.ProcessIndexDelete))
	params.Executor.RegisterHandler(types.TypeKBDelete, background(params.KnowledgeBaseService.ProcessKBDelete))
	params.Executor.RegisterHandler(types.TypeImageMultimodal, background(params.ImageMultimodal.Handle))
	params.Executor.RegisterHandler(types.TypeKnowledgePostProcess, background(params.KnowledgePostProcess.Handle))
	params.Executor.RegisterHandler(types.TypeDataSourceSync, background(params.DataSourceService.ProcessSync))
	params.Executor.RegisterHandler(types.TypeWikiIngest, background(params.WikiIngest.Handle))
	params.Executor.RegisterHandler(types.TypeKnowledgeTerminalRepair, background(repairer.Handle))
	logger.Infof(context.Background(), "[SyncTask] All task handlers registered (Lite mode, no Redis)")
}
