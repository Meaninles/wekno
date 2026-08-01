package derivativequeue

import (
	"context"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/types"
)

// SummaryReconcileStats describes generation-fenced repairs made from the
// durable derivative ledger. Redis/Asynq is intentionally absent: it is only
// a wake-up transport and cannot prove whether business work still exists.
type SummaryReconcileStats struct {
	Preserved int
	Completed int64
	Failed    int64
}

type summaryIdentity struct {
	tenantID        uint64
	knowledgeBaseID string
	knowledgeID     string
	generation      string
}

type summaryTerminal struct {
	active    bool
	completed bool
	failed    bool
}

// ReconcileStaleSummaries projects authoritative durable work-item state back
// to stale summary_status rows. Non-terminal durable work is always
// preserved, irrespective of whether a Redis wake is queued, active, delayed,
// lost, or duplicated.
func ReconcileStaleSummaries(
	ctx context.Context,
	db *gorm.DB,
	candidates []types.Knowledge,
	cutoff time.Time,
) (SummaryReconcileStats, error) {
	var stats SummaryReconcileStats
	if db == nil {
		return stats, errors.New("reconcile summaries: database is unavailable")
	}
	if len(candidates) == 0 {
		return stats, nil
	}
	ids := make([]string, 0, len(candidates))
	for i := range candidates {
		ids = append(ids, candidates[i].ID)
	}
	var items []WorkItem
	if err := db.WithContext(ctx).
		Select("tenant_id", "knowledge_base_id", "knowledge_id", "processing_generation", "state").
		Where("knowledge_id IN ? AND work_kind = ?", ids, WorkSummary).
		Find(&items).Error; err != nil {
		return stats, fmt.Errorf("read durable summary work: %w", err)
	}
	states := make(map[summaryIdentity]summaryTerminal, len(items))
	for _, item := range items {
		key := summaryIdentity{
			tenantID: item.TenantID, knowledgeBaseID: item.KnowledgeBaseID,
			knowledgeID: item.KnowledgeID, generation: item.ProcessingGeneration,
		}
		state := states[key]
		switch item.State {
		case StateCompleted:
			state.completed = true
		case StateFailed, StateProviderUnknown, StateCancelled:
			state.failed = true
		default:
			state.active = true
		}
		states[key] = state
	}

	for i := range candidates {
		candidate := &candidates[i]
		key := summaryIdentity{
			tenantID:        candidate.TenantID,
			knowledgeBaseID: candidate.KnowledgeBaseID,
			knowledgeID:     candidate.ID,
			generation:      candidate.ProcessingGeneration,
		}
		state := states[key]
		if state.active {
			stats.Preserved++
			continue
		}
		target := types.SummaryStatusFailed
		if state.completed && !state.failed {
			target = types.SummaryStatusCompleted
		}
		result := db.WithContext(ctx).Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ?",
				candidate.TenantID, candidate.ID, candidate.KnowledgeBaseID,
				candidate.ProcessingGeneration,
			).
			Where("summary_status = ? AND updated_at < ?", types.SummaryStatusProcessing, cutoff).
			Update("summary_status", target)
		if result.Error != nil {
			return stats, fmt.Errorf("project durable summary state for %s: %w", candidate.ID, result.Error)
		}
		if target == types.SummaryStatusCompleted {
			stats.Completed += result.RowsAffected
		} else {
			stats.Failed += result.RowsAffected
		}
	}
	return stats, nil
}
