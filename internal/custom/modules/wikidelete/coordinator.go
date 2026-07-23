// Package wikidelete coordinates the durable database state required before
// deleting knowledge that may already have Wiki contributions.
package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	wikiOpIngest  = "ingest"
	wikiOpRetract = "retract"
)

var (
	// ErrInvalidRequest identifies malformed or internally inconsistent input.
	ErrInvalidRequest = errors.New("wiki delete prepare: invalid request")
	// ErrKnowledgeIdentity identifies a missing, deleted, cross-tenant, or
	// cross-knowledge-base knowledge row.
	ErrKnowledgeIdentity = errors.New("wiki delete prepare: knowledge identity mismatch")
)

// Request describes one knowledge deletion and the durable Wiki retract that
// must survive the deletion. PendingOp must identify the same tenant,
// knowledge base, and knowledge as the request.
type Request struct {
	TenantID        uint64
	KnowledgeID     string
	KnowledgeBaseID string
	PendingOp       *types.TaskPendingOp
}

// Intent is the minimal durable identity used to claim a knowledge row for
// deletion before waiting for active writers to stop. The full retract payload
// is intentionally built only after that quiescence barrier, from a fresh
// page/chunk snapshot.
type Intent struct {
	TenantID        uint64
	KnowledgeID     string
	KnowledgeBaseID string
	PendingOp       *types.TaskPendingOp
}

// Coordinator prepares knowledge deletion as one PostgreSQL transaction.
// The caller may start irreversible cleanup only after Prepare succeeds.
type Coordinator struct {
	db *gorm.DB
}

// New constructs a Wiki delete coordinator backed by db.
func New(db *gorm.DB) *Coordinator {
	return &Coordinator{db: db}
}

type knowledgeIdentity struct {
	ID              string
	TenantID        uint64
	KnowledgeBaseID string
}

type deletingKnowledge struct {
	ID          string
	StorageSize int64
}

type retractPayloadIdentity struct {
	Op          string `json:"op"`
	KnowledgeID string `json:"knowledge_id"`
}

// Prepare atomically marks every requested knowledge row as deleting,
// removes obsolete Wiki ingest operations, and ensures exactly one durable
// Wiki retract operation exists for each knowledge.
//
// PostgreSQL callers lock rows in deterministic ID order with FOR UPDATE.
// This serializes same-knowledge delete retries; migration 000071's narrowly
// scoped partial unique index additionally serializes the retract row against
// a concurrent Wiki queue consumer.
func (c *Coordinator) Prepare(ctx context.Context, requests []Request) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	ordered, err := validateAndOrderRequests(requests)
	if err != nil {
		return err
	}
	if len(ordered) == 0 {
		return nil
	}

	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		kbLocks := make([]knowledgeBaseWriteLock, 0, len(ordered))
		for _, request := range ordered {
			kbLocks = append(kbLocks, knowledgeBaseWriteLock{
				tenantID: request.TenantID, knowledgeBaseID: request.KnowledgeBaseID,
				allowDeleteTombstone: true,
			})
		}
		if err := lockKnowledgeBasesForWikiWrite(tx, kbLocks...); err != nil {
			return fmt.Errorf("%w: lock Wiki parent knowledge bases: %w", ErrKnowledgeIdentity, err)
		}

		ids := make([]string, 0, len(ordered))
		for _, request := range ordered {
			ids = append(ids, request.KnowledgeID)
		}

		query := tx.Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id").
			Where("id IN ?", ids).
			Where("deleted_at IS NULL").
			Order("id ASC")
		// SQLite is used by repository unit tests and serializes writes at the
		// database level; it does not accept SELECT ... FOR UPDATE. Production
		// PostgreSQL uses row locks so overlapping batches are deadlock-safe.
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}

		var rows []knowledgeIdentity
		if err := query.Find(&rows).Error; err != nil {
			return fmt.Errorf("lock active knowledge rows: %w", err)
		}
		if len(rows) != len(ordered) {
			return fmt.Errorf("%w: requested %d active rows, locked %d", ErrKnowledgeIdentity, len(ordered), len(rows))
		}
		for i := range rows {
			request := ordered[i]
			row := rows[i]
			if row.ID != request.KnowledgeID || row.TenantID != request.TenantID || row.KnowledgeBaseID != request.KnowledgeBaseID {
				return fmt.Errorf(
					"%w: knowledge %q belongs to tenant=%d kb=%q, request has tenant=%d kb=%q",
					ErrKnowledgeIdentity,
					row.ID,
					row.TenantID,
					row.KnowledgeBaseID,
					request.TenantID,
					request.KnowledgeBaseID,
				)
			}
		}

		result := tx.Table("knowledges").
			Where("id IN ?", ids).
			Where("deleted_at IS NULL").
			Updates(map[string]interface{}{
				"parse_status": types.ParseStatusDeleting,
				"updated_at":   time.Now().UTC(),
			})
		if result.Error != nil {
			return fmt.Errorf("mark knowledge deleting: %w", result.Error)
		}
		if result.RowsAffected != int64(len(ordered)) {
			return fmt.Errorf("%w: expected to update %d active rows, updated %d", ErrKnowledgeIdentity, len(ordered), result.RowsAffected)
		}

		for _, request := range ordered {
			if err := deleteExactIngest(tx, request); err != nil {
				return err
			}
			if err := ensureExactRetract(tx, request); err != nil {
				return err
			}
		}
		return nil
	})
}

