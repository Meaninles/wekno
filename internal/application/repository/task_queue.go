package repository

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// taskPendingOpsRepository implements interfaces.TaskPendingOpsRepository.
type taskPendingOpsRepository struct {
	db *gorm.DB
}

// NewTaskPendingOpsRepository constructs a GORM-backed implementation.
func NewTaskPendingOpsRepository(db *gorm.DB) interfaces.TaskPendingOpsRepository {
	return &taskPendingOpsRepository{db: db}
}

// AcquireWikiIngestLease is the production-only extension used by the durable
// Wiki coordinator after it acquires the Redis/Lite process lock. Keeping it
// structural avoids widening the generic queue interface, while the service
// fails closed when its production repository does not implement this method.
func (r *taskPendingOpsRepository) AcquireWikiIngestLease(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (wikilease.Identity, error) {
	return wikilease.Acquire(ctx, r.db, tenantID, knowledgeBaseID)
}

// withWikiLeaseMutation wraps queue bookkeeping performed by a durable Wiki
// worker. Tombstoned KBs are allowed here because terminal queue draining is a
// delete-side operation; the exact lease still prevents a former worker from
// deleting/incrementing/archiving rows after a replacement advanced the epoch.
func (r *taskPendingOpsRepository) withWikiLeaseMutation(
	ctx context.Context,
	mutation func(tx *gorm.DB, identity wikilease.Identity) error,
) (bool, error) {
	identity, hasIdentity := wikilease.IdentityFromContext(ctx)
	if !wikilease.Required(ctx) && !hasIdentity {
		return false, nil
	}
	if !hasIdentity {
		return true, wikilease.ErrLeaseRequired
	}
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := kbwritefence.LockExisting(tx, identity.TenantID, identity.KnowledgeBaseID); err != nil {
			return err
		}
		if err := wikiingestguard.ValidateScope(
			ctx, tx, identity.TenantID, identity.KnowledgeBaseID,
		); err != nil {
			return err
		}
		return mutation(tx, identity)
	})
	return true, err
}

// Enqueue inserts a single op. Callers must populate TenantID/TaskType/
// Scope/ScopeID/Op (Payload optional). ID, FailCount default to zero;
// EnqueuedAt is filled with the DB-side default if left zero.
func (r *taskPendingOpsRepository) Enqueue(ctx context.Context, op *types.TaskPendingOp) error {
	if op == nil {
		return errors.New("task pending ops: nil op")
	}
	if op.TaskType == "" || op.Scope == "" || op.ScopeID == "" {
		return errors.New("task pending ops: task_type, scope, scope_id are required")
	}
	if op.Op == "" {
		return errors.New("task pending ops: op is required")
	}
	if len(op.Payload) == 0 {
		// Make sure the JSONB column never sees NULL — the schema sets a
		// default but explicit "{}" keeps the row uniform regardless of
		// driver-level default handling.
		op.Payload = []byte("{}")
	}
	if op.EnqueuedAt.IsZero() {
		// EnqueuedAt is not named CreatedAt, so GORM does not treat it as an
		// auto-create timestamp. Without an explicit value GORM includes Go's
		// year-0001 zero time in the INSERT and bypasses the database DEFAULT.
		op.EnqueuedAt = time.Now().UTC()
	}
	if op.TaskType == types.TypeWikiIngest && op.Scope == types.TaskScopeKnowledgeBase {
		return kbwritefence.WithActive(ctx, r.db, op.TenantID, op.ScopeID, func(tx *gorm.DB) error {
			if err := wikiingestguard.ValidateScope(ctx, tx, op.TenantID, op.ScopeID); err != nil {
				return err
			}
			// Wiki producers are deliberately replayable: document
			// finalization, recovery scans, and a restarted replica may all
			// publish the same generation. The partial unique indexes make
			// the durable row the idempotency boundary; DO NOTHING turns a
			// conflicting replay into success without replacing a richer
			// Map/page checkpoint already stored in the canonical payload.
			return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(op).Error
		})
	}
	return r.db.WithContext(ctx).Create(op).Error
}

