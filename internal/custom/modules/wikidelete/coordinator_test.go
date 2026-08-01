package wikidelete

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newCoordinatorDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:wikidelete-%s?mode=memory&cache=shared", t.Name())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
			id TEXT PRIMARY KEY,
			tenant_id INTEGER NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE knowledges (
            id TEXT PRIMARY KEY,
            tenant_id INTEGER NOT NULL,
			 knowledge_base_id TEXT NOT NULL,
			 parse_status TEXT NOT NULL,
			processing_generation TEXT NOT NULL DEFAULT '',
			processing_owner TEXT NOT NULL DEFAULT '',
			processing_workflow_id TEXT NOT NULL DEFAULT '',
			processed_at DATETIME,
			error_message TEXT NOT NULL DEFAULT '',
			storage_size INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME,
            deleted_at DATETIME
        )`,
		`CREATE TABLE tenants (
			id INTEGER PRIMARY KEY,
			storage_used INTEGER NOT NULL DEFAULT 0,
			deleted_at DATETIME
		)`,
		`CREATE TABLE knowledge_tag_relations (
			knowledge_id TEXT NOT NULL,
			tag_id TEXT NOT NULL
		)`,
		`CREATE TABLE knowledge_fanout_completions (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL,
			knowledge_base_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL,
			item_id TEXT NOT NULL,
			completed_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id)
		)`,
		`CREATE TABLE custom_enrichment_outcomes (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_generated_question_claims (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_processing_spans_v2 (
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_document_split_plans (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_document_split_parts (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE wiki_log_entries (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_content_cache_entries (
			tenant_id INTEGER NOT NULL,
			cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			version_hash TEXT NOT NULL,
			ref_count INTEGER NOT NULL DEFAULT 0,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (tenant_id, cache_kind, content_hash, version_hash)
		)`,
		`CREATE TABLE custom_content_cache_refs (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL,
			cache_kind TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			version_hash TEXT NOT NULL
		)`,
		`CREATE TABLE embeddings (
			knowledge_id TEXT NOT NULL,
			knowledge_base_id TEXT,
			source_id TEXT,
			content TEXT NOT NULL DEFAULT '',
			deleted_at DATETIME
		)`,
		`CREATE TABLE chunks (
			tenant_id INTEGER NOT NULL,
			knowledge_id TEXT NOT NULL,
			content TEXT NOT NULL DEFAULT '',
			deleted_at DATETIME
		)`,
		`CREATE TABLE task_pending_ops (
            id INTEGER PRIMARY KEY AUTOINCREMENT,
            tenant_id INTEGER NOT NULL,
            task_type TEXT NOT NULL,
            scope TEXT NOT NULL,
            scope_id TEXT NOT NULL,
            op TEXT NOT NULL,
            dedup_key TEXT NOT NULL DEFAULT '',
            payload JSON NOT NULL DEFAULT '{}',
            fail_count INTEGER NOT NULL DEFAULT 0,
            enqueued_at DATETIME NOT NULL,
            claimed_at DATETIME,
			CONSTRAINT reject_boom_retract CHECK (NOT (op = 'retract' AND dedup_key = 'boom'))
		)`,
		`CREATE UNIQUE INDEX uq_task_pending_ops_wiki_retract
			ON task_pending_ops (tenant_id, task_type, scope, scope_id, op, dedup_key)
			WHERE task_type = 'wiki:ingest' AND scope = 'knowledge_base' AND op = 'retract'`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

func insertKnowledge(t *testing.T, db *gorm.DB, id string, tenantID uint64, kbID, status string) {
	t.Helper()
	insertKnowledgeBase(t, db, tenantID, kbID)
	var processedAt interface{}
	if status == types.ParseStatusCompleted {
		processedAt = time.Unix(1_700_000_000, 0)
	}
	require.NoError(t, db.Exec(
		"INSERT INTO knowledges (id, tenant_id, knowledge_base_id, parse_status, processing_generation, processed_at) VALUES (?, ?, ?, ?, ?, ?)",
		id, tenantID, kbID, status, "generation-1", processedAt,
	).Error)
}

func insertKnowledgeBase(t *testing.T, db *gorm.DB, tenantID uint64, kbID string) {
	t.Helper()
	require.NoError(t, db.Exec(
		"INSERT OR IGNORE INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", kbID, tenantID,
	).Error)
}

func pendingOp(tenantID uint64, kbID, knowledgeID, op string) *types.TaskPendingOp {
	return &types.TaskPendingOp{
		TenantID: tenantID,
		TaskType: types.TypeWikiIngest,
		Scope:    types.TaskScopeKnowledgeBase,
		ScopeID:  kbID,
		Op:       op,
		DedupKey: knowledgeID,
		Payload:  []byte(fmt.Sprintf(`{"op":%q,"knowledge_id":%q}`, op, knowledgeID)),
	}
}

func request(tenantID uint64, kbID, knowledgeID string) Request {
	return Request{
		TenantID:        tenantID,
		KnowledgeID:     knowledgeID,
		KnowledgeBaseID: kbID,
		PendingOp:       pendingOp(tenantID, kbID, knowledgeID, wikiOpRetract),
	}
}

func insertPending(t *testing.T, db *gorm.DB, op *types.TaskPendingOp) {
	t.Helper()
	if op.EnqueuedAt.IsZero() {
		op.EnqueuedAt = time.Now().UTC()
	}
	require.NoError(t, db.Create(op).Error)
}

func knowledgeStatus(t *testing.T, db *gorm.DB, id string) string {
	t.Helper()
	var status string
	require.NoError(t, db.Table("knowledges").Select("parse_status").Where("id = ?", id).Scan(&status).Error)
	return status
}

func pendingCount(t *testing.T, db *gorm.DB, knowledgeID, op string) int64 {
	t.Helper()
	var count int64
	require.NoError(t, db.Model(&types.TaskPendingOp{}).
		Where("dedup_key = ? AND op = ?", knowledgeID, op).
		Count(&count).Error)
	return count
}

func TestCoordinatorPrepareSuccessAndRetryIdempotency(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusCompleted)
	ingest := pendingOp(7, "kb-1", "knowledge-1", wikiOpIngest)
	ingest.EnqueuedAt = time.Now().UTC()
	insertPending(t, db, ingest)

	coordinator := New(db)
	req := request(7, "kb-1", "knowledge-1")
	require.NoError(t, coordinator.Prepare(context.Background(), []Request{req}))
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "knowledge-1"))
	assert.EqualValues(t, 0, pendingCount(t, db, "knowledge-1", wikiOpIngest))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-1", wikiOpRetract))

	// Same-knowledge retries see the deleting row and existing exact retract.
	require.NoError(t, coordinator.Prepare(context.Background(), []Request{req}))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-1", wikiOpRetract))
}

func TestCoordinatorBeginPersistsExecutableMinimalRetract(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-begin", 7, "kb-1", types.ParseStatusCompleted)
	ingest := pendingOp(7, "kb-1", "knowledge-begin", wikiOpIngest)
	insertPending(t, db, ingest)

	retract := pendingOp(7, "kb-1", "knowledge-begin", wikiOpRetract)
	require.NoError(t, New(db).Begin(context.Background(), []Intent{{
		TenantID: 7, KnowledgeID: "knowledge-begin", KnowledgeBaseID: "kb-1", PendingOp: retract,
	}}))

	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "knowledge-begin"))
	assert.Zero(t, pendingCount(t, db, "knowledge-begin", wikiOpIngest))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-begin", wikiOpRetract))
}

func TestCoordinatorRecoveryClaimAndRetryNeverReviveRepairedRow(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-recovery", 7, "kb-1", types.ParseStatusDeleting)
	claimedAt := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-recovery").
		Update("updated_at", claimedAt).Error)
	intent := Intent{TenantID: 7, KnowledgeID: "knowledge-recovery", KnowledgeBaseID: "kb-1"}

	claimed, err := New(db).ClaimRecovery(context.Background(), intent, claimedAt)
	require.NoError(t, err)
	assert.True(t, claimed)
	continued, err := New(db).ContinueRecovery(context.Background(), intent)
	require.NoError(t, err)
	assert.True(t, continued, "an Asynq retry must retain the deleting lease")

	require.NoError(t, db.Table("knowledges").Where("id = ?", "knowledge-recovery").
		Update("parse_status", types.ParseStatusCompleted).Error)
	continued, err = New(db).ContinueRecovery(context.Background(), intent)
	require.NoError(t, err)
	assert.False(t, continued, "an obsolete recovery retry must not revive a repaired row")
}

func TestCoordinatorPrepareKeepsExistingRetract(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusCompleted)
	existing := pendingOp(7, "kb-1", "knowledge-1", wikiOpRetract)
	existing.Payload = []byte(`{"existing":true}`)
	existing.FailCount = 9
	existing.ClaimedAt = new(time.Time)
	existing.EnqueuedAt = time.Now().UTC()
	oldEnqueuedAt := existing.EnqueuedAt
	insertPending(t, db, existing)

	require.NoError(t, New(db).Prepare(context.Background(), []Request{request(7, "kb-1", "knowledge-1")}))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-1", wikiOpRetract))
	var row struct {
		Payload    string
		FailCount  int
		EnqueuedAt time.Time
		ClaimedAt  *time.Time
	}
	require.NoError(t, db.Table("task_pending_ops").Where("id = ?", existing.ID).Scan(&row).Error)
	assert.JSONEq(t, `{"op":"retract","knowledge_id":"knowledge-1"}`, row.Payload,
		"retry refreshes the canonical row with the latest complete snapshot")
	assert.Zero(t, row.FailCount, "a renewed delete intent must receive a fresh retry budget")
	assert.Nil(t, row.ClaimedAt)
	assert.True(t, row.EnqueuedAt.After(oldEnqueuedAt) || row.EnqueuedAt.Equal(oldEnqueuedAt))
}

func TestCoordinatorPrepareRecreatesRetractDeletedBetweenRetries(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusCompleted)
	coordinator := New(db)
	req := request(7, "kb-1", "knowledge-1")

	require.NoError(t, coordinator.Prepare(context.Background(), []Request{req}))
	var firstID int64
	require.NoError(t, db.Table("task_pending_ops").
		Select("id").Where("dedup_key = ? AND op = ?", "knowledge-1", wikiOpRetract).
		Scan(&firstID).Error)
	require.NotZero(t, firstID)
	require.NoError(t, db.Exec("DELETE FROM task_pending_ops WHERE id = ?", firstID).Error)

	require.NoError(t, coordinator.Prepare(context.Background(), []Request{req}))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-1", wikiOpRetract))
	var secondID int64
	require.NoError(t, db.Table("task_pending_ops").
		Select("id").Where("dedup_key = ? AND op = ?", "knowledge-1", wikiOpRetract).
		Scan(&secondID).Error)
	assert.NotEqual(t, firstID, secondID)
}

func TestCoordinatorPrepareConstraintFailureRollsBackStatusAndQueue(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "boom", 7, "kb-1", types.ParseStatusCompleted)
	ingest := pendingOp(7, "kb-1", "boom", wikiOpIngest)
	ingest.EnqueuedAt = time.Now().UTC()
	insertPending(t, db, ingest)

	err := New(db).Prepare(context.Background(), []Request{request(7, "kb-1", "boom")})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upsert Wiki retract")
	assert.Equal(t, types.ParseStatusCompleted, knowledgeStatus(t, db, "boom"))
	assert.EqualValues(t, 1, pendingCount(t, db, "boom", wikiOpIngest))
	assert.EqualValues(t, 0, pendingCount(t, db, "boom", wikiOpRetract))
}

func TestCoordinatorPrepareRejectsPayloadIdentityMismatch(t *testing.T) {
	tests := []struct {
		name    string
		payload string
	}{
		{name: "wrong operation", payload: `{"op":"ingest","knowledge_id":"knowledge-1"}`},
		{name: "wrong knowledge", payload: `{"op":"retract","knowledge_id":"knowledge-other"}`},
		{name: "wrong field type", payload: `{"op":"retract","knowledge_id":42}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			db := newCoordinatorDB(t)
			insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusCompleted)
			req := request(7, "kb-1", "knowledge-1")
			req.PendingOp.Payload = []byte(tc.payload)

			err := New(db).Prepare(context.Background(), []Request{req})
			require.Error(t, err)
			assert.True(t, errors.Is(err, ErrInvalidRequest))
			assert.Equal(t, types.ParseStatusCompleted, knowledgeStatus(t, db, "knowledge-1"))
			assert.EqualValues(t, 0, pendingCount(t, db, "knowledge-1", wikiOpRetract))
		})
	}
}

func TestCoordinatorPrepareRejectsWrongIdentityWithoutChanges(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusCompleted)
	ingest := pendingOp(7, "kb-1", "knowledge-1", wikiOpIngest)
	ingest.EnqueuedAt = time.Now().UTC()
	insertPending(t, db, ingest)

	err := New(db).Prepare(context.Background(), []Request{request(8, "kb-other", "knowledge-1")})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKnowledgeIdentity))
	assert.Equal(t, types.ParseStatusCompleted, knowledgeStatus(t, db, "knowledge-1"))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-1", wikiOpIngest))
	assert.EqualValues(t, 0, pendingCount(t, db, "knowledge-1", wikiOpRetract))
}

