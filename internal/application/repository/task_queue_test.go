package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// taskPendingOpsTestDDL mirrors the production schema in
// migrations/versioned/000041_task_queue_and_wiki_indexes.up.sql but uses
// SQLite-compatible types. INTEGER PRIMARY KEY AUTOINCREMENT preserves
// the monotonically-increasing ID semantics PeekBatch/cursor pagination
// rely on. JSONB → TEXT is fine since GORM round-trips json.RawMessage
// as bytes either way.
const taskPendingOpsTestDDL = `
CREATE TABLE IF NOT EXISTS task_pending_ops (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER NOT NULL,
    task_type   VARCHAR(64) NOT NULL,
    scope       VARCHAR(32) NOT NULL,
    scope_id    VARCHAR(64) NOT NULL,
    op          VARCHAR(32) NOT NULL,
    dedup_key   VARCHAR(128) NOT NULL DEFAULT '',
    payload     TEXT NOT NULL DEFAULT '{}',
    fail_count  INTEGER NOT NULL DEFAULT 0,
    enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    claimed_at  DATETIME,
    map_ready_at DATETIME,
    next_attempt_at DATETIME,
    map_resource_pool_id VARCHAR(64) NOT NULL DEFAULT '',
    map_dispatch_epoch INTEGER NOT NULL DEFAULT 0,
    map_dispatch_task_id VARCHAR(190) NOT NULL DEFAULT '',
    map_dispatch_lease_until DATETIME
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_task_pending_ops_wiki_ingest
    ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
    WHERE task_type = 'wiki:ingest'
      AND scope = 'knowledge_base'
      AND op = 'ingest';
`

const taskDeadLettersTestDDL = `
CREATE TABLE IF NOT EXISTS task_dead_letters (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    tenant_id   INTEGER NOT NULL,
    task_type   VARCHAR(64) NOT NULL,
    scope       VARCHAR(32) NOT NULL,
    scope_id    VARCHAR(64) NOT NULL,
    related_id  VARCHAR(64) NOT NULL DEFAULT '',
    payload     TEXT NOT NULL,
    last_error  TEXT NOT NULL DEFAULT '',
    fail_count  INTEGER NOT NULL,
    failed_at   DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

const taskQueueKnowledgeBasesTestDDL = `
CREATE TABLE IF NOT EXISTS knowledge_bases (
    id         VARCHAR(64) PRIMARY KEY,
    tenant_id  INTEGER NOT NULL,
    deleted_at DATETIME
);
CREATE TABLE IF NOT EXISTS knowledges (
    id                    VARCHAR(64) PRIMARY KEY,
    tenant_id             INTEGER NOT NULL,
    knowledge_base_id     VARCHAR(64) NOT NULL,
    processing_generation VARCHAR(64) NOT NULL,
    parse_status          VARCHAR(32) NOT NULL,
    processed_at          DATETIME,
    wiki_status           VARCHAR(32) NOT NULL DEFAULT 'pending',
    deleted_at            DATETIME
);
`

func setupTaskQueueTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(taskPendingOpsTestDDL).Error)
	require.NoError(t, db.Exec(taskDeadLettersTestDDL).Error)
	require.NoError(t, db.Exec(taskQueueKnowledgeBasesTestDDL).Error)
	for _, id := range []string{"kb", "kb-1", "kb-A", "kb-B"} {
		require.NoError(t, db.Exec(
			"INSERT INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", id, 1,
		).Error)
	}
	return db
}

func makePendingOp(taskType, scope, scopeID, op, dedup string, payload []byte) *types.TaskPendingOp {
	return &types.TaskPendingOp{
		TenantID: 1,
		TaskType: taskType,
		Scope:    scope,
		ScopeID:  scopeID,
		Op:       op,
		DedupKey: dedup,
		Payload:  payload,
	}
}

func TestTaskPendingOps_DistributedWikiMapPublishesReadyAtomicallyAndBypassesSlowHead(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db).(*taskPendingOpsRepository)
	ctx := context.Background()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
		 (id, tenant_id, knowledge_base_id, processing_generation, parse_status, processed_at, wiki_status)
		 VALUES
		 ('slow', 1, 'kb-1', 'g-slow', 'completed', CURRENT_TIMESTAMP, 'pending'),
		 ('ready', 1, 'kb-1', 'g-ready', 'completed', CURRENT_TIMESTAMP, 'pending')`,
	).Error)

	slowPayload := []byte(`{"op":"ingest","knowledge_id":"slow","processing_generation":"g-slow"}`)
	readyPayload := []byte(`{"op":"ingest","knowledge_id":"ready","processing_generation":"g-ready","prepared":{"doc_title":"Ready","summary":"ok","pages":[],"updates":[]}}`)
	require.NoError(t, repo.Enqueue(ctx, makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest", "slow:g-slow", slowPayload,
	)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest", "ready:g-ready", readyPayload,
	)))

	row, err := repo.GetWikiIngestByDedupKey(ctx, 1, "kb-1", "ready:g-ready")
	require.NoError(t, err)
	require.NotNil(t, row)
	validationCtx := wikiingestguard.WithValidation(ctx, wikiingestguard.Identity{
		TenantID: 1, KnowledgeBaseID: "kb-1",
		KnowledgeID: "ready", ProcessingGeneration: "g-ready",
	})
	updated, err := repo.MarkWikiMapReady(
		validationCtx, row.ID, 1, "kb-1", readyPayload,
	)
	require.NoError(t, err)
	require.True(t, updated)

	batch, err := repo.PeekWikiCommitBatch(ctx, 1, "kb-1", 10)
	require.NoError(t, err)
	require.Len(t, batch, 1)
	assert.Equal(t, "ready:g-ready", batch[0].DedupKey,
		"an unprepared FIFO head must not block later ready documents")
	require.NotNil(t, batch[0].MapReadyAt)
	count, err := repo.CountWikiCommitReady(ctx, 1, "kb-1")
	require.NoError(t, err)
	assert.EqualValues(t, 1, count)
}

