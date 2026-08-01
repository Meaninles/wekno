package knowledgepurge

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Coordinator fences PostgreSQL-authoritative derivative work before a
// document tombstone physically purges its supporting rows. It also repairs
// residue left by older completed KB deletions. Redis cleanup is delegated to
// the production TaskInspector through an exact-ID extension; no shared queue
// history scan is performed here.
type Coordinator struct {
	db        *gorm.DB
	inspector interfaces.TaskInspector
}

func NewCoordinator(db *gorm.DB, inspector interfaces.TaskInspector) *Coordinator {
	return &Coordinator{db: db, inspector: inspector}
}

type derivativePurgeRow struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	KnowledgeID          string
	ProcessingGeneration string
	DispatchEpoch        uint64
	State                string
}

type generationRow struct {
	KnowledgeID          string
	ProcessingGeneration string
}

var derivativeTerminalStates = []string{
	"completed", "cancelled", "failed", "provider_unknown",
}

// QuiesceDerivativeTasks atomically fences every matching durable work item,
// snapshots its Redis high-water mark, then cancels and purges only those
// deterministic wake IDs. Passing an empty knowledgeIDs slice scopes the
// operation to the complete knowledge base.
func (c *Coordinator) QuiesceDerivativeTasks(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeIDs []string,
) error {
	if c == nil || c.db == nil || tenantID == 0 {
		return errors.New("knowledge purge: derivative cleanup requires database and tenant")
	}
	kbID := strings.TrimSpace(knowledgeBaseID)
	ids, err := normalizedIDs(knowledgeIDs)
	if err != nil {
		return err
	}
	if kbID == "" && len(ids) == 0 {
		return errors.New("knowledge purge: derivative cleanup requires a KB or knowledge identity")
	}

	rows := make([]derivativePurgeRow, 0)
	generations := make(map[string]generationRow)
	err = c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Table("custom_derivative_work_items").
			Select("id, tenant_id, knowledge_base_id, knowledge_id, processing_generation, dispatch_epoch, state").
			Where("tenant_id = ?", tenantID).
			Order("id ASC")
		if kbID != "" {
			query = query.Where("knowledge_base_id = ?", kbID)
		}
		if len(ids) > 0 {
			query = query.Where("knowledge_id IN ?", ids)
		}
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Find(&rows).Error; err != nil {
			return fmt.Errorf("knowledge purge: lock derivative work items: %w", err)
		}

		now := time.Now().UTC()
		for _, row := range rows {
			if row.ID == "" || row.TenantID != tenantID || row.KnowledgeID == "" || row.KnowledgeBaseID == "" {
				return errors.New("knowledge purge: derivative work-item snapshot has an invalid identity")
			}
			if kbID != "" && row.KnowledgeBaseID != kbID {
				return errors.New("knowledge purge: derivative work item escaped the KB scope")
			}
			generations[row.KnowledgeID+"\x00"+row.ProcessingGeneration] = generationRow{
				KnowledgeID: row.KnowledgeID, ProcessingGeneration: row.ProcessingGeneration,
			}
		}

		update := tx.Table("custom_derivative_work_items").Where("tenant_id = ?", tenantID)
		if kbID != "" {
			update = update.Where("knowledge_base_id = ?", kbID)
		}
		if len(ids) > 0 {
			update = update.Where("knowledge_id IN ?", ids)
		}
		if err := update.Updates(map[string]interface{}{
			"state": gorm.Expr(
				"CASE WHEN state IN ? THEN state ELSE ? END",
				derivativeTerminalStates, "cancelled",
			),
			"completed_at":         gorm.Expr("COALESCE(completed_at, ?)", now),
			"owner_instance_id":    "",
			"lease_token":          "",
			"lease_until":          nil,
			"dispatch_lease_until": nil,
			"last_heartbeat_at":    nil,
			"version":              gorm.Expr("version + 1"),
			"updated_at":           now,
		}).Error; err != nil {
			return fmt.Errorf("knowledge purge: fence derivative work items: %w", err)
		}

		// A pre-durable-queue datatable task may exist even when no derivative
		// work-item row was ever committed. Recover its exact stable ID from
		// both workflow and knowledge generation state.
		for _, table := range []string{"custom_document_queue_workflows", "knowledges"} {
			var generationRows []generationRow
			generationQuery := tx.Unscoped().Table(table).
				Select("knowledge_id, processing_generation").
				Where("tenant_id = ?", tenantID)
			if table == "knowledges" {
				generationQuery = tx.Unscoped().Table(table).
					Select("id AS knowledge_id, processing_generation").
					Where("tenant_id = ?", tenantID)
			}
			if kbID != "" {
				generationQuery = generationQuery.Where("knowledge_base_id = ?", kbID)
			}
			if len(ids) > 0 {
				column := "knowledge_id"
				if table == "knowledges" {
					column = "id"
				}
				generationQuery = generationQuery.Where(column+" IN ?", ids)
			}
			if err := generationQuery.Find(&generationRows).Error; err != nil {
				return fmt.Errorf("knowledge purge: list %s generations: %w", table, err)
			}
			for _, generation := range generationRows {
				if strings.TrimSpace(generation.KnowledgeID) == "" || strings.TrimSpace(generation.ProcessingGeneration) == "" {
					continue
				}
				generations[generation.KnowledgeID+"\x00"+generation.ProcessingGeneration] = generation
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	targets := make([]interfaces.DerivativeTaskHistoryTarget, 0, len(rows)+len(generations))
	for _, row := range rows {
		targets = append(targets, interfaces.DerivativeTaskHistoryTarget{
			WorkItemID: row.ID, DispatchEpoch: row.DispatchEpoch,
		})
	}
	for _, generation := range sortedGenerationRows(generations) {
		targets = append(targets, interfaces.DerivativeTaskHistoryTarget{
			LegacyTaskID: "datatable-summary:" + generation.KnowledgeID + ":" + generation.ProcessingGeneration,
		})
	}
	if len(targets) == 0 {
		return nil
	}
	purger, ok := c.inspector.(interfaces.DerivativeTaskHistoryPurger)
	if !ok || purger == nil {
		return errors.New("knowledge purge: derivative task-history purger is unavailable")
	}
	deleted, err := purger.QuiesceAndPurgeDerivativeTaskHistory(ctx, targets)
	if err != nil {
		return fmt.Errorf("knowledge purge: quiesce derivative task history: %w", err)
	}
	logger.Infof(ctx,
		"[knowledge purge] derivative task history drained tenant=%d kb=%s knowledge_count=%d work_items=%d redis_records=%d",
		tenantID, kbID, len(ids), len(rows), deleted,
	)
	return nil
}

type residueCandidate struct {
	TenantID        uint64
	KnowledgeBaseID string
}

// SweepCompletedKnowledgeBases repairs only KB tombstones whose durable
// delete outbox has already been consumed. It therefore cannot race the main
// deletion worker. This is a compatibility cleanup for residue produced by
// older builds and a crash-recovery safety net for the new ordered cleanup.
func (c *Coordinator) SweepCompletedKnowledgeBases(ctx context.Context, limit int) (int, error) {
	if c == nil || c.db == nil {
		return 0, errors.New("knowledge purge: residue sweep database is unavailable")
	}
	if limit <= 0 || limit > 100 {
		limit = 10
	}
	jsonKB := "json_extract(payload, '$.expected_snapshot.knowledge_base_id')"
	if c.db.Dialector.Name() == "postgres" {
		jsonKB = "payload->'expected_snapshot'->>'knowledge_base_id'"
	}
	query := fmt.Sprintf(`
		SELECT residue.tenant_id, residue.knowledge_base_id
		FROM (
			SELECT tenant_id, knowledge_base_id FROM custom_derivative_work_items
			UNION
			SELECT tenant_id, knowledge_base_id FROM custom_document_queue_workflows
			UNION
			SELECT tenant_id, knowledge_base_id FROM custom_document_queue_schedule_groups
			UNION
			SELECT tenant_id, %s AS knowledge_base_id
			  FROM task_dead_letters
			 WHERE %s IS NOT NULL
		) AS residue
		JOIN knowledge_bases AS kb
		  ON kb.tenant_id = residue.tenant_id
		 AND kb.id = residue.knowledge_base_id
		 AND kb.deleted_at IS NOT NULL
		WHERE NOT EXISTS (
			SELECT 1 FROM task_pending_ops AS pending
			 WHERE pending.tenant_id = residue.tenant_id
			   AND pending.task_type = ?
			   AND pending.scope = ?
			   AND pending.scope_id = residue.knowledge_base_id
		)
		ORDER BY residue.tenant_id, residue.knowledge_base_id
		LIMIT ?`, jsonKB, jsonKB)
	var candidates []residueCandidate
	if err := c.db.WithContext(ctx).Raw(
		query, types.TypeKBDelete, types.TaskScopeKnowledgeBase, limit,
	).Scan(&candidates).Error; err != nil {
		return 0, fmt.Errorf("knowledge purge: list completed KB residue: %w", err)
	}
	cleaned := 0
	var sweepErr error
	for _, candidate := range candidates {
		if err := c.CleanupCompletedKnowledgeBase(
			ctx, candidate.TenantID, candidate.KnowledgeBaseID,
		); err != nil {
			sweepErr = errors.Join(sweepErr, err)
			continue
		}
		cleaned++
	}
	return cleaned, sweepErr
}

// CleanupCompletedKnowledgeBase removes relational and queue history left
// after an already-completed deletion. Both pre- and post-Redis checks prove
// that no active knowledge or durable KB-delete intent appeared meanwhile.
func (c *Coordinator) CleanupCompletedKnowledgeBase(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) error {
	kbID := strings.TrimSpace(knowledgeBaseID)
	if c == nil || c.db == nil || tenantID == 0 || kbID == "" {
		return errors.New("knowledge purge: completed KB cleanup requires a complete identity")
	}
	ready, err := c.completedKBReady(ctx, tenantID, kbID)
	if err != nil {
		return err
	}
	if !ready {
		return nil
	}
	if err := c.QuiesceDerivativeTasks(ctx, tenantID, kbID, nil); err != nil {
		return err
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Unscoped().Table("knowledge_bases").
			Select("id").
			Where("tenant_id = ? AND id = ? AND deleted_at IS NOT NULL", tenantID, kbID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var row struct{ ID string }
		if err := query.Take(&row).Error; err != nil {
			return fmt.Errorf("knowledge purge: lock completed KB tombstone: %w", err)
		}
		ready, err := completedKBReadyTx(tx, tenantID, kbID)
		if err != nil {
			return err
		}
		if !ready {
			return errors.New("knowledge purge: completed KB became active during residue cleanup")
		}
		if err := DeleteKnowledgeBaseArtifacts(tx, tenantID, kbID); err != nil {
			return err
		}
		logger.Infof(ctx, "[knowledge purge] removed completed KB residue tenant=%d kb=%s", tenantID, kbID)
		return nil
	})
}

func (c *Coordinator) completedKBReady(ctx context.Context, tenantID uint64, kbID string) (bool, error) {
	var ready bool
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		ready, err = completedKBReadyTx(tx, tenantID, kbID)
		return err
	})
	return ready, err
}

