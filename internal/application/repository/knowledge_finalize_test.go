package repository

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// knowledgesTestDDL mirrors the columns of `knowledges` that
// SetFinalizing / FinalizeSubtask / UpdateKnowledge actually read or write.
// We inline the DDL (instead of AutoMigrate) so the schema is explicit,
// and we include pending_subtasks_count from migration 000056 plus the
// processing/finalizing/completed columns the helpers care about.
const knowledgesTestDDL = `
CREATE TABLE IF NOT EXISTS knowledges (
    id VARCHAR(36) PRIMARY KEY,
    tenant_id INTEGER NOT NULL,
    knowledge_base_id VARCHAR(36) NOT NULL,
    type VARCHAR(50) NOT NULL DEFAULT '',
    title VARCHAR(255) NOT NULL DEFAULT '',
    description TEXT,
    source VARCHAR(2048) NOT NULL DEFAULT '',
    parse_status VARCHAR(50) NOT NULL DEFAULT 'unprocessed',
    processing_generation VARCHAR(36) NOT NULL DEFAULT '',
    processing_owner VARCHAR(160) NOT NULL DEFAULT '',
    processing_fanout TEXT,
    enable_status VARCHAR(50) NOT NULL DEFAULT 'enabled',
    embedding_model_id VARCHAR(64),
    file_name VARCHAR(255),
    file_type VARCHAR(50),
    file_size BIGINT,
    file_path TEXT,
    file_hash VARCHAR(64),
    storage_size BIGINT NOT NULL DEFAULT 0,
    metadata TEXT,
    tag_id VARCHAR(36),
    summary_status VARCHAR(32) DEFAULT 'none',
    core_status VARCHAR(32) NOT NULL DEFAULT 'pending',
    core_completed_at DATETIME,
    enrichment_status VARCHAR(32) NOT NULL DEFAULT 'none',
    enrichment_completed_at DATETIME,
    enrichment_error_summary TEXT NOT NULL DEFAULT '',
    wiki_status VARCHAR(32) NOT NULL DEFAULT 'none',
    wiki_error_message TEXT NOT NULL DEFAULT '',
    last_faq_import_result TEXT DEFAULT NULL,
    channel VARCHAR(50) NOT NULL DEFAULT 'web',
    pending_subtasks_count INT NOT NULL DEFAULT 0,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    processed_at DATETIME,
    error_message TEXT,
    deleted_at DATETIME
);
`

// setupKnowledgeTestDB returns an in-memory SQLite db with the knowledges
// table. SQLite has a single-writer constraint, so we cap MaxOpenConns at 1
// and set a busy timeout: concurrent goroutines line up on the same
// connection (just like production write workloads serialize at the row
// level). This is enough to exercise the atomic semantics of the helpers
// without flaking on "database table is locked".
func setupKnowledgeTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := "file:" + uuid.New().String() + "?mode=memory&cache=shared&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Exec(knowledgesTestDDL).Error)
	t.Cleanup(func() { _ = sqlDB.Close() })
	return db
}

// insertProcessingKnowledge seeds a row in `processing` state ready for a
// SetFinalizing transition.
func insertProcessingKnowledge(t *testing.T, db *gorm.DB) string {
	t.Helper()
	id := uuid.New().String()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, source, parse_status, pending_subtasks_count)
		VALUES (?, 1, ?, 'document', 'finalize-test', 'manual', 'processing', 0)
	`, id, uuid.New().String()).Error)
	return id
}

// reloadKnowledgeRow returns the parse_status and pending_subtasks_count of
// a row directly via raw SQL — bypasses any GORM hook noise.
func reloadKnowledgeRow(t *testing.T, db *gorm.DB, id string) (status string, count int) {
	t.Helper()
	row := db.Raw(`SELECT parse_status, pending_subtasks_count FROM knowledges WHERE id = ?`, id).Row()
	require.NoError(t, row.Scan(&status, &count))
	return status, count
}

func insertKnowledgeWithStatus(t *testing.T, db *gorm.DB, status string, deleted bool) string {
	t.Helper()
	id := uuid.New().String()
	deletedAt := interface{}(nil)
	if deleted {
		deletedAt = "2026-06-16 12:00:00"
	}
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (id, tenant_id, knowledge_base_id, type, title, source, parse_status, deleted_at)
		VALUES (?, 1, ?, 'document', 'delete-test', 'manual', ?, ?)
	`, id, uuid.New().String(), status, deletedAt).Error)
	return id
}

