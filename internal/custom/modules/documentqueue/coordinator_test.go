package documentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/processingtrace"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/types"
)

type queueTestKnowledge struct {
	ID                   string `gorm:"primaryKey"`
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	ProcessingOwner      string
	ProcessingWorkflowID string
	ProcessingFanout     types.JSON `gorm:"type:json"`
	ParseStatus          string
	EnableStatus         string
	Description          string
	ProcessedAt          *time.Time
	EmbeddingModelID     string
	ErrorMessage         string
	PendingSubtasksCount int
	SummaryStatus        string
	EnrichmentStatus     string
	WikiStatus           string
	WikiErrorMessage     string
	UpdatedAt            time.Time
	DeletedAt            gorm.DeletedAt
}

func (queueTestKnowledge) TableName() string { return "knowledges" }

func requireWorkflowTerminalDiagnostic(
	t *testing.T,
	workflow Workflow,
	status string,
	errorCode string,
	errorMessage string,
) {
	t.Helper()
	require.NotEmpty(t, workflow.TerminalDiagnostic)
	var diagnostic map[string]string
	require.NoError(t, json.Unmarshal(workflow.TerminalDiagnostic, &diagnostic))
	require.Equal(t, "workflow", diagnostic["source"])
	require.Equal(t, status, diagnostic["status"])
	require.Equal(t, errorCode, diagnostic["error_code"])
	require.Equal(t, errorMessage, diagnostic["error_message"])
}

func newQueueTestCoordinator(t *testing.T, instanceID, bootID string, capacity int) *Coordinator {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared&_busy_timeout=5000", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// SQLite is only the deterministic state-machine harness. PostgreSQL
	// concurrency is covered by the integration suite; one connection avoids
	// SQLite's database-wide writer lock obscuring CAS assertions.
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.AutoMigrate(&queueTestKnowledge{}, &types.TaskPendingOp{}))
	cfg := DefaultConfig()
	cfg.HeartbeatInterval = time.Hour
	cfg.RecoveryInterval = time.Hour
	cfg.WorkflowPollInterval = 5 * time.Millisecond
	cfg.LeaseDuration = 3 * time.Hour
	cfg.InstanceStaleAfter = 2 * time.Hour
	cfg.TrustStableInstanceRestart = true
	coordinator := NewCoordinatorWithConfig(db, nil, instanceID, bootID, capacity, cfg)
	require.NoError(t, coordinator.Migrate(context.Background()))
	require.NoError(t, coordinator.registerAndAdopt(context.Background()))
	require.NoError(t, coordinator.MarkReady(context.Background()))
	return coordinator
}

func workflowPayload(t *testing.T, tenantID uint64, knowledgeID, generation string) []byte {
	return workflowPayloadForKB(t, tenantID, "kb-1", knowledgeID, generation)
}

func workflowPayloadForKB(
	t *testing.T,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	generation string,
) []byte {
	t.Helper()
	payload, err := json.Marshal(types.DocumentProcessPayload{
		TenantID: tenantID, KnowledgeID: knowledgeID, KnowledgeBaseID: knowledgeBaseID,
		ProcessingGeneration: generation, ProcessingOwner: "owner-" + generation,
	})
	require.NoError(t, err)
	return payload
}

func TestClaimFairnessLetsLateKnowledgeBaseRunBeforeOlderBacklog(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-fairness", "boot-1", 4)
	type seed struct {
		tenantID        uint64
		knowledgeBaseID string
		knowledgeID     string
		generation      string
	}
	create := func(item seed, enqueuedAt time.Time) *Workflow {
		t.Helper()
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(),
			types.TypeDocumentProcess,
			workflowPayloadForKB(
				t, item.tenantID, item.knowledgeBaseID, item.knowledgeID, item.generation,
			),
		)
		require.NoError(t, err)
		bindWorkflowForTest(t, coordinator, workflow)
		require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
			Update("enqueued_at", enqueuedAt).Error)
		require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(workflow).Error)
		return workflow
	}

	base := time.Now().Add(-time.Hour)
	firstA := create(seed{41, "kb-a", "a-1", "a-generation-1"}, base)
	secondA := create(seed{41, "kb-a", "a-2", "a-generation-2"}, base.Add(time.Second))
	lateB := create(seed{42, "kb-b", "b-1", "b-generation-1"}, base.Add(time.Minute))

	leaseA, err := coordinator.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryPayload(t, firstA),
	)
	require.NoError(t, err)
	require.Equal(t, firstA.ID, leaseA.WorkflowID)

	// The UI position uses the same fair ordering as recovery: the late KB is
	// next because KB A already owns one active document slot.
	status, err := coordinator.QueueStatus(
		context.Background(), secondA.TenantID, []string{secondA.KnowledgeID},
	)
	require.NoError(t, err)
	require.EqualValues(t, 2, status.WaitingTotal)
	require.EqualValues(t, 2, status.Items[secondA.KnowledgeID].Position)

	oldEpoch := secondA.DispatchEpoch
	_, err = coordinator.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryPayload(t, secondA),
	)
	require.ErrorIs(t, err, ErrFairnessDeferred)
	var deferred Workflow
	require.NoError(t, coordinator.db.Where("id = ?", secondA.ID).Take(&deferred).Error)
	require.Equal(t, StateQueued, deferred.State)
	require.EqualValues(t, oldEpoch+1, deferred.DispatchEpoch)
	require.Empty(t, deferred.DispatchTaskID)
	require.Nil(t, deferred.LastDispatchedAt)

	leaseB, err := coordinator.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryPayload(t, lateB),
	)
	require.NoError(t, err)
	require.Equal(t, lateB.ID, leaseB.WorkflowID)

	var groups []ScheduleGroup
	require.NoError(t, coordinator.db.Order("tenant_id ASC").Find(&groups).Error)
	require.Len(t, groups, 2)
	require.Equal(t, firstA.TenantID, groups[0].TenantID)
	require.Equal(t, lateB.TenantID, groups[1].TenantID)
}

func TestQueuedDispatchPublishesOnlyOneHeadAndDoesNotSkipItsLiveOutbox(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "bounded-dispatch", "boot-1", 4)
	create := func(tenantID uint64, knowledgeBaseID, knowledgeID string, enqueuedAt time.Time) *Workflow {
		t.Helper()
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(),
			types.TypeDocumentProcess,
			workflowPayloadForKB(
				t, tenantID, knowledgeBaseID, knowledgeID, knowledgeID+"-generation",
			),
		)
		require.NoError(t, err)
		bindWorkflowForTest(t, coordinator, workflow)
		require.NoError(t, coordinator.db.Model(&Workflow{}).
			Where("id = ?", workflow.ID).
			Update("enqueued_at", enqueuedAt).Error)
		require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(workflow).Error)
		return workflow
	}

	base := time.Now().Add(-time.Hour)
	head := create(501, "kb-a", "bounded-head", base)
	_ = create(501, "kb-a", "bounded-second", base.Add(time.Second))
	_ = create(502, "kb-b", "bounded-other-group", base.Add(2*time.Second))

	queued, err := coordinator.listQueuedForDispatch(
		context.Background(), time.Now().Add(-30*time.Second),
	)
	require.NoError(t, err)
	require.Len(t, queued, 1, "only the current fair head may enter the Redis delivery lane")
	require.Equal(t, head.ID, queued[0].ID)

	now := time.Now()
	require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", head.ID).
		Updates(map[string]interface{}{
			"dispatch_task_id":   workflowTaskID(head.ID, head.DispatchEpoch),
			"last_dispatched_at": now,
		}).Error)

	queued, err = coordinator.listQueuedForDispatch(
		context.Background(), now.Add(-30*time.Second),
	)
	require.NoError(t, err)
	require.Empty(t, queued,
		"a live outbox for the fair head must block later rows instead of preloading them out of order")

	stale := now.Add(-time.Minute)
	require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", head.ID).
		Update("last_dispatched_at", stale).Error)
	queued, err = coordinator.listQueuedForDispatch(
		context.Background(), now.Add(-30*time.Second),
	)
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, head.ID, queued[0].ID)
}

func deliveryPayload(t *testing.T, workflow *Workflow) []byte {
	t.Helper()
	payload, err := addDeliveryIdentity(workflow.Payload, workflow.ID, workflow.DispatchEpoch)
	require.NoError(t, err)
	return payload
}

