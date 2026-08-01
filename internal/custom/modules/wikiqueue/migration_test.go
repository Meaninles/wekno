package wikiqueue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestMigrateAddsMapDispatchSchemaAndBackfillsDueTime(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.Exec(`
		CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			tenant_id INTEGER NOT NULL,
			task_type VARCHAR(64) NOT NULL,
			scope VARCHAR(32) NOT NULL,
			scope_id VARCHAR(64) NOT NULL,
			op VARCHAR(32) NOT NULL,
			dedup_key VARCHAR(128) NOT NULL DEFAULT '',
			payload TEXT NOT NULL DEFAULT '{}',
			fail_count INTEGER NOT NULL DEFAULT 0,
			enqueued_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			claimed_at DATETIME,
			map_ready_at DATETIME
		)`).Error)
	require.NoError(t, db.Exec(`
		INSERT INTO task_pending_ops
			(tenant_id, task_type, scope, scope_id, op, dedup_key)
		VALUES (7, 'wiki:ingest', 'knowledge_base', 'kb-a', 'ingest', 'doc:g-1')
	`).Error)

	require.NoError(t, Migrate(context.Background(), db))
	require.NoError(t, Migrate(context.Background(), db), "migration must be idempotent")
	for _, column := range []string{
		"next_attempt_at",
		"map_resource_pool_id",
		"map_dispatch_epoch",
		"map_dispatch_task_id",
		"map_dispatch_lease_until",
	} {
		assert.Truef(t, db.Migrator().HasColumn("task_pending_ops", column), "missing %s", column)
	}
	var due int64
	require.NoError(t, db.Table("task_pending_ops").
		Where("next_attempt_at IS NOT NULL").Count(&due).Error)
	assert.EqualValues(t, 1, due)
}
