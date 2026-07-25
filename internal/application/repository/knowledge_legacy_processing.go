package repository

import (
	"context"
	"errors"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

// RepairLegacyProcessing is the sole write path for generation-less tasks
// left in Redis during the processing-ownership rolling upgrade. The empty
// generation/owner predicates are part of the UPDATE, so a legacy task cannot
// terminalize a row claimed by any current producer.
func (r *knowledgeRepository) RepairLegacyProcessing(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	expectedStatus string,
	expectedProcessedAt bool,
	repairGeneration string,
	completeCore bool,
	message string,
	updatedAt time.Time,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		repairGeneration == "" || message == "" || updatedAt.IsZero() {
		return false, errors.New("repair legacy processing: complete identity is required")
	}
	switch expectedStatus {
	case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing:
	default:
		return false, errors.New("repair legacy processing: active expected status is required")
	}

	query := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(processing_generation, '') = '' AND COALESCE(processing_owner, '') = '' AND deleted_at IS NULL",
			tenantID,
			knowledgeID,
			knowledgeBaseID,
			expectedStatus,
		)
	if expectedProcessedAt {
		query = query.Where("processed_at IS NOT NULL")
	} else {
		query = query.Where("processed_at IS NULL")
	}
	usableCoreSQL := `? AND EXISTS (
		SELECT 1 FROM chunks
		WHERE chunks.knowledge_id = knowledges.id
		  AND chunks.tenant_id = knowledges.tenant_id
		  AND chunks.deleted_at IS NULL
	)`
	result := query.Updates(map[string]interface{}{
		"parse_status": gorm.Expr(
			"CASE WHEN "+usableCoreSQL+" THEN ? ELSE ? END",
			completeCore,
			types.ParseStatusCompleted,
			types.ParseStatusFailed,
		),
		"processing_generation":  repairGeneration,
		"processing_owner":       "",
		"processing_fanout":      nil,
		"pending_subtasks_count": 0,
		"enrichment_status": gorm.Expr(
			"CASE WHEN "+usableCoreSQL+" THEN ? ELSE ? END",
			completeCore,
			types.EnrichmentStatusDegraded,
			types.EnrichmentStatusNone,
		),
		"wiki_status":        types.WikiStatusNone,
		"wiki_error_message": "",
		"error_message": gorm.Expr(
			"CASE WHEN "+usableCoreSQL+" THEN '' ELSE ? END",
			completeCore,
			message,
		),
		"summary_status": gorm.Expr(
			"CASE WHEN summary_status IN (?, ?) THEN ? ELSE summary_status END",
			types.SummaryStatusPending,
			types.SummaryStatusProcessing,
			types.SummaryStatusFailed,
		),
		"updated_at": updatedAt,
	})
	return result.RowsAffected == 1, result.Error
}