func bindWorkflowForTest(t *testing.T, coordinator *Coordinator, workflow *Workflow) {
	t.Helper()
	binding, err := BindingForWorkflow(workflow)
	require.NoError(t, err)
	var count int64
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", binding.KnowledgeID).Count(&count).Error)
	if count == 0 {
		require.NoError(t, coordinator.db.Create(&queueTestKnowledge{
			ID: binding.KnowledgeID, TenantID: binding.TenantID,
			KnowledgeBaseID:      binding.KnowledgeBaseID,
			ProcessingGeneration: binding.ProcessingGeneration,
			ProcessingOwner:      binding.ProcessingOwner,
			ProcessingWorkflowID: binding.WorkflowID,
			ParseStatus:          types.ParseStatusPending, UpdatedAt: time.Now(),
		}).Error)
		return
	}
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", binding.KnowledgeID).Updates(map[string]interface{}{
		"tenant_id": binding.TenantID, "knowledge_base_id": binding.KnowledgeBaseID,
		"processing_generation":  binding.ProcessingGeneration,
		"processing_owner":       binding.ProcessingOwner,
		"processing_workflow_id": binding.WorkflowID,
		"parse_status":           types.ParseStatusPending,
	}).Error)
}

func cancellationBindingForTest(workflow *Workflow) CancellationBinding {
	return CancellationBinding{
		WorkflowID:           workflow.ID,
		TenantID:             workflow.TenantID,
		KnowledgeBaseID:      workflow.KnowledgeBaseID,
		KnowledgeID:          workflow.KnowledgeID,
		ProcessingGeneration: workflow.ProcessingGeneration,
	}
}

func TestConcurrentRegisterAndClaimHasExactlyOneWinner(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "pod-a", "boot-a", 8)
	payload := workflowPayload(t, 7, "knowledge-1", "generation-1")

	const goroutines = 32
	ids := make(chan string, goroutines)
	errs := make(chan error, goroutines)
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			workflow, _, err := coordinator.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
			if err != nil {
				errs <- err
				return
			}
			ids <- workflow.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	firstID := ""
	for id := range ids {
		if firstID == "" {
			firstID = id
		}
		require.Equal(t, firstID, id)
	}
	var count int64
	require.NoError(t, coordinator.db.Model(&Workflow{}).Count(&count).Error)
	require.EqualValues(t, 1, count)

	var workflow Workflow
	require.NoError(t, coordinator.db.First(&workflow).Error)
	bindWorkflowForTest(t, coordinator, &workflow)
	delivery := deliveryPayload(t, &workflow)
	var winners atomic.Int32
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := coordinator.Claim(context.Background(), types.TypeDocumentProcess, delivery)
			if err == nil {
				winners.Add(1)
				return
			}
			require.True(t, errors.Is(err, ErrAlreadyLeased) || errors.Is(err, ErrStaleDelivery), err)
		}()
	}
	wg.Wait()
	require.EqualValues(t, 1, winners.Load())
}

func TestConcurrentRecordDispatchHasOnePublisherWithoutInflatingAttempts(t *testing.T) {
	first := newQueueTestCoordinator(t, "dispatch-racer-a", "boot-a", 1)
	workflow, _, err := first.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, 70, "knowledge-dispatch-race", "generation-1"),
	)
	require.NoError(t, err)
	second := NewCoordinatorWithConfig(
		first.db,
		nil,
		"dispatch-racer-b",
		"boot-b",
		1,
		first.config,
	)
	taskID := workflowTaskID(workflow.ID, workflow.DispatchEpoch)

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wait sync.WaitGroup
	snapshots := []Workflow{*workflow, *workflow}
	for index, coordinator := range []*Coordinator{first, second} {
		wait.Add(1)
		go func(candidate *Coordinator, snapshot *Workflow) {
			defer wait.Done()
			<-start
			errs <- candidate.recordDispatch(context.Background(), snapshot, taskID)
		}(coordinator, &snapshots[index])
	}
	close(start)
	wait.Wait()
	close(errs)

	var published, fenced int
	for dispatchErr := range errs {
		switch {
		case dispatchErr == nil:
			published++
		case errors.Is(dispatchErr, ErrStaleDelivery):
			fenced++
		default:
			require.NoError(t, dispatchErr)
		}
	}
	require.Equal(t, 1, published)
	require.Equal(t, 1, fenced)

	var current Workflow
	require.NoError(t, first.db.Where("id = ?", workflow.ID).Take(&current).Error)
	require.Equal(t, taskID, current.DispatchTaskID)
	require.Equal(t, 1, current.DispatchAttempts)
	require.NotNil(t, current.LastDispatchedAt)
	firstDispatchAt := *current.LastDispatchedAt

	// A later recovery liveness probe for the same stable TaskID advances its
	// outbox timestamp, but it is not a second logical delivery attempt.
	time.Sleep(time.Millisecond)
	require.NoError(t, first.recordDispatch(context.Background(), &current, taskID))
	require.NoError(t, first.db.Where("id = ?", workflow.ID).Take(&current).Error)
	require.Equal(t, 1, current.DispatchAttempts)
	require.True(t, current.LastDispatchedAt.After(firstDispatchAt))
}

func TestClaimAcceptsDurableResumeBoundariesAfterOwnerTermination(t *testing.T) {
	tests := []struct {
		name        string
		status      string
		owner       string
		processedAt bool
		claimable   bool
	}{
		{name: "pending original owner", status: types.ParseStatusPending, owner: "owner-generation-1", claimable: true},
		{name: "core in progress original owner", status: types.ParseStatusProcessing, owner: "owner-generation-1", claimable: true},
		{name: "core committed", status: types.ParseStatusProcessing, processedAt: true, claimable: true},
		{name: "derivatives in progress", status: types.ParseStatusFinalizing, processedAt: true, claimable: true},
		{name: "core and derivatives committed while wiki is pending", status: types.ParseStatusCompleted, processedAt: true, claimable: true},
		{name: "empty owner before core commit", status: types.ParseStatusProcessing, claimable: false},
		{name: "different owner", status: types.ParseStatusProcessing, owner: "another-owner", claimable: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newQueueTestCoordinator(t, "resume-parser", "resume-boot", 1)
			payload := workflowPayload(t, 701, "knowledge-resume", "generation-1")
			workflow, _, err := coordinator.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess, payload,
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, coordinator, workflow)
			updates := map[string]interface{}{
				"parse_status": test.status, "processing_owner": test.owner,
			}
			if test.processedAt {
				updates["processed_at"] = time.Now()
			}
			require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
				Where("id = ?", workflow.KnowledgeID).Updates(updates).Error)

			_, err = coordinator.Claim(
				context.Background(), workflow.TaskType, deliveryPayload(t, workflow),
			)
			if test.claimable {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, ErrStaleDelivery)
			}
		})
	}
}

func TestStableInstanceRestartAdoptsAndFencesPreviousBoot(t *testing.T) {
	old := newQueueTestCoordinator(t, "parser-0", "boot-old", 2)
	require.NoError(t, old.Start(context.Background()))
	payload := workflowPayload(t, 9, "knowledge-restart", "generation-1")
	workflow, _, err := old.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, old, workflow)
	oldDelivery := deliveryPayload(t, workflow)
	oldLease, err := old.Claim(context.Background(), types.TypeDocumentProcess, oldDelivery)
	require.NoError(t, err)

	restarted := NewCoordinatorWithConfig(old.db, nil, "parser-0", "boot-new", 2, old.config)
	require.NoError(t, restarted.Start(context.Background()))
	t.Cleanup(restarted.Stop)

	var adopted Workflow
	require.NoError(t, old.db.Where("id = ?", workflow.ID).Take(&adopted).Error)
	require.Equal(t, StateQueued, adopted.State)
	require.EqualValues(t, oldLease.Epoch+1, adopted.DispatchEpoch)
	require.Empty(t, adopted.OwnerBootID)
	require.ErrorIs(t, old.renew(context.Background(), oldLease, "core", time.Now()), ErrLeaseLost)
	_, err = restarted.Claim(context.Background(), types.TypeDocumentProcess, oldDelivery)
	require.ErrorIs(t, err, ErrStaleDelivery)
	_, err = restarted.Claim(context.Background(), types.TypeDocumentProcess, deliveryPayload(t, &adopted))
	require.NoError(t, err)

	// A delayed old-process cleanup must not mark the new boot stopped.
	old.Stop()
	var instance Instance
	require.NoError(t, old.db.Where("instance_id = ?", "parser-0").Take(&instance).Error)
	require.Equal(t, "boot-new", instance.BootID)
	require.Equal(t, InstanceReady, instance.State)
}

