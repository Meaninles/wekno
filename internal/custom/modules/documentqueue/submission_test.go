package documentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

func preparedWorkflowForTest(t *testing.T, c *Coordinator, tenant uint64, knowledgeID string, opts ...asynq.Option) *Workflow {
	t.Helper()
	workflow, _, err := c.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, tenant, knowledgeID, "generation-1"), opts,
	)
	require.NoError(t, err)
	require.Equal(t, StatePreparing, workflow.State)
	require.NotEmpty(t, workflow.PlanHash)
	return workflow
}

func TestPreparingIsInvisibleAndCannotDispatchOrClaim(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-invisible", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 31, "knowledge-preparing")
	bindWorkflowForTest(t, c, workflow)

	status, err := c.QueueStatus(context.Background(), 31, []string{workflow.KnowledgeID})
	require.NoError(t, err)
	require.Zero(t, status.WaitingTotal)
	require.Equal(t, "none", status.Items[workflow.KnowledgeID].State)
	require.Zero(t, status.Items[workflow.KnowledgeID].Position)
	_, err = c.Dispatch(context.Background(), workflow)
	require.ErrorIs(t, err, ErrStaleDelivery)
	_, err = c.Claim(context.Background(), workflow.TaskType, deliveryPayload(t, workflow))
	require.ErrorIs(t, err, ErrStaleDelivery)
}

func TestPrepareRejectsSameGenerationDifferentPayloadOrOptions(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-plan", "boot-1", 1)
	payload := workflowPayload(t, 32, "knowledge-plan", "generation-1")
	first, created, err := c.PrepareWorkflowWithOptions(context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.Timeout(time.Hour), asynq.TaskID("producer-a")})
	require.NoError(t, err)
	require.True(t, created)

	again, created, err := c.PrepareWorkflowWithOptions(context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.Timeout(time.Hour), asynq.TaskID("producer-a")})
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, again.ID)

	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["language"] = "zh-CN"
	differentPayload, err := json.Marshal(fields)
	require.NoError(t, err)
	_, _, err = c.PrepareWorkflowWithOptions(context.Background(), types.TypeDocumentProcess, differentPayload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.Timeout(time.Hour), asynq.TaskID("producer-a")})
	require.ErrorIs(t, err, ErrPlanConflict)

	_, _, err = c.PrepareWorkflowWithOptions(context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{asynq.MaxRetry(4), asynq.Timeout(time.Hour), asynq.TaskID("producer-a")})
	require.ErrorIs(t, err, ErrPlanConflict)
}

func TestPrepareConvergesWhenOnlyTracingContextChanges(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-observability", "boot-1", 1)
	payload := workflowPayload(t, 320, "knowledge-observability", "generation-1")
	var fields map[string]interface{}
	require.NoError(t, json.Unmarshal(payload, &fields))
	fields["request_id"] = "request-first"
	fields["lf_trace_id"] = "trace-first"
	fields["lf_parent_obs_id"] = "span-first"
	firstPayload, err := json.Marshal(fields)
	require.NoError(t, err)

	first, created, err := c.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, firstPayload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.TaskID("producer-observability")},
	)
	require.NoError(t, err)
	require.True(t, created)

	fields["request_id"] = "request-retry"
	fields["lf_trace_id"] = "trace-retry"
	fields["lf_parent_obs_id"] = "span-retry"
	fields["lf_user_id"] = "user-retry"
	fields["lf_session_id"] = "session-retry"
	retryPayload, err := json.Marshal(fields)
	require.NoError(t, err)
	again, created, err := c.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, retryPayload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.TaskID("producer-observability")},
	)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, first.ID, again.ID)
	require.JSONEq(t, string(firstPayload), string(again.Payload), "first accepted immutable payload must be retained")
}