// WithActiveWikiKnowledgeBase serializes a Wiki wake-up publication with KB
// deletion. The callback intentionally runs while the parent-row lock is
// owned: if publication wins, deletion subsequently observes/cancels the
// signal; if deletion wins, the tombstone prevents a late signal entirely.
//
// This is an optional capability discovered structurally by the Wiki service;
// keeping it out of the generic pending-op interface avoids coupling unrelated
// queues to a Wiki-only external-publication contract.
func (r *taskPendingOpsRepository) WithActiveWikiKnowledgeBase(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	publish func() error,
) error {
	guarded, err := r.withWikiLeaseMutation(ctx, func(_ *gorm.DB, identity wikilease.Identity) error {
		if identity.TenantID != tenantID || identity.KnowledgeBaseID != knowledgeBaseID {
			return wikiingestguard.ErrInvalidIdentity
		}
		return publish()
	})
	if guarded {
		return err
	}
	return kbwritefence.WithActive(ctx, r.db, tenantID, knowledgeBaseID, func(tx *gorm.DB) error {
		if err := wikiingestguard.ValidateScope(ctx, tx, tenantID, knowledgeBaseID); err != nil {
			return err
		}
		return publish()
	})
}

// PeekBatch returns up to `limit` rows for the (task_type, scope, scope_id)
// tuple. Wiki work is ordered by the bounded-retry rotation
// (lowest fail_count, never-attempted first, then oldest attempt/id) so a
// poisoned head row cannot monopolize every batch. Other generic consumers
// retain strict id ASC ordering. Rows are not removed; callers must
// DeleteByIDs once they have been consumed (or IncrFailCount and leave
// them for the next pass). `limit` <= 0 falls back to 1; we clamp the
// upper bound generously so callers can pull large windows when they
// know the consumer can handle them.
func (r *taskPendingOpsRepository) PeekBatch(
	ctx context.Context,
	taskType, scope, scopeID string,
	limit int,
) ([]*types.TaskPendingOp, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	var ops []*types.TaskPendingOp
	query := r.db.WithContext(ctx).
		Where("task_type = ? AND scope = ? AND scope_id = ?", taskType, scope, scopeID)
	if taskType == types.TypeWikiIngest && scope == types.TaskScopeKnowledgeBase {
		query = applyWikiRetryRotation(query)
	} else {
		query = query.Order("id ASC")
	}
	if err := query.Limit(limit).Find(&ops).Error; err != nil {
		return nil, err
	}
	return ops, nil
}

// GetWikiIngestByDedupKey resolves one exact document-generation row for a
// distributed Wiki Map worker. The normalized tuple is part of the lookup so
// a delayed or forged task cannot address another tenant/KB/op by row id.
func (r *taskPendingOpsRepository) GetWikiIngestByDedupKey(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	dedupKey string,
) (*types.TaskPendingOp, error) {
	if tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(dedupKey) == "" {
		return nil, errors.New("task pending ops: complete Wiki Map identity is required")
	}
	var op types.TaskPendingOp
	err := r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			tenantID,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
			"ingest",
			dedupKey,
		).
		First(&op).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

// ClaimWikiMapDispatch validates the disposable wake-up epoch and extends its
// database reservation into a short renewable execution lease. A stale Redis
// message returns nil and is safe to ACK; PostgreSQL remains authoritative.
func (r *taskPendingOpsRepository) ClaimWikiMapDispatch(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	dedupKey string,
	epoch uint64,
	lease time.Duration,
) (*types.TaskPendingOp, error) {
	if tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" ||
		strings.TrimSpace(dedupKey) == "" || epoch == 0 {
		return nil, nil
	}
	if lease <= 0 {
		lease = 45 * time.Second
	}
	var op types.TaskPendingOp
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ? AND map_ready_at IS NULL AND map_dispatch_epoch = ? AND map_dispatch_task_id <> ''",
			tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			knowledgeBaseID, "ingest", dedupKey, epoch,
		).Clauses(clause.Locking{Strength: "UPDATE"}).First(&op).Error; err != nil {
			return err
		}
		expectedTaskID := fmt.Sprintf("wiki-map:%d:%d", op.ID, epoch)
		result := tx.Table("task_pending_ops").
			Where(
				"id = ? AND map_dispatch_epoch = ? AND map_dispatch_task_id = ? AND map_ready_at IS NULL",
				op.ID, epoch, expectedTaskID,
			).
			Update("map_dispatch_lease_until", time.Now().UTC().Add(lease))
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		op.MapDispatchEpoch = epoch
		op.MapDispatchTaskID = expectedTaskID
		until := time.Now().UTC().Add(lease)
		op.MapDispatchLeaseUntil = &until
		return nil
	})
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &op, nil
}