func TestCancellationCommitAndStableRestartCannotReviveWorkflow(t *testing.T) {
	for _, adoptBeforeCancel := range []bool{false, true} {
		name := "cancel-before-restart"
		if adoptBeforeCancel {
			name = "restart-before-cancel"
		}
		t.Run(name, func(t *testing.T) {
			old := newQueueTestCoordinator(t, "parser-cancel", "boot-old", 1)
			workflow, _, err := old.RegisterWorkflow(
				context.Background(),
				types.TypeDocumentProcess,
				workflowPayload(t, 10, "knowledge-cancel", "generation-cancel"),
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, old, workflow)
			lease, err := old.Claim(
				context.Background(),
				types.TypeDocumentProcess,
				deliveryPayload(t, workflow),
			)
			require.NoError(t, err)
			require.NoError(t, old.db.Model(&queueTestKnowledge{}).
				Where("id = ?", workflow.KnowledgeID).
				Updates(map[string]interface{}{
					"parse_status":           types.ParseStatusCancelling,
					"pending_subtasks_count": 7,
					"summary_status":         types.SummaryStatusProcessing,
					"enrichment_status":      types.EnrichmentStatusPending,
					"wiki_status":            types.WikiStatusPending,
					"wiki_error_message":     "old error",
				}).Error)

			restarted := NewCoordinatorWithConfig(
				old.db, nil, "parser-cancel", "boot-new", 1, old.config,
			)
			if adoptBeforeCancel {
				require.NoError(t, restarted.registerAndAdopt(context.Background()))
				require.NoError(t, restarted.MarkReady(context.Background()))
				var adopted Workflow
				require.NoError(t, old.db.Where("id = ?", workflow.ID).Take(&adopted).Error)
				require.Equal(t, StateQueued, adopted.State)
			}

			now := time.Now()
			require.NoError(t, old.CommitWorkflowCancellation(
				context.Background(), cancellationBindingForTest(workflow), now,
			))

			if !adoptBeforeCancel {
				require.NoError(t, restarted.registerAndAdopt(context.Background()))
				require.NoError(t, restarted.MarkReady(context.Background()))
			}

			var persisted Workflow
			require.NoError(t, old.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
			require.Equal(t, StateCancelled, persisted.State)
			require.Equal(t, "cancelled", persisted.Stage)
			require.Empty(t, persisted.OwnerInstanceID)
			require.Empty(t, persisted.OwnerBootID)
			require.Nil(t, persisted.LeaseUntil)
			require.Empty(t, persisted.DispatchTaskID)
			require.NotNil(t, persisted.CompletedAt)
			requireWorkflowTerminalDiagnostic(
				t,
				persisted,
				types.SpanStatusCancelled,
				"USER_CANCELLED",
				"cancelled by user",
			)
			expectedEpoch := lease.Epoch + 1
			if adoptBeforeCancel {
				expectedEpoch++
			}
			require.EqualValues(t, expectedEpoch, persisted.DispatchEpoch)

			var knowledge queueTestKnowledge
			require.NoError(t, old.db.Where("id = ?", workflow.KnowledgeID).Take(&knowledge).Error)
			require.Equal(t, types.ParseStatusCancelled, knowledge.ParseStatus)
			require.Zero(t, knowledge.PendingSubtasksCount)
			require.Equal(t, types.SummaryStatusNone, knowledge.SummaryStatus)
			require.Equal(t, types.EnrichmentStatusNone, knowledge.EnrichmentStatus)
			require.Equal(t, types.WikiStatusNone, knowledge.WikiStatus)
			require.Empty(t, knowledge.WikiErrorMessage)
			require.Empty(t, knowledge.ProcessingOwner)

			_, err = restarted.Claim(
				context.Background(),
				types.TypeDocumentProcess,
				deliveryPayload(t, &persisted),
			)
			require.ErrorIs(t, err, ErrStaleDelivery)
		})
	}
}

func TestQueuedRecoveryIncludesAndTerminalizesSupersededGeneration(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-stale-queued", "boot-1", 1)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, 12, "knowledge-stale-queued", "generation-old"),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)

	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", workflow.KnowledgeID).
		Updates(map[string]interface{}{
			"processing_generation":  "generation-new",
			"processing_workflow_id": "workflow-new",
			"processing_owner":       "owner-generation-new",
			"parse_status":           types.ParseStatusPending,
		}).Error)

	queued, err := coordinator.listQueuedForDispatch(context.Background(), time.Now())
	require.NoError(t, err)
	require.Len(t, queued, 1)
	require.Equal(t, workflow.ID, queued[0].ID)

	terminal, _, err := coordinator.reconcileQueuedTerminal(context.Background(), &queued[0])
	require.NoError(t, err)
	require.True(t, terminal)
	var persisted Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&persisted).Error)
	require.Equal(t, StateSuperseded, persisted.State)
	require.Equal(t, "superseded", persisted.Stage)
	requireWorkflowTerminalDiagnostic(
		t,
		persisted,
		types.SpanStatusCancelled,
		"DOCUMENT_WORKFLOW_CANCELLED",
		"",
	)
}

func TestCrossInstanceTakeoverRequiresTerminationProofBeyondStaleHeartbeat(t *testing.T) {
	owner := newQueueTestCoordinator(t, "parser-a", "boot-a", 1)
	require.NoError(t, owner.registerAndAdopt(context.Background()))
	require.NoError(t, owner.MarkReady(context.Background()))
	payload := workflowPayload(t, 11, "knowledge-failover", "generation-1")
	workflow, _, err := owner.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, owner, workflow)
	lease, err := owner.Claim(context.Background(), types.TypeDocumentProcess, deliveryPayload(t, workflow))
	require.NoError(t, err)

	other := NewCoordinatorWithConfig(owner.db, nil, "parser-b", "boot-b", 1, owner.config)
	now := time.Now()
	// Lease expired but heartbeat remains fresh: no transfer.
	require.NoError(t, owner.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
		Update("lease_until", now.Add(-time.Second)).Error)
	stale, err := other.instanceIsStale(context.Background(), "parser-a", "boot-a", now)
	require.NoError(t, err)
	require.False(t, stale)

	// Heartbeat stale as well is still not termination proof: a paused or
	// partitioned process could resume and write with the same generation.
	require.NoError(t, owner.db.Model(&Instance{}).Where("instance_id = ?", "parser-a").
		Update("last_heartbeat_at", now.Add(-3*owner.config.InstanceStaleAfter)).Error)
	stale, err = other.instanceIsStale(context.Background(), "parser-a", "boot-a", now)
	require.NoError(t, err)
	require.True(t, stale)
	var expired Workflow
	require.NoError(t, owner.db.Where("id = ?", workflow.ID).Take(&expired).Error)
	reclaimed, err := other.requeueExpired(context.Background(), &expired, now)
	require.NoError(t, err)
	require.Nil(t, reclaimed)

	// An external orchestrator (or graceful Stop) proves the exact boot is
	// gone. Only now may one CAS requeue and advance the epoch.
	require.NoError(t, other.ConfirmInstanceTermination(
		context.Background(), "parser-a", "boot-a", "test-runtime-confirmed-stopped",
	))
	reclaimed, err = other.requeueExpired(context.Background(), &expired, now)
	require.NoError(t, err)
	require.NotNil(t, reclaimed)
	require.EqualValues(t, lease.Epoch+1, reclaimed.DispatchEpoch)
	require.Equal(t, StateQueued, reclaimed.State)
	// A concurrent reconciler sees the CAS already won and cannot advance twice.
	again, err := other.requeueExpired(context.Background(), &expired, now)
	require.NoError(t, err)
	require.Nil(t, again)
}

func TestFreshDuplicateStableInstanceBootIsRejectedWithoutRuntimeTrust(t *testing.T) {
	owner := newQueueTestCoordinator(t, "stable-parser", "boot-owner", 1)
	cfg := owner.config
	cfg.TrustStableInstanceRestart = false
	duplicate := NewCoordinatorWithConfig(owner.db, nil, "stable-parser", "boot-duplicate", 1, cfg)
	err := duplicate.registerAndAdopt(context.Background())
	require.ErrorIs(t, err, ErrInstanceIdentityConflict)

	var instance Instance
	require.NoError(t, owner.db.Where("instance_id = ?", "stable-parser").Take(&instance).Error)
	require.Equal(t, "boot-owner", instance.BootID)
	require.Equal(t, InstanceReady, instance.State)
}

func TestTerminationAttestationRejectsFreshOrWrongBoot(t *testing.T) {
	owner := newQueueTestCoordinator(t, "attested-parser", "boot-current", 1)
	err := owner.ConfirmInstanceTermination(
		context.Background(), "attested-parser", "boot-current", "runtime-proof",
	)
	require.ErrorIs(t, err, ErrTerminationNotProven)

	now := time.Now()
	require.NoError(t, owner.db.Model(&Instance{}).Where("instance_id = ?", "attested-parser").
		Update("last_heartbeat_at", now.Add(-3*owner.config.InstanceStaleAfter)).Error)
	err = owner.ConfirmInstanceTermination(
		context.Background(), "attested-parser", "wrong-boot", "runtime-proof",
	)
	require.ErrorIs(t, err, ErrStaleDelivery)
	require.NoError(t, owner.ConfirmInstanceTermination(
		context.Background(), "attested-parser", "boot-current", "runtime-proof",
	))
}

