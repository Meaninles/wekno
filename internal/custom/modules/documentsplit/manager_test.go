package documentsplit

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

var managerTestSequence atomic.Uint64

func TestConfigClampsTableEnrichmentStrataToGenericBounds(t *testing.T) {
	cfg := (Config{
		QuestionStrata:      32,
		GraphStrata:         48,
		TableQuestionStrata: 64,
		TableGraphStrata:    128,
	}).normalized()
	require.Equal(t, 32, cfg.TableQuestionStrata)
	require.Equal(t, 48, cfg.TableGraphStrata)
}

func TestConfigSeparatesRenewableLeaseFromTaskDeadline(t *testing.T) {
	cfg := (Config{
		LeaseDuration: 5 * time.Minute,
		TaskTimeout:   time.Minute,
	}).normalized()
	require.Equal(t, 5*time.Minute, cfg.LeaseDuration)
	require.Equal(t, 10*time.Minute, cfg.TaskTimeout)
}

func TestProviderOutageRefundsSplitPartAttemptAtRetryLimit(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	manager := NewManagerWithConfig(db, &splitEnqueuerStub{}, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 1,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RetryBackoffBase: 10 * time.Second, RetryBackoffMax: time.Minute,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(1)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, epoch, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Attempt)

	providerErr := &modeladmission.ProviderUnavailableError{
		Kind:       modeladmission.KindEmbedding,
		RetryAfter: 30 * time.Second,
		Cause:      errors.New("embedding upstream 503"),
	}
	require.NoError(t, manager.ReleasePart(ctx, claimed, epoch, providerErr))

	var stored Part
	require.NoError(t, db.First(&stored, "id = ?", claimed.ID).Error)
	require.Equal(t, PartPreparing, stored.State)
	require.Equal(t, 0, stored.Attempt, "provider outage must refund the claimed business attempt")
	require.NotNil(t, stored.LeaseUntil)
	require.Greater(t, time.Until(*stored.LeaseUntil), 20*time.Second)
	var storedPlan Plan
	require.NoError(t, db.First(&storedPlan, "id = ?", created.ID).Error)
	require.NotEqual(t, PlanFailed, storedPlan.State)
	require.Zero(t, storedPlan.FailedParts)
}

func TestShutdownCancellationRefundsSplitPartAttemptAtRetryLimit(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	manager := NewManagerWithConfig(db, &splitEnqueuerStub{}, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 1,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RetryBackoffBase: 10 * time.Second, RetryBackoffMax: time.Minute,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(1)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, epoch, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Attempt)

	require.NoError(t, manager.ReleasePart(ctx, claimed, epoch, context.Canceled))

	var stored Part
	require.NoError(t, db.First(&stored, "id = ?", claimed.ID).Error)
	require.Equal(t, PartPreparing, stored.State)
	require.Equal(t, 0, stored.Attempt)
	require.NotNil(t, stored.LeaseUntil)
	var storedPlan Plan
	require.NoError(t, db.First(&storedPlan, "id = ?", created.ID).Error)
	require.NotEqual(t, PlanFailed, storedPlan.State)
	require.Zero(t, storedPlan.FailedParts)
}

func TestPartTaskIDAdvancesWithLeaseDeliveryEpoch(t *testing.T) {
	first := PartTaskID("plan", 7, 1)
	recovery := PartTaskID("plan", 7, 2)
	require.NotEqual(t, first, recovery)
	require.Equal(t, first, PartTaskID("plan", 7, 1))
	require.Contains(t, recovery, "delivery:000002")
}

type splitEnqueuerStub struct {
	mu    sync.Mutex
	tasks []*asynq.Task
	err   error
}

func (s *splitEnqueuerStub) Enqueue(
	task *asynq.Task, _ ...asynq.Option,
) (*asynq.TaskInfo, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = append(s.tasks, task)
	if s.err != nil {
		return nil, s.err
	}
	return &asynq.TaskInfo{
		ID: fmt.Sprintf("task-%d", len(s.tasks)), Type: task.Type(), Queue: QueuePart,
	}, nil
}

func (s *splitEnqueuerStub) setError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *splitEnqueuerStub) count(taskType string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, task := range s.tasks {
		if task.Type() == taskType {
			count++
		}
	}
	return count
}

func newManagerTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf(
		"file:document-split-manager-%d?mode=memory&cache=shared",
		managerTestSequence.Add(1),
	)
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	require.NoError(t, db.AutoMigrate(&Plan{}, &Part{}))
	return db
}

func newSplitQueueCoordinator(
	t *testing.T,
	db *gorm.DB,
	instanceID string,
	bootID string,
) *documentqueue.Coordinator {
	t.Helper()
	cfg := documentqueue.DefaultConfig()
	cfg.HeartbeatInterval = time.Hour
	cfg.RecoveryInterval = time.Hour
	cfg.InstanceStaleAfter = 2 * time.Hour
	cfg.ShutdownDrainTimeout = 200 * time.Millisecond
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, instanceID, bootID, 1, cfg,
	)
	require.NoError(t, coordinator.Start(context.Background()))
	t.Cleanup(coordinator.Stop)
	return coordinator
}

func newFencedSplitManager(
	db *gorm.DB,
	enqueuer *splitEnqueuerStub,
	queue *documentqueue.Coordinator,
	cfg Config,
) *Manager {
	manager := NewManager(ManagerParams{
		DB: db, Enqueuer: enqueuer, Queue: queue,
	})
	manager.config = cfg.normalized()
	return manager
}

func newManagerTestPlan(partCount int) (*Plan, []*Part) {
	plan := &Plan{
		TenantID: 7, KnowledgeBaseID: "00000000-0000-0000-0000-000000000001",
		KnowledgeID:          "00000000-0000-0000-0000-000000000002",
		ProcessingGeneration: "00000000-0000-0000-0000-000000000003",
		ProcessingOwner:      "owner", SourcePath: "local://source",
		SourceName: "source.pdf", SourceType: "pdf", SourceSize: 100,
		SourceSHA256:   "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		PlannerVersion: "test-v1",
	}
	parts := make([]*Part, 0, partCount)
	for index := 0; index < partCount; index++ {
		parts = append(parts, &Part{
			PartIndex: index, FileName: fmt.Sprintf("part-%d.pdf", index),
			FileType: "pdf", InputPath: fmt.Sprintf("local://part-%d", index),
			InputSize:   10,
			InputSHA256: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
			Locator:     []byte(`{"kind":"pages","page_start":1,"page_end":1}`),
			Metrics:     []byte(`{"pages":1}`),
		})
	}
	return plan, parts
}

func payloadFor(part *Part) PartPayload {
	return PartPayload{
		TenantID: part.TenantID, KnowledgeBaseID: part.KnowledgeBaseID,
		KnowledgeID:          part.KnowledgeID,
		ProcessingGeneration: part.ProcessingGeneration,
		PlanID:               part.PlanID, PartID: part.ID, PartIndex: part.PartIndex,
		DeliveryEpoch: part.LeaseEpoch + 1,
	}
}

func payloadForEpoch(part *Part, deliveryEpoch int64) PartPayload {
	payload := payloadFor(part)
	payload.DeliveryEpoch = deliveryEpoch
	return payload
}

func TestManagerDispatchUsesBoundedPerDocumentWindow(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 2, PerDocumentWindow: 2, MaxRetry: 3,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(5)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 2, enqueuer.count(TypePartProcess))

	var queued, preparing int64
	require.NoError(t, db.Model(&Part{}).
		Where("plan_id = ? AND state = ?", created.ID, PartQueued).
		Count(&queued).Error)
	require.NoError(t, db.Model(&Part{}).
		Where("plan_id = ? AND state = ?", created.ID, PartPreparing).
		Count(&preparing).Error)
	require.EqualValues(t, 2, queued)
	require.EqualValues(t, 3, preparing)

	claimed, epoch, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	allComplete, err := manager.CompletePart(ctx, claimed, epoch, PartCompletion{
		MarkdownChars: 100, ChunkCount: 1, FirstChunkID: "first", LastChunkID: "last",
	})
	require.NoError(t, err)
	require.False(t, allComplete)
	// One completion opens one slot and admits exactly one additional part.
	require.Equal(t, 3, enqueuer.count(TypePartProcess))
}