func TestWrongKnowledgeBindingCannotActivate(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-wrong-binding", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 33, "knowledge-binding")
	bindWorkflowForTest(t, c, workflow)
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)

	wrong := binding
	wrong.ProcessingOwner = "another-owner"
	_, _, err = c.ActivatePreparedWorkflow(context.Background(), wrong)
	require.ErrorIs(t, err, ErrWorkflowNotBound)
	wrong = binding
	wrong.KnowledgeBaseID = "another-kb"
	_, _, err = c.ActivatePreparedWorkflow(context.Background(), wrong)
	require.ErrorIs(t, err, ErrWorkflowNotBound)

	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StatePreparing, persisted.State)
}

func TestConcurrentActivationHasOneCASWinner(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-activate-a", "boot-a", 1)
	workflow := preparedWorkflowForTest(t, c, 34, "knowledge-activation")
	bindWorkflowForTest(t, c, workflow)
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)

	other := NewCoordinatorWithConfig(c.db, nil, "prepare-activate-b", "boot-b", 1, c.config)
	const workers = 16
	var activated atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			coordinator := c
			if index%2 == 1 {
				coordinator = other
			}
			_, won, activateErr := coordinator.ActivatePreparedWorkflow(context.Background(), binding)
			require.NoError(t, activateErr)
			if won {
				activated.Add(1)
			}
		}(i)
	}
	wg.Wait()
	require.EqualValues(t, 1, activated.Load())
	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StateQueued, persisted.State)
}

func TestRecoverPreparingActivatesOnlyExactCommittedBinding(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-recover", "boot-1", 1)
	unbound := preparedWorkflowForTest(t, c, 35, "knowledge-unbound")
	bound := preparedWorkflowForTest(t, c, 35, "knowledge-bound")
	bindWorkflowForTest(t, c, bound)
	c.config.RecoveryBatchSize = 1

	require.NoError(t, c.recoverPreparing(context.Background()))
	var recovered, untouched Workflow
	require.NoError(t, c.db.Where("id = ?", bound.ID).Take(&recovered).Error)
	require.NoError(t, c.db.Where("id = ?", unbound.ID).Take(&untouched).Error)
	require.Equal(t, StateQueued, recovered.State)
	require.Equal(t, StatePreparing, untouched.State)
}

func TestClaimRevalidatesKnowledgeWorkflowBinding(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-claim-fence", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 36, "knowledge-claim-fence")
	bindWorkflowForTest(t, c, workflow)
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)
	workflow, _, err = c.ActivatePreparedWorkflow(context.Background(), binding)
	require.NoError(t, err)

	require.NoError(t, c.db.Model(&queueTestKnowledge{}).Where("id = ?", workflow.KnowledgeID).
		Update("processing_workflow_id", "wrong-workflow").Error)
	_, err = c.Claim(context.Background(), workflow.TaskType, deliveryPayload(t, workflow))
	require.True(t, errors.Is(err, ErrStaleDelivery), err)
}

func TestResumeActivatesPersistedPlanWithoutReconstruction(t *testing.T) {
	c := newQueueTestCoordinator(t, "prepare-resume", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 37, "knowledge-resume",
		asynq.MaxRetry(9), asynq.Timeout(17*time.Minute), asynq.TaskID("original-producer-id"))
	bindWorkflowForTest(t, c, workflow)
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)

	enqueuer := &Enqueuer{coordinator: c}
	info, err := enqueuer.ResumeDocumentWorkflow(context.Background(), binding)
	require.NoError(t, err)
	require.Equal(t, workflowTaskID(workflow.ID, workflow.DispatchEpoch), info.ID)

	loaded, err := enqueuer.LoadDocumentWorkflow(context.Background(), binding)
	require.NoError(t, err)
	require.Equal(t, StateQueued, loaded.State)
	require.Equal(t, 9, loaded.MaxRetry)
	require.Equal(t, int64(17*time.Minute), loaded.DelegateTimeoutNanos)
	require.Equal(t, workflow.PlanHash, loaded.PlanHash)
}