func TestTerminationAttestationRejectsOversizedProof(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-proof-limit", "boot-current", 1)
	err := coordinator.ConfirmInstanceTermination(
		context.Background(),
		"parser-proof-limit",
		"boot-current",
		strings.Repeat("x", maxTerminationProofBytes+1),
	)
	require.ErrorIs(t, err, ErrTerminationNotProven)
}

func TestProcessReleasesDocumentSlotWhileWikiDurableWorkContinues(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-wiki", "boot-1", 1)
	now := time.Now()
	knowledge := queueTestKnowledge{
		ID: "knowledge-wiki", TenantID: 13, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusPending, UpdatedAt: now,
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	payload := workflowPayload(t, 13, knowledge.ID, knowledge.ProcessingGeneration)
	workflow, _, err := coordinator.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	task := asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow))
	dedupKey, err := wikiqueue.IngestDedupKey(knowledge.ID, knowledge.ProcessingGeneration)
	require.NoError(t, err)

	delegateCommitted := make(chan struct{})
	processDone := make(chan error, 1)
	go func() {
		processDone <- coordinator.Process(context.Background(), task, func(ctx context.Context, _ *asynq.Task) error {
			op := types.TaskPendingOp{
				TenantID: 13, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
				ScopeID: "kb-1", Op: "ingest", DedupKey: dedupKey, Payload: []byte(`{}`),
			}
			if createErr := coordinator.db.Create(&op).Error; createErr != nil {
				return createErr
			}
			if updateErr := coordinator.db.Model(&queueTestKnowledge{}).Where("id = ?", knowledge.ID).
				Updates(map[string]interface{}{
					"parse_status":      types.ParseStatusCompleted,
					"processing_owner":  "",
					"processed_at":      time.Now(),
					"enrichment_status": types.EnrichmentStatusCompleted,
					"wiki_status":       types.WikiStatusPending,
					"updated_at":        time.Now(),
				}).Error; updateErr != nil {
				return updateErr
			}
			close(delegateCommitted)
			return nil
		})
	}()
	<-delegateCommitted
	select {
	case processErr := <-processDone:
		require.NoError(t, processErr)
	case <-time.After(time.Second):
		t.Fatal("document workflow did not release its slot after durable Wiki hand-off")
	}

	var waiting Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&waiting).Error)
	require.Equal(t, StateWaitingExternal, waiting.State)
	require.Equal(t, "wiki", waiting.Stage)
	require.Empty(t, waiting.OwnerInstanceID)
	require.Empty(t, waiting.OwnerBootID)
	require.Nil(t, waiting.LeaseUntil)
	require.Nil(t, waiting.LastHeartbeatAt)
	require.Len(t, coordinator.slots, 0)

	var stillParsing queueTestKnowledge
	require.NoError(t, coordinator.db.Where("id = ?", knowledge.ID).Take(&stillParsing).Error)
	require.Equal(t, types.ParseStatusCompleted, stillParsing.ParseStatus)
	require.Equal(t, types.WikiStatusPending, stillParsing.WikiStatus,
		"the derivative state must remain visible after the document slot is released")

	status, err := coordinator.QueueStatus(
		context.Background(), knowledge.TenantID, []string{knowledge.ID},
	)
	require.NoError(t, err)
	require.Zero(t, status.ActiveTotal,
		"waiting derivatives must not consume document parser capacity")
	require.Equal(t, "active", status.Items[knowledge.ID].State,
		"the API must retain a non-terminal workflow item without presenting a queue position")
	require.Equal(t, "wiki", status.Items[knowledge.ID].Stage)

	require.NoError(t, coordinator.db.Where(
		"tenant_id = ? AND task_type = ? AND scope_id = ?",
		knowledge.TenantID, types.TypeWikiIngest, knowledge.KnowledgeBaseID,
	).Delete(&types.TaskPendingOp{}).Error)
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).Where("id = ?", knowledge.ID).
		Updates(map[string]interface{}{
			"wiki_status": types.WikiStatusCompleted,
			"updated_at":  time.Now(),
		}).Error)
	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var finished Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&finished).Error)
	require.Equal(t, StateCompleted, finished.State)
	require.Equal(t, "completed", finished.Stage)
	requireWorkflowTerminalDiagnostic(t, finished, types.SpanStatusDone, "", "")
	require.Len(t, coordinator.slots, 0)
}

func TestGraphDerivativesDoNotBlockTheNextCoreDocumentAtCapacityOne(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-graph-background", "boot-1", 1)
	type document struct {
		id         string
		generation string
	}
	documents := []document{
		{id: "knowledge-graph-background", generation: "generation-graph"},
		{id: "knowledge-next-core", generation: "generation-next"},
	}
	workflows := make([]*Workflow, 0, len(documents))
	for _, document := range documents {
		require.NoError(t, coordinator.db.Create(&queueTestKnowledge{
			ID: document.id, TenantID: 130, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: document.generation,
			ProcessingOwner:      "owner-" + document.generation,
			ParseStatus:          types.ParseStatusPending,
			UpdatedAt:            time.Now(),
		}).Error)
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(), types.TypeDocumentProcess,
			workflowPayload(t, 130, document.id, document.generation),
		)
		require.NoError(t, err)
		bindWorkflowForTest(t, coordinator, workflow)
		workflows = append(workflows, workflow)
	}

	require.NoError(t, coordinator.Process(
		context.Background(),
		asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflows[0])),
		func(context.Context, *asynq.Task) error {
			now := time.Now()
			return coordinator.db.Model(&queueTestKnowledge{}).
				Where("id = ?", documents[0].id).
				Updates(map[string]interface{}{
					"parse_status":           types.ParseStatusFinalizing,
					"processing_owner":       "",
					"processed_at":           now,
					"pending_subtasks_count": 1,
					"enrichment_status":      types.EnrichmentStatusPending,
					"wiki_status":            types.WikiStatusNone,
					"updated_at":             now,
				}).Error
		},
	))

	var graphWaiting Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflows[0].ID).Take(&graphWaiting).Error)
	require.Equal(t, StateWaitingExternal, graphWaiting.State)
	require.Equal(t, "derivatives", graphWaiting.Stage)
	require.Len(t, coordinator.slots, 0)

	var secondDelegateCalls atomic.Int32
	require.NoError(t, coordinator.Process(
		context.Background(),
		asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflows[1])),
		func(context.Context, *asynq.Task) error {
			secondDelegateCalls.Add(1)
			now := time.Now()
			return coordinator.db.Model(&queueTestKnowledge{}).
				Where("id = ?", documents[1].id).
				Updates(map[string]interface{}{
					"parse_status":      types.ParseStatusCompleted,
					"processing_owner":  "",
					"processed_at":      now,
					"enrichment_status": types.EnrichmentStatusCompleted,
					"wiki_status":       types.WikiStatusNone,
					"updated_at":        now,
				}).Error
		},
	))
	require.EqualValues(t, 1, secondDelegateCalls.Load(),
		"the next core parser must run while the first document's graph task is pending")

	var firstKnowledge queueTestKnowledge
	require.NoError(t, coordinator.db.Where("id = ?", documents[0].id).Take(&firstKnowledge).Error)
	require.Equal(t, types.ParseStatusFinalizing, firstKnowledge.ParseStatus)
	require.Equal(t, types.EnrichmentStatusPending, firstKnowledge.EnrichmentStatus)
	require.Equal(t, 1, firstKnowledge.PendingSubtasksCount)

	var secondFinished Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflows[1].ID).Take(&secondFinished).Error)
	require.Equal(t, StateCompleted, secondFinished.State)
	requireWorkflowTerminalDiagnostic(t, secondFinished, types.SpanStatusDone, "", "")

	now := time.Now()
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", documents[0].id).
		Updates(map[string]interface{}{
			"parse_status":           types.ParseStatusCompleted,
			"pending_subtasks_count": 0,
			"enrichment_status":      types.EnrichmentStatusCompleted,
			"updated_at":             now,
		}).Error)
	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var graphFinished Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflows[0].ID).Take(&graphFinished).Error)
	require.Equal(t, StateCompleted, graphFinished.State)
	requireWorkflowTerminalDiagnostic(t, graphFinished, types.SpanStatusDone, "", "")
}

