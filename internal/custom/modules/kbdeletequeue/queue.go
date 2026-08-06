// Package kbdeletequeue owns the durable hand-off between a knowledge-base
// soft-delete and its asynchronous external-resource cleanup.
package kbdeletequeue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgepurge"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

const (
	operationDelete  = "delete"
	triggerUniqueTTL = 15 * time.Minute
	triggerTimeout   = 2 * time.Hour
)

// Coordinator persists and consumes KB deletion intents in PostgreSQL.
type Coordinator struct {
	db *gorm.DB
}

func New(db *gorm.DB) *Coordinator { return &Coordinator{db: db} }

// Prepare atomically soft-deletes the exact KB and replaces its durable
// outbox row. Locking the KB serializes concurrent API retries; delete+insert
// is therefore sufficient without another schema-specific unique index.
func (c *Coordinator) Prepare(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	payload []byte,
) error {
	if c == nil || c.db == nil || tenantID == 0 || kbID == "" || !json.Valid(payload) {
		return errors.New("KB delete outbox: complete identity and valid payload are required")
	}
	return kbwritefence.WithDeleteTransaction(ctx, c.db, func(tx *gorm.DB) error {
		deleted, err := kbwritefence.LockExisting(tx, tenantID, kbID)
		if err != nil {
			return fmt.Errorf("KB delete outbox: lock KB: %w", err)
		}

		if err := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			tenantID, types.TypeKBDelete, types.TaskScopeKnowledgeBase, kbID, operationDelete, kbID,
		).Delete(&types.TaskPendingOp{}).Error; err != nil {
			return fmt.Errorf("KB delete outbox: replace existing intent: %w", err)
		}
		intent := &types.TaskPendingOp{
			TenantID: tenantID, TaskType: types.TypeKBDelete,
			Scope: types.TaskScopeKnowledgeBase, ScopeID: kbID,
			Op: operationDelete, DedupKey: kbID,
			Payload: payload, EnqueuedAt: time.Now().UTC(),
		}
		if err := tx.Create(intent).Error; err != nil {
			return fmt.Errorf("KB delete outbox: persist intent: %w", err)
		}
		if !deleted {
			now := time.Now().UTC()
			result := tx.Unscoped().Table("knowledge_bases").
				Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", kbID, tenantID).
				Updates(map[string]interface{}{"deleted_at": now, "updated_at": now})
			if result.Error != nil {
				return fmt.Errorf("KB delete outbox: soft-delete KB: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return errors.New("KB delete outbox: KB soft-delete lost its row lock")
			}
		}
		return nil
	})
}

// Complete consumes the durable intent only after all cleanup and database
// finalization succeeded. It is idempotent for duplicate worker deliveries.
func (c *Coordinator) Complete(ctx context.Context, tenantID uint64, kbID string) error {
	if c == nil || c.db == nil || tenantID == 0 || kbID == "" {
		return errors.New("KB delete outbox: complete identity is required")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockTombstone(tx, tenantID, kbID); err != nil {
			return fmt.Errorf("KB delete outbox: lock tombstone for completion: %w", err)
		}
		if err := wikilease.DeleteLocked(tx, tenantID, kbID); err != nil {
			return fmt.Errorf("KB delete outbox: purge Wiki database lease: %w", err)
		}
		var leaseCount int64
		if err := tx.Model(&wikilease.Lease{}).
			Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
			Count(&leaseCount).Error; err != nil {
			return fmt.Errorf("KB delete outbox: assert Wiki database lease drained: %w", err)
		}
		if leaseCount != 0 {
			return fmt.Errorf("KB delete outbox: refusing completion while Wiki database lease count is %d", leaseCount)
		}

		// This is the final database-side proof, under the same KB lock that
		// every child writer must own. Once these counts are zero, no writer can
		// commit a replacement row: a waiter observes the tombstone and aborts.
		checks := []struct {
			name  string
			query string
			args  []interface{}
		}{
			{"active knowledge", "SELECT count(*) FROM knowledges WHERE tenant_id = ? AND knowledge_base_id = ? AND deleted_at IS NULL", []interface{}{tenantID, kbID}},
			{"Wiki pages", "SELECT count(*) FROM wiki_pages WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki folders", "SELECT count(*) FROM wiki_folders WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki page issues", "SELECT count(*) FROM wiki_page_issues WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki log entries", "SELECT count(*) FROM wiki_log_entries WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki pending ops", `SELECT count(*) FROM task_pending_ops
				WHERE tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?`,
				[]interface{}{tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, kbID}},
		}
		for _, check := range checks {
			var count int64
			if err := tx.Raw(check.query, check.args...).Scan(&count).Error; err != nil {
				return fmt.Errorf("KB delete outbox: assert %s drained: %w", check.name, err)
			}
			if count != 0 {
				return fmt.Errorf("KB delete outbox: refusing completion while %s count is %d", check.name, count)
			}
		}
		// Auxiliary ownership is KB-scoped, including reservations made before
		// their knowledge row exists. The exact tuple is indexable and avoids a
		// tenant-wide JSON scan for large installations.
		var auxiliaryCount int64
		if err := tx.Model(&types.TaskPendingOp{}).Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
			tenantID, knowledgeaux.TaskType, types.TaskScopeKnowledgeBase, kbID,
		).Count(&auxiliaryCount).Error; err != nil {
			return fmt.Errorf("KB delete outbox: assert auxiliary ownership drained: %w", err)
		}
		if auxiliaryCount != 0 {
			return fmt.Errorf("KB delete outbox: refusing completion while auxiliary ownership count is %d", auxiliaryCount)
		}
		// The document finalizers remove per-document rows. This KB-scoped
		// sweep consumes the fair-scheduler group and any defensive residue
		// (including tenant-scoped batch dead letters) in the same transaction
		// as the durable outbox. A rollback therefore preserves the retry anchor.
		if err := knowledgepurge.DeleteKnowledgeBaseArtifacts(tx, tenantID, kbID); err != nil {
			return fmt.Errorf("KB delete outbox: purge KB execution residue: %w", err)
		}

		if err := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			tenantID, types.TypeKBDelete, types.TaskScopeKnowledgeBase, kbID, operationDelete, kbID,
		).Delete(&types.TaskPendingOp{}).Error; err != nil {
			return fmt.Errorf("KB delete outbox: consume intent: %w", err)
		}
		return nil
	})
}

