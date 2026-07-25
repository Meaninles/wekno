package repository

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// KnowledgeSpanRepository persists the per-attempt span tree used by the
// processing pipeline. Operations are deliberately narrow:
//
//   - Upsert covers Begin/End/Fail/Skip — every state transition routes
//     through the same write so the row stays internally consistent.
//   - NextAttempt allocates a new attempt for re-parses without touching
//     historical rows. Old attempts stay queryable for post-mortem.
//   - ListByAttempt is the only read path; the handler builds the tree
//     in memory rather than recursing through the DB.
type KnowledgeSpanRepository interface {
	Upsert(ctx context.Context, row *types.KnowledgeProcessingSpan) error
	// CreateNextAttemptRoot atomically allocates the next per-document
	// attempt, terminalizes every older pending/running span, and inserts the
	// new root. PostgreSQL callers are serialized by a transaction-scoped
	// advisory lock, so two application pods cannot allocate the same attempt.
	// The returned count is the number of older open rows superseded.
	CreateNextAttemptRoot(
		ctx context.Context,
		root *types.KnowledgeProcessingSpan,
		supersedeErrorCode string,
		supersedeReason string,
	) (attempt int, superseded int64, err error)
	NextAttempt(ctx context.Context, knowledgeID string) (int, error)
	LatestAttempt(ctx context.Context, knowledgeID string) (int, error)
	ListByAttempt(ctx context.Context, knowledgeID string, attempt int) ([]types.KnowledgeProcessingSpan, error)
	GetSpan(ctx context.Context, knowledgeID string, attempt int, spanID string) (*types.KnowledgeProcessingSpan, error)
	// CancelDescendants marks every descendant of a parent span as
	// "cancelled" with the given reason. Used by the tracker to
	// cascade an upstream failure across a stage's downstream subtree
	// without iterating in Go memory.
	CancelDescendants(ctx context.Context, knowledgeID string, attempt int, parentSpanID, reason string) (int64, error)
	// CancelAllOpenSpans flips every non-terminal (pending/running) span
	// for (knowledgeID, attempt) to "cancelled" in one statement,
	// regardless of tree position. Used by the user-cancel path where
	// fan-out stages (e.g. "多模态识别") flip themselves to done as soon
	// as they finish dispatching, while their async children are still
	// running — a tree walk that stops at terminal parents would miss
	// those orphan leaves.
	CancelAllOpenSpans(ctx context.Context, knowledgeID string, attempt int, errorCode, reason string) (int64, error)
	// CancelOpenSpansByName flips pending/running rows with the given span
	// name for (knowledgeID, attempt). Used before re-opening a subspan
	// after asynq retry or server restart so the trace tree does not
	// accumulate duplicate postprocess.summary / question rows.
	CancelOpenSpansByName(ctx context.Context, knowledgeID string, attempt int, name, errorCode, reason string) (int64, error)
}

type knowledgeSpanRepository struct {
	db *gorm.DB
	// SQLite cannot be shared by horizontally scaled pods and has no
	// transaction-scoped advisory lock. Serializing allocation inside the
	// single process gives its supported local mode the same invariant.
	sqliteAttemptMu sync.Mutex
}

// NewKnowledgeSpanRepository wires the GORM-backed implementation.
func NewKnowledgeSpanRepository(db *gorm.DB) KnowledgeSpanRepository {
	return &knowledgeSpanRepository{db: db}
}

func (r *knowledgeSpanRepository) Upsert(ctx context.Context, row *types.KnowledgeProcessingSpan) error {
	if row == nil || row.KnowledgeID == "" || row.SpanID == "" {
		return errors.New("knowledgeSpanRepository.Upsert: knowledge_id and span_id required")
	}
	if row.Attempt == 0 {
		row.Attempt = 1
	}
	// We let GORM populate created_at/updated_at via the autoCreate /
	// autoUpdate tags. ON CONFLICT updates only the fields that may
	// transition between calls — name/kind/parent are immutable once
	// set so we don't list them in DoUpdates (saves a few bytes per
	// write, and any mismatch indicates a programming error).
	//
	// CRITICAL: input / output / metadata are CONTENT fields that
	// individual call sites only fill when they have something to set.
	// EndSpan e.g. only sets `output`; if we always listed `input` in
	// DoUpdates, the End call would clobber the input set by Begin with
	// NULL. Same for metadata. Build the DoUpdates list dynamically and
	// skip these three columns when the incoming row has nothing to
	// write — so "no value" preserves the existing column instead of
	// nuking it.
	cols := []string{
		"status",
		"error_code",
		"error_message",
		"error_detail",
		"started_at",
		"finished_at",
		"duration_ms",
		"updated_at",
	}
	if row.Input != nil {
		cols = append(cols, "input")
	}
	if row.Output != nil {
		cols = append(cols, "output")
	}
	if row.Metadata != nil {
		cols = append(cols, "metadata")
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "knowledge_id"},
			{Name: "attempt"},
			{Name: "span_id"},
		},
		DoUpdates: clause.AssignmentColumns(cols),
	}).Create(row).Error
}