func TestRecoveryClosesOnlyCurrentGenerationLatestAttemptOpenSpans(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-span-reconcile", "boot-1", 1)
	require.NoError(t, coordinator.db.AutoMigrate(&processingtrace.Span{}))

	knowledge := queueTestKnowledge{
		ID: "knowledge-span-reconcile", TenantID: 131, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-current", ProcessingOwner: "owner-generation-current",
		ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", knowledge.ID).
		Updates(map[string]interface{}{
			"parse_status":      types.ParseStatusCompleted,
			"processing_owner":  "",
			"processed_at":      time.Now(),
			"enrichment_status": types.EnrichmentStatusCompleted,
			"wiki_status":       types.WikiStatusCompleted,
			"updated_at":        time.Now(),
		}).Error)
	require.NoError(t, coordinator.db.Model(&Workflow{}).
		Where("id = ?", workflow.ID).
		Updates(map[string]interface{}{
			"state": StateCompleted, "stage": "completed",
			"completed_at": time.Now(), "updated_at": time.Now(),
		}).Error)

	started := time.Now().Add(-time.Minute)
	spans := []processingtrace.Span{
		{
			KnowledgeID: knowledge.ID, Attempt: 1, LogicalKey: "root", SpanID: "old-attempt-root",
			Name: "knowledge_processing", Kind: types.SpanKindRoot,
			Status: types.SpanStatusRunning, StartedAt: started,
		},
		{
			KnowledgeID: knowledge.ID, Attempt: 2, LogicalKey: "root", SpanID: "latest-root",
			Name: "knowledge_processing", Kind: types.SpanKindRoot,
			Status: types.SpanStatusRunning, StartedAt: started,
		},
		{
			KnowledgeID: knowledge.ID, Attempt: 2, LogicalKey: "wiki:postprocess.wiki", SpanID: "latest-wiki",
			ParentLogicalKey: "root", Name: "postprocess.wiki",
			Kind: types.SpanKindSubSpan, Status: types.SpanStatusRunning,
			StartedAt: started,
		},
		{
			KnowledgeID: knowledge.ID, Attempt: 2, LogicalKey: "wiki:postprocess.wiki.page[entity/acme]", SpanID: "latest-page",
			ParentLogicalKey: "wiki:postprocess.wiki", Name: "postprocess.wiki.page[entity/acme]",
			Kind: types.SpanKindSubSpan, Status: types.SpanStatusPending,
			StartedAt: started,
		},
		{
			KnowledgeID: knowledge.ID, Attempt: 2, LogicalKey: "subspan:historical.failure", SpanID: "historical-failure",
			ParentLogicalKey: "root", Name: "historical.failure",
			Kind: types.SpanKindSubSpan, Status: types.SpanStatusFailed,
			LastErrorCode: "WIKI_MATERIALIZATION_FAILED", StartedAt: started,
			FinishedAt: &started,
		},
	}
	require.NoError(t, coordinator.db.Create(&spans).Error)

	require.NoError(t, coordinator.reconcileTerminalSpanOrphans(context.Background()))
	require.NoError(t, coordinator.reconcileTerminalSpanOrphans(context.Background()),
		"repeated multi-replica recovery must be idempotent")

	var got []processingtrace.Span
	require.NoError(t, coordinator.db.
		Where("knowledge_id = ?", knowledge.ID).
		Order("attempt ASC, logical_key ASC").
		Find(&got).Error)
	require.Len(t, got, len(spans))
	byID := make(map[string]processingtrace.Span, len(got))
	for _, span := range got {
		byID[span.SpanID] = span
	}
	require.Equal(t, types.SpanStatusRunning, byID["old-attempt-root"].Status,
		"an older attempt is immutable history")
	require.Equal(t, types.SpanStatusDone, byID["latest-root"].Status)
	require.Equal(t, types.SpanStatusCancelled, byID["latest-wiki"].Status)
	require.Equal(t, "DOCUMENT_WORKFLOW_TERMINAL", byID["latest-wiki"].LastErrorCode)
	require.NotNil(t, byID["latest-wiki"].FinishedAt)
	require.Equal(t, types.SpanStatusCancelled, byID["latest-page"].Status)
	require.Equal(t, types.SpanStatusFailed, byID["historical-failure"].Status,
		"terminal history must not be rewritten")
}

func TestTerminalSpanRecoveryIgnoresSupersededWorkflowGeneration(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-span-generation", "boot-1", 1)
	require.NoError(t, coordinator.db.AutoMigrate(&processingtrace.Span{}))

	knowledge := queueTestKnowledge{
		ID: "knowledge-span-generation", TenantID: 132, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-old", ProcessingOwner: "owner-generation-old",
		ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	require.NoError(t, coordinator.db.Model(&Workflow{}).
		Where("id = ?", workflow.ID).
		Updates(map[string]interface{}{
			"state": StateCompleted, "stage": "completed",
			"completed_at": time.Now(), "updated_at": time.Now(),
		}).Error)
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", knowledge.ID).
		Updates(map[string]interface{}{
			"processing_generation": "generation-new",
			"processing_owner":      "owner-generation-new",
			"updated_at":            time.Now(),
		}).Error)

	started := time.Now().Add(-time.Minute)
	require.NoError(t, coordinator.db.Create(&processingtrace.Span{
		KnowledgeID: knowledge.ID, Attempt: 2, LogicalKey: "root", SpanID: "new-generation-root",
		Name: "knowledge_processing", Kind: types.SpanKindRoot,
		Status: types.SpanStatusRunning, StartedAt: started,
	}).Error)

	require.NoError(t, coordinator.reconcileTerminalSpanOrphans(context.Background()))
	var root processingtrace.Span
	require.NoError(t, coordinator.db.
		Where("span_id = ?", "new-generation-root").
		Take(&root).Error)
	require.Equal(t, types.SpanStatusRunning, root.Status,
		"an old terminal workflow must never close a newer generation's attempt")
}

func TestRecoverWaitingExternalPreservesDurableWikiStateUntilTerminal(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-waiting-wiki", "boot-1", 1)
	knowledge := queueTestKnowledge{
		ID: "knowledge-waiting-wiki", TenantID: 14, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	dedupKey, err := wikiqueue.IngestDedupKey(knowledge.ID, knowledge.ProcessingGeneration)
	require.NoError(t, err)
	require.NoError(t, coordinator.db.Create(&types.TaskPendingOp{
		TenantID: knowledge.TenantID, TaskType: types.TypeWikiIngest,
		Scope: types.TaskScopeKnowledgeBase, ScopeID: knowledge.KnowledgeBaseID,
		Op: "ingest", DedupKey: dedupKey, Payload: []byte(`{}`),
	}).Error)
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).Where("id = ?", knowledge.ID).
		Updates(map[string]interface{}{
			"parse_status":      types.ParseStatusCompleted,
			"processing_owner":  "",
			"processed_at":      time.Now(),
			"enrichment_status": types.EnrichmentStatusCompleted,
			"wiki_status":       types.WikiStatusPending,
			"updated_at":        time.Now(),
		}).Error)
	require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
		Updates(map[string]interface{}{
			"state": StateWaitingExternal, "stage": "wiki",
			"owner_instance_id": "", "owner_boot_id": "",
			"lease_until": nil, "last_heartbeat_at": nil,
		}).Error)
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(workflow).Error)
	oldEpoch := workflow.DispatchEpoch

	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var waiting Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&waiting).Error)
	require.Equal(t, StateWaitingExternal, waiting.State)
	require.Equal(t, "wiki", waiting.Stage)
	require.EqualValues(t, oldEpoch, waiting.DispatchEpoch,
		"derivative observation must not create a new root delivery epoch")
	require.NotNil(t, waiting.LastProgressAt)
	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var afterSecondRecovery Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&afterSecondRecovery).Error)
	require.Equal(t, StateWaitingExternal, afterSecondRecovery.State)
	require.EqualValues(t, oldEpoch, afterSecondRecovery.DispatchEpoch)
	require.Greater(t, afterSecondRecovery.Version, waiting.Version,
		"each healthy observation remains durably recorded without losing task identity")

	require.NoError(t, coordinator.db.Where(
		"tenant_id = ? AND task_type = ? AND scope_id = ?",
		knowledge.TenantID, types.TypeWikiIngest, knowledge.KnowledgeBaseID,
	).Delete(&types.TaskPendingOp{}).Error)
	require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).Where("id = ?", knowledge.ID).
		Updates(map[string]interface{}{
			"wiki_status": types.WikiStatusCompleted,
			"updated_at":  time.Now(),
		}).Error)
	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var finished Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&finished).Error)
	require.Equal(t, StateCompleted, finished.State)
	require.Equal(t, "completed", finished.Stage)
	requireWorkflowTerminalDiagnostic(t, finished, types.SpanStatusDone, "", "")
}

func TestRecoverWaitingExternalRequeuesOnlyWhenCommittedCoreFenceIsMissing(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-unsafe-wait", "boot-1", 1)
	knowledge := queueTestKnowledge{
		ID: "knowledge-unsafe-wait", TenantID: 15, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusProcessing, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
		Updates(map[string]interface{}{
			"state": StateWaitingExternal, "stage": "core",
			"owner_instance_id": "", "owner_boot_id": "",
			"lease_until": nil, "last_heartbeat_at": nil,
		}).Error)
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(workflow).Error)
	oldEpoch := workflow.DispatchEpoch

	require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
	var resumed Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&resumed).Error)
	require.Equal(t, StateQueued, resumed.State)
	require.EqualValues(t, oldEpoch+1, resumed.DispatchEpoch)
	require.Empty(t, resumed.DispatchTaskID)
	require.Contains(t, resumed.LastError, "committed-core fence")
}