// IntentExists verifies that the disposable trigger still corresponds to the
// exact durable payload. A task without an outbox row may only be acknowledged
// after the service separately proves that no active documents remain.
func (c *Coordinator) IntentExists(
	ctx context.Context, tenantID uint64, kbID string, payload []byte,
) (bool, error) {
	if c == nil || c.db == nil || tenantID == 0 || kbID == "" || !json.Valid(payload) {
		return false, errors.New("KB delete outbox: complete intent identity is required")
	}
	var row types.TaskPendingOp
	result := c.db.WithContext(ctx).Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
		tenantID, types.TypeKBDelete, types.TaskScopeKnowledgeBase, kbID, operationDelete, kbID,
	).Limit(1).Find(&row)
	if result.Error != nil {
		return false, fmt.Errorf("KB delete outbox: inspect intent: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return false, nil
	}
	var storedValue, triggerValue interface{}
	if err := json.Unmarshal(row.Payload, &storedValue); err != nil {
		return false, fmt.Errorf("KB delete outbox: decode stored intent: %w", err)
	}
	if err := json.Unmarshal(payload, &triggerValue); err != nil {
		return false, fmt.Errorf("KB delete outbox: decode trigger payload: %w", err)
	}
	if !reflect.DeepEqual(storedValue, triggerValue) {
		return false, errors.New("KB delete outbox: trigger payload does not match durable intent")
	}
	return true, nil
}

// PurgeWikiState hard-deletes generated Wiki material for one committed KB
// tombstone. Wiki log entries are operational content (titles/summaries), not
// the immutable audit log, so they are purged with pages. The KB-delete outbox
// is deliberately excluded and remains the retry anchor until Complete.
func (c *Coordinator) PurgeWikiState(ctx context.Context, tenantID uint64, kbID string) error {
	if c == nil || c.db == nil || tenantID == 0 || kbID == "" {
		return errors.New("KB delete outbox: Wiki purge identity is required")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := kbwritefence.LockTombstone(tx, tenantID, kbID); err != nil {
			return fmt.Errorf("KB delete outbox: refusing Wiki purge: %w", err)
		}
		if err := wikilease.DeleteLocked(tx, tenantID, kbID); err != nil {
			return fmt.Errorf("KB delete outbox: purge Wiki database lease: %w", err)
		}

		statements := []struct {
			name  string
			query string
			args  []interface{}
		}{
			{"Wiki page issues", "DELETE FROM wiki_page_issues WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki pages", "DELETE FROM wiki_pages WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki folders", "DELETE FROM wiki_folders WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki log entries", "DELETE FROM wiki_log_entries WHERE tenant_id = ? AND knowledge_base_id = ?", []interface{}{tenantID, kbID}},
			{"Wiki pending ops", `DELETE FROM task_pending_ops
				WHERE tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?`,
				[]interface{}{tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, kbID}},
		}
		for _, statement := range statements {
			if err := tx.Exec(statement.query, statement.args...).Error; err != nil {
				return fmt.Errorf("KB delete outbox: purge %s: %w", statement.name, err)
			}
		}
		return nil
	})
}

// EnqueueTrigger publishes a disposable wake-up. PostgreSQL remains the
// source of truth, so duplicate signals are success and recovery may publish
// another after the uniqueness lease expires.
func EnqueueTrigger(enqueuer interfaces.TaskEnqueuer, payload []byte, delay time.Duration) error {
	if enqueuer == nil {
		return errors.New("KB delete outbox: task enqueuer is unavailable")
	}
	if !json.Valid(payload) {
		return errors.New("KB delete outbox: trigger payload is invalid")
	}
	task := asynq.NewTask(types.TypeKBDelete, payload)
	_, err := enqueuer.Enqueue(task,
		asynq.Queue(types.QueueLow),
		asynq.MaxRetry(20),
		asynq.Timeout(triggerTimeout),
		asynq.ProcessIn(delay),
		asynq.Unique(triggerUniqueTTL),
	)
	if err == nil || errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return fmt.Errorf("KB delete outbox: enqueue trigger: %w", err)
}