func TestTaskPendingOps_WikiMapDispatchClaimIsEpochAndTaskFenced(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db).(*taskPendingOpsRepository)
	ctx := context.Background()
	payload := []byte(`{"op":"ingest","knowledge_id":"ready","processing_generation":"g-ready"}`)
	op := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1",
		"ingest", "ready:g-ready", payload,
	)
	require.NoError(t, repo.Enqueue(ctx, op))
	require.NoError(t, db.Table("task_pending_ops").Where("id = ?", op.ID).Updates(map[string]any{
		"map_resource_pool_id":     "pool-a",
		"map_dispatch_epoch":       3,
		"map_dispatch_task_id":     fmt.Sprintf("wiki-map:%d:3", op.ID),
		"map_dispatch_lease_until": time.Now().UTC().Add(time.Minute),
	}).Error)
	var dispatched types.TaskPendingOp
	require.NoError(t, db.First(&dispatched, "id = ?", op.ID).Error)
	require.Equal(t, fmt.Sprintf("wiki-map:%d:3", op.ID), dispatched.MapDispatchTaskID)
	require.EqualValues(t, 3, dispatched.MapDispatchEpoch)
	var claimable int64
	require.NoError(t, db.Table("task_pending_ops").Where(
		"id = ? AND map_dispatch_epoch = ? AND map_dispatch_task_id = ? AND map_ready_at IS NULL",
		op.ID, 3, fmt.Sprintf("wiki-map:%d:3", op.ID),
	).Count(&claimable).Error)
	require.EqualValues(t, 1, claimable)

	claimed, err := repo.ClaimWikiMapDispatch(
		ctx, 1, "kb-1", "ready:g-ready", 3, 2*time.Minute,
	)
	require.NoError(t, err)
	require.NotNil(t, claimed)
	assert.EqualValues(t, 3, claimed.MapDispatchEpoch)
	assert.Equal(t, fmt.Sprintf("wiki-map:%d:3", op.ID), claimed.MapDispatchTaskID)
	stale, err := repo.ClaimWikiMapDispatch(
		ctx, 1, "kb-1", "ready:g-ready", 2, time.Minute,
	)
	require.NoError(t, err)
	require.Nil(t, stale)

	renewed, err := repo.RenewWikiMapDispatch(ctx, op.ID, 3, time.Minute)
	require.NoError(t, err)
	require.True(t, renewed)
	require.NoError(t, repo.DeferWikiMapDispatch(ctx, op.ID, 3, time.Minute))

	var deferred types.TaskPendingOp
	require.NoError(t, db.First(&deferred, "id = ?", op.ID).Error)
	require.NotNil(t, deferred.NextAttemptAt)
	assert.Empty(t, deferred.MapDispatchTaskID)
	assert.Nil(t, deferred.MapDispatchLeaseUntil)
	assert.EqualValues(t, 0, deferred.FailCount)

	obsolete, err := repo.ClaimWikiMapDispatch(
		ctx, 1, "kb-1", "ready:g-ready", 3, time.Minute,
	)
	require.NoError(t, err)
	require.Nil(t, obsolete,
		"clearing the durable task credential must invalidate an old Redis copy even before the epoch advances")
}

// ---------------- TaskPendingOpsRepository ----------------

// TestTaskPendingOps_Enqueue_AssignsIDAndDefaults verifies a freshly
// inserted op gets a positive ID and the empty payload becomes "{}"
// rather than NULL/empty.
func TestTaskPendingOps_Enqueue_AssignsIDAndDefaults(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb-1", "ingest", "k-1", nil)
	require.NoError(t, repo.Enqueue(ctx, op))
	assert.NotZero(t, op.ID)
	assert.Equal(t, json.RawMessage("{}"), op.Payload, "nil payload should default to {}")
	assert.False(t, op.EnqueuedAt.IsZero(), "repository must not persist Go's year-0001 zero time")
	assert.Equal(t, time.UTC, op.EnqueuedAt.Location())
	assert.WithinDuration(t, time.Now().UTC(), op.EnqueuedAt, time.Second)

	var persisted time.Time
	require.NoError(t, db.Table("task_pending_ops").
		Select("enqueued_at").Where("id = ?", op.ID).Scan(&persisted).Error)
	assert.WithinDuration(t, op.EnqueuedAt, persisted, time.Second)
}