// TestFinalizeSubtask_Concurrent_ExactlyOnePromote spawns N goroutines that
// each call FinalizeSubtask after SetFinalizing(N), and asserts:
//   - the counter ends at zero,
//   - parse_status is "completed",
//   - exactly one caller observed promoted=true.
//
// This is the behavior the original "stuck pending_subtasks_count" bug
// violated: clobbered counters meant some callers saw a non-zero value
// after the true count had reached zero, and none of them promoted.
func TestFinalizeSubtask_Concurrent_ExactlyOnePromote(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	const n = 20
	id := insertProcessingKnowledge(t, db)

	transitioned, err := repo.SetFinalizing(ctx, id, n)
	require.NoError(t, err)
	require.True(t, transitioned, "SetFinalizing should transition processing -> finalizing")

	var promoteWins atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, promoted, ferr := repo.FinalizeSubtask(ctx, id)
			if ferr != nil {
				t.Errorf("FinalizeSubtask: %v", ferr)
				return
			}
			if promoted {
				promoteWins.Add(1)
			}
		}()
	}
	wg.Wait()

	assert.Equal(t, int32(1), promoteWins.Load(),
		"exactly one caller must observe promoted=true even under concurrent decrements")

	status, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusCompleted, status)
	assert.Equal(t, 0, count)
}

// TestFinalizeSubtask_PartialDecrement_StaysFinalizing verifies the row
// remains in "finalizing" with the expected residual count when fewer
// callers decrement than were seeded — the promote guard must not fire
// early.
func TestFinalizeSubtask_PartialDecrement_StaysFinalizing(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	_, err := repo.SetFinalizing(ctx, id, 3)
	require.NoError(t, err)

	for i := 0; i < 2; i++ {
		_, promoted, ferr := repo.FinalizeSubtask(ctx, id)
		require.NoError(t, ferr)
		assert.False(t, promoted, "promote must not fire while count > 0")
	}

	status, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusFinalizing, status)
	assert.Equal(t, 1, count)
}

// TestFinalizeSubtask_DecrementClampedAtZero verifies the safety-net
// clamp on the decrement: extra calls past the seeded count must not
// underflow pending_subtasks_count below zero. (Reconciliation's
// shortfall-release loop relies on this.)
func TestFinalizeSubtask_DecrementClampedAtZero(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	_, err := repo.SetFinalizing(ctx, id, 1)
	require.NoError(t, err)

	// First decrement drains the only slot and promotes.
	_, promoted, err := repo.FinalizeSubtask(ctx, id)
	require.NoError(t, err)
	assert.True(t, promoted)

	// Subsequent decrements must be no-ops, not underflow.
	for i := 0; i < 3; i++ {
		_, promoted, err := repo.FinalizeSubtask(ctx, id)
		require.NoError(t, err)
		assert.False(t, promoted)
	}

	status, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusCompleted, status)
	assert.Equal(t, 0, count, "pending_subtasks_count must be clamped at zero")
}

func TestFinalizeSubtaskWaitsForWikiAndWikiTerminalPromotes(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	var identity struct {
		TenantID        uint64
		KnowledgeBaseID string
	}
	require.NoError(t, db.Model(&types.Knowledge{}).
		Select("tenant_id", "knowledge_base_id").
		Where("id = ?", id).
		Take(&identity).Error)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"processing_generation": "generation-wiki-last",
			"wiki_status":           types.WikiStatusPending,
			"enrichment_status":     types.EnrichmentStatusPending,
		}).Error)
	transitioned, err := repo.SetFinalizing(ctx, id, 1)
	require.NoError(t, err)
	require.True(t, transitioned)

	count, promoted, err := repo.FinalizeSubtask(ctx, id)
	require.NoError(t, err)
	require.Zero(t, count)
	require.False(t, promoted, "pending Wiki must keep the document finalizing")

	var waiting types.Knowledge
	require.NoError(t, db.Where("id = ?", id).Take(&waiting).Error)
	require.Equal(t, types.ParseStatusFinalizing, waiting.ParseStatus)
	require.Equal(t, types.WikiStatusPending, waiting.WikiStatus)
	require.Equal(t, types.EnrichmentStatusDegraded, waiting.EnrichmentStatus)

	updated, err := repo.UpdateWikiStatusGeneration(
		ctx,
		identity.TenantID,
		id,
		identity.KnowledgeBaseID,
		"generation-wiki-last",
		types.WikiStatusCompleted,
		"",
	)
	require.NoError(t, err)
	require.True(t, updated)

	var completed types.Knowledge
	require.NoError(t, db.Where("id = ?", id).Take(&completed).Error)
	require.Equal(t, types.ParseStatusCompleted, completed.ParseStatus)
	require.Equal(t, types.WikiStatusCompleted, completed.WikiStatus)
	require.Zero(t, completed.PendingSubtasksCount)
	require.NotNil(t, completed.ProcessedAt)
}