func TestCoordinatorPrepareBatchIsAllOrNothing(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-a", 7, "kb-1", types.ParseStatusCompleted)
	insertKnowledge(t, db, "knowledge-b", 8, "kb-2", types.ParseStatusFailed)
	for _, op := range []*types.TaskPendingOp{
		pendingOp(7, "kb-1", "knowledge-a", wikiOpIngest),
		pendingOp(8, "kb-2", "knowledge-b", wikiOpIngest),
	} {
		op.EnqueuedAt = time.Now().UTC()
		insertPending(t, db, op)
	}

	err := New(db).Prepare(context.Background(), []Request{
		request(7, "kb-1", "knowledge-a"),
		request(999, "kb-2", "knowledge-b"),
	})
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrKnowledgeIdentity))
	assert.Equal(t, types.ParseStatusCompleted, knowledgeStatus(t, db, "knowledge-a"))
	assert.Equal(t, types.ParseStatusFailed, knowledgeStatus(t, db, "knowledge-b"))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-a", wikiOpIngest))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-b", wikiOpIngest))
	assert.EqualValues(t, 0, pendingCount(t, db, "knowledge-a", wikiOpRetract))
	assert.EqualValues(t, 0, pendingCount(t, db, "knowledge-b", wikiOpRetract))
}