func TestTaskPendingOps_EnqueuePreservesExplicitTimestamp(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	want := time.Date(2025, time.January, 2, 3, 4, 5, 0, time.UTC)
	op := makePendingOp("wiki:ingest", "knowledge_base", "kb-1", "ingest", "k-1", nil)
	op.EnqueuedAt = want

	require.NoError(t, repo.Enqueue(context.Background(), op))
	assert.Equal(t, want, op.EnqueuedAt)
	var persisted time.Time
	require.NoError(t, db.Table("task_pending_ops").
		Select("enqueued_at").Where("id = ?", op.ID).Scan(&persisted).Error)
	assert.True(t, persisted.Equal(want), "persisted timestamp = %s, want %s", persisted, want)
}

func TestTaskPendingOps_WikiIngestReplayPreservesCanonicalCheckpoint(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()
	dedupKey := "knowledge-1:generation-1"
	checkpoint := []byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1","prepared":{"doc_title":"kept"}}`)

	canonical := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest", dedupKey, checkpoint,
	)
	require.NoError(t, repo.Enqueue(ctx, canonical))
	require.NotZero(t, canonical.ID)

	replay := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest", dedupKey,
		[]byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1"}`),
	)
	require.NoError(t, repo.Enqueue(ctx, replay))

	var rows []types.TaskPendingOp
	require.NoError(t, db.Find(&rows).Error)
	require.Len(t, rows, 1)
	assert.Equal(t, canonical.ID, rows[0].ID)
	assert.JSONEq(t, string(checkpoint), string(rows[0].Payload),
		"a plain recovery replay must not erase a durable Map checkpoint")
}

func TestTaskPendingOps_WikiIngestSettledGenerationCannotBeRecreated(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
		 (id, tenant_id, knowledge_base_id, processing_generation, parse_status, processed_at, wiki_status)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"knowledge-1", 1, "kb-1", "generation-1",
		types.ParseStatusFinalizing, now, types.WikiStatusCompleted,
	).Error)
	identity := wikiingestguard.Identity{
		TenantID: 1, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	op := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest",
		"knowledge-1:generation-1",
		[]byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1"}`),
	)

	err := repo.Enqueue(wikiingestguard.WithValidation(context.Background(), identity), op)
	require.Equal(t, []wikiingestguard.Identity{identity}, wikiingestguard.StaleIdentities(err))
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&count).Error)
	require.Zero(t, count)
}

func TestTaskPendingOps_WikiIngestConcurrentReplayCreatesOneRow(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// This fixture uses a private in-memory SQLite database. One connection
	// keeps every goroutine on that schema while still exercising concurrent
	// repository callers and the database uniqueness boundary.
	sqlDB.SetMaxOpenConns(1)
	repo := NewTaskPendingOpsRepository(db)

	const producers = 32
	start := make(chan struct{})
	errs := make(chan error, producers)
	var wg sync.WaitGroup
	for i := 0; i < producers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs <- repo.Enqueue(context.Background(), makePendingOp(
				types.TypeWikiIngest,
				types.TaskScopeKnowledgeBase,
				"kb-1",
				"ingest",
				"knowledge-1:generation-1",
				[]byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1"}`),
			))
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for enqueueErr := range errs {
		require.NoError(t, enqueueErr)
	}

	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&count).Error)
	assert.Equal(t, int64(1), count)
}

func TestTaskPendingOps_WikiEnqueueRejectsTombstonedKnowledgeBase(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	require.NoError(t, db.Exec(
		"UPDATE knowledge_bases SET deleted_at = ? WHERE id = ? AND tenant_id = ?",
		time.Now().UTC(), "kb-1", 1,
	).Error)

	err := repo.Enqueue(context.Background(),
		makePendingOp(types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest", "late", nil))
	require.ErrorIs(t, err, kbwritefence.ErrKnowledgeBaseUnavailable)
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).Count(&count).Error)
	assert.Zero(t, count)
}