func (r *taskPendingOpsRepository) RenewWikiMapDispatch(
	ctx context.Context,
	id int64,
	epoch uint64,
	lease time.Duration,
) (bool, error) {
	if id <= 0 || epoch == 0 {
		return false, nil
	}
	if lease <= 0 {
		lease = 45 * time.Second
	}
	result := r.db.WithContext(ctx).Table("task_pending_ops").
		Where("id = ? AND map_dispatch_epoch = ? AND map_ready_at IS NULL", id, epoch).
		Update("map_dispatch_lease_until", time.Now().UTC().Add(lease))
	return result.RowsAffected == 1, result.Error
}

// DeferWikiMapDispatch returns a row to the PostgreSQL due queue without
// changing fail_count. This is used for capacity/circuit waits and disposable
// lock conflicts; the maintenance dispatcher publishes a fresh epoch later.
func (r *taskPendingOpsRepository) DeferWikiMapDispatch(
	ctx context.Context,
	id int64,
	epoch uint64,
	delay time.Duration,
) error {
	if id <= 0 || epoch == 0 {
		return nil
	}
	if delay < time.Second {
		delay = time.Second
	}
	now := time.Now().UTC()
	return r.db.WithContext(ctx).Table("task_pending_ops").
		Where("id = ? AND map_dispatch_epoch = ? AND map_ready_at IS NULL", id, epoch).
		Updates(map[string]any{
			"claimed_at":               now,
			"next_attempt_at":          now.Add(delay),
			"map_dispatch_task_id":     "",
			"map_dispatch_lease_until": nil,
		}).Error
}

// DeleteWikiIngestByDedupKey terminally removes one stale generation using
// the same complete identity as the distributed Map lookup. It is safe to
// race KB deletion (both remove the row) and cannot touch another tenant even
// if an external payload reuses textual identifiers.
func (r *taskPendingOpsRepository) DeleteWikiIngestByDedupKey(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	dedupKey string,
) error {
	if tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" || strings.TrimSpace(dedupKey) == "" {
		return errors.New("task pending ops: complete stale Wiki Map identity is required")
	}
	return r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			tenantID,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
			"ingest",
			dedupKey,
		).
		Delete(&types.TaskPendingOp{}).Error
}

func wikiCommitReadyPredicate(dialect string) string {
	retractReady := "COALESCE(payload->>'retract_plan_state', '') <> 'intent'"
	if dialect == "sqlite" {
		retractReady = "COALESCE(json_extract(payload, '$.retract_plan_state'), '') <> 'intent'"
	}
	return "(op = 'ingest' AND map_ready_at IS NOT NULL)" +
		" OR (op = 'retract' AND " + retractReady + ")" +
		" OR op NOT IN ('ingest', 'retract')"
}

func applyWikiRetryRotation(query *gorm.DB) *gorm.DB {
	return query.
		Order("fail_count ASC").
		Order("CASE WHEN claimed_at IS NULL THEN 0 ELSE 1 END ASC").
		Order("claimed_at ASC").
		Order("id ASC")
}

// PeekWikiCommitBatch returns only rows whose document-local work is ready for
// the shared-KB materialization boundary. Unprepared ingest rows are
// deliberately bypassed, so one slow document cannot head-of-line block
// hundreds of already-mapped documents.
func (r *taskPendingOpsRepository) PeekWikiCommitBatch(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	limit int,
) ([]*types.TaskPendingOp, error) {
	if limit <= 0 {
		limit = 1
	}
	if limit > 1000 {
		limit = 1000
	}
	var ops []*types.TaskPendingOp
	err := r.db.WithContext(ctx).
		Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
			tenantID,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
		).
		Where(wikiCommitReadyPredicate(r.db.Dialector.Name())).
		Scopes(applyWikiRetryRotation).
		Limit(limit).
		Find(&ops).Error
	if err != nil {
		return nil, err
	}
	return ops, nil
}

