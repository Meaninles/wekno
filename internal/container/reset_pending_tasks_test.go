package container

import (
	"os"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

const resetPendingKnowledgeDDL = `
CREATE TABLE IF NOT EXISTS knowledges (
    id              VARCHAR(64) PRIMARY KEY,
    parse_status    VARCHAR(32) NOT NULL DEFAULT 'pending',
    summary_status  VARCHAR(32) NOT NULL DEFAULT 'none',
    pending_subtasks_count INTEGER NOT NULL DEFAULT 0,
    error_message   TEXT,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    deleted_at      DATETIME
);
`

const resetPendingSyncLogDDL = `
CREATE TABLE IF NOT EXISTS sync_logs (
    id              VARCHAR(64) PRIMARY KEY,
    data_source_id  VARCHAR(64) NOT NULL DEFAULT '',
    tenant_id       INTEGER NOT NULL DEFAULT 0,
    status          VARCHAR(32) NOT NULL,
    started_at      DATETIME,
    finished_at     DATETIME,
    error_message   TEXT,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

func setupResetPendingDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(resetPendingKnowledgeDDL).Error)
	require.NoError(t, db.Exec(resetPendingSyncLogDDL).Error)
	return db
}

func TestResetPendingTasks_PreservesStaleKnowledgeLifecycleStates(t *testing.T) {
	db := setupResetPendingDB(t)
	stale := time.Now().Add(-2 * time.Hour)
	statuses := []string{
		types.ParseStatusPending,
		types.ParseStatusProcessing,
		types.ParseStatusFinalizing,
		types.ParseStatusDeleting,
	}
	for i, status := range statuses {
		require.NoError(t, db.Exec(
			`INSERT INTO knowledges
			 (id, parse_status, summary_status, pending_subtasks_count, error_message, updated_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			"k-"+status, status, types.SummaryStatusProcessing, i+1, "keep me", stale,
		).Error)
	}

	t.Setenv("REDIS_ADDR", "redis:6379")
	resetPendingTasks(db)

	for i, status := range statuses {
		var gotStatus, summaryStatus, errMsg string
		var pending int
		require.NoError(t, db.Raw(
			`SELECT parse_status, summary_status, pending_subtasks_count, error_message
			 FROM knowledges WHERE id = ?`, "k-"+status,
		).Row().Scan(&gotStatus, &summaryStatus, &pending, &errMsg))
		assert.Equal(t, status, gotStatus)
		assert.Equal(t, types.SummaryStatusProcessing, summaryStatus)
		assert.Equal(t, i+1, pending)
		assert.Equal(t, "keep me", errMsg)
	}
}

func TestResetPendingTasks_KnowledgeFreshInDistributedMode(t *testing.T) {
	db := setupResetPendingDB(t)
	fresh := time.Now().Add(-5 * time.Minute)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges (id, parse_status, updated_at) VALUES (?, ?, ?)`,
		"k-fresh", types.ParseStatusProcessing, fresh,
	).Error)

	t.Setenv("REDIS_ADDR", "redis:6379")
	resetPendingTasks(db)

	var status string
	require.NoError(t, db.Raw(
		`SELECT parse_status FROM knowledges WHERE id = ?`, "k-fresh",
	).Row().Scan(&status))
	assert.Equal(t, types.ParseStatusProcessing, status)
}

func TestResetPendingTasks_SyncLogStaleRunning(t *testing.T) {
	db := setupResetPendingDB(t)
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, db.Exec(
		`INSERT INTO sync_logs (id, status, started_at) VALUES (?, ?, ?)`,
		"sync-1", types.SyncLogStatusRunning, stale,
	).Error)

	t.Setenv("REDIS_ADDR", "redis:6379")
	resetPendingTasks(db)

	var status string
	var finishedAt *time.Time
	require.NoError(t, db.Raw(
		`SELECT status, finished_at FROM sync_logs WHERE id = ?`, "sync-1",
	).Row().Scan(&status, &finishedAt))
	assert.Equal(t, types.SyncLogStatusFailed, status)
	require.NotNil(t, finishedAt)
}

func TestResetPendingTasks_SyncLogLiteMode(t *testing.T) {
	db := setupResetPendingDB(t)
	os.Unsetenv("REDIS_ADDR")
	require.NoError(t, db.Exec(
		`INSERT INTO sync_logs (id, status, started_at) VALUES (?, ?, ?)`,
		"sync-lite", types.SyncLogStatusRunning, time.Now(),
	).Error)

	resetPendingTasks(db)

	var status string
	require.NoError(t, db.Raw(
		`SELECT status FROM sync_logs WHERE id = ?`, "sync-lite",
	).Row().Scan(&status))
	assert.Equal(t, types.SyncLogStatusFailed, status)
}

func TestResetPendingTasks_PreservesKnowledgeLifecycleInLiteMode(t *testing.T) {
	db := setupResetPendingDB(t)
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
		 (id, parse_status, summary_status, pending_subtasks_count, error_message, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		"k-delete-lite", types.ParseStatusDeleting, types.SummaryStatusPending, 2, "durable intent", stale,
	).Error)

	os.Unsetenv("REDIS_ADDR")
	resetPendingTasks(db)

	var status, summaryStatus, errMsg string
	var pending int
	require.NoError(t, db.Raw(
		`SELECT parse_status, summary_status, pending_subtasks_count, error_message
		 FROM knowledges WHERE id = ?`, "k-delete-lite",
	).Row().Scan(&status, &summaryStatus, &pending, &errMsg))
	assert.Equal(t, types.ParseStatusDeleting, status)
	assert.Equal(t, types.SummaryStatusPending, summaryStatus)
	assert.Equal(t, 2, pending)
	assert.Equal(t, "durable intent", errMsg)
}
