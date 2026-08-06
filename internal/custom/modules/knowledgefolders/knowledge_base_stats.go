package knowledgefolders

import (
	"context"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
)

// GetKnowledgeBaseTaskStats applies exactly the same workflow projection used
// by folder statistics, but covers every active document including root-level
// documents that do not belong to a folder.
func (s *Service) GetKnowledgeBaseTaskStats(
	ctx context.Context,
	kbID string,
) (*KnowledgeBaseTaskStats, error) {
	tenantID, err := tenantIDFromContext(ctx)
	if err != nil {
		return nil, err
	}
	var stats KnowledgeBaseTaskStats
	query := fmt.Sprintf(`
		SELECT
			COUNT(*) AS document_count,
			COALESCE(SUM(CASE WHEN parse_status = 'pending' THEN 1 ELSE 0 END), 0) AS parse_pending_count,
			COALESCE(SUM(CASE WHEN parse_status IN ('processing', 'cancelling') THEN 1 ELSE 0 END), 0) AS parse_running_count,
			COALESCE(SUM(CASE WHEN pending_subtasks_count > 0 THEN pending_subtasks_count ELSE 0 END), 0) AS enrichment_pending_task_count,
			COALESCE(SUM(CASE WHEN COALESCE(wiki_status, '') = 'pending' THEN 1 ELSE 0 END), 0) AS wiki_pending_task_count,
			COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END), 0) AS abnormal_document_count,
			COALESCE(SUM(CASE WHEN %s THEN 1 ELSE 0 END), 0) AS failed_document_count
		FROM knowledges
		WHERE tenant_id = ?
		  AND knowledge_base_id = ?
		  AND deleted_at IS NULL
		  AND parse_status <> ?`,
		folderStatsAbnormalSQL("knowledges"),
		folderStatsTerminalFailureSQL("knowledges"),
	)
	if err := s.db.WithContext(ctx).Raw(
		query, tenantID, kbID, types.ParseStatusDeleting,
	).Scan(&stats).Error; err != nil {
		return nil, err
	}
	return &stats, nil
}