func TestWikiTerminalFirstStillWaitsForLastSubtask(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	var identity struct {
		TenantID        uint64
		KnowledgeBaseID string
	}
	require.NoError(t, db.Model(&types.Knowledge{}).
		Select("tenant_id", "knowledge_base_id").
		Where("id = ?", id).
		Take(&identity).Error)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).
		Updates(map[string]interface{}{
			"processing_generation": "generation-derivative-last",
			"wiki_status":           types.WikiStatusPending,
			"enrichment_status":     types.EnrichmentStatusPending,
		}).Error)
	transitioned, err := repo.SetFinalizing(ctx, id, 1)
	require.NoError(t, err)
	require.True(t, transitioned)

	updated, err := repo.UpdateWikiStatusGeneration(
		ctx,
		identity.TenantID,
		id,
		identity.KnowledgeBaseID,
		"generation-derivative-last",
		types.WikiStatusCompleted,
		"",
	)
	require.NoError(t, err)
	require.True(t, updated)
	status, count := reloadKnowledgeRow(t, db, id)
	require.Equal(t, types.ParseStatusFinalizing, status)
	require.Equal(t, 1, count)

	count, promoted, err := repo.FinalizeSubtask(ctx, id)
	require.NoError(t, err)
	require.Zero(t, count)
	require.True(t, promoted, "the last derivative must promote after Wiki is terminal")
	status, count = reloadKnowledgeRow(t, db, id)
	require.Equal(t, types.ParseStatusCompleted, status)
	require.Zero(t, count)
}

// TestUpdateKnowledge_DoesNotClobberPendingCounter is the regression test
// for the original bug: a full-row Save with a stale in-memory counter
// must not write that stale value back, otherwise it overwrites atomic
// decrements made by other goroutines.
//
// Sequence:
//  1. SetFinalizing(N=5) -> counter=5
//  2. Caller A loads the row (sees counter=5)
//  3. FinalizeSubtask runs concurrently and decrements to counter=4
//  4. Caller A modifies an unrelated field (Title) and calls UpdateKnowledge
//  5. Counter must still be 4 (not 5).
func TestUpdateKnowledge_DoesNotClobberPendingCounter(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	_, err := repo.SetFinalizing(ctx, id, 5)
	require.NoError(t, err)

	// Step 2: caller A snapshots the row with counter=5 in memory.
	loaded, err := repo.GetKnowledgeByID(ctx, 1, id)
	require.NoError(t, err)
	require.Equal(t, 5, loaded.PendingSubtasksCount)

	// Step 3: an enrichment subtask decrements concurrently.
	_, _, err = repo.FinalizeSubtask(ctx, id)
	require.NoError(t, err)

	// Step 4: caller A persists an unrelated change. The in-memory copy
	// of PendingSubtasksCount is the STALE 5 — Save must NOT write it.
	loaded.Title = "renamed-after-stale-load"
	require.NoError(t, repo.UpdateKnowledge(ctx, loaded))

	// Step 5: the live counter is still 4, not clobbered back to 5.
	status, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, types.ParseStatusFinalizing, status)
	assert.Equal(t, 4, count,
		"UpdateKnowledge must omit pending_subtasks_count so a stale in-memory value cannot clobber atomic decrements")

	// And the unrelated field WAS persisted.
	reloaded, err := repo.GetKnowledgeByID(ctx, 1, id)
	require.NoError(t, err)
	assert.Equal(t, "renamed-after-stale-load", reloaded.Title)
}

// TestUpdateKnowledge_PendingCounterOmittedOnReset verifies the inverse
// case the reparse paths rely on: even setting PendingSubtasksCount=0
// in memory and calling UpdateKnowledge does NOT persist that value.
// Reparse must use UpdateKnowledgeColumn explicitly.
func TestUpdateKnowledge_PendingCounterOmittedOnReset(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := insertProcessingKnowledge(t, db)
	_, err := repo.SetFinalizing(ctx, id, 7)
	require.NoError(t, err)

	loaded, err := repo.GetKnowledgeByID(ctx, 1, id)
	require.NoError(t, err)

	// Caller tries to reset the counter via Save — this must be a no-op
	// for that column. The dedicated UpdateKnowledgeColumn is the only
	// path that actually writes pending_subtasks_count.
	loaded.PendingSubtasksCount = 0
	require.NoError(t, repo.UpdateKnowledge(ctx, loaded))

	_, count := reloadKnowledgeRow(t, db, id)
	assert.Equal(t, 7, count, "UpdateKnowledge with PendingSubtasksCount=0 must NOT persist the reset")

	// The explicit column write IS the supported path.
	require.NoError(t, repo.UpdateKnowledgeColumn(ctx, id, "pending_subtasks_count", 0))
	_, count = reloadKnowledgeRow(t, db, id)
	assert.Equal(t, 0, count)
}