// CountWikiCommitReady is the cheap follow-up predicate for the commit lane.
// Distributed Map workers wake the lane when they publish a new ready row;
// counting all unprepared rows here would otherwise create a permanent
// five-second polling loop while long documents are still mapping.
func (r *taskPendingOpsRepository) CountWikiCommitReady(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).
		Model(&types.TaskPendingOp{}).
		Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
			tenantID,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
		).
		Where(wikiCommitReadyPredicate(r.db.Dialector.Name())).
		Count(&count).Error
	return count, err
}

// UpdateWikiPayload checkpoints service-owned Wiki progress without widening
// the generic queue interface. The source generation is validated under the
// same KB -> knowledge lock order as Wiki page/log writes, and the exact queue
// tuple prevents a stale worker from repurposing an unrelated row ID.
func (r *taskPendingOpsRepository) UpdateWikiPayload(
	ctx context.Context,
	id int64,
	tenantID uint64,
	knowledgeBaseID string,
	payload []byte,
) (bool, error) {
	if id <= 0 || tenantID == 0 || knowledgeBaseID == "" || len(payload) == 0 {
		return false, errors.New("task pending ops: complete Wiki payload checkpoint identity is required")
	}
	updated := false
	err := kbwritefence.WithActive(ctx, r.db, tenantID, knowledgeBaseID, func(tx *gorm.DB) error {
		if err := wikiingestguard.ValidateScope(ctx, tx, tenantID, knowledgeBaseID); err != nil {
			return err
		}
		result := tx.Model(&types.TaskPendingOp{}).
			Where("id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ?",
				id, tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, knowledgeBaseID, "ingest").
			Update("payload", payload)
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected == 1
		return nil
	})
	return updated, err
}

// MarkWikiMapReady publishes a complete document-local Map result. Payload
// and readiness marker share one transaction and one generation validation,
// so a commit worker can never materialize stale or partially-written output.
func (r *taskPendingOpsRepository) MarkWikiMapReady(
	ctx context.Context,
	id int64,
	tenantID uint64,
	knowledgeBaseID string,
	payload []byte,
) (bool, error) {
	if id <= 0 || tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" || len(payload) == 0 {
		return false, errors.New("task pending ops: complete Wiki Map checkpoint identity is required")
	}
	updated := false
	err := kbwritefence.WithActive(ctx, r.db, tenantID, knowledgeBaseID, func(tx *gorm.DB) error {
		if err := wikiingestguard.ValidateScope(ctx, tx, tenantID, knowledgeBaseID); err != nil {
			return err
		}
		now := time.Now().UTC()
		result := tx.Exec(
			`UPDATE task_pending_ops
			 SET payload = ?, map_ready_at = ?,
			     map_dispatch_task_id = '', map_dispatch_lease_until = NULL
			 WHERE id = ? AND tenant_id = ? AND task_type = ?
			   AND scope = ? AND scope_id = ? AND op = ?`,
			payload,
			now,
			id,
			tenantID,
			types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase,
			knowledgeBaseID,
			"ingest",
		)
		if result.Error != nil {
			return result.Error
		}
		updated = result.RowsAffected == 1
		return nil
	})
	return updated, err
}

// DeleteByIDs removes the given rows in one statement. Empty input is a
// no-op so the caller can invoke unconditionally at the end of a batch.
func (r *taskPendingOpsRepository) DeleteByIDs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	guarded, err := r.withWikiLeaseMutation(ctx, func(tx *gorm.DB, identity wikilease.Identity) error {
		return tx.Where(
			"id IN ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
			ids, identity.TenantID, types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase, identity.KnowledgeBaseID,
		).Delete(&types.TaskPendingOp{}).Error
	})
	if guarded {
		return err
	}
	return r.db.WithContext(ctx).
		Where("id IN ?", ids).
		Delete(&types.TaskPendingOp{}).Error
}