func TestRecoverWaitingExternalFinalizesDerivativeFailureAndDeletion(t *testing.T) {
	tests := []struct {
		name             string
		parseStatus      string
		enrichmentStatus string
		wikiStatus       string
		wantState        WorkflowState
		wantStage        string
	}{
		{
			name:             "graph derivative failed",
			parseStatus:      types.ParseStatusCompleted,
			enrichmentStatus: types.EnrichmentStatusFailed,
			wikiStatus:       types.WikiStatusNone,
			wantState:        StateFailed,
			wantStage:        "enrichment_failed",
		},
		{
			name:             "wiki derivative failed",
			parseStatus:      types.ParseStatusCompleted,
			enrichmentStatus: types.EnrichmentStatusCompleted,
			wikiStatus:       types.WikiStatusFailed,
			wantState:        StateFailed,
			wantStage:        "wiki_failed",
		},
		{
			name:             "graph derivative degraded",
			parseStatus:      types.ParseStatusCompleted,
			enrichmentStatus: types.EnrichmentStatusDegraded,
			wikiStatus:       types.WikiStatusNone,
			wantState:        StateCompleted,
			wantStage:        "completed_degraded_enrichment",
		},
		{
			name:             "document deleted during derivatives",
			parseStatus:      types.ParseStatusDeleting,
			enrichmentStatus: types.EnrichmentStatusPending,
			wikiStatus:       types.WikiStatusPending,
			wantState:        StateCancelled,
			wantStage:        types.ParseStatusDeleting,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newQueueTestCoordinator(
				t, fmt.Sprintf("parser-wait-terminal-%d", index), "boot-1", 1,
			)
			now := time.Now()
			knowledge := queueTestKnowledge{
				ID:       fmt.Sprintf("knowledge-wait-terminal-%d", index),
				TenantID: uint64(160 + index), KnowledgeBaseID: "kb-1",
				ProcessingGeneration: "generation-1", ProcessingOwner: "",
				ProcessedAt: &now, ParseStatus: test.parseStatus,
				EnrichmentStatus: test.enrichmentStatus, WikiStatus: test.wikiStatus,
				UpdatedAt: now,
			}
			require.NoError(t, coordinator.db.Create(&knowledge).Error)
			workflow, _, err := coordinator.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess,
				workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, coordinator, workflow)
			require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).
				Where("id = ?", knowledge.ID).
				Updates(map[string]interface{}{
					"parse_status":      test.parseStatus,
					"processing_owner":  "",
					"processed_at":      now,
					"enrichment_status": test.enrichmentStatus,
					"wiki_status":       test.wikiStatus,
					"updated_at":        now,
				}).Error)
			require.NoError(t, coordinator.db.Model(&Workflow{}).
				Where("id = ?", workflow.ID).
				Updates(map[string]interface{}{
					"state": StateWaitingExternal, "stage": "derivatives",
					"owner_instance_id": "", "owner_boot_id": "",
					"lease_until": nil, "last_heartbeat_at": nil,
				}).Error)

			require.NoError(t, coordinator.recoverWaitingExternal(context.Background()))
			var finished Workflow
			require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&finished).Error)
			require.Equal(t, test.wantState, finished.State)
			require.Equal(t, test.wantStage, finished.Stage)
			require.NotNil(t, finished.CompletedAt)
			if test.wantState == StateFailed {
				message := "required document derivative finished with status " + test.wantStage
				requireWorkflowTerminalDiagnostic(
					t,
					finished,
					types.SpanStatusFailed,
					"DOCUMENT_WORKFLOW_FAILED",
					message,
				)
			} else if test.wantState == StateCompleted {
				requireWorkflowTerminalDiagnostic(
					t, finished, types.SpanStatusDone, "", "",
				)
			} else {
				requireWorkflowTerminalDiagnostic(
					t,
					finished,
					types.SpanStatusCancelled,
					"DOCUMENT_WORKFLOW_CANCELLED",
					"",
				)
			}
		})
	}
}

func TestProcessDistinguishesFailedAndDegradedDerivatives(t *testing.T) {
	tests := []struct {
		name             string
		enrichmentStatus string
		wikiStatus       string
		wantState        WorkflowState
		wantStage        string
	}{
		{
			name:             "enrichment failed",
			enrichmentStatus: types.EnrichmentStatusFailed,
			wikiStatus:       types.WikiStatusNone,
			wantState:        StateFailed,
			wantStage:        "enrichment_failed",
		},
		{
			name:             "enrichment degraded",
			enrichmentStatus: types.EnrichmentStatusDegraded,
			wikiStatus:       types.WikiStatusNone,
			wantState:        StateCompleted,
			wantStage:        "completed_degraded_enrichment",
		},
		{
			name:             "wiki failed",
			enrichmentStatus: types.EnrichmentStatusCompleted,
			wikiStatus:       types.WikiStatusFailed,
			wantState:        StateFailed,
			wantStage:        "wiki_failed",
		},
		{
			name:             "wiki degraded",
			enrichmentStatus: types.EnrichmentStatusCompleted,
			wikiStatus:       types.WikiStatusDegraded,
			wantState:        StateCompleted,
			wantStage:        "completed_degraded_wiki",
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			coordinator := newQueueTestCoordinator(
				t, fmt.Sprintf("parser-derivative-%d", index), "boot-1", 1,
			)
			knowledgeID := fmt.Sprintf("knowledge-derivative-%d", index)
			generation := fmt.Sprintf("generation-%d", index)
			require.NoError(t, coordinator.db.Create(&queueTestKnowledge{
				ID: knowledgeID, TenantID: uint64(50 + index), KnowledgeBaseID: "kb-1",
				ProcessingGeneration: generation, ProcessingOwner: "owner-" + generation,
				ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
			}).Error)
			workflow, _, err := coordinator.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess,
				workflowPayload(t, uint64(50+index), knowledgeID, generation),
			)
			require.NoError(t, err)
			bindWorkflowForTest(t, coordinator, workflow)

			err = coordinator.Process(
				context.Background(),
				asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow)),
				func(context.Context, *asynq.Task) error {
					return coordinator.db.Model(&queueTestKnowledge{}).
						Where("id = ?", knowledgeID).
						Updates(map[string]interface{}{
							"parse_status":      types.ParseStatusCompleted,
							"processing_owner":  "",
							"enrichment_status": test.enrichmentStatus,
							"wiki_status":       test.wikiStatus,
							"updated_at":        time.Now(),
						}).Error
				},
			)
			require.NoError(t, err)
			var finished Workflow
			require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&finished).Error)
			require.Equal(t, test.wantState, finished.State)
			require.Equal(t, test.wantStage, finished.Stage)
		})
	}
}

func TestQueuePositionIsGlobalBeforeTenantFiltering(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-position", "boot-1", 1)
	base := time.Now().Add(-time.Minute)
	entries := []struct {
		tenant uint64
		id     string
	}{
		{1, "tenant-1-first"},
		{2, "tenant-2-middle"},
		{1, "tenant-1-last"},
	}
	for index, entry := range entries {
		generation := "generation-1"
		require.NoError(t, coordinator.db.Create(&queueTestKnowledge{
			ID: entry.id, TenantID: entry.tenant, KnowledgeBaseID: "kb-1",
			ProcessingGeneration: generation, ParseStatus: types.ParseStatusPending,
			UpdatedAt: base.Add(time.Duration(index) * time.Second),
		}).Error)
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(), types.TypeDocumentProcess,
			workflowPayload(t, entry.tenant, entry.id, generation),
		)
		require.NoError(t, err)
		require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
			Update("enqueued_at", base.Add(time.Duration(index)*time.Second)).Error)
	}
	status, err := coordinator.QueueStatus(context.Background(), 1, []string{"tenant-1-first", "tenant-1-last"})
	require.NoError(t, err)
	require.EqualValues(t, 3, status.WaitingTotal)
	require.EqualValues(t, 1, status.Items["tenant-1-first"].Position)
	require.EqualValues(t, 3, status.Items["tenant-1-last"].Position)
}

func TestQueueStatusReturnsOneConsistentLargeSnapshot(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-large-position", "boot-1", 1)
	base := time.Now().Add(-time.Minute)
	ids := make([]string, 0, 205)
	for index := 0; index < 205; index++ {
		id := fmt.Sprintf("large-position-%03d", index)
		ids = append(ids, id)
		generation := "generation-1"
		require.NoError(t, coordinator.db.Create(&queueTestKnowledge{
			ID: id, TenantID: 91, KnowledgeBaseID: "kb-large-position",
			ProcessingGeneration: generation, ParseStatus: types.ParseStatusPending,
			UpdatedAt: base.Add(time.Duration(index) * time.Millisecond),
		}).Error)
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(), types.TypeDocumentProcess,
			workflowPayload(t, 91, id, generation),
		)
		require.NoError(t, err)
		require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
			Update("enqueued_at", base.Add(time.Duration(index)*time.Millisecond)).Error)
	}

	status, err := coordinator.QueueStatus(context.Background(), 91, ids)
	require.NoError(t, err)
	require.EqualValues(t, len(ids), status.WaitingTotal)
	require.Len(t, status.Items, len(ids))
	positions := make(map[int64]struct{}, len(ids))
	for _, id := range ids {
		item := status.Items[id]
		require.Equal(t, "waiting", item.State)
		require.GreaterOrEqual(t, item.Position, int64(1))
		positions[item.Position] = struct{}{}
	}
	require.Len(t, positions, len(ids), "one status response must never contain duplicate ranks")
}