func TestTaskPendingOps_UpdateWikiPayloadIsGenerationFenced(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	require.NoError(t, db.AutoMigrate(&types.Knowledge{}))
	processedAt := time.Now().UTC()
	require.NoError(t, db.Create(&types.Knowledge{
		ID: "knowledge-1", TenantID: 1, KnowledgeBaseID: "kb-1",
		ProcessingGeneration: "generation-1", ParseStatus: types.ParseStatusCompleted,
		ProcessedAt: &processedAt,
	}).Error)
	repo := NewTaskPendingOpsRepository(db)
	pending := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-1", "ingest",
		"knowledge-1:generation-1",
		[]byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1"}`),
	)
	require.NoError(t, repo.Enqueue(context.Background(), pending))
	identity := wikiingestguard.Identity{
		TenantID: 1, KnowledgeBaseID: "kb-1", KnowledgeID: "knowledge-1",
		ProcessingGeneration: "generation-1",
	}
	guardedCtx := wikiingestguard.WithValidation(context.Background(), identity)
	checkpointer := repo.(*taskPendingOpsRepository)
	prepared := []byte(`{"op":"ingest","knowledge_id":"knowledge-1","processing_generation":"generation-1","prepared":{"doc_title":"kept"}}`)

	updated, err := checkpointer.UpdateWikiPayload(guardedCtx, pending.ID, 1, "kb-1", prepared)
	require.NoError(t, err)
	assert.True(t, updated)

	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", identity.KnowledgeID).
		Update("processing_generation", "generation-2").Error)
	stalePayload := []byte(`{"must_not":"commit"}`)
	updated, err = checkpointer.UpdateWikiPayload(guardedCtx, pending.ID, 1, "kb-1", stalePayload)
	assert.False(t, updated)
	require.NotEmpty(t, wikiingestguard.StaleIdentities(err))

	var stored types.TaskPendingOp
	require.NoError(t, db.First(&stored, pending.ID).Error)
	assert.JSONEq(t, string(prepared), string(stored.Payload))
}

// TestTaskPendingOps_Enqueue_RejectsMissingFields covers the validation
// layer: every required field must be set, otherwise the call returns an
// error WITHOUT touching the DB.
func TestTaskPendingOps_Enqueue_RejectsMissingFields(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	cases := []struct {
		name string
		op   *types.TaskPendingOp
	}{
		{"nil op", nil},
		{"missing task_type", makePendingOp("", "knowledge_base", "kb", "ingest", "", nil)},
		{"missing scope", makePendingOp("t", "", "kb", "ingest", "", nil)},
		{"missing scope_id", makePendingOp("t", "s", "", "ingest", "", nil)},
		{"missing op", makePendingOp("t", "s", "id", "", "", nil)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := repo.Enqueue(ctx, c.op)
			assert.Error(t, err)
		})
	}

	var n int64
	db.Table("task_pending_ops").Count(&n)
	assert.Equal(t, int64(0), n)
}

// TestTaskPendingOps_PeekBatch_ScopedAndOrdered verifies PeekBatch only
// returns rows for the matching tuple, in id ASC order, and respects
// the limit.
func TestTaskPendingOps_PeekBatch_ScopedAndOrdered(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	// Three ops in kb-A, two in kb-B, one in different task_type.
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-A", "ingest", "k1", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-A", "retract", "k2", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-A", "ingest", "k3", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-B", "ingest", "k4", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-B", "ingest", "k5", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("summary:gen", "knowledge_base", "kb-A", "ingest", "k6", nil)))

	// Peek up to 10 from kb-A — should see exactly 3, in insertion order.
	got, err := repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb-A", 10)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, "k1", got[0].DedupKey)
	assert.Equal(t, "k2", got[1].DedupKey)
	assert.Equal(t, "k3", got[2].DedupKey)
	assert.True(t, got[0].ID < got[1].ID && got[1].ID < got[2].ID, "ids should be ascending")

	// Limit caps result size.
	got, err = repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb-A", 2)
	require.NoError(t, err)
	assert.Len(t, got, 2)

	// Different task_type isolated.
	got, err = repo.PeekBatch(ctx, "summary:gen", "knowledge_base", "kb-A", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "k6", got[0].DedupKey)
}

func TestTaskPendingOps_WikiRetryRotationBypassesPoisonedHead(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db).(*taskPendingOpsRepository)
	ctx := context.Background()

	poisoned := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "retract", "poisoned", nil,
	)
	attempted := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "retract", "attempted", nil,
	)
	fresh := makePendingOp(
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "retract", "fresh", nil,
	)
	require.NoError(t, repo.Enqueue(ctx, poisoned))
	require.NoError(t, repo.Enqueue(ctx, attempted))
	require.NoError(t, repo.Enqueue(ctx, fresh))
	oldAttempt := time.Now().UTC().Add(-time.Hour)
	recentAttempt := time.Now().UTC().Add(-time.Minute)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("id = ?", poisoned.ID).
		Updates(map[string]any{"fail_count": 2, "claimed_at": oldAttempt}).Error)
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("id = ?", attempted.ID).
		Update("claimed_at", recentAttempt).Error)

	got, err := repo.PeekBatch(
		ctx, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", 10,
	)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"fresh", "attempted", "poisoned"}, []string{
		got[0].DedupKey, got[1].DedupKey, got[2].DedupKey,
	})

	commitReady, err := repo.PeekWikiCommitBatch(ctx, 1, "kb-A", 10)
	require.NoError(t, err)
	require.Len(t, commitReady, 3)
	assert.Equal(t, []string{"fresh", "attempted", "poisoned"}, []string{
		commitReady[0].DedupKey, commitReady[1].DedupKey, commitReady[2].DedupKey,
	})

	require.NoError(t, repo.TouchWikiAttempt(ctx, fresh.ID))
	var touched types.TaskPendingOp
	require.NoError(t, db.First(&touched, fresh.ID).Error)
	require.NotNil(t, touched.ClaimedAt)
	assert.WithinDuration(t, time.Now().UTC(), touched.ClaimedAt.UTC(), time.Second)

	got, err = repo.PeekBatch(
		ctx, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", 10,
	)
	require.NoError(t, err)
	require.Len(t, got, 3)
	assert.Equal(t, []string{"attempted", "fresh", "poisoned"}, []string{
		got[0].DedupKey, got[1].DedupKey, got[2].DedupKey,
	})
}

