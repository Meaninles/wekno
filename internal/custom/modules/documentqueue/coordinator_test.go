package documentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

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
	ParseStatus          string
	EnableStatus         string
	Description          string
	ProcessedAt          *time.Time
	EmbeddingModelID     string
	ErrorMessage         string
	PendingSubtasksCount int
	UpdatedAt            time.Time
	DeletedAt            gorm.DeletedAt
}

func (queueTestKnowledge) TableName() string { return "knowledges" }

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
	t.Helper()
	payload, err := json.Marshal(types.DocumentProcessPayload{
		TenantID: tenantID, KnowledgeID: knowledgeID, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: generation, ProcessingOwner: "owner-" + generation,
	})
	require.NoError(t, err)
	return payload
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

func TestProcessHoldsSlotUntilWikiDurableWorkDrains(t *testing.T) {
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

	deleteDelay := 80 * time.Millisecond
	started := time.Now()
	err = coordinator.Process(context.Background(), task, func(ctx context.Context, _ *asynq.Task) error {
		dedupKey, keyErr := wikiqueue.IngestDedupKey(knowledge.ID, knowledge.ProcessingGeneration)
		require.NoError(t, keyErr)
		op := types.TaskPendingOp{
			TenantID: 13, TaskType: types.TypeWikiIngest, Scope: types.TaskScopeKnowledgeBase,
			ScopeID: "kb-1", Op: "ingest", DedupKey: dedupKey, Payload: []byte(`{}`),
		}
		require.NoError(t, coordinator.db.Create(&op).Error)
		require.NoError(t, coordinator.db.Model(&queueTestKnowledge{}).Where("id = ?", knowledge.ID).
			Updates(map[string]interface{}{
				"parse_status":     types.ParseStatusCompleted,
				"processing_owner": "", "updated_at": time.Now(),
			}).Error)
		go func() {
			time.Sleep(deleteDelay)
			_ = coordinator.db.Delete(&types.TaskPendingOp{}, op.ID).Error
		}()
		return nil
	})
	require.NoError(t, err)
	require.GreaterOrEqual(t, time.Since(started), deleteDelay)
	var finished Workflow
	require.NoError(t, coordinator.db.Where("id = ?", workflow.ID).Take(&finished).Error)
	require.Equal(t, StateCompleted, finished.State)
	require.Equal(t, "completed", finished.Stage)
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