func TestQueueStatusExposesActiveOwnershipFenceEvidence(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-observable", "boot-observable", 1)
	payload := workflowPayload(t, 92, "knowledge-observable", "generation-1")
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess, payload,
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	lease, err := coordinator.Claim(
		context.Background(), types.TypeDocumentProcess,
		deliveryPayload(t, workflow),
	)
	require.NoError(t, err)

	status, err := coordinator.QueueStatus(context.Background(), 92, []string{"knowledge-observable"})
	require.NoError(t, err)
	item := status.Items["knowledge-observable"]
	require.Equal(t, "active", item.State)
	require.Equal(t, "parser-observable", item.OwnerInstanceID)
	require.Equal(t, "boot-observable", item.OwnerBootID)
	require.Equal(t, lease.Epoch, item.ExecutionEpoch)
	require.NotNil(t, item.LeaseUntil)
}

func TestFencedBootCannotClaimOrRenew(t *testing.T) {
	old := newQueueTestCoordinator(t, "parser-shared", "boot-old", 2)
	payload := workflowPayload(t, 21, "knowledge-fenced", "generation-1")
	workflow, _, err := old.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, old, workflow)
	lease, err := old.Claim(context.Background(), types.TypeDocumentProcess, deliveryPayload(t, workflow))
	require.NoError(t, err)

	newBoot := NewCoordinatorWithConfig(old.db, nil, "parser-shared", "boot-new", 2, old.config)
	require.NoError(t, newBoot.registerAndAdopt(context.Background()))
	require.NoError(t, newBoot.MarkReady(context.Background()))

	require.ErrorIs(t, old.renew(context.Background(), lease, "core", time.Now()), ErrLeaseLost)
	var adopted Workflow
	require.NoError(t, old.db.Where("id = ?", workflow.ID).Take(&adopted).Error)
	_, err = old.Claim(context.Background(), types.TypeDocumentProcess, deliveryPayload(t, &adopted))
	require.ErrorIs(t, err, ErrInstanceFenced)
	_, err = newBoot.Claim(context.Background(), types.TypeDocumentProcess, deliveryPayload(t, &adopted))
	require.NoError(t, err)
}

func TestAssertCurrentBootDistinguishesClaimFromDrainAndSupersession(t *testing.T) {
	old := newQueueTestCoordinator(t, "parser-current-boot", "boot-old", 2)
	require.NoError(t, old.AssertCurrentBoot(context.Background(), false))
	require.NoError(t, old.AssertCurrentBoot(context.Background(), true))

	old.MarkDraining()
	require.ErrorIs(t, old.AssertCurrentBoot(context.Background(), false), ErrInstanceFenced)
	require.NoError(t, old.AssertCurrentBoot(context.Background(), true))

	replacement := NewCoordinatorWithConfig(
		old.db, nil, old.instanceID, "boot-new", old.capacity, old.config,
	)
	require.NoError(t, replacement.registerAndAdopt(context.Background()))
	require.NoError(t, replacement.MarkReady(context.Background()))
	require.ErrorIs(t, old.AssertCurrentBoot(context.Background(), true), ErrInstanceFenced)
	require.NoError(t, replacement.AssertCurrentBoot(context.Background(), false))
}

func TestAuxiliaryExecutionPreventsPrematureStoppedProof(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-auxiliary", "boot-1", 1)
	coordinator.config.ShutdownDrainTimeout = 500 * time.Millisecond
	executionCtx, cancelExecution := context.WithCancel(context.Background())
	releaseExecution, err := coordinator.RegisterAuxiliaryExecution(cancelExecution)
	require.NoError(t, err)

	stopDone := make(chan struct{})
	go func() {
		coordinator.Stop()
		close(stopDone)
	}()
	require.Eventually(t, func() bool {
		return errors.Is(executionCtx.Err(), context.Canceled)
	}, 200*time.Millisecond, 5*time.Millisecond)
	require.Eventually(t, func() bool {
		var instance Instance
		if err := coordinator.db.Where(
			"instance_id = ?", coordinator.instanceID,
		).Take(&instance).Error; err != nil {
			return false
		}
		return instance.State == InstanceDraining
	}, 200*time.Millisecond, 5*time.Millisecond)
	select {
	case <-stopDone:
		t.Fatal("coordinator published stop before auxiliary handler returned")
	case <-time.After(40 * time.Millisecond):
	}

	releaseExecution()
	releaseExecution() // release is deliberately idempotent
	select {
	case <-stopDone:
	case <-time.After(time.Second):
		t.Fatal("coordinator did not finish after auxiliary handler returned")
	}
	var stopped Instance
	require.NoError(t, coordinator.db.Where(
		"instance_id = ?", coordinator.instanceID,
	).Take(&stopped).Error)
	require.Equal(t, InstanceStopped, stopped.State)
}

func TestAuxiliaryExecutionDrainTimeoutFailsClosed(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-auxiliary-timeout", "boot-1", 1)
	coordinator.config.ShutdownDrainTimeout = 20 * time.Millisecond
	_, cancelExecution := context.WithCancel(context.Background())
	releaseExecution, err := coordinator.RegisterAuxiliaryExecution(cancelExecution)
	require.NoError(t, err)

	coordinator.Stop()
	var instance Instance
	require.NoError(t, coordinator.db.Where(
		"instance_id = ?", coordinator.instanceID,
	).Take(&instance).Error)
	require.Equal(t, InstanceDraining, instance.State)

	releaseExecution()
	coordinator.Stop()
	require.NoError(t, coordinator.db.Where(
		"instance_id = ?", coordinator.instanceID,
	).Take(&instance).Error)
	require.Equal(t, InstanceStopped, instance.State)
}

func TestAlreadyLeasedDeliveryRemainsRetryable(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-retry", "boot-1", 2)
	payload := workflowPayload(t, 22, "knowledge-retry", "generation-1")
	workflow, _, err := coordinator.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)
	delivery := deliveryPayload(t, workflow)
	_, err = coordinator.Claim(context.Background(), types.TypeDocumentProcess, delivery)
	require.NoError(t, err)

	err = coordinator.Process(context.Background(), asynq.NewTask(types.TypeDocumentProcess, delivery),
		func(context.Context, *asynq.Task) error {
			t.Fatal("duplicate delivery must not execute delegate")
			return nil
		})
	require.ErrorIs(t, err, ErrAlreadyLeased)
}

func TestClaimEnforcesDurableCapacityWhenLocalSlotWasReleased(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-durable-capacity", "boot-1", 1)

	first, _, err := coordinator.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, 220, "knowledge-capacity-first", "generation-first"),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, first)
	second, _, err := coordinator.RegisterWorkflow(
		context.Background(),
		types.TypeDocumentProcess,
		workflowPayload(t, 220, "knowledge-capacity-second", "generation-second"),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, second)

	// A direct Claim intentionally leaves no process-local execution entry.
	// This reproduces a handler whose DB release failed during a PostgreSQL
	// outage after its in-memory semaphore slot had already been returned.
	_, err = coordinator.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryPayload(t, first),
	)
	require.NoError(t, err)
	require.False(t, coordinator.hasActiveExecution(first.ID, first.DispatchEpoch))

	var delegateCalls atomic.Int32
	err = coordinator.Process(
		context.Background(),
		asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, second)),
		func(context.Context, *asynq.Task) error {
			delegateCalls.Add(1)
			return nil
		},
	)
	require.ErrorIs(t, err, ErrInstanceCapacity)
	require.Zero(t, delegateCalls.Load())

	var durableLeases int64
	require.NoError(t, coordinator.db.Model(&Workflow{}).
		Where("state = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			StateLeased, coordinator.instanceID, coordinator.bootID).
		Count(&durableLeases).Error)
	require.EqualValues(t, coordinator.capacity, durableLeases)

	var stillQueued Workflow
	require.NoError(t, coordinator.db.Where("id = ?", second.ID).Take(&stillQueued).Error)
	require.Equal(t, StateQueued, stillQueued.State)
	require.Empty(t, stillQueued.OwnerInstanceID)
	require.Equal(t, second.DispatchEpoch, stillQueued.DispatchEpoch)
}