func (r *knowledgeSpanRepository) CreateNextAttemptRoot(
	ctx context.Context,
	root *types.KnowledgeProcessingSpan,
	supersedeErrorCode string,
	supersedeReason string,
) (attempt int, superseded int64, err error) {
	if root == nil || root.KnowledgeID == "" || root.SpanID == "" {
		return 0, 0, errors.New(
			"knowledgeSpanRepository.CreateNextAttemptRoot: knowledge_id and span_id required",
		)
	}
	if root.Kind != types.SpanKindRoot {
		return 0, 0, fmt.Errorf(
			"knowledgeSpanRepository.CreateNextAttemptRoot: kind %q is not root",
			root.Kind,
		)
	}
	if supersedeErrorCode == "" {
		supersedeErrorCode = "SUPERSEDED_ATTEMPT"
	}
	if supersedeReason == "" {
		supersedeReason = "a newer document-processing attempt was accepted"
	}

	isSQLite := r.db != nil && r.db.Dialector != nil &&
		r.db.Dialector.Name() == "sqlite"
	if isSQLite {
		r.sqliteAttemptMu.Lock()
		defer r.sqliteAttemptMu.Unlock()
	}

	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector != nil && tx.Dialector.Name() == "postgres" {
			// The lock key is stable across application pods yet namespaced
			// away from unrelated advisory-lock users. It is released
			// automatically on commit/rollback.
			if lockErr := tx.Exec(
				"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
				"weknora:knowledge-span-attempt:"+root.KnowledgeID,
			).Error; lockErr != nil {
				return fmt.Errorf("lock knowledge span attempt allocation: %w", lockErr)
			}
		}

		var maxAttempt int
		if scanErr := tx.Model(&types.KnowledgeProcessingSpan{}).
			Where("knowledge_id = ?", root.KnowledgeID).
			Select("COALESCE(MAX(attempt), 0)").
			Row().
			Scan(&maxAttempt); scanErr != nil {
			return fmt.Errorf("read latest knowledge span attempt: %w", scanErr)
		}
		attempt = maxAttempt + 1

		now := time.Now()
		result := tx.Model(&types.KnowledgeProcessingSpan{}).
			Where(
				"knowledge_id = ? AND attempt < ? AND status IN ?",
				root.KnowledgeID,
				attempt,
				[]string{types.SpanStatusPending, types.SpanStatusRunning},
			).
			Updates(map[string]any{
				"status":        types.SpanStatusCancelled,
				"error_code":    supersedeErrorCode,
				"error_message": supersedeReason,
				"finished_at":   now,
				"updated_at":    now,
			})
		if result.Error != nil {
			return fmt.Errorf("supersede older knowledge spans: %w", result.Error)
		}
		superseded = result.RowsAffected

		candidate := *root
		candidate.Attempt = attempt
		if createErr := tx.Create(&candidate).Error; createErr != nil {
			return fmt.Errorf("create knowledge span attempt root: %w", createErr)
		}
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	root.Attempt = attempt
	return attempt, superseded, nil
}

func (r *knowledgeSpanRepository) NextAttempt(ctx context.Context, knowledgeID string) (int, error) {
	var max int
	err := r.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ?", knowledgeID).
		Select("COALESCE(MAX(attempt), 0)").
		Row().Scan(&max)
	if err != nil {
		return 0, err
	}
	return max + 1, nil
}

func (r *knowledgeSpanRepository) LatestAttempt(ctx context.Context, knowledgeID string) (int, error) {
	var max int
	err := r.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ?", knowledgeID).
		Select("COALESCE(MAX(attempt), 0)").
		Row().Scan(&max)
	return max, err
}

func (r *knowledgeSpanRepository) ListByAttempt(ctx context.Context, knowledgeID string, attempt int) ([]types.KnowledgeProcessingSpan, error) {
	if knowledgeID == "" {
		return nil, nil
	}
	var rows []types.KnowledgeProcessingSpan
	q := r.db.WithContext(ctx).Where("knowledge_id = ?", knowledgeID)
	if attempt > 0 {
		q = q.Where("attempt = ?", attempt)
	}
	// id ASC keeps the natural insertion order — useful for stable
	// rendering of fan-out subspans (e.g. multimodal.image[0..N] in
	// the order they were enqueued).
	err := q.Order("id ASC").Find(&rows).Error
	return rows, err
}