// Begin atomically claims active knowledge rows for deletion, removes obsolete
// Wiki ingest work, and publishes a minimal executable retract. The Wiki
// worker resolves pages and unscoped chunk IDs at run time, so this remains a
// complete crash boundary without taking a stale pre-quiescence snapshot.
// Prepare later refreshes the same row with richer post-barrier provenance.
func (c *Coordinator) Begin(ctx context.Context, intents []Intent) error {
	if c == nil || c.db == nil {
		return fmt.Errorf("%w: nil database", ErrInvalidRequest)
	}
	ordered := append([]Intent(nil), intents...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].KnowledgeID < ordered[j].KnowledgeID })
	for i, intent := range ordered {
		if intent.TenantID == 0 || intent.KnowledgeID == "" || intent.KnowledgeBaseID == "" || intent.PendingOp == nil {
			return fmt.Errorf("%w at index %d: complete intent identity and minimal retract are required", ErrInvalidRequest, i)
		}
		if i > 0 && ordered[i-1].KnowledgeID == intent.KnowledgeID {
			return fmt.Errorf("%w: duplicate knowledge id %q", ErrInvalidRequest, intent.KnowledgeID)
		}
		if err := validateRetractIdentity(
			intent.TenantID, intent.KnowledgeID, intent.KnowledgeBaseID, intent.PendingOp,
		); err != nil {
			return err
		}
	}
	if len(ordered) == 0 {
		return nil
	}

	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		kbLocks := make([]knowledgeBaseWriteLock, 0, len(ordered))
		for _, intent := range ordered {
			kbLocks = append(kbLocks, knowledgeBaseWriteLock{
				tenantID: intent.TenantID, knowledgeBaseID: intent.KnowledgeBaseID,
				allowDeleteTombstone: true,
			})
		}
		if err := lockKnowledgeBasesForWikiWrite(tx, kbLocks...); err != nil {
			return fmt.Errorf("%w: lock Wiki parent knowledge bases: %w", ErrKnowledgeIdentity, err)
		}

		ids := make([]string, 0, len(ordered))
		for _, intent := range ordered {
			ids = append(ids, intent.KnowledgeID)
		}
		query := tx.Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id").
			Where("id IN ?", ids).
			Where("deleted_at IS NULL").
			Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var rows []knowledgeIdentity
		if err := query.Find(&rows).Error; err != nil {
			return fmt.Errorf("lock active knowledge rows: %w", err)
		}
		if len(rows) != len(ordered) {
			return fmt.Errorf("%w: requested %d active rows, locked %d", ErrKnowledgeIdentity, len(ordered), len(rows))
		}
		for i, row := range rows {
			intent := ordered[i]
			if row.ID != intent.KnowledgeID || row.TenantID != intent.TenantID || row.KnowledgeBaseID != intent.KnowledgeBaseID {
				return fmt.Errorf(
					"%w: knowledge %q belongs to tenant=%d kb=%q, intent has tenant=%d kb=%q",
					ErrKnowledgeIdentity, row.ID, row.TenantID, row.KnowledgeBaseID,
					intent.TenantID, intent.KnowledgeBaseID,
				)
			}
		}
		result := tx.Table("knowledges").
			Where("id IN ?", ids).
			Where("deleted_at IS NULL").
			Updates(map[string]interface{}{
				"parse_status": types.ParseStatusDeleting,
				"updated_at":   time.Now().UTC(),
			})
		if result.Error != nil {
			return fmt.Errorf("mark knowledge deleting: %w", result.Error)
		}
		if result.RowsAffected != int64(len(ordered)) {
			return fmt.Errorf("%w: expected to update %d active rows, updated %d", ErrKnowledgeIdentity, len(ordered), result.RowsAffected)
		}
		for _, intent := range ordered {
			request := Request{
				TenantID: intent.TenantID, KnowledgeID: intent.KnowledgeID,
				KnowledgeBaseID: intent.KnowledgeBaseID, PendingOp: intent.PendingOp,
			}
			if err := deleteExactIngest(tx, request); err != nil {
				return err
			}
			if err := ensureExactRetract(tx, request); err != nil {
				return err
			}
		}
		return nil
	})
}