func TestResumeQueuedAcceptsEveryClaimableRecoveryBoundary(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		owner       string
		processedAt bool
	}{
		{name: "core in progress by planned owner", status: types.ParseStatusProcessing, owner: "owner-generation-1"},
		{name: "core committed awaiting fanout", status: types.ParseStatusProcessing, processedAt: true},
		{name: "finalizing fanout replay", status: types.ParseStatusFinalizing, processedAt: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newQueueTestCoordinator(t, "queued-resume", "boot-1", 1)
			workflow, _, err := c.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess,
				workflowPayload(t, 371, "knowledge-queued-resume", "generation-1"),
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, c, workflow)
			updates := map[string]interface{}{
				"parse_status": test.status, "processing_owner": test.owner,
			}
			if test.processedAt {
				updates["processed_at"] = time.Now()
			}
			require.NoError(t, c.db.Model(&queueTestKnowledge{}).
				Where("id = ?", workflow.KnowledgeID).Updates(updates).Error)

			enqueuer := &Enqueuer{coordinator: c}
			info, err := enqueuer.ResumeDocumentWorkflow(
				context.Background(), WorkflowBinding{
					WorkflowID: workflow.ID, TenantID: workflow.TenantID,
					KnowledgeBaseID: workflow.KnowledgeBaseID, KnowledgeID: workflow.KnowledgeID,
					ProcessingGeneration: workflow.ProcessingGeneration,
					ProcessingOwner:      "owner-generation-1",
				},
			)
			require.NoError(t, err)
			require.Equal(t, workflowTaskID(workflow.ID, workflow.DispatchEpoch), info.ID)
		})
	}
}

func TestResumeQueuedRejectsUncommittedOwnerlessProcessing(t *testing.T) {
	c := newQueueTestCoordinator(t, "queued-resume-invalid", "boot-1", 1)
	workflow, _, err := c.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, 373, "knowledge-queued-resume-invalid", "generation-1"),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, c, workflow)
	require.NoError(t, c.db.Model(&queueTestKnowledge{}).Where("id = ?", workflow.KnowledgeID).
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusProcessing, "processing_owner": "", "processed_at": nil,
		}).Error)
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)

	_, err = (&Enqueuer{coordinator: c}).ResumeDocumentWorkflow(context.Background(), binding)
	require.ErrorIs(t, err, ErrWorkflowNotBound)
}

func TestResumeLeasedAndTerminalStatesRequireExactGenerationWorkflowBinding(t *testing.T) {
	tests := []struct {
		name          string
		workflowState WorkflowState
		parseStatus   string
	}{
		{name: "leased after core commit", workflowState: StateLeased, parseStatus: types.ParseStatusProcessing},
		{name: "completed", workflowState: StateCompleted, parseStatus: types.ParseStatusCompleted},
		{name: "failed", workflowState: StateFailed, parseStatus: types.ParseStatusFailed},
		{name: "cancelled", workflowState: StateCancelled, parseStatus: types.ParseStatusCancelled},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			c := newQueueTestCoordinator(t, "strict-resume", "boot-1", 1)
			workflow, _, err := c.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess,
				workflowPayload(t, 372, "knowledge-strict-resume", "generation-1"),
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, c, workflow)
			require.NoError(t, c.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
				Updates(map[string]interface{}{"state": test.workflowState, "stage": string(test.workflowState)}).Error)
			require.NoError(t, c.db.Model(&queueTestKnowledge{}).Where("id = ?", workflow.KnowledgeID).
				Updates(map[string]interface{}{
					"parse_status": test.parseStatus, "processing_owner": "", "processed_at": time.Now(),
				}).Error)
			binding, err := BindingForWorkflow(workflow)
			require.NoError(t, err)
			enqueuer := &Enqueuer{coordinator: c}

			info, err := enqueuer.ResumeDocumentWorkflow(context.Background(), binding)
			require.NoError(t, err)
			require.Equal(t, workflowTaskID(workflow.ID, workflow.DispatchEpoch), info.ID)

			require.NoError(t, c.db.Model(&queueTestKnowledge{}).Where("id = ?", workflow.KnowledgeID).
				Update("processing_workflow_id", "another-workflow").Error)
			_, err = enqueuer.ResumeDocumentWorkflow(context.Background(), binding)
			require.ErrorIs(t, err, ErrWorkflowNotBound)

			require.NoError(t, c.db.Model(&queueTestKnowledge{}).Where("id = ?", workflow.KnowledgeID).
				Updates(map[string]interface{}{
					"processing_workflow_id": workflow.ID, "processing_generation": "another-generation",
				}).Error)
			_, err = enqueuer.ResumeDocumentWorkflow(context.Background(), binding)
			require.ErrorIs(t, err, ErrWorkflowNotBound)
		})
	}
}