// TestTaskPendingOps_DeleteByIDs_RemovesOnlyTargets verifies the
// delete-after-consume path. Empty input must be a no-op so the consumer
// can call it unconditionally.
func TestTaskPendingOps_DeleteByIDs_RemovesOnlyTargets(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	a := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "a", nil)
	b := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "b", nil)
	c := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "c", nil)
	require.NoError(t, repo.Enqueue(ctx, a))
	require.NoError(t, repo.Enqueue(ctx, b))
	require.NoError(t, repo.Enqueue(ctx, c))

	// No-op: empty slice.
	require.NoError(t, repo.DeleteByIDs(ctx, nil))
	require.NoError(t, repo.DeleteByIDs(ctx, []int64{}))

	// Delete a + c, keep b.
	require.NoError(t, repo.DeleteByIDs(ctx, []int64{a.ID, c.ID}))

	got, err := repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb", 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, "b", got[0].DedupKey)
}

// TestTaskPendingOps_ArchiveToDeadLetter_AtomicMove verifies the happy
// path removes the pending row and creates exactly one archive record in
// the same repository operation.
func TestTaskPendingOps_ArchiveToDeadLetter_AtomicMove(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k1", nil)
	op.FailCount = 5
	require.NoError(t, repo.Enqueue(ctx, op))
	dl := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k1", "retry budget exhausted")

	require.NoError(t, repo.ArchiveToDeadLetter(ctx, op.ID, dl))
	assert.NotZero(t, dl.ID)
	assert.False(t, dl.FailedAt.IsZero(), "atomic archive must stamp an operator-meaningful failure time")

	var pendingCount, deadLetterCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&pendingCount).Error)
	require.NoError(t, db.Table("task_dead_letters").Count(&deadLetterCount).Error)
	assert.Equal(t, int64(0), pendingCount)
	assert.Equal(t, int64(1), deadLetterCount)
}

// TestTaskPendingOps_ArchiveToDeadLetter_InsertFailureRollsBackDelete
// forces the archive INSERT to fail and proves the pending op remains
// available for a later retry.
func TestTaskPendingOps_ArchiveToDeadLetter_InsertFailureRollsBackDelete(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k1", nil)
	op.FailCount = 5
	require.NoError(t, repo.Enqueue(ctx, op))
	require.NoError(t, db.Exec(`
		CREATE TRIGGER reject_task_dead_letter_insert
		BEFORE INSERT ON task_dead_letters
		BEGIN
			SELECT RAISE(ABORT, 'forced archive insert failure');
		END;
	`).Error)

	err := repo.ArchiveToDeadLetter(ctx, op.ID,
		makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k1", "boom"))
	require.Error(t, err)

	var pendingCount, deadLetterCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&pendingCount).Error)
	require.NoError(t, db.Table("task_dead_letters").Count(&deadLetterCount).Error)
	assert.Equal(t, int64(1), pendingCount, "failed archive insert must roll back the delete")
	assert.Equal(t, int64(0), deadLetterCount)
}

// TestTaskPendingOps_ArchiveToDeadLetter_MissingIDIsNoOp proves an
// already-consumed/unknown pending ID does not create an orphan archive.
func TestTaskPendingOps_ArchiveToDeadLetter_MissingIDIsNoOp(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()
	dl := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "missing", "boom")

	require.NoError(t, repo.ArchiveToDeadLetter(ctx, 99999, dl))
	assert.Zero(t, dl.ID)

	var deadLetterCount int64
	require.NoError(t, db.Table("task_dead_letters").Count(&deadLetterCount).Error)
	assert.Equal(t, int64(0), deadLetterCount)
}

// TestTaskPendingOps_ArchiveToDeadLetter_RepeatedCallIsIdempotent
// simulates two consumers racing on the same pending ID: only the first
// call may write an archive row.
func TestTaskPendingOps_ArchiveToDeadLetter_RepeatedCallIsIdempotent(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k1", nil)
	op.FailCount = 5
	require.NoError(t, repo.Enqueue(ctx, op))
	first := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k1", "first")
	second := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k1", "second")

	require.NoError(t, repo.ArchiveToDeadLetter(ctx, op.ID, first))
	require.NoError(t, repo.ArchiveToDeadLetter(ctx, op.ID, second))
	assert.NotZero(t, first.ID)
	assert.Zero(t, second.ID)

	var pendingCount, deadLetterCount int64
	require.NoError(t, db.Table("task_pending_ops").Count(&pendingCount).Error)
	require.NoError(t, db.Table("task_dead_letters").Count(&deadLetterCount).Error)
	assert.Equal(t, int64(0), pendingCount)
	assert.Equal(t, int64(1), deadLetterCount)
}