func TestCoordinatorPrepareSuccessfulBatch(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-b", 8, "kb-2", types.ParseStatusFailed)
	insertKnowledge(t, db, "knowledge-a", 7, "kb-1", types.ParseStatusCompleted)

	require.NoError(t, New(db).Prepare(context.Background(), []Request{
		request(8, "kb-2", "knowledge-b"),
		request(7, "kb-1", "knowledge-a"),
	}))
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "knowledge-a"))
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "knowledge-b"))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-a", wikiOpRetract))
	assert.EqualValues(t, 1, pendingCount(t, db, "knowledge-b", wikiOpRetract))
}

func TestCoordinatorFinalizeSoftDeletesAndChargesStorageExactlyOnce(t *testing.T) {
	db := newCoordinatorDB(t)
	require.NoError(t, db.Exec(
		"INSERT INTO tenants (id, storage_used) VALUES (7, 1000)",
	).Error)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusDeleting)
	require.NoError(t, db.Exec(
		"UPDATE knowledges SET storage_size = 125, processing_owner = 'owner-1' WHERE id = 'knowledge-1'",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_tag_relations (knowledge_id, tag_id) VALUES ('knowledge-1', 'tag-1')",
	).Error)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledge_fanout_completions
		 (tenant_id, knowledge_id, knowledge_base_id, processing_generation, item_id, completed_at)
		 VALUES (7, 'knowledge-1', 'kb-1', 'generation-1', 'image:0', CURRENT_TIMESTAMP),
		        (8, 'other-knowledge', 'kb-2', 'generation-1', 'image:0', CURRENT_TIMESTAMP)`,
	).Error)

	removed, err := New(db).Finalize(context.Background(), 7, []string{"knowledge-1"})
	require.NoError(t, err)
	assert.EqualValues(t, 125, removed)

	var deletedAt *time.Time
	var processingOwner string
	require.NoError(t, db.Raw(
		"SELECT deleted_at, processing_owner FROM knowledges WHERE id = 'knowledge-1'",
	).Row().Scan(&deletedAt, &processingOwner))
	assert.NotNil(t, deletedAt)
	assert.Empty(t, processingOwner, "final delete must release the core processing owner")
	var storageUsed int64
	require.NoError(t, db.Raw("SELECT storage_used FROM tenants WHERE id = 7").Row().Scan(&storageUsed))
	assert.EqualValues(t, 875, storageUsed)
	var relationCount int64
	require.NoError(t, db.Table("knowledge_tag_relations").Count(&relationCount).Error)
	assert.Zero(t, relationCount)
	var completionCount int64
	require.NoError(t, db.Table("knowledge_fanout_completions").Count(&completionCount).Error)
	assert.EqualValues(t, 1, completionCount, "soft delete must remove only the target tenant/knowledge ledger")

	_, err = New(db).Finalize(context.Background(), 7, []string{"knowledge-1"})
	require.Error(t, err)
	require.NoError(t, db.Raw("SELECT storage_used FROM tenants WHERE id = 7").Row().Scan(&storageUsed))
	assert.EqualValues(t, 875, storageUsed, "a retry cannot double-decrement storage")
}

func TestCoordinatorFinalizeRollsBackWhenTenantIsMissing(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "knowledge-1", 7, "kb-1", types.ParseStatusDeleting)
	require.NoError(t, db.Exec(
		"UPDATE knowledges SET storage_size = 50, processing_owner = 'owner-1' WHERE id = 'knowledge-1'",
	).Error)
	require.NoError(t, db.Exec(
		"INSERT INTO knowledge_tag_relations (knowledge_id, tag_id) VALUES ('knowledge-1', 'tag-1')",
	).Error)

	_, err := New(db).Finalize(context.Background(), 7, []string{"knowledge-1"})
	require.Error(t, err)
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "knowledge-1"))
	var deletedAt *time.Time
	var processingOwner string
	require.NoError(t, db.Raw(
		"SELECT deleted_at, processing_owner FROM knowledges WHERE id = 'knowledge-1'",
	).Row().Scan(&deletedAt, &processingOwner))
	assert.Nil(t, deletedAt)
	assert.Equal(t, "owner-1", processingOwner, "owner cleanup must roll back with the soft delete")
	var relationCount int64
	require.NoError(t, db.Table("knowledge_tag_relations").Count(&relationCount).Error)
	assert.EqualValues(t, 1, relationCount)
}