func insertProcessingReparseForTest(t *testing.T, c *Coordinator, workflow *Workflow) WorkflowBinding {
	t.Helper()
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)
	require.NoError(t, c.db.Create(&queueTestKnowledge{
		ID: binding.KnowledgeID, TenantID: binding.TenantID,
		KnowledgeBaseID:      binding.KnowledgeBaseID,
		ProcessingGeneration: binding.ProcessingGeneration,
		ProcessingOwner:      binding.ProcessingOwner,
		ParseStatus:          types.ParseStatusProcessing,
		EnableStatus:         "enabled",
		Description:          "old description",
		EmbeddingModelID:     "old-model",
		PendingSubtasksCount: 9,
		UpdatedAt:            time.Now().Add(-time.Minute),
	}).Error)
	return binding
}

func TestCommitPreparedReparseAtomicallyBindsPendingGeneration(t *testing.T) {
	c := newQueueTestCoordinator(t, "reparse-atomic", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 41, "knowledge-reparse-atomic")
	binding := insertProcessingReparseForTest(t, c, workflow)
	now := time.Now().UTC().Truncate(time.Microsecond)

	require.NoError(t, c.CommitPreparedReparse(context.Background(), binding, ReparsePendingTransition{
		EmbeddingModelID: "embedding-new",
		ErrorMessage:     "batch-reparse-ready:generation-1:3",
		UpdatedAt:        now,
	}))

	var knowledge queueTestKnowledge
	require.NoError(t, c.db.Where("id = ?", binding.KnowledgeID).Take(&knowledge).Error)
	require.Equal(t, types.ParseStatusPending, knowledge.ParseStatus)
	require.Equal(t, binding.WorkflowID, knowledge.ProcessingWorkflowID)
	require.Equal(t, "disabled", knowledge.EnableStatus)
	require.Empty(t, knowledge.Description)
	require.Nil(t, knowledge.ProcessedAt)
	require.Equal(t, "embedding-new", knowledge.EmbeddingModelID)
	require.Zero(t, knowledge.PendingSubtasksCount)
	require.Equal(t, "batch-reparse-ready:generation-1:3", knowledge.ErrorMessage)

	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StatePreparing, persisted.State, "business acceptance must precede activation")
	_, activated, err := c.ActivatePreparedWorkflow(context.Background(), binding)
	require.NoError(t, err)
	require.True(t, activated)
}