func TestConcurrentProcessCannotExceedDurableCapacityWithOrphanedLeases(t *testing.T) {
	const capacity = 4
	coordinator := newQueueTestCoordinator(
		t, "parser-concurrent-durable-capacity", "boot-1", capacity,
	)
	workflows := make([]*Workflow, 0, capacity*3)
	for index := 0; index < capacity*3; index++ {
		workflow, _, err := coordinator.RegisterWorkflow(
			context.Background(),
			types.TypeDocumentProcess,
			workflowPayload(
				t,
				221,
				fmt.Sprintf("knowledge-capacity-%02d", index),
				fmt.Sprintf("generation-%02d", index),
			),
		)
		require.NoError(t, err)
		bindWorkflowForTest(t, coordinator, workflow)
		workflows = append(workflows, workflow)
	}

	// Fill the durable allocation without occupying any local semaphore slot,
	// matching the post-outage state that originally produced 2x capacity.
	for index := 0; index < capacity; index++ {
		_, err := coordinator.Claim(
			context.Background(),
			types.TypeDocumentProcess,
			deliveryPayload(t, workflows[index]),
		)
		require.NoError(t, err)
	}
	require.Zero(t, coordinator.activeExecutionCount())

	var delegateCalls atomic.Int32
	errs := make(chan error, len(workflows)-capacity)
	var wait sync.WaitGroup
	for _, workflow := range workflows[capacity:] {
		workflow := workflow
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- coordinator.Process(
				context.Background(),
				asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow)),
				func(context.Context, *asynq.Task) error {
					delegateCalls.Add(1)
					return nil
				},
			)
		}()
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		require.ErrorIs(t, err, ErrInstanceCapacity)
	}
	require.Zero(t, delegateCalls.Load())

	var durableLeases int64
	require.NoError(t, coordinator.db.Model(&Workflow{}).
		Where("state = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			StateLeased, coordinator.instanceID, coordinator.bootID).
		Count(&durableLeases).Error)
	require.EqualValues(t, capacity, durableLeases)
}

func TestDelegatePanicReleasesLease(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-panic", "boot-1", 1)
	now := time.Now()
	knowledge := queueTestKnowledge{
		ID: "knowledge-panic", TenantID: 23, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusPending, UpdatedAt: now,
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)

	err = coordinator.Process(
		context.Background(),
		asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow)),
		func(context.Context, *asynq.Task) error { panic("boom") },
	)
	require.Error(t, err)
	require.Contains(t, err.Error(), "handler panic")
	var released Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&released).Error)
	require.Equal(t, StateQueued, released.State)
	require.Empty(t, released.OwnerInstanceID)
	require.Contains(t, released.LastError, "panic")
	require.False(t, coordinator.hasActiveExecution(workflow.ID, workflow.DispatchEpoch))
}

func TestInitialObserveFailureRequeuesWithoutRunningDelegate(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-observe-error", "boot-1", 1)
	knowledge := queueTestKnowledge{
		ID: "knowledge-observe-error", TenantID: 26, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)

	observeFailure := errors.New("temporary knowledge observation failure")
	coordinator.observeHook = func(context.Context, *Lease) error { return observeFailure }
	var delegateCalls atomic.Int32
	err = coordinator.Process(
		context.Background(),
		asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow)),
		func(context.Context, *asynq.Task) error {
			delegateCalls.Add(1)
			return nil
		},
	)
	require.ErrorIs(t, err, observeFailure)
	require.Zero(t, delegateCalls.Load())
	var released Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&released).Error)
	require.Equal(t, StateQueued, released.State)
	require.Empty(t, released.OwnerInstanceID)
	require.Contains(t, released.LastError, observeFailure.Error())
}

func TestRegistrationPersistsOriginalDeliveryBudget(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-options", "boot-1", 1)
	payload := workflowPayload(t, 24, "knowledge-options", "generation-1")
	deadline := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Second)
	workflow, _, err := coordinator.RegisterWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, payload,
		[]asynq.Option{
			asynq.MaxRetry(3), asynq.Timeout(2 * time.Hour),
			asynq.Deadline(deadline), asynq.Retention(time.Minute),
		},
	)
	require.NoError(t, err)
	require.Equal(t, 3, workflow.MaxRetry)
	require.Equal(t, int64(2*time.Hour), workflow.DelegateTimeoutNanos)
	require.Equal(t, int64(coordinator.config.WorkflowTimeout), workflow.WorkflowTimeoutNanos)
	require.NotNil(t, workflow.DeadlineAt)
	require.WithinDuration(t, deadline, *workflow.DeadlineAt, time.Second)
	require.Equal(t, int64(time.Minute), workflow.RetentionNanos)
}

func TestSlowRecoveryDoesNotStarveHeartbeat(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-independent-loops", "boot-1", 1)
	coordinator.config.HeartbeatInterval = 10 * time.Millisecond
	coordinator.config.RecoveryInterval = 10 * time.Millisecond
	coordinator.config.RecoveryCycleTimeout = 500 * time.Millisecond
	recoveryStarted := make(chan struct{})
	releaseRecovery := make(chan struct{})
	var once sync.Once
	coordinator.recoverCycleHook = func(ctx context.Context) error {
		once.Do(func() { close(recoveryStarted) })
		select {
		case <-releaseRecovery:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go coordinator.run(ctx, done)
	<-recoveryStarted
	var before Instance
	require.NoError(t, coordinator.db.Where("instance_id = ?", coordinator.instanceID).Take(&before).Error)
	require.Eventually(t, func() bool {
		var current Instance
		if err := coordinator.db.Where("instance_id = ?", coordinator.instanceID).Take(&current).Error; err != nil {
			return false
		}
		return current.LastHeartbeatAt.After(before.LastHeartbeatAt)
	}, 300*time.Millisecond, 10*time.Millisecond)
	close(releaseRecovery)
	cancel()
	<-done
}

func TestDeadlineIgnoringDelegateTripsAndClearsLiveness(t *testing.T) {
	coordinator := newQueueTestCoordinator(t, "parser-stuck", "boot-1", 1)
	coordinator.config.StuckHandlerGrace = 10 * time.Millisecond
	knowledge := queueTestKnowledge{
		ID: "knowledge-stuck", TenantID: 25, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ProcessingOwner: "owner-generation-1",
		ParseStatus: types.ParseStatusPending, UpdatedAt: time.Now(),
	}
	require.NoError(t, coordinator.db.Create(&knowledge).Error)
	workflow, _, err := coordinator.RegisterWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess,
		workflowPayload(t, knowledge.TenantID, knowledge.ID, knowledge.ProcessingGeneration),
		[]asynq.Option{asynq.Timeout(20 * time.Millisecond)},
	)
	require.NoError(t, err)
	bindWorkflowForTest(t, coordinator, workflow)

	releaseDelegate := make(chan struct{})
	processDone := make(chan error, 1)
	go func() {
		processDone <- coordinator.Process(
			context.Background(),
			asynq.NewTask(types.TypeDocumentProcess, deliveryPayload(t, workflow)),
			func(context.Context, *asynq.Task) error {
				<-releaseDelegate // deliberately ignore the cancelled delegate context
				return context.DeadlineExceeded
			},
		)
	}()
	require.Eventually(t, func() bool { return !coordinator.IsLive() && !coordinator.IsReady() },
		300*time.Millisecond, 5*time.Millisecond)
	close(releaseDelegate)
	require.Error(t, <-processDone)
	require.Eventually(t, coordinator.IsLive, 100*time.Millisecond, 5*time.Millisecond)

	var released Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&released).Error)
	require.Equal(t, StateQueued, released.State)
	require.Empty(t, released.OwnerInstanceID)
}

func TestZeroCountFinalizingWikiStageDoesNotReinvokeCoreDelegate(t *testing.T) {
	now := time.Now()
	snapshot := &knowledgeSnapshot{
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessedAt:          &now,
		PendingSubtasksCount: 0,
		WikiStatus:           types.WikiStatusPending,
	}
	stage := stageForKnowledge(snapshot)
	require.Equal(t, "wiki", stage)
	require.True(t, shouldAwaitCommittedDerivatives(snapshot, stage))
	require.True(t, coreCommittedForExternalWait(snapshot))
}

func TestFinalizingWithDerivativeSlotsStillReportsDerivativeStage(t *testing.T) {
	now := time.Now()
	snapshot := &knowledgeSnapshot{
		ParseStatus:          types.ParseStatusFinalizing,
		ProcessedAt:          &now,
		PendingSubtasksCount: 1,
		WikiStatus:           types.WikiStatusPending,
	}
	stage := stageForKnowledge(snapshot)
	require.Equal(t, "derivatives", stage)
	require.False(t, shouldAwaitCommittedDerivatives(snapshot, stage))
	require.True(t, coreCommittedForExternalWait(snapshot))
}