func TestManagerCompletionSurvivesWakeupFailure(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 2, PerDocumentWindow: 2, MaxRetry: 3,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(3)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, epoch, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)

	enqueuer.setError(errors.New("redis unavailable"))
	allComplete, err := manager.CompletePart(ctx, claimed, epoch, PartCompletion{
		MarkdownChars: 100, ChunkCount: 1, FirstChunkID: "first", LastChunkID: "last",
	})
	require.NoError(t, err)
	require.False(t, allComplete)

	var stored Part
	require.NoError(t, db.First(&stored, "id = ?", claimed.ID).Error)
	require.Equal(t, PartCompleted, stored.State)
	require.Empty(t, stored.LeaseOwner)
	require.Nil(t, stored.LeaseUntil)
}

func TestManagerCompletePartIsIdempotent(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 3,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(2)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, epoch, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	completion := PartCompletion{
		MarkdownChars: 100, ChunkCount: 1, FirstChunkID: "first", LastChunkID: "last",
	}
	_, err = manager.CompletePart(ctx, claimed, epoch, completion)
	require.NoError(t, err)
	_, err = manager.CompletePart(ctx, claimed, epoch, completion)
	require.NoError(t, err)

	var stored Plan
	require.NoError(t, db.First(&stored, "id = ?", created.ID).Error)
	require.Equal(t, 1, stored.CompletedParts)
}

func TestManagerGenerationCursorDoesNotSkipDuplicateChunkIndexes(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	manager := NewManagerWithConfig(db, &splitEnqueuerStub{}, Config{})

	chunks := []*types.Chunk{
		{ID: "00000000-0000-0000-0000-000000000001", TenantID: 7, KnowledgeID: "knowledge",
			ProcessingGeneration: "generation", SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeText},
		{ID: "00000000-0000-0000-0000-000000000002", TenantID: 7, KnowledgeID: "knowledge",
			ProcessingGeneration: "generation", SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeImageOCR},
		{ID: "00000000-0000-0000-0000-000000000003", TenantID: 7, KnowledgeID: "knowledge",
			ProcessingGeneration: "generation", SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeImageCaption},
		{ID: "00000000-0000-0000-0000-000000000004", TenantID: 7, KnowledgeID: "knowledge",
			ProcessingGeneration: "generation", SplitPartIndex: 0, ChunkIndex: 1, ChunkType: types.ChunkTypeText},
	}
	require.NoError(t, db.Create(&chunks).Error)

	cursor := GenerationChunkCursor{ChunkIndex: -1}
	var ids []string
	for {
		page, err := manager.ListGenerationChunksByTypeAfter(
			ctx, 7, "knowledge", "generation", nil, cursor, 2,
		)
		require.NoError(t, err)
		if len(page) == 0 {
			break
		}
		for _, chunk := range page {
			ids = append(ids, chunk.ID)
			cursor = GenerationChunkCursor{ChunkIndex: chunk.ChunkIndex, ChunkID: chunk.ID}
		}
	}
	require.Equal(t, []string{
		"00000000-0000-0000-0000-000000000001",
		"00000000-0000-0000-0000-000000000002",
		"00000000-0000-0000-0000-000000000003",
		"00000000-0000-0000-0000-000000000004",
	}, ids)
}

