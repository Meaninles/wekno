package knowledgepurge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

type derivativePurgerCapture struct {
	interfaces.TaskInspector
	targets []interfaces.DerivativeTaskHistoryTarget
	err     error
}

func (c *derivativePurgerCapture) QuiesceAndPurgeDerivativeTaskHistory(
	_ context.Context,
	targets []interfaces.DerivativeTaskHistoryTarget,
) (int, error) {
	c.targets = append(c.targets, targets...)
	return len(targets), c.err
}

func newCoordinatorDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(
		sqlite.Open(fmt.Sprintf("file:knowledge-purge-coordinator-%s?mode=memory&cache=shared", t.Name())),
		&gorm.Config{},
	)
	require.NoError(t, err)
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL, deleted_at DATETIME
		)`,
		`CREATE TABLE knowledges (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, processing_generation TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE custom_derivative_work_items (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL, dispatch_epoch INTEGER NOT NULL,
			state TEXT NOT NULL, completed_at DATETIME,
			owner_instance_id TEXT NOT NULL DEFAULT '', lease_token TEXT NOT NULL DEFAULT '',
			lease_until DATETIME, dispatch_lease_until DATETIME, last_heartbeat_at DATETIME,
			version INTEGER NOT NULL DEFAULT 1, updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE custom_derivative_provider_calls (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_derivative_results (
			id TEXT PRIMARY KEY, work_item_id TEXT NOT NULL
		)`,
		`CREATE TABLE custom_document_queue_workflows (
			id TEXT PRIMARY KEY, tenant_id INTEGER NOT NULL,
			knowledge_base_id TEXT NOT NULL, knowledge_id TEXT NOT NULL,
			processing_generation TEXT NOT NULL
		)`,
		`CREATE TABLE custom_document_queue_schedule_groups (
			tenant_id INTEGER NOT NULL, knowledge_base_id TEXT NOT NULL,
			PRIMARY KEY (tenant_id, knowledge_base_id)
		)`,
		`CREATE TABLE task_pending_ops (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER NOT NULL,
			task_type TEXT NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			op TEXT NOT NULL DEFAULT '', dedup_key TEXT NOT NULL DEFAULT '', payload JSON NOT NULL DEFAULT '{}'
		)`,
		`CREATE TABLE task_dead_letters (
			id INTEGER PRIMARY KEY AUTOINCREMENT, tenant_id INTEGER NOT NULL,
			task_type TEXT NOT NULL, scope TEXT NOT NULL, scope_id TEXT NOT NULL,
			related_id TEXT NOT NULL DEFAULT '', payload JSON NOT NULL DEFAULT '{}'
		)`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

func TestQuiesceDerivativeTasksFencesRowsAndBuildsExactTargets(t *testing.T) {
	db := newCoordinatorDB(t)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledges
		  (id, tenant_id, knowledge_base_id, processing_generation)
		VALUES ('knowledge-1', 7, 'kb-1', 'generation-1'),
		       ('knowledge-other', 7, 'kb-other', 'generation-other');
		INSERT INTO custom_document_queue_workflows
		  (id, tenant_id, knowledge_base_id, knowledge_id, processing_generation)
		VALUES ('workflow-1', 7, 'kb-1', 'knowledge-1', 'generation-legacy');
		INSERT INTO custom_derivative_work_items
		  (id, tenant_id, knowledge_base_id, knowledge_id, processing_generation,
		   dispatch_epoch, state, owner_instance_id, lease_token, lease_until,
		   dispatch_lease_until, last_heartbeat_at, updated_at)
		VALUES ('work-1', 7, 'kb-1', 'knowledge-1', 'generation-1',
		        3, 'provider_running', 'worker-1', 'lease-1', ?, ?, ?, ?),
		       ('work-other', 7, 'kb-other', 'knowledge-other', 'generation-other',
		        5, 'queued', '', '', NULL, NULL, NULL, ?)`, now, now, now, now, now).Error)

	capture := &derivativePurgerCapture{}
	coordinator := NewCoordinator(db, capture)
	require.NoError(t, coordinator.QuiesceDerivativeTasks(
		context.Background(), 7, "kb-1", []string{"knowledge-1"},
	))

	var row derivativePurgeRow
	require.NoError(t, db.Table("custom_derivative_work_items").Where("id = ?", "work-1").Take(&row).Error)
	require.Equal(t, "cancelled", row.State)
	var leaseState struct {
		OwnerInstanceID    string
		LeaseToken         string
		LeaseUntil         *time.Time
		DispatchLeaseUntil *time.Time
	}
	require.NoError(t, db.Table("custom_derivative_work_items").Where("id = ?", "work-1").Take(&leaseState).Error)
	require.Empty(t, leaseState.OwnerInstanceID)
	require.Empty(t, leaseState.LeaseToken)
	require.Nil(t, leaseState.LeaseUntil)
	require.Nil(t, leaseState.DispatchLeaseUntil)

	var otherState string
	require.NoError(t, db.Table("custom_derivative_work_items").
		Where("id = ?", "work-other").Pluck("state", &otherState).Error)
	require.Equal(t, "queued", otherState)

	require.Contains(t, capture.targets, interfaces.DerivativeTaskHistoryTarget{
		WorkItemID: "work-1", DispatchEpoch: 3,
	})
	require.Contains(t, capture.targets, interfaces.DerivativeTaskHistoryTarget{
		LegacyTaskID: "datatable-summary:knowledge-1:generation-1",
	})
	require.Contains(t, capture.targets, interfaces.DerivativeTaskHistoryTarget{
		LegacyTaskID: "datatable-summary:knowledge-1:generation-legacy",
	})
}