// TestTaskPendingOps_ArchiveToDeadLetter_StaleFailureCountPreservesRenewedRow
// models the delete race: an old worker observes an exhausted retry count,
// then the delete coordinator renews the same retract and resets fail_count.
// The stale worker must not delete or archive that renewed intent.
func TestTaskPendingOps_ArchiveToDeadLetter_StaleFailureCountPreservesRenewedRow(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb", "retract", "k1", nil)
	op.FailCount = 5
	require.NoError(t, repo.Enqueue(ctx, op))
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("id = ?", op.ID).
		Update("fail_count", 0).Error)

	stale := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k1", "stale worker")
	require.Equal(t, 5, stale.FailCount)
	require.NoError(t, repo.ArchiveToDeadLetter(ctx, op.ID, stale))
	assert.Zero(t, stale.ID)

	var persisted types.TaskPendingOp
	require.NoError(t, db.First(&persisted, op.ID).Error)
	assert.Equal(t, 0, persisted.FailCount, "the coordinator-renewed retract must survive")
	var deadLetterCount int64
	require.NoError(t, db.Table("task_dead_letters").Count(&deadLetterCount).Error)
	assert.Equal(t, int64(0), deadLetterCount)
}

// TestTaskPendingOps_IncrFailCount_ReturnsNewValueAndPersists exercises
// the UPDATE...RETURNING flow. Successive bumps should observe monotonic
// counts.
func TestTaskPendingOps_IncrFailCount_ReturnsNewValueAndPersists(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	op := makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k", nil)
	require.NoError(t, repo.Enqueue(ctx, op))

	n, err := repo.IncrFailCount(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	n, err = repo.IncrFailCount(ctx, op.ID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	// Persisted value matches what was returned.
	rows, err := repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, 2, rows[0].FailCount)
	require.NotNil(t, rows[0].ClaimedAt)
	assert.WithinDuration(t, time.Now().UTC(), rows[0].ClaimedAt.UTC(), time.Second)
}

// TestTaskPendingOps_PendingCount_ScopedTuple confirms the count covers
// only the (task_type, scope, scope_id) tuple.
func TestTaskPendingOps_PendingCount_ScopedTuple(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-A", "ingest", "k1", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-A", "ingest", "k2", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb-B", "ingest", "k3", nil)))

	n, err := repo.PendingCount(ctx, "wiki:ingest", "knowledge_base", "kb-A")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n)

	n, err = repo.PendingCount(ctx, "wiki:ingest", "knowledge_base", "missing")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

// TestTaskPendingOps_ExistsByDedupKey_ExactTuple exercises the indexed
// per-object lookup used by task inspection. The knowledge ID alone is not
// sufficient: task type, scope, KB, and operation must all match so a retract
// or an unrelated KB cannot keep a document in finalizing forever.
func TestTaskPendingOps_ExistsByDedupKey_ExactTuple(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	require.NoError(t, repo.Enqueue(ctx,
		makePendingOp(types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "ingest", "k1", nil)))
	require.NoError(t, repo.Enqueue(ctx,
		makePendingOp(types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "retract", "k2", nil)))
	require.NoError(t, repo.Enqueue(ctx,
		makePendingOp(types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-B", "ingest", "k3", nil)))

	exists, err := repo.ExistsByDedupKey(ctx,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "k1", "ingest")
	require.NoError(t, err)
	assert.True(t, exists)

	exists, err = repo.ExistsByDedupKey(ctx,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "k2", "ingest")
	require.NoError(t, err)
	assert.False(t, exists, "a retract must not masquerade as pending wiki enrichment")

	exists, err = repo.ExistsByDedupKey(ctx,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "k3", "ingest")
	require.NoError(t, err)
	assert.False(t, exists, "a matching key in a different KB must remain isolated")

	exists, err = repo.ExistsByDedupKey(ctx,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "k2", "")
	require.NoError(t, err)
	assert.True(t, exists, "empty op intentionally means any operation kind")

	_, err = repo.ExistsByDedupKey(ctx,
		types.TypeWikiIngest, types.TaskScopeKnowledgeBase, "kb-A", "", "ingest")
	assert.Error(t, err, "empty dedup key must be rejected")
}

func TestTaskPendingOps_DedupPrefixScopesWikiGenerations(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()
	for _, row := range []*types.TaskPendingOp{
		{TenantID: 1, TaskType: "wiki:ingest", Scope: "knowledge_base", ScopeID: "kb", Op: "ingest", DedupKey: "kid:generation-1"},
		{TenantID: 1, TaskType: "wiki:ingest", Scope: "knowledge_base", ScopeID: "kb", Op: "ingest", DedupKey: "kid:generation-2"},
		{TenantID: 1, TaskType: "wiki:ingest", Scope: "knowledge_base", ScopeID: "kb", Op: "retract", DedupKey: "kid"},
		{TenantID: 1, TaskType: "wiki:ingest", Scope: "knowledge_base", ScopeID: "kb", Op: "ingest", DedupKey: "kid-other:generation-1"},
	} {
		require.NoError(t, repo.Enqueue(ctx, row))
	}

	exists, err := repo.ExistsByDedupKeyPrefix(ctx, "wiki:ingest", "knowledge_base", "kb", "kid:", "ingest")
	require.NoError(t, err)
	require.True(t, exists)

	require.NoError(t, repo.DeleteByDedupKeyPrefix(ctx, "wiki:ingest", "knowledge_base", "kb", "kid:", "ingest"))
	exists, err = repo.ExistsByDedupKeyPrefix(ctx, "wiki:ingest", "knowledge_base", "kb", "kid:", "ingest")
	require.NoError(t, err)
	require.False(t, exists)

	var remaining []types.TaskPendingOp
	require.NoError(t, db.Order("id").Find(&remaining).Error)
	require.Len(t, remaining, 2)
	assert.Equal(t, "retract", remaining[0].Op)
	assert.Equal(t, "kid-other:generation-1", remaining[1].DedupKey)
}

// TestTaskPendingOps_DeleteByDedupKey_Filters tests the wiki delete-race
// helper: matching rows go away, others survive, optional op filter
// narrows the scope, and an empty dedup_key is rejected (so a buggy
// caller can't wipe the entire queue).
func TestTaskPendingOps_DeleteByDedupKey_Filters(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskPendingOpsRepository(db)
	ctx := context.Background()

	// One ingest + one retract, both keyed on knowledge "k1"; one ingest
	// for unrelated "k2". A second identical ingest cannot exist because the
	// durable Wiki generation index now collapses it at INSERT time.
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k1", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb", "retract", "k1", nil)))
	require.NoError(t, repo.Enqueue(ctx, makePendingOp("wiki:ingest", "knowledge_base", "kb", "ingest", "k2", nil)))

	// Empty key is an error, queue unchanged.
	err := repo.DeleteByDedupKey(ctx, "wiki:ingest", "knowledge_base", "kb", "", "")
	assert.Error(t, err)
	n, _ := repo.PendingCount(ctx, "wiki:ingest", "knowledge_base", "kb")
	assert.Equal(t, int64(3), n)

	// Drop only "ingest" rows for k1; retract survives.
	require.NoError(t, repo.DeleteByDedupKey(ctx, "wiki:ingest", "knowledge_base", "kb", "k1", "ingest"))
	rows, err := repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	// The two survivors must be the retract for k1 and the ingest for k2.
	keys := map[string]string{}
	for _, r := range rows {
		keys[r.Op] = r.DedupKey
	}
	assert.Equal(t, "k1", keys["retract"])
	assert.Equal(t, "k2", keys["ingest"])

	// Drop everything keyed on k1 regardless of op (empty op = wildcard).
	require.NoError(t, repo.DeleteByDedupKey(ctx, "wiki:ingest", "knowledge_base", "kb", "k1", ""))
	rows, err = repo.PeekBatch(ctx, "wiki:ingest", "knowledge_base", "kb", 10)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "k2", rows[0].DedupKey)
}