func TestManagerRecoveryRequeuesExpiredLease(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL,
			processing_owner TEXT NOT NULL,
			parse_status TEXT NOT NULL,
			error_message TEXT NOT NULL DEFAULT '',
			pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
			enrichment_status TEXT NOT NULL DEFAULT 'none',
			wiki_status TEXT NOT NULL DEFAULT 'none',
			wiki_error_message TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NULL,
			deleted_at DATETIME NULL
		)
	`).Error)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 3,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(1)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
		 (id, tenant_id, knowledge_base_id, processing_generation, processing_owner, parse_status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		created.KnowledgeID, created.TenantID, created.KnowledgeBaseID,
		created.ProcessingGeneration, created.ProcessingOwner, types.ParseStatusProcessing,
	).Error)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, _, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, PartLeased, claimed.State)
	require.NoError(t, db.Model(&Part{}).Where("id = ?", claimed.ID).Update(
		"lease_until", time.Now().Add(-time.Minute),
	).Error)

	before := enqueuer.count(TypePartProcess)
	require.NoError(t, manager.recoverOnce(ctx))
	require.Greater(t, enqueuer.count(TypePartProcess), before)
	reclaimed, _, err := manager.ClaimPart(
		ctx, payloadForEpoch(parts[0], claimed.LeaseEpoch+1),
	)
	require.NoError(t, err)
	require.Equal(t, PartLeased, reclaimed.State)
	require.Contains(t, reclaimed.LastError, "lease expired")
}

func TestManagerRestartImmediatelyReclaimsSameInstanceOldBoot(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	cfg := Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 3,
		LeaseDuration: time.Minute, TaskTimeout: 10 * time.Minute,
		RecoveryInterval: time.Second, RecoveryBatchSize: 20,
		ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	}
	oldBoot := NewManagerWithConfig(db, enqueuer, cfg)
	oldBoot.ownerPrefix = "split:replica-a:identity:"
	oldBoot.owner = oldBoot.ownerPrefix + "boot-old:worker"
	plan, parts := newManagerTestPlan(1)
	created, err := oldBoot.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, oldBoot.DispatchPlan(ctx, created.ID))
	claimed, _, err := oldBoot.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Attempt)
	require.NotNil(t, claimed.LeaseUntil)
	require.True(t, claimed.LeaseUntil.After(time.Now()))

	restarted := NewManagerWithConfig(db, enqueuer, cfg)
	restarted.ownerPrefix = oldBoot.ownerPrefix
	restarted.owner = restarted.ownerPrefix + "boot-new:worker"
	require.NoError(t, restarted.recoverSupersededLocalBootLeases(ctx, time.Now()))

	var recovered Part
	require.NoError(t, db.First(&recovered, "id = ?", claimed.ID).Error)
	require.Equal(t, PartQueued, recovered.State)
	require.Equal(t, 0, recovered.Attempt)
	require.Equal(t, int64(1), recovered.LeaseEpoch)
	require.Empty(t, recovered.LeaseOwner)
	require.Nil(t, recovered.LeaseUntil)
	require.Contains(t, recovered.LastError, "boot superseded")

	reclaimed, epoch, err := restarted.ClaimPart(
		ctx, payloadForEpoch(parts[0], claimed.LeaseEpoch+1),
	)
	require.NoError(t, err)
	require.Equal(t, 1, reclaimed.Attempt)
	require.Equal(t, int64(2), epoch)
	_, err = restarted.CompletePart(ctx, reclaimed, epoch, PartCompletion{
		MarkdownChars: 42, ChunkCount: 1, FirstChunkID: "first", LastChunkID: "last",
	})
	require.NoError(t, err)

	var completed Part
	require.NoError(t, db.First(&completed, "id = ?", claimed.ID).Error)
	var metrics map[string]interface{}
	require.NoError(t, json.Unmarshal(completed.Metrics, &metrics))
	require.Equal(t, restarted.owner, metrics["execution_owner"])
	require.EqualValues(t, 2, metrics["execution_lease_epoch"])
}

func TestExpiredPartWaitsForExactOwnerTerminationAndFencesPausedOwner(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	cfg := Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 3,
		LeaseDuration: time.Minute, TaskTimeout: 10 * time.Minute,
		RecoveryInterval: time.Second, RecoveryBatchSize: 20,
		ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	}
	ownerQueue := newSplitQueueCoordinator(t, db, "split-pod-owner", "boot-owner")
	survivorQueue := newSplitQueueCoordinator(t, db, "split-pod-survivor", "boot-survivor")
	owner := newFencedSplitManager(db, enqueuer, ownerQueue, cfg)
	survivor := newFencedSplitManager(db, enqueuer, survivorQueue, cfg)

	plan, parts := newManagerTestPlan(1)
	created, err := owner.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, owner.DispatchPlan(ctx, created.ID))
	claimed, oldEpoch, err := owner.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, "split-pod-owner", claimed.LeaseInstanceID)
	require.Equal(t, "boot-owner", claimed.LeaseBootID)
	require.NoError(t, db.Model(&Part{}).Where("id = ?", claimed.ID).
		Update("lease_until", time.Now().Add(-time.Minute)).Error)

	// Lease age alone is never proof: neither the same live boot nor a
	// different healthy replica may recover a paused owner.
	require.NoError(t, owner.recoverExpiredLeases(ctx, time.Now()))
	require.NoError(t, survivor.recoverExpiredLeases(ctx, time.Now()))
	var stillOwned Part
	require.NoError(t, db.First(&stillOwned, "id = ?", claimed.ID).Error)
	require.Equal(t, PartLeased, stillOwned.State)
	require.Equal(t, owner.owner, stillOwned.LeaseOwner)

	ownerQueue.Stop()
	require.NoError(t, survivor.recoverExpiredLeases(ctx, time.Now()))
	var recovered Part
	require.NoError(t, db.First(&recovered, "id = ?", claimed.ID).Error)
	require.Equal(t, PartQueued, recovered.State)
	require.Empty(t, recovered.LeaseOwner)
	require.Empty(t, recovered.LeaseInstanceID)
	require.Empty(t, recovered.LeaseBootID)

	// A paused handler from the dead boot can no longer heartbeat, complete,
	// or release the replacement's work.
	require.ErrorIs(t, owner.HeartbeatPart(ctx, claimed.ID, oldEpoch), ErrLeaseLost)
	_, err = owner.CompletePart(ctx, claimed, oldEpoch, PartCompletion{
		MarkdownChars: 10, ChunkCount: 1, FirstChunkID: "old", LastChunkID: "old",
	})
	require.ErrorIs(t, err, ErrLeaseLost)
	require.ErrorIs(t, owner.ReleasePart(ctx, claimed, oldEpoch, errors.New("late")), ErrLeaseLost)

	reclaimed, newEpoch, err := survivor.ClaimPart(
		ctx, payloadForEpoch(parts[0], oldEpoch+1),
	)
	require.NoError(t, err)
	require.Equal(t, oldEpoch+1, newEpoch)
	require.Equal(t, "split-pod-survivor", reclaimed.LeaseInstanceID)
	_, err = survivor.CompletePart(ctx, reclaimed, newEpoch, PartCompletion{
		MarkdownChars: 11, ChunkCount: 1, FirstChunkID: "new", LastChunkID: "new",
	})
	require.NoError(t, err)
}

func TestConcurrentSurvivorsRecoverExpiredPartOnlyOnce(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	cfg := Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 3,
		LeaseDuration: time.Minute, TaskTimeout: 10 * time.Minute,
		RecoveryInterval: time.Second, RecoveryBatchSize: 20,
		ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	}
	ownerQueue := newSplitQueueCoordinator(t, db, "split-race-owner", "boot-owner")
	firstQueue := newSplitQueueCoordinator(t, db, "split-race-first", "boot-first")
	secondQueue := newSplitQueueCoordinator(t, db, "split-race-second", "boot-second")
	owner := newFencedSplitManager(db, enqueuer, ownerQueue, cfg)
	first := newFencedSplitManager(db, enqueuer, firstQueue, cfg)
	second := newFencedSplitManager(db, enqueuer, secondQueue, cfg)

	plan, parts := newManagerTestPlan(1)
	created, err := owner.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, owner.DispatchPlan(ctx, created.ID))
	claimed, oldEpoch, err := owner.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.NoError(t, db.Model(&Part{}).Where("id = ?", claimed.ID).
		Update("lease_until", time.Now().Add(-time.Minute)).Error)
	ownerQueue.Stop()

	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, survivor := range []*Manager{first, second} {
		wg.Add(1)
		go func(candidate *Manager) {
			defer wg.Done()
			<-start
			errs <- candidate.recoverExpiredLeases(ctx, time.Now())
		}(survivor)
	}
	close(start)
	wg.Wait()
	close(errs)
	for recoverErr := range errs {
		require.NoError(t, recoverErr)
	}
	var recovered Part
	require.NoError(t, db.First(&recovered, "id = ?", claimed.ID).Error)
	require.Equal(t, PartQueued, recovered.State)
	require.Equal(t, oldEpoch, recovered.LeaseEpoch)

	payload := payloadForEpoch(parts[0], oldEpoch+1)
	var winners atomic.Int32
	for _, survivor := range []*Manager{first, second} {
		_, _, claimErr := survivor.ClaimPart(ctx, payload)
		if claimErr == nil {
			winners.Add(1)
			continue
		}
		require.True(t,
			errors.Is(claimErr, ErrStalePart) || errors.Is(claimErr, ErrPartLeased),
			"unexpected claim error: %v", claimErr,
		)
	}
	require.EqualValues(t, 1, winners.Load())
}

func TestManagerRateLimitBackoffFencesOldDeliveryAndPausesDocument(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 6,
		LeaseDuration: time.Minute, TaskTimeout: 10 * time.Minute,
		RecoveryInterval: time.Second, RecoveryBatchSize: 20,
		RetryBackoffBase: time.Minute, RetryBackoffMax: 4 * time.Minute,
		ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(2)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 1, enqueuer.count(TypePartProcess))

	firstDelivery := payloadFor(parts[0])
	claimed, epoch, err := manager.ClaimPart(ctx, firstDelivery)
	require.NoError(t, err)
	require.NoError(t, manager.ReleasePart(
		ctx, claimed, epoch,
		errors.New("HTTP 429 Too Many Requests: TPM rate limit reached"),
	))

	var deferred Part
	require.NoError(t, db.First(&deferred, "id = ?", claimed.ID).Error)
	require.Equal(t, PartPreparing, deferred.State)
	require.Equal(t, 1, deferred.Attempt)
	require.NotNil(t, deferred.LeaseUntil)
	require.True(t, deferred.LeaseUntil.After(time.Now()))
	_, _, err = manager.ClaimPart(ctx, firstDelivery)
	require.ErrorIs(t, err, ErrStalePart)

	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 1, enqueuer.count(TypePartProcess))
	var sibling Part
	require.NoError(t, db.First(&sibling, "id = ?", parts[1].ID).Error)
	require.Equal(t, PartPreparing, sibling.State)
	require.NotNil(t, sibling.LeaseUntil)

	require.NoError(t, db.Model(&Part{}).Where(
		"plan_id = ? AND state = ?", created.ID, PartPreparing,
	).Update("lease_until", time.Now().Add(-time.Second)).Error)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 2, enqueuer.count(TypePartProcess))
	reclaimed, newEpoch, err := manager.ClaimPart(
		ctx, payloadForEpoch(parts[0], claimed.LeaseEpoch+1),
	)
	require.NoError(t, err)
	require.Equal(t, int64(2), newEpoch)
	require.Equal(t, 2, reclaimed.Attempt)
}

func TestManagerTransientRetryUsesSinglePartProbe(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 4, PerDocumentWindow: 4, MaxRetry: 6,
		LeaseDuration: time.Minute, TaskTimeout: 10 * time.Minute,
		RecoveryInterval: time.Second, RecoveryBatchSize: 20,
		RetryBackoffBase: time.Minute, RetryBackoffMax: 4 * time.Minute,
		ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(5)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, db.Model(&Part{}).Where("id = ?", parts[0].ID).Updates(
		map[string]interface{}{
			"attempt":     1,
			"last_error":  "HTTP 429 Too Many Requests",
			"lease_until": time.Now().Add(-time.Second),
		},
	).Error)

	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 1, enqueuer.count(TypePartProcess))

	var queued int64
	require.NoError(t, db.Model(&Part{}).Where(
		"plan_id = ? AND state = ?", created.ID, PartQueued,
	).Count(&queued).Error)
	require.EqualValues(t, 1, queued)

	claimed, _, err := manager.ClaimPart(
		ctx, payloadForEpoch(parts[0], parts[0].LeaseEpoch+1),
	)
	require.NoError(t, err)
	require.Greater(t, claimed.Attempt, 1)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 1, enqueuer.count(TypePartProcess),
		"an in-flight retry probe must keep fresh siblings paused")

	allComplete, err := manager.CompletePart(
		ctx, claimed, claimed.LeaseEpoch, PartCompletion{},
	)
	require.NoError(t, err)
	require.False(t, allComplete)
	require.Equal(t, 1, enqueuer.count(TypePartProcess),
		"a successful retry must pace the next part instead of spending residual TPM")
	require.NoError(t, db.Model(&Part{}).Where(
		"plan_id = ? AND state = ?", created.ID, PartPreparing,
	).Update("lease_until", time.Now().Add(-time.Second)).Error)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	require.Equal(t, 2, enqueuer.count(TypePartProcess),
		"the next single-part probe must resume after its cooldown")
	require.NoError(t, db.Model(&Part{}).Where(
		"plan_id = ? AND state = ?", created.ID, PartQueued,
	).Count(&queued).Error)
	require.EqualValues(t, 1, queued)
}

func TestManagerRecoveryFailsTerminalExpiredLeaseAndKnowledge(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	require.NoError(t, db.Exec(`
		CREATE TABLE knowledges (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL,
			processing_owner TEXT NOT NULL,
			parse_status TEXT NOT NULL,
			error_message TEXT NOT NULL DEFAULT '',
			pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
			enrichment_status TEXT NOT NULL DEFAULT 'none',
			wiki_status TEXT NOT NULL DEFAULT 'none',
			wiki_error_message TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NULL,
			deleted_at DATETIME NULL
		)
	`).Error)
	enqueuer := &splitEnqueuerStub{}
	manager := NewManagerWithConfig(db, enqueuer, Config{
		PartConcurrency: 1, PerDocumentWindow: 1, MaxRetry: 1,
		LeaseDuration: time.Minute, RecoveryInterval: time.Second,
		RecoveryBatchSize: 20, ArchiveMaxParts: 100, FinalizeBatchSize: 10,
	})
	plan, parts := newManagerTestPlan(1)
	created, err := manager.CreatePlan(ctx, plan, parts)
	require.NoError(t, err)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
		 (id, tenant_id, knowledge_base_id, processing_generation, processing_owner, parse_status)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		created.KnowledgeID, created.TenantID, created.KnowledgeBaseID,
		created.ProcessingGeneration, created.ProcessingOwner, types.ParseStatusProcessing,
	).Error)
	require.NoError(t, manager.DispatchPlan(ctx, created.ID))
	claimed, _, err := manager.ClaimPart(ctx, payloadFor(parts[0]))
	require.NoError(t, err)
	require.Equal(t, 1, claimed.Attempt)
	require.NoError(t, db.Model(&Part{}).Where("id = ?", claimed.ID).Update(
		"lease_until", time.Now().Add(-time.Minute),
	).Error)

	require.NoError(t, manager.recoverOnce(ctx))

	var storedPart Part
	require.NoError(t, db.First(&storedPart, "id = ?", claimed.ID).Error)
	require.Equal(t, PartFailed, storedPart.State)
	require.Contains(t, storedPart.LastError, "retry budget")
	var storedPlan Plan
	require.NoError(t, db.First(&storedPlan, "id = ?", created.ID).Error)
	require.Equal(t, PlanFailed, storedPlan.State)
	require.Equal(t, 1, storedPlan.FailedParts)
	var knowledge struct {
		ParseStatus string
		Owner       string `gorm:"column:processing_owner"`
		Error       string `gorm:"column:error_message"`
	}
	require.NoError(t, db.Table("knowledges").Select(
		"parse_status, processing_owner, error_message",
	).Where("id = ?", created.KnowledgeID).Scan(&knowledge).Error)
	require.Equal(t, types.ParseStatusFailed, knowledge.ParseStatus)
	require.Empty(t, knowledge.Owner)
	require.Contains(t, knowledge.Error, "expired")
}

func TestManagerPublishGenerationAtomicallySwapsLogicalChunks(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	manager := NewManagerWithConfig(db, &splitEnqueuerStub{}, Config{})
	old := &types.Chunk{
		ID: "00000000-0000-0000-0000-000000000010", TenantID: 7,
		KnowledgeID: "knowledge", ProcessingGeneration: "old-generation",
		SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeText,
		IsEnabled: true,
	}
	first := &types.Chunk{
		ID: "00000000-0000-0000-0000-000000000011", TenantID: 7,
		KnowledgeID: "knowledge", ProcessingGeneration: "new-generation",
		SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeText,
		IsEnabled: false,
	}
	second := &types.Chunk{
		ID: "00000000-0000-0000-0000-000000000012", TenantID: 7,
		KnowledgeID: "knowledge", ProcessingGeneration: "new-generation",
		SplitPartIndex: 1, ChunkIndex: splitChunkIndexStrideForTest,
		ChunkType: types.ChunkTypeText, IsEnabled: false,
	}
	require.NoError(t, db.Create([]*types.Chunk{old, first, second}).Error)
	parts := []*Part{
		{PartIndex: 0, FirstChunkID: first.ID, LastChunkID: first.ID},
		{PartIndex: 1, FirstChunkID: second.ID, LastChunkID: second.ID},
	}
	require.NoError(t, manager.PublishGeneration(
		ctx, 7, "knowledge", "new-generation", parts,
	))

	var gotOld, gotFirst, gotSecond types.Chunk
	require.NoError(t, db.First(&gotOld, "id = ?", old.ID).Error)
	require.NoError(t, db.First(&gotFirst, "id = ?", first.ID).Error)
	require.NoError(t, db.First(&gotSecond, "id = ?", second.ID).Error)
	require.False(t, gotOld.IsEnabled)
	require.True(t, gotFirst.IsEnabled)
	require.True(t, gotSecond.IsEnabled)
	require.Equal(t, second.ID, gotFirst.NextChunkID)
	require.Equal(t, first.ID, gotSecond.PreChunkID)
}

func TestManagerNormalizesSplitTextIndexesToLogicalDocumentOrder(t *testing.T) {
	ctx := context.Background()
	db := newManagerTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Chunk{}))
	manager := NewManagerWithConfig(db, &splitEnqueuerStub{}, Config{})
	chunks := []*types.Chunk{
		{
			ID: "00000000-0000-0000-0000-000000000021", TenantID: 7,
			KnowledgeID: "knowledge", ProcessingGeneration: "generation",
			SplitPartIndex: 1, ChunkIndex: splitChunkIndexStrideForTest + 9,
			ChunkType: types.ChunkTypeText,
		},
		{
			ID: "00000000-0000-0000-0000-000000000022", TenantID: 7,
			KnowledgeID: "knowledge", ProcessingGeneration: "generation",
			SplitPartIndex: 0, ChunkIndex: 4, ChunkType: types.ChunkTypeText,
		},
		{
			ID: "00000000-0000-0000-0000-000000000023", TenantID: 7,
			KnowledgeID: "knowledge", ProcessingGeneration: "generation",
			SplitPartIndex: 1, ChunkIndex: splitChunkIndexStrideForTest + 2,
			ChunkType: types.ChunkTypeText,
		},
		{
			ID: "00000000-0000-0000-0000-000000000024", TenantID: 7,
			KnowledgeID: "knowledge", ProcessingGeneration: "generation",
			SplitPartIndex: 0, ChunkIndex: 1, ChunkType: types.ChunkTypeText,
		},
		{
			ID: "00000000-0000-0000-0000-000000000025", TenantID: 7,
			KnowledgeID: "knowledge", ProcessingGeneration: "generation",
			SplitPartIndex: 0, ChunkIndex: 0, ChunkType: types.ChunkTypeParentText,
		},
	}
	require.NoError(t, db.Create(chunks).Error)
	require.NoError(t, manager.NormalizeGenerationTextChunkIndexes(
		ctx, 7, "knowledge", "generation",
	))

	var got []*types.Chunk
	require.NoError(t, db.Where(
		"knowledge_id = ? AND chunk_type = ?", "knowledge", types.ChunkTypeText,
	).Order("chunk_index ASC").Find(&got).Error)
	require.Len(t, got, 4)
	require.Equal(t, chunks[3].ID, got[0].ID)
	require.Equal(t, chunks[1].ID, got[1].ID)
	require.Equal(t, chunks[2].ID, got[2].ID)
	require.Equal(t, chunks[0].ID, got[3].ID)
	for index, chunk := range got {
		require.Equal(t, index, chunk.ChunkIndex)
	}

	var parent types.Chunk
	require.NoError(t, db.First(&parent, "id = ?", chunks[4].ID).Error)
	require.Equal(t, 0, parent.ChunkIndex)
}

const splitChunkIndexStrideForTest = 1_000_000