func TestCleanupCompletedKnowledgeBasePurgesExecutionResidueButKeepsTombstones(t *testing.T) {
	db := newCoordinatorDB(t)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(`
		INSERT INTO knowledge_bases (id, tenant_id, deleted_at) VALUES ('kb-1', 7, ?);
		INSERT INTO knowledges
		  (id, tenant_id, knowledge_base_id, processing_generation, deleted_at)
		VALUES ('knowledge-1', 7, 'kb-1', 'generation-1', ?);
		INSERT INTO custom_derivative_work_items
		  (id, tenant_id, knowledge_base_id, knowledge_id, processing_generation,
		   dispatch_epoch, state, updated_at)
		VALUES ('work-1', 7, 'kb-1', 'knowledge-1', 'generation-1', 2, 'completed', ?);
		INSERT INTO custom_derivative_provider_calls (id, work_item_id) VALUES ('call-1', 'work-1');
		INSERT INTO custom_derivative_results (id, work_item_id) VALUES ('result-1', 'work-1');
		INSERT INTO custom_document_queue_workflows
		  (id, tenant_id, knowledge_base_id, knowledge_id, processing_generation)
		VALUES ('workflow-1', 7, 'kb-1', 'knowledge-1', 'generation-1');
		INSERT INTO custom_document_queue_schedule_groups (tenant_id, knowledge_base_id)
		VALUES (7, 'kb-1');
		INSERT INTO task_dead_letters
		  (tenant_id, task_type, scope, scope_id, payload)
		VALUES (7, 'knowledge:list_reparse', 'tenant', '7',
		 '{"knowledge_ids":["knowledge-1"],"expected_snapshot":{"knowledge_base_id":"kb-1"}}')`,
		now, now, now).Error)

	capture := &derivativePurgerCapture{}
	cleaned, err := NewCoordinator(db, capture).SweepCompletedKnowledgeBases(context.Background(), 10)
	require.NoError(t, err)
	require.Equal(t, 1, cleaned)

	for _, table := range []string{
		"custom_derivative_work_items", "custom_derivative_provider_calls",
		"custom_derivative_results", "custom_document_queue_workflows",
		"custom_document_queue_schedule_groups", "task_dead_letters",
	} {
		var count int64
		require.NoError(t, db.Table(table).Count(&count).Error)
		require.Zero(t, count, table)
	}
	for _, table := range []string{"knowledge_bases", "knowledges"} {
		var count int64
		require.NoError(t, db.Table(table).Count(&count).Error)
		require.EqualValues(t, 1, count, table+" tombstone")
	}
	require.Contains(t, capture.targets, interfaces.DerivativeTaskHistoryTarget{
		WorkItemID: "work-1", DispatchEpoch: 2,
	})
}