// ClaimRecovery accepts a stale recovery task only if the row is still the
// exact deleting generation claimed by the scanner. It returns false for an
// obsolete task (manual repair, a newer worker heartbeat, or a completed
// retry), which must be acknowledged without touching the row.
func (c *Coordinator) ClaimRecovery(ctx context.Context, intent Intent, claimedAt time.Time) (bool, error) {
	if c == nil || c.db == nil || intent.TenantID == 0 || intent.KnowledgeID == "" ||
		intent.KnowledgeBaseID == "" || claimedAt.IsZero() {
		return false, fmt.Errorf("%w: complete recovery identity is required", ErrInvalidRequest)
	}
	result := c.db.WithContext(ctx).
		Table("knowledges").
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND deleted_at IS NULL AND updated_at = ?",
			intent.TenantID, intent.KnowledgeID, intent.KnowledgeBaseID,
			types.ParseStatusDeleting, claimedAt,
		).
		Update("updated_at", time.Now().UTC())
	if result.Error != nil {
		return false, fmt.Errorf("claim knowledge delete recovery: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// ContinueRecovery renews ownership for a retry of a recovery task that
// already won ClaimRecovery on its first delivery. It never revives a row:
// manual repair to completed/processing or a KB move makes the guarded update
// affect zero rows and the obsolete retry is acknowledged.
func (c *Coordinator) ContinueRecovery(ctx context.Context, intent Intent) (bool, error) {
	if c == nil || c.db == nil || intent.TenantID == 0 || intent.KnowledgeID == "" || intent.KnowledgeBaseID == "" {
		return false, fmt.Errorf("%w: complete recovery identity is required", ErrInvalidRequest)
	}
	result := c.db.WithContext(ctx).
		Table("knowledges").
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND deleted_at IS NULL",
			intent.TenantID, intent.KnowledgeID, intent.KnowledgeBaseID, types.ParseStatusDeleting,
		).
		Update("updated_at", time.Now().UTC())
	if result.Error != nil {
		return false, fmt.Errorf("continue knowledge delete recovery: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// Finalize atomically soft-deletes rows whose external resources have been
// cleaned, removes tag relations, and decrements tenant storage exactly once.
// A crash before this commit leaves parse_status=deleting for recovery; a
// crash after it cannot double-charge storage because deleted rows no longer
// satisfy the guarded query.
func (c *Coordinator) Finalize(
	ctx context.Context,
	tenantID uint64,
	knowledgeIDs []string,
) (int64, error) {
	if c == nil || c.db == nil || tenantID == 0 {
		return 0, fmt.Errorf("%w: finalize requires database and tenant", ErrInvalidRequest)
	}
	ids := append([]string(nil), knowledgeIDs...)
	sort.Strings(ids)
	for i, id := range ids {
		if id == "" || (i > 0 && ids[i-1] == id) {
			return 0, fmt.Errorf("%w: finalize knowledge IDs must be non-empty and unique", ErrInvalidRequest)
		}
	}
	if len(ids) == 0 {
		return 0, nil
	}

	var removedStorage int64
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Table("knowledges").
			Select("id", "storage_size").
			Where("tenant_id = ? AND id IN ? AND deleted_at IS NULL AND parse_status = ?",
				tenantID, ids, types.ParseStatusDeleting).
			Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var rows []deletingKnowledge
		if err := query.Find(&rows).Error; err != nil {
			return fmt.Errorf("lock deleting knowledge rows: %w", err)
		}
		if len(rows) != len(ids) {
			return fmt.Errorf("%w: expected %d deleting rows, locked %d", ErrKnowledgeIdentity, len(ids), len(rows))
		}
		for i, row := range rows {
			if row.ID != ids[i] || row.StorageSize < 0 {
				return fmt.Errorf("%w: invalid deleting row %q", ErrKnowledgeIdentity, row.ID)
			}
			removedStorage += row.StorageSize
		}

		if err := tx.Exec("DELETE FROM knowledge_tag_relations WHERE knowledge_id IN ?", ids).Error; err != nil {
			return fmt.Errorf("delete knowledge tag relations: %w", err)
		}
		// Knowledge rows are soft-deleted, so the ledger FK's ON DELETE CASCADE
		// never fires. Remove all generations explicitly in this same final
		// transaction; a failure must roll back the soft delete so recovery can
		// retry without leaving permanent completion rows.
		if err := tx.Exec(
			"DELETE FROM knowledge_fanout_completions WHERE tenant_id = ? AND knowledge_id IN ?",
			tenantID, ids,
		).Error; err != nil {
			return fmt.Errorf("delete knowledge fanout completions: %w", err)
		}
		now := time.Now().UTC()
		result := tx.Table("knowledges").
			Where("tenant_id = ? AND id IN ? AND deleted_at IS NULL AND parse_status = ?",
				tenantID, ids, types.ParseStatusDeleting).
			Updates(map[string]interface{}{
				"deleted_at":       now,
				"processing_owner": "",
				"updated_at":       now,
			})
		if result.Error != nil {
			return fmt.Errorf("soft-delete knowledge rows: %w", result.Error)
		}
		if result.RowsAffected != int64(len(ids)) {
			return fmt.Errorf("%w: expected to delete %d rows, deleted %d", ErrKnowledgeIdentity, len(ids), result.RowsAffected)
		}

		if removedStorage > 0 {
			result = tx.Exec(
				`UPDATE tenants
				 SET storage_used = CASE WHEN storage_used >= ? THEN storage_used - ? ELSE 0 END
				 WHERE id = ? AND deleted_at IS NULL`,
				removedStorage, removedStorage, tenantID,
			)
			if result.Error != nil {
				return fmt.Errorf("decrement tenant storage: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("%w: tenant %d is not active", ErrKnowledgeIdentity, tenantID)
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removedStorage, nil
}

func validateAndOrderRequests(requests []Request) ([]Request, error) {
	ordered := append([]Request(nil), requests...)
	sort.Slice(ordered, func(i, j int) bool {
		return ordered[i].KnowledgeID < ordered[j].KnowledgeID
	})
	for i := range ordered {
		request := ordered[i]
		if request.TenantID == 0 || request.KnowledgeID == "" || request.KnowledgeBaseID == "" || request.PendingOp == nil {
			return nil, fmt.Errorf("%w at index %d: tenant, knowledge, knowledge base, and pending op are required", ErrInvalidRequest, i)
		}
		if i > 0 && ordered[i-1].KnowledgeID == request.KnowledgeID {
			return nil, fmt.Errorf("%w: duplicate knowledge id %q", ErrInvalidRequest, request.KnowledgeID)
		}
		if err := validateRetractIdentity(
			request.TenantID, request.KnowledgeID, request.KnowledgeBaseID, request.PendingOp,
		); err != nil {
			return nil, err
		}
	}
	return ordered, nil
}

func validateRetractIdentity(
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	op *types.TaskPendingOp,
) error {
	if op == nil || op.ID != 0 || op.TenantID != tenantID || op.TaskType != types.TypeWikiIngest ||
		op.Scope != types.TaskScopeKnowledgeBase || op.ScopeID != knowledgeBaseID ||
		op.Op != wikiOpRetract || op.DedupKey != knowledgeID {
		return fmt.Errorf("%w for knowledge %q: retract queue identity does not match request", ErrInvalidRequest, knowledgeID)
	}
	if len(op.Payload) == 0 || !json.Valid(op.Payload) {
		return fmt.Errorf("%w for knowledge %q: retract payload must be valid JSON", ErrInvalidRequest, knowledgeID)
	}
	var payloadIdentity retractPayloadIdentity
	if err := json.Unmarshal(op.Payload, &payloadIdentity); err != nil {
		return fmt.Errorf("%w for knowledge %q: decode retract payload: %v", ErrInvalidRequest, knowledgeID, err)
	}
	if payloadIdentity.Op != wikiOpRetract || payloadIdentity.KnowledgeID != knowledgeID {
		return fmt.Errorf(
			"%w for knowledge %q: retract payload identity op=%q knowledge=%q does not match request",
			ErrInvalidRequest, knowledgeID, payloadIdentity.Op, payloadIdentity.KnowledgeID,
		)
	}
	return nil
}

func deleteExactIngest(tx *gorm.DB, request Request) error {
	result := tx.Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
		request.TenantID,
		types.TypeWikiIngest,
		types.TaskScopeKnowledgeBase,
		request.KnowledgeBaseID,
		wikiOpIngest,
		request.KnowledgeID,
	).Delete(&types.TaskPendingOp{})
	if result.Error != nil {
		return fmt.Errorf("delete Wiki ingest for knowledge %q: %w", request.KnowledgeID, result.Error)
	}
	return nil
}

func ensureExactRetract(tx *gorm.DB, request Request) error {
	op := *request.PendingOp
	op.Payload = append([]byte(nil), request.PendingOp.Payload...)
	if op.EnqueuedAt.IsZero() {
		op.EnqueuedAt = time.Now().UTC()
	}

	// The partial unique index installed by migration 000071 is the
	// serialization point between deletion retries and a concurrent Wiki
	// consumer. A single UPSERT cannot observe an existing row and then miss it
	// if the consumer deletes/archive-moves that row between a COUNT and UPDATE.
	// The conflict-target predicate must textually match the partial index or
	// PostgreSQL cannot infer it (SQLSTATE 42P10).
	const upsert = `
		INSERT INTO task_pending_ops
			(tenant_id, task_type, scope, scope_id, op, dedup_key, payload, fail_count, enqueued_at, claimed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, 0, ?, NULL)
		ON CONFLICT (tenant_id, task_type, scope, scope_id, op, dedup_key)
			WHERE task_type = 'wiki:ingest'
			  AND scope = 'knowledge_base'
			  AND op = 'retract'
		DO UPDATE SET
			payload = EXCLUDED.payload,
			fail_count = 0,
			enqueued_at = EXCLUDED.enqueued_at,
			claimed_at = NULL`
	if err := tx.Exec(
		upsert,
		op.TenantID,
		op.TaskType,
		op.Scope,
		op.ScopeID,
		op.Op,
		op.DedupKey,
		op.Payload,
		op.EnqueuedAt,
	).Error; err != nil {
		return fmt.Errorf("upsert Wiki retract for knowledge %q: %w", request.KnowledgeID, err)
	}
	return nil
}