// ---------------- TaskDeadLetterRepository ----------------

func makeDeadLetter(taskType, scope, scopeID, relatedID, lastErr string) *types.TaskDeadLetter {
	return &types.TaskDeadLetter{
		TenantID:  1,
		TaskType:  taskType,
		Scope:     scope,
		ScopeID:   scopeID,
		RelatedID: relatedID,
		Payload:   json.RawMessage(`{"x":1}`),
		LastError: lastErr,
		FailCount: 5,
	}
}

// TestTaskDeadLetter_Insert_DefaultsAndAssignsID covers the empty-payload
// fallback and ID assignment.
func TestTaskDeadLetter_Insert_DefaultsAndAssignsID(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	dl := &types.TaskDeadLetter{
		TenantID:  1,
		TaskType:  "wiki:ingest",
		ScopeID:   "kb",
		FailCount: 3,
		// Scope intentionally empty — should default to "unknown".
		// Payload intentionally nil — should default to "{}".
	}
	require.NoError(t, repo.Insert(ctx, dl))
	assert.NotZero(t, dl.ID)
	assert.Equal(t, types.TaskScopeUnknown, dl.Scope)
	assert.Equal(t, json.RawMessage("{}"), dl.Payload)
	assert.False(t, dl.FailedAt.IsZero(), "direct dead-letter inserts must default failed_at")
}