// ArchiveToDeadLetter atomically removes one pending op and writes its
// dead-letter record. The delete is a compare-and-swap on fail_count: if a
// coordinator has renewed/reset the row since the caller observed it, the
// stale archive attempt becomes an idempotent no-op. Any validation or INSERT
// failure rolls the delete back.
func (r *taskPendingOpsRepository) ArchiveToDeadLetter(
	ctx context.Context,
	pendingID int64,
	dl *types.TaskDeadLetter,
) error {
	archive := func(tx *gorm.DB, identity *wikilease.Identity) error {
		if dl == nil {
			return validateAndDefaultDeadLetter(dl)
		}
		if identity != nil && (dl.TenantID != identity.TenantID || dl.TaskType != types.TypeWikiIngest ||
			dl.Scope != types.TaskScopeKnowledgeBase || dl.ScopeID != identity.KnowledgeBaseID) {
			return wikiingestguard.ErrInvalidIdentity
		}
		query := tx.Where("id = ? AND fail_count = ?", pendingID, dl.FailCount)
		if identity != nil {
			query = query.Where(
				"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
				identity.TenantID, types.TypeWikiIngest,
				types.TaskScopeKnowledgeBase, identity.KnowledgeBaseID,
			)
		}
		result := query.
			Delete(&types.TaskPendingOp{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}

		if err := validateAndDefaultDeadLetter(dl); err != nil {
			return err
		}
		return tx.Create(dl).Error
	}
	guarded, err := r.withWikiLeaseMutation(ctx, func(tx *gorm.DB, identity wikilease.Identity) error {
		return archive(tx, &identity)
	})
	if guarded {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return archive(tx, nil) })
}

// IncrFailCount atomically bumps fail_count, records the attempt timestamp,
// and returns the
// new value. We use UPDATE ... RETURNING so the read+write happens in
// one round trip and races between concurrent IncrFailCount callers
// resolve to monotonic counts.
//
// A missing row returns (0, nil): the caller's ID may have been removed
// by a concurrent DeleteByIDs (e.g. dead-letter path), which is benign.
func (r *taskPendingOpsRepository) IncrFailCount(ctx context.Context, id int64) (int, error) {
	var newCount int
	guarded, err := r.withWikiLeaseMutation(ctx, func(tx *gorm.DB, identity wikilease.Identity) error {
		return tx.Raw(
			`UPDATE task_pending_ops
			 SET fail_count = fail_count + 1, claimed_at = CURRENT_TIMESTAMP
			 WHERE id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?
			 RETURNING fail_count`,
			id, identity.TenantID, types.TypeWikiIngest,
			types.TaskScopeKnowledgeBase, identity.KnowledgeBaseID,
		).Scan(&newCount).Error
	})
	if guarded {
		return newCount, err
	}
	err = r.db.WithContext(ctx).Raw(
		`UPDATE task_pending_ops
		 SET fail_count = fail_count + 1, claimed_at = CURRENT_TIMESTAMP
		 WHERE id = ? RETURNING fail_count`,
		id,
	).Scan(&newCount).Error
	if err != nil {
		return 0, err
	}
	return newCount, nil
}

// TouchWikiAttempt rotates a budget-free Wiki delivery behind untouched work
// without incrementing fail_count. It is used for circuit/admission rejects
// that never reached the provider: later ready documents may continue, while
// the rejected row remains durable for the scheduled follow-up.
func (r *taskPendingOpsRepository) TouchWikiAttempt(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("task pending ops: positive Wiki attempt id is required")
	}
	update := func(tx *gorm.DB, identity *wikilease.Identity) error {
		query := tx.Model(&types.TaskPendingOp{}).Where("id = ?", id)
		if identity != nil {
			query = query.Where(
				"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
				identity.TenantID,
				types.TypeWikiIngest,
				types.TaskScopeKnowledgeBase,
				identity.KnowledgeBaseID,
			)
		}
		return query.Update("claimed_at", time.Now().UTC()).Error
	}
	guarded, err := r.withWikiLeaseMutation(ctx, func(tx *gorm.DB, identity wikilease.Identity) error {
		return update(tx, &identity)
	})
	if guarded {
		return err
	}
	return update(r.db.WithContext(ctx), nil)
}

// PendingCount returns how many rows are currently queued for the
// tuple. Covered by idx_task_pending_ops_scope.
func (r *taskPendingOpsRepository) PendingCount(
	ctx context.Context,
	taskType, scope, scopeID string,
) (int64, error) {
	var n int64
	if err := r.db.WithContext(ctx).
		Model(&types.TaskPendingOp{}).
		Where("task_type = ? AND scope = ? AND scope_id = ?", taskType, scope, scopeID).
		Count(&n).Error; err != nil {
		return 0, err
	}
	return n, nil
}