func TestCommitPreparedReparseDatabaseFailureRollsBackBindingAndPending(t *testing.T) {
	c := newQueueTestCoordinator(t, "reparse-rollback", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 42, "knowledge-reparse-rollback")
	binding := insertProcessingReparseForTest(t, c, workflow)
	require.NoError(t, c.db.Exec(`
		CREATE TRIGGER fail_reparse_binding
		BEFORE UPDATE OF processing_workflow_id ON knowledges
		WHEN NEW.processing_workflow_id <> ''
		BEGIN SELECT RAISE(ABORT, 'injected reparse binding failure'); END;
	`).Error)

	err := c.CommitPreparedReparse(context.Background(), binding, ReparsePendingTransition{
		EmbeddingModelID: "embedding-new", UpdatedAt: time.Now(),
	})
	require.ErrorContains(t, err, "injected reparse binding failure")

	var knowledge queueTestKnowledge
	require.NoError(t, c.db.Where("id = ?", binding.KnowledgeID).Take(&knowledge).Error)
	require.Equal(t, types.ParseStatusProcessing, knowledge.ParseStatus)
	require.Empty(t, knowledge.ProcessingWorkflowID)
	require.Equal(t, "enabled", knowledge.EnableStatus)
	require.Equal(t, "old description", knowledge.Description)
	require.Equal(t, "old-model", knowledge.EmbeddingModelID)
	require.Equal(t, 9, knowledge.PendingSubtasksCount)

	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StatePreparing, persisted.State)
}

func TestStableReparsePreparationCanCommitAfterTransientBusinessFailure(t *testing.T) {
	c := newQueueTestCoordinator(t, "reparse-retry", "boot-1", 1)
	payload := workflowPayload(t, 43, "knowledge-reparse-retry", "generation-1")
	workflow, created, err := c.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.TaskID("stable-reparse-task")},
	)
	require.NoError(t, err)
	require.True(t, created)
	binding := insertProcessingReparseForTest(t, c, workflow)
	require.NoError(t, c.db.Exec(`
		CREATE TRIGGER fail_stable_reparse_once
		BEFORE UPDATE OF processing_workflow_id ON knowledges
		WHEN NEW.processing_workflow_id <> ''
		BEGIN SELECT RAISE(ABORT, 'transient reparse failure'); END;
	`).Error)
	require.Error(t, c.CommitPreparedReparse(context.Background(), binding, ReparsePendingTransition{
		EmbeddingModelID: "embedding-new", UpdatedAt: time.Now(),
	}))
	require.NoError(t, c.db.Exec("DROP TRIGGER fail_stable_reparse_once").Error)

	retry, created, err := c.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{asynq.MaxRetry(3), asynq.TaskID("stable-reparse-task")},
	)
	require.NoError(t, err)
	require.False(t, created)
	require.Equal(t, workflow.ID, retry.ID)
	require.Equal(t, StatePreparing, retry.State)
	require.NoError(t, c.CommitPreparedReparse(context.Background(), binding, ReparsePendingTransition{
		EmbeddingModelID: "embedding-new", UpdatedAt: time.Now(),
	}))
}

func TestCommitPreparedReparseAndAbortCannotSplitBusinessState(t *testing.T) {
	c := newQueueTestCoordinator(t, "reparse-abort-race", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 44, "knowledge-reparse-abort-race")
	binding := insertProcessingReparseForTest(t, c, workflow)
	start := make(chan struct{})
	errs := make(chan error, 2)
	go func() {
		<-start
		errs <- c.CommitPreparedReparse(context.Background(), binding, ReparsePendingTransition{
			EmbeddingModelID: "embedding-new", UpdatedAt: time.Now(),
		})
	}()
	go func() {
		<-start
		errs <- c.AbortPreparedWorkflow(context.Background(), binding, "concurrent producer abort")
	}()
	close(start)
	first, second := <-errs, <-errs
	require.NotEqual(t, first == nil, second == nil, "exactly one of commit or abort must win")

	var knowledge queueTestKnowledge
	require.NoError(t, c.db.Where("id = ?", binding.KnowledgeID).Take(&knowledge).Error)
	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	if knowledge.ParseStatus == types.ParseStatusPending {
		require.Equal(t, binding.WorkflowID, knowledge.ProcessingWorkflowID)
		require.Equal(t, StatePreparing, persisted.State)
		return
	}
	require.Equal(t, types.ParseStatusProcessing, knowledge.ParseStatus)
	require.Empty(t, knowledge.ProcessingWorkflowID)
	require.Equal(t, StateCancelled, persisted.State)
}

