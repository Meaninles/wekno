package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"
)

const (
	deleteRecoveryInterval = time.Minute
	deleteRecoveryTimeout  = 15 * time.Second
	deleteRecoveryLimit    = 500
	deleteTaskTimeout      = 6 * time.Hour
	deleteTaskUniqueTTL    = 5 * time.Minute
	// A normal delete worker owns the row for at most deleteTaskTimeout. Only
	// rows older than that window are recoverable; scanning fresh deleting rows
	// used to launch a duplicate per-document cleanup beside the original batch.
	deleteRecoveryStaleAfter = deleteTaskTimeout + 5*time.Minute
)

// Recovery republishes per-document delete tasks for durable deleting rows.
// parse_status=deleting is committed in the same transaction as the Wiki
// retract, so it is a trustworthy crash-recovery intent rather than a
// transient UI state.
type Recovery struct {
	db       *gorm.DB
	enqueuer interfaces.TaskEnqueuer

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewRecovery(db *gorm.DB, enqueuer interfaces.TaskEnqueuer) *Recovery {
	return &Recovery{db: db, enqueuer: enqueuer}
}

func (r *Recovery) Start(parent context.Context) {
	if r == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	r.mu.Unlock()
	go r.run(ctx, done)
}

func (r *Recovery) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil || done == nil {
		return
	}
	cancel()
	<-done
}

func (r *Recovery) run(ctx context.Context, done chan struct{}) {
	defer func() {
		close(done)
		r.mu.Lock()
		if r.done == done {
			r.cancel, r.done = nil, nil
		}
		r.mu.Unlock()
	}()
	r.runCycle(ctx)
	ticker := time.NewTicker(deleteRecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runCycle(ctx)
		}
	}
}

func (r *Recovery) runCycle(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, deleteRecoveryTimeout)
	defer cancel()
	if err := r.RecoverNow(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[knowledge delete recovery] scan failed; deleting rows remain durable: %v", err)
	}
}

type deletingScope struct {
	TenantID        uint64    `gorm:"column:tenant_id"`
	KnowledgeID     string    `gorm:"column:id"`
	KnowledgeBaseID string    `gorm:"column:knowledge_base_id"`
	UpdatedAt       time.Time `gorm:"column:updated_at"`
}

// RecoverNow performs one bounded stale-intent scan and publishes stable
// wake-up tasks. A successful publish advances updated_at with a compare-and-
// swap claim; only the delete worker's guarded Finalize closes the intent.
func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil || r.db == nil || r.enqueuer == nil {
		return errors.New("knowledge delete recovery: dependencies unavailable")
	}
	claimCutoff := time.Now().UTC().Add(-deleteRecoveryStaleAfter)
	var rows []deletingScope
	if err := r.db.WithContext(ctx).
		Table("knowledges").
		Select("tenant_id, id, knowledge_base_id, updated_at").
		Where("parse_status = ? AND deleted_at IS NULL AND updated_at <= ?", types.ParseStatusDeleting, claimCutoff).
		Order("updated_at ASC, tenant_id ASC, id ASC").
		Limit(deleteRecoveryLimit).
		Scan(&rows).Error; err != nil {
		return fmt.Errorf("knowledge delete recovery: list deleting rows: %w", err)
	}

	var errs []error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(errs...), err)
		}
		claimedAt := time.Now().UTC()
		// Claim the exact stale generation before publishing. This prevents a
		// fast worker from observing the pre-claim timestamp, and makes multiple
		// recovery replicas elect exactly one publisher. If enqueue fails we
		// compare-and-swap the timestamp back so the next scan can retry now.
		result := r.db.WithContext(ctx).
			Table("knowledges").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND deleted_at IS NULL AND updated_at = ?",
				row.TenantID, row.KnowledgeID, row.KnowledgeBaseID,
				types.ParseStatusDeleting, row.UpdatedAt,
			).
			Update("updated_at", claimedAt)
		if result.Error != nil {
			errs = append(errs, fmt.Errorf("claim delete recovery for %s: %w", row.KnowledgeID, result.Error))
			continue
		}
		if result.RowsAffected != 1 {
			continue
		}
		payload, err := json.Marshal(types.KnowledgeListDeletePayload{
			TenantID:                row.TenantID,
			KnowledgeIDs:            []string{row.KnowledgeID},
			ExpectedKnowledgeBaseID: row.KnowledgeBaseID,
			RecoveryClaimedAt:       &claimedAt,
		})
		if err != nil {
			errs = append(errs, err)
			_ = r.restoreClaim(ctx, row, claimedAt)
			continue
		}
		task := asynq.NewTask(types.TypeKnowledgeListDelete, payload)
		_, err = r.enqueuer.Enqueue(
			task,
			asynq.Queue(types.QueueLow),
			asynq.MaxRetry(10),
			asynq.Timeout(deleteTaskTimeout),
			asynq.ProcessIn(time.Second),
			asynq.Unique(deleteTaskUniqueTTL),
		)
		if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
			errs = append(errs, fmt.Errorf("enqueue delete recovery for %s: %w", row.KnowledgeID, err))
			if restoreErr := r.restoreClaim(ctx, row, claimedAt); restoreErr != nil {
				errs = append(errs, restoreErr)
			}
			continue
		}
	}
	if len(rows) > 0 {
		logger.Infof(ctx, "[knowledge delete recovery] ensured tasks for %d deleting rows", len(rows))
	}
	return errors.Join(errs...)
}

func (r *Recovery) restoreClaim(ctx context.Context, row deletingScope, claimedAt time.Time) error {
	result := r.db.WithContext(ctx).
		Table("knowledges").
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND deleted_at IS NULL AND updated_at = ?",
			row.TenantID, row.KnowledgeID, row.KnowledgeBaseID,
			types.ParseStatusDeleting, claimedAt,
		).
		Update("updated_at", row.UpdatedAt)
	if result.Error != nil {
		return fmt.Errorf("restore failed delete recovery claim for %s: %w", row.KnowledgeID, result.Error)
	}
	return nil
}