// ExistsByDedupKey reports whether the queue contains at least one row
// matching (task_type, scope, scope_id, dedup_key), optionally narrowed to
// an exact op. It selects only the primary key and stops after one row so
// lifecycle probes do not deserialize payloads or scan the rest of a large
// KB queue.
func (r *taskPendingOpsRepository) ExistsByDedupKey(
	ctx context.Context,
	taskType, scope, scopeID, dedupKey, op string,
) (bool, error) {
	if taskType == "" || scope == "" || scopeID == "" || dedupKey == "" {
		return false, errors.New("task pending ops: task_type, scope, scope_id, dedup_key are required")
	}

	q := r.db.WithContext(ctx).
		Model(&types.TaskPendingOp{}).
		Select("id").
		Where("task_type = ? AND scope = ? AND scope_id = ? AND dedup_key = ?",
			taskType, scope, scopeID, dedupKey)
	if op != "" {
		q = q.Where("op = ?", op)
	}

	var row types.TaskPendingOp
	res := q.Limit(1).Find(&row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

func escapePendingDedupLikePrefix(prefix string) string {
	prefix = strings.ReplaceAll(prefix, `\`, `\\`)
	prefix = strings.ReplaceAll(prefix, `%`, `\%`)
	prefix = strings.ReplaceAll(prefix, `_`, `\_`)
	return prefix
}

// ExistsByDedupKeyPrefix reports whether any generation-scoped row exists for
// a service-owned prefix. Equality filters on the queue tuple and operation
// keep the prefix range bounded by idx_task_pending_ops_lookup.
func (r *taskPendingOpsRepository) ExistsByDedupKeyPrefix(
	ctx context.Context,
	taskType, scope, scopeID, dedupKeyPrefix, op string,
) (bool, error) {
	if taskType == "" || scope == "" || scopeID == "" || dedupKeyPrefix == "" {
		return false, errors.New("task pending ops: task_type, scope, scope_id, dedup_key_prefix are required")
	}
	q := r.db.WithContext(ctx).
		Model(&types.TaskPendingOp{}).
		Select("id").
		Where("task_type = ? AND scope = ? AND scope_id = ?", taskType, scope, scopeID).
		Where(`dedup_key LIKE ? ESCAPE '\'`, escapePendingDedupLikePrefix(dedupKeyPrefix)+"%")
	if op != "" {
		q = q.Where("op = ?", op)
	}
	var row types.TaskPendingOp
	res := q.Limit(1).Find(&row)
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// DeleteByDedupKey drops rows in the tuple whose dedup_key matches.
// If `op` is non-empty, only rows with the matching op are dropped;
// otherwise every matching row is removed. Empty dedup_key is rejected
// to prevent accidentally wiping the entire queue for a KB.
//
// Used by:
//   - Wiki delete path: scrub queued WikiOpIngest entries for a
//     knowledge that is being deleted, while preserving WikiOpRetract
//     so the cleanup can still unlink pages.
//   - Wiki reparse path: same scrub of pending ingests so the new
//     parse can repopulate cleanly.
func (r *taskPendingOpsRepository) DeleteByDedupKey(
	ctx context.Context,
	taskType, scope, scopeID, dedupKey, op string,
) error {
	if dedupKey == "" {
		return fmt.Errorf("task pending ops: empty dedup_key in DeleteByDedupKey")
	}
	q := r.db.WithContext(ctx).
		Where("task_type = ? AND scope = ? AND scope_id = ? AND dedup_key = ?",
			taskType, scope, scopeID, dedupKey)
	if op != "" {
		q = q.Where("op = ?", op)
	}
	return q.Delete(&types.TaskPendingOp{}).Error
}

func (r *taskPendingOpsRepository) DeleteByDedupKeyPrefix(
	ctx context.Context,
	taskType, scope, scopeID, dedupKeyPrefix, op string,
) error {
	if taskType == "" || scope == "" || scopeID == "" || dedupKeyPrefix == "" {
		return errors.New("task pending ops: task_type, scope, scope_id, dedup_key_prefix are required")
	}
	q := r.db.WithContext(ctx).
		Where("task_type = ? AND scope = ? AND scope_id = ?", taskType, scope, scopeID).
		Where(`dedup_key LIKE ? ESCAPE '\'`, escapePendingDedupLikePrefix(dedupKeyPrefix)+"%")
	if op != "" {
		q = q.Where("op = ?", op)
	}
	return q.Delete(&types.TaskPendingOp{}).Error
}

// taskDeadLetterRepository implements interfaces.TaskDeadLetterRepository.
type taskDeadLetterRepository struct {
	db *gorm.DB
}

// NewTaskDeadLetterRepository constructs a GORM-backed implementation.
func NewTaskDeadLetterRepository(db *gorm.DB) interfaces.TaskDeadLetterRepository {
	return &taskDeadLetterRepository{db: db}
}

// Insert records one dead letter. Best-effort caller: the asynq
// middleware swallows the error so a failed insert never masks the
// underlying task error.
func (r *taskDeadLetterRepository) Insert(ctx context.Context, dl *types.TaskDeadLetter) error {
	if err := validateAndDefaultDeadLetter(dl); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Create(dl).Error
}

// validateAndDefaultDeadLetter centralizes the invariants shared by
// direct dead-letter inserts and atomic pending-op archival. Keep the
// validation/default order stable so Insert retains its existing behavior.
func validateAndDefaultDeadLetter(dl *types.TaskDeadLetter) error {
	if dl == nil {
		return errors.New("task dead letters: nil entry")
	}
	if dl.TaskType == "" {
		return errors.New("task dead letters: task_type is required")
	}
	if dl.Scope == "" {
		dl.Scope = types.TaskScopeUnknown
	}
	if len(dl.Payload) == 0 {
		dl.Payload = []byte("{}")
	}
	if dl.FailedAt.IsZero() {
		dl.FailedAt = time.Now()
	}
	return nil
}

// ListByScope returns dead letters for (scope, scope_id) newest-first
// with a stringified id cursor. `limit` is clamped to [1, 200]. Empty
// nextCursor signals the tail.
func (r *taskDeadLetterRepository) ListByScope(
	ctx context.Context,
	scope, scopeID, cursor string,
	limit int,
) ([]*types.TaskDeadLetter, string, error) {
	if scope == "" || scopeID == "" {
		return nil, "", errors.New("task dead letters: scope and scope_id are required")
	}
	return r.list(ctx, cursor, limit, func(q *gorm.DB) *gorm.DB {
		return q.Where("scope = ? AND scope_id = ?", scope, scopeID)
	})
}

// ListByTaskType returns dead letters for the given task_type
// newest-first with a stringified id cursor. Same clamping rules.
func (r *taskDeadLetterRepository) ListByTaskType(
	ctx context.Context,
	taskType, cursor string,
	limit int,
) ([]*types.TaskDeadLetter, string, error) {
	if taskType == "" {
		return nil, "", errors.New("task dead letters: task_type is required")
	}
	return r.list(ctx, cursor, limit, func(q *gorm.DB) *gorm.DB {
		return q.Where("task_type = ?", taskType)
	})
}

// list is the shared cursor pagination implementation, parametrized by
// the caller-supplied filter. Mirrors wikiLogEntryRepository.List.
func (r *taskDeadLetterRepository) list(
	ctx context.Context,
	cursor string,
	limit int,
	filter func(*gorm.DB) *gorm.DB,
) ([]*types.TaskDeadLetter, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	q := r.db.WithContext(ctx).Order("id DESC").Limit(limit)
	q = filter(q)

	if cursor != "" {
		cursorID, err := strconv.ParseInt(cursor, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor %q: %w", cursor, err)
		}
		q = q.Where("id < ?", cursorID)
	}

	var rows []*types.TaskDeadLetter
	if err := q.Find(&rows).Error; err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(rows) == limit {
		nextCursor = strconv.FormatInt(rows[len(rows)-1].ID, 10)
	}
	return rows, nextCursor, nil
}

// DeleteByID drops a single dead letter row. Returns nil even if the
// row is already gone — operators issuing concurrent deletes shouldn't
// see spurious errors.
func (r *taskDeadLetterRepository) DeleteByID(ctx context.Context, id int64) error {
	return r.db.WithContext(ctx).
		Where("id = ?", id).
		Delete(&types.TaskDeadLetter{}).Error
}