func (r *knowledgeSpanRepository) GetSpan(ctx context.Context, knowledgeID string, attempt int, spanID string) (*types.KnowledgeProcessingSpan, error) {
	var row types.KnowledgeProcessingSpan
	err := r.db.WithContext(ctx).
		Where("knowledge_id = ? AND attempt = ? AND span_id = ?", knowledgeID, attempt, spanID).
		Take(&row).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &row, nil
}

// CancelDescendants performs an iterative SQL walk: each level we update
// every row whose parent_span_id is in the previous level's span_id set,
// flipping pending/running rows to cancelled. We bail when a level adds
// zero rows (fixed point reached) or after a generous depth bound.
//
// Postgres-specific WITH RECURSIVE would be denser but harder to test on
// the SQLite Lite backend. The iterative path stays portable.
func (r *knowledgeSpanRepository) CancelDescendants(ctx context.Context, knowledgeID string, attempt int, parentSpanID, reason string) (int64, error) {
	frontier := []string{parentSpanID}
	var totalAffected int64
	for depth := 0; depth < 16 && len(frontier) > 0; depth++ {
		var nextFrontier []string
		// Find children of every span currently on the frontier
		// that are still in a non-terminal state — terminal rows
		// (done/failed/skipped/cancelled) are left as-is so the UI
		// can still see their original outcome.
		var children []types.KnowledgeProcessingSpan
		err := r.db.WithContext(ctx).
			Where("knowledge_id = ? AND attempt = ? AND parent_span_id IN ? AND status IN ?",
				knowledgeID, attempt, frontier,
				[]string{types.SpanStatusPending, types.SpanStatusRunning}).
			Find(&children).Error
		if err != nil {
			return totalAffected, err
		}
		if len(children) == 0 {
			break
		}
		ids := make([]string, 0, len(children))
		for _, c := range children {
			ids = append(ids, c.SpanID)
			nextFrontier = append(nextFrontier, c.SpanID)
		}
		res := r.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
			Where("knowledge_id = ? AND attempt = ? AND span_id IN ?", knowledgeID, attempt, ids).
			Updates(map[string]any{
				"status":        types.SpanStatusCancelled,
				"error_code":    "UPSTREAM_FAILED",
				"error_message": reason,
			})
		if res.Error != nil {
			return totalAffected, res.Error
		}
		totalAffected += res.RowsAffected
		frontier = nextFrontier
	}
	return totalAffected, nil
}

// CancelAllOpenSpans is the "abort the attempt" counterpart to
// CancelDescendants. It avoids the BFS entirely so spans whose parent
// is already terminal (typical for stage fan-outs that EndSpan as soon
// as they finish dispatching async work) still get flipped to cancelled.
// We deliberately do NOT touch finished_at / duration_ms here — the
// span row remains observable in the trace tree with its original
// start time and gets a cancelled status + reason, which is enough
// for the UI to drop the running-bar styling.
func (r *knowledgeSpanRepository) CancelAllOpenSpans(
	ctx context.Context, knowledgeID string, attempt int, errorCode, reason string,
) (int64, error) {
	now := time.Now()
	updates := map[string]any{
		"status":        types.SpanStatusCancelled,
		"error_code":    errorCode,
		"error_message": reason,
		"finished_at":   now,
		"updated_at":    now,
	}
	res := r.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ? AND attempt = ? AND status IN ?",
			knowledgeID, attempt,
			[]string{types.SpanStatusPending, types.SpanStatusRunning}).
		Updates(updates)
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}

func (r *knowledgeSpanRepository) CancelOpenSpansByName(
	ctx context.Context, knowledgeID string, attempt int, name, errorCode, reason string,
) (int64, error) {
	if knowledgeID == "" || attempt <= 0 || name == "" {
		return 0, nil
	}
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ? AND attempt = ? AND name = ? AND status IN ?",
			knowledgeID, attempt, name,
			[]string{types.SpanStatusPending, types.SpanStatusRunning}).
		Updates(map[string]any{
			"status":        types.SpanStatusCancelled,
			"error_code":    errorCode,
			"error_message": reason,
			"finished_at":   now,
			"updated_at":    now,
		})
	if res.Error != nil {
		return 0, res.Error
	}
	return res.RowsAffected, nil
}