// TestTaskDeadLetter_Insert_RejectsMissingFields verifies the guard
// against rows that would leave the table without the columns ops queries
// rely on.
func TestTaskDeadLetter_Insert_RejectsMissingFields(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	assert.Error(t, repo.Insert(ctx, nil))
	assert.Error(t, repo.Insert(ctx, &types.TaskDeadLetter{ScopeID: "kb"}))

	var n int64
	db.Table("task_dead_letters").Count(&n)
	assert.Equal(t, int64(0), n)
}

// TestTaskDeadLetter_ListByScope_NewestFirstAndCursored exercises the
// cursor pagination path used by the ops console.
func TestTaskDeadLetter_ListByScope_NewestFirstAndCursored(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	// Insert 5 rows for kb-A and 2 for kb-B.
	for i := 0; i < 5; i++ {
		require.NoError(t, repo.Insert(ctx, makeDeadLetter("wiki:ingest", "knowledge_base", "kb-A", "k", "boom")))
	}
	require.NoError(t, repo.Insert(ctx, makeDeadLetter("wiki:ingest", "knowledge_base", "kb-B", "k", "boom")))
	require.NoError(t, repo.Insert(ctx, makeDeadLetter("wiki:ingest", "knowledge_base", "kb-B", "k", "boom")))

	// First page of 2 from kb-A, newest first.
	page1, cursor, err := repo.ListByScope(ctx, "knowledge_base", "kb-A", "", 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	assert.True(t, page1[0].ID > page1[1].ID, "newest first")
	require.NotEmpty(t, cursor)

	// Second page of 2.
	page2, cursor, err := repo.ListByScope(ctx, "knowledge_base", "kb-A", cursor, 2)
	require.NoError(t, err)
	require.Len(t, page2, 2)
	assert.True(t, page1[1].ID > page2[0].ID, "page2 should continue past page1")
	require.NotEmpty(t, cursor)

	// Last page — only 1 row left, cursor goes empty since len < limit.
	page3, cursor, err := repo.ListByScope(ctx, "knowledge_base", "kb-A", cursor, 2)
	require.NoError(t, err)
	require.Len(t, page3, 1)
	assert.Empty(t, cursor)

	// kb-B is isolated.
	pageB, _, err := repo.ListByScope(ctx, "knowledge_base", "kb-B", "", 10)
	require.NoError(t, err)
	require.Len(t, pageB, 2)
}

// TestTaskDeadLetter_ListByScope_RejectsMissingScope guards the input
// validation in the public method.
func TestTaskDeadLetter_ListByScope_RejectsMissingScope(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	_, _, err := repo.ListByScope(ctx, "", "kb", "", 10)
	assert.Error(t, err)
	_, _, err = repo.ListByScope(ctx, "knowledge_base", "", "", 10)
	assert.Error(t, err)
}

// TestTaskDeadLetter_ListByTaskType_FiltersAndPaginates is the cross-KB
// view: "all summary:generation failures" regardless of which KB they
// belong to.
func TestTaskDeadLetter_ListByTaskType_FiltersAndPaginates(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	require.NoError(t, repo.Insert(ctx, makeDeadLetter("wiki:ingest", "knowledge_base", "kb-A", "k1", "")))
	require.NoError(t, repo.Insert(ctx, makeDeadLetter("summary:gen", "knowledge_base", "kb-A", "k2", "")))
	require.NoError(t, repo.Insert(ctx, makeDeadLetter("summary:gen", "knowledge_base", "kb-B", "k3", "")))
	require.NoError(t, repo.Insert(ctx, makeDeadLetter("wiki:ingest", "knowledge_base", "kb-B", "k4", "")))

	rows, _, err := repo.ListByTaskType(ctx, "summary:gen", "", 10)
	require.NoError(t, err)
	require.Len(t, rows, 2)
	for _, r := range rows {
		assert.Equal(t, "summary:gen", r.TaskType)
	}

	_, _, err = repo.ListByTaskType(ctx, "", "", 10)
	assert.Error(t, err)
}

// TestTaskDeadLetter_DeleteByID_IsIdempotent confirms a missing row does
// not produce an error — operators triggering concurrent deletes should
// see clean success.
func TestTaskDeadLetter_DeleteByID_IsIdempotent(t *testing.T) {
	db := setupTaskQueueTestDB(t)
	repo := NewTaskDeadLetterRepository(db)
	ctx := context.Background()

	dl := makeDeadLetter("wiki:ingest", "knowledge_base", "kb", "k", "")
	require.NoError(t, repo.Insert(ctx, dl))

	require.NoError(t, repo.DeleteByID(ctx, dl.ID))
	// Second delete on the same id should silently succeed.
	require.NoError(t, repo.DeleteByID(ctx, dl.ID))
	// Delete of unknown id should silently succeed.
	require.NoError(t, repo.DeleteByID(ctx, 99999))

	rows, _, err := repo.ListByScope(ctx, "knowledge_base", "kb", "", 10)
	require.NoError(t, err)
	assert.Len(t, rows, 0)
}