func TestOrdinaryKnowledgeWritesCannotOverwriteDeletingFence(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()
	id := insertProcessingKnowledge(t, db)

	stale, err := repo.GetKnowledgeByID(ctx, 1, id)
	require.NoError(t, err)
	require.NoError(t, db.Model(&types.Knowledge{}).Where("id = ?", id).
		Update("parse_status", types.ParseStatusDeleting).Error)

	stale.Title = "stale writer"
	stale.ParseStatus = types.ParseStatusCompleted
	require.ErrorIs(t, repo.UpdateKnowledge(ctx, stale), ErrKnowledgeDeleting)
	require.ErrorIs(t, repo.UpdateKnowledgeColumn(ctx, id, "title", "column writer"), ErrKnowledgeDeleting)
	require.ErrorIs(t, repo.UpdateKnowledgeColumns(ctx, id, map[string]interface{}{
		"parse_status": types.ParseStatusFailed,
		"title":        "multi-column writer",
	}), ErrKnowledgeDeleting)

	var got struct {
		ParseStatus string
		Title       string
	}
	require.NoError(t, db.Model(&types.Knowledge{}).
		Select("parse_status", "title").Where("id = ?", id).Scan(&got).Error)
	assert.Equal(t, types.ParseStatusDeleting, got.ParseStatus)
	assert.Equal(t, "finalize-test", got.Title)
}

func TestUpdateActiveDeletingKnowledgeColumns_GuardsStateAndSoftDelete(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	activeDeletingID := insertKnowledgeWithStatus(t, db, types.ParseStatusDeleting, false)
	activeCompletedID := insertKnowledgeWithStatus(t, db, types.ParseStatusCompleted, false)
	deletedDeletingID := insertKnowledgeWithStatus(t, db, types.ParseStatusDeleting, true)

	updated, err := repo.UpdateActiveDeletingKnowledgeColumns(ctx, activeDeletingID, map[string]interface{}{
		"parse_status":  types.ParseStatusFailed,
		"error_message": "delete task exhausted retries",
	})
	require.NoError(t, err)
	assert.True(t, updated)

	updated, err = repo.UpdateActiveDeletingKnowledgeColumns(ctx, activeCompletedID, map[string]interface{}{
		"parse_status": types.ParseStatusFailed,
	})
	require.NoError(t, err)
	assert.False(t, updated)

	updated, err = repo.UpdateActiveDeletingKnowledgeColumns(ctx, deletedDeletingID, map[string]interface{}{
		"parse_status": types.ParseStatusFailed,
	})
	require.NoError(t, err)
	assert.False(t, updated)

	status, _ := reloadKnowledgeRow(t, db, activeDeletingID)
	assert.Equal(t, types.ParseStatusFailed, status)
	status, _ = reloadKnowledgeRow(t, db, activeCompletedID)
	assert.Equal(t, types.ParseStatusCompleted, status)
	status, _ = reloadKnowledgeRow(t, db, deletedDeletingID)
	assert.Equal(t, types.ParseStatusDeleting, status)
}

func TestUpdateWikiStatusGenerationIsExactAndDoesNotTouchLifecycle(t *testing.T) {
	db := setupKnowledgeTestDB(t)
	repo := NewKnowledgeRepository(db).(*knowledgeRepository)
	ctx := context.Background()

	id := uuid.NewString()
	kbID := uuid.NewString()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges (
			id, tenant_id, knowledge_base_id, type, title, source,
			parse_status, processing_generation, pending_subtasks_count,
			wiki_status, wiki_error_message
		) VALUES (?, 9, ?, 'document', 'wiki-status-test', 'manual',
			'completed', 'generation-current', 0, 'pending', '')
	`, id, kbID).Error)

	updated, err := repo.UpdateWikiStatusGeneration(
		ctx, 9, id, kbID, "generation-current",
		types.WikiStatusDegraded, "one citation batch failed",
	)
	require.NoError(t, err)
	require.True(t, updated)

	// A delayed task from an older generation cannot overwrite the current
	// outcome, even if all other identity fields are valid.
	updated, err = repo.UpdateWikiStatusGeneration(
		ctx, 9, id, kbID, "generation-stale",
		types.WikiStatusFailed, "stale task",
	)
	require.NoError(t, err)
	require.False(t, updated)

	var got struct {
		ParseStatus          string
		ProcessingGeneration string
		WikiStatus           string
		WikiErrorMessage     string
	}
	require.NoError(t, db.Model(&types.Knowledge{}).
		Select("parse_status", "processing_generation", "wiki_status", "wiki_error_message").
		Where("id = ?", id).
		Scan(&got).Error)
	assert.Equal(t, types.ParseStatusCompleted, got.ParseStatus)
	assert.Equal(t, "generation-current", got.ProcessingGeneration)
	assert.Equal(t, types.WikiStatusDegraded, got.WikiStatus)
	assert.Equal(t, "one citation batch failed", got.WikiErrorMessage)
}