func TestPreparedWorkflowTransitionWinningRaceCannotBeCancelledByAbort(t *testing.T) {
	c := newQueueTestCoordinator(t, "move-bind-abort-race", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 45, "knowledge-move-bind-abort-race")
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)
	require.NoError(t, c.db.Create(&queueTestKnowledge{
		ID: binding.KnowledgeID, TenantID: binding.TenantID,
		KnowledgeBaseID: binding.KnowledgeBaseID, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: binding.ProcessingGeneration,
		ProcessingOwner:      binding.ProcessingOwner,
		UpdatedAt:            time.Now(),
	}).Error)

	transitionStarted := make(chan struct{})
	releaseTransition := make(chan struct{})
	bindResult := make(chan error, 1)
	go func() {
		bindResult <- c.db.Transaction(func(tx *gorm.DB) error {
			return c.BindPreparedWorkflowTransitionTx(tx, binding, func(tx *gorm.DB) error {
				close(transitionStarted)
				<-releaseTransition
				return tx.Model(&queueTestKnowledge{}).
					Where("id = ?", binding.KnowledgeID).
					Update("processing_workflow_id", binding.WorkflowID).Error
			})
		})
	}()
	<-transitionStarted
	abortResult := make(chan error, 1)
	go func() {
		abortResult <- c.AbortPreparedWorkflow(context.Background(), binding, "concurrent abort")
	}()

	select {
	case abortErr := <-abortResult:
		t.Fatalf("abort completed before the binding transaction released its workflow lock: %v", abortErr)
	case <-time.After(25 * time.Millisecond):
	}
	close(releaseTransition)
	require.NoError(t, <-bindResult)
	require.ErrorContains(t, <-abortResult, "bound prepared workflow cannot be aborted")

	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StatePreparing, persisted.State)
	var knowledge queueTestKnowledge
	require.NoError(t, c.db.Where("id = ?", binding.KnowledgeID).Take(&knowledge).Error)
	require.Equal(t, binding.WorkflowID, knowledge.ProcessingWorkflowID)
}

func TestFailedPreparedWorkflowTransitionRollsBackAndCanBeSafelyAborted(t *testing.T) {
	c := newQueueTestCoordinator(t, "move-bind-failure-abort", "boot-1", 1)
	workflow := preparedWorkflowForTest(t, c, 46, "knowledge-move-bind-failure-abort")
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)
	require.NoError(t, c.db.Create(&queueTestKnowledge{
		ID: binding.KnowledgeID, TenantID: binding.TenantID,
		KnowledgeBaseID: binding.KnowledgeBaseID, ParseStatus: types.ParseStatusPending,
		ProcessingGeneration: binding.ProcessingGeneration,
		ProcessingOwner:      binding.ProcessingOwner,
		UpdatedAt:            time.Now(),
	}).Error)
	injected := errors.New("injected business rollback")
	err = c.db.Transaction(func(tx *gorm.DB) error {
		return c.BindPreparedWorkflowTransitionTx(tx, binding, func(tx *gorm.DB) error {
			if err := tx.Model(&queueTestKnowledge{}).
				Where("id = ?", binding.KnowledgeID).
				Update("processing_workflow_id", binding.WorkflowID).Error; err != nil {
				return err
			}
			return injected
		})
	})
	require.ErrorIs(t, err, injected)

	var knowledge queueTestKnowledge
	require.NoError(t, c.db.Where("id = ?", binding.KnowledgeID).Take(&knowledge).Error)
	require.Empty(t, knowledge.ProcessingWorkflowID)
	require.NoError(t, c.AbortPreparedWorkflow(context.Background(), binding, "business transaction rolled back"))
	var persisted Workflow
	require.NoError(t, c.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StateCancelled, persisted.State)
	requireWorkflowTerminalDiagnostic(
		t,
		persisted,
		types.SpanStatusCancelled,
		"DOCUMENT_WORKFLOW_CANCELLED",
		"business transaction rolled back",
	)
}