func completedKBReadyTx(tx *gorm.DB, tenantID uint64, kbID string) (bool, error) {
	var tombstoneCount int64
	if err := tx.Unscoped().Table("knowledge_bases").
		Where("tenant_id = ? AND id = ? AND deleted_at IS NOT NULL", tenantID, kbID).
		Count(&tombstoneCount).Error; err != nil {
		return false, fmt.Errorf("knowledge purge: inspect KB tombstone: %w", err)
	}
	if tombstoneCount != 1 {
		return false, nil
	}
	var activeKnowledge int64
	if err := tx.Table("knowledges").
		Where("tenant_id = ? AND knowledge_base_id = ? AND deleted_at IS NULL", tenantID, kbID).
		Count(&activeKnowledge).Error; err != nil {
		return false, fmt.Errorf("knowledge purge: inspect active KB knowledge: %w", err)
	}
	if activeKnowledge != 0 {
		return false, nil
	}
	var pendingDelete int64
	if err := tx.Table("task_pending_ops").Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
		tenantID, types.TypeKBDelete, types.TaskScopeKnowledgeBase, kbID,
	).Count(&pendingDelete).Error; err != nil {
		return false, fmt.Errorf("knowledge purge: inspect KB delete outbox: %w", err)
	}
	return pendingDelete == 0, nil
}

func sortedGenerationRows(values map[string]generationRow) []generationRow {
	rows := make([]generationRow, 0, len(values))
	for _, row := range values {
		rows = append(rows, row)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].KnowledgeID == rows[j].KnowledgeID {
			return rows[i].ProcessingGeneration < rows[j].ProcessingGeneration
		}
		return rows[i].KnowledgeID < rows[j].KnowledgeID
	})
	return rows
}
