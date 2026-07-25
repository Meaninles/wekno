package knowledgeworkflowfilter

import (
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

var (
	derivativeSuccess = []string{"", "none", "completed", "done", "skipped"}
	derivativeActive  = []string{"pending", "processing"}
)

const (
	summaryStatusSQL    = "LOWER(COALESCE(knowledges.summary_status, ''))"
	enrichmentStatusSQL = "LOWER(COALESCE(knowledges.enrichment_status, ''))"
	wikiStatusSQL       = "LOWER(COALESCE(knowledges.wiki_status, ''))"
)

// Apply projects the user-visible document workflow status onto the durable
// core + derivative lifecycle columns. Keep this separate from ParseStatus:
// internal callers still need the raw core state for retrieval and recovery,
// while the knowledge-base UI must not call a document "completed" when
// summary, questions/graph, or Wiki is pending or failed.
func Apply(query *gorm.DB, workflowStatus string) *gorm.DB {
	status := strings.ToLower(strings.TrimSpace(workflowStatus))
	switch status {
	case "":
		return query
	case types.ParseStatusPending:
		return query.Where("knowledges.parse_status = ?", types.ParseStatusPending)
	case types.ParseStatusProcessing:
		return query.Where(
			`knowledges.parse_status IN ? OR (
				knowledges.parse_status = ? AND (
					`+summaryStatusSQL+` IN ? OR
					`+enrichmentStatusSQL+` IN ? OR
					`+wikiStatusSQL+` IN ?
				)
			)`,
			[]string{types.ParseStatusProcessing, types.ParseStatusFinalizing},
			types.ParseStatusCompleted,
			derivativeActive,
			derivativeActive,
			derivativeActive,
		)
	case types.ParseStatusCompleted:
		return query.Where(
			`knowledges.parse_status = ? AND
				`+summaryStatusSQL+` IN ? AND
				`+enrichmentStatusSQL+` IN ? AND
				`+wikiStatusSQL+` IN ?`,
			types.ParseStatusCompleted,
			derivativeSuccess,
			derivativeSuccess,
			derivativeSuccess,
		)
	case types.ParseStatusFailed:
		return query.Where(
			`knowledges.parse_status = ? OR (
				knowledges.parse_status = ? AND
				`+summaryStatusSQL+` NOT IN ? AND
				`+enrichmentStatusSQL+` NOT IN ? AND
				`+wikiStatusSQL+` NOT IN ? AND NOT (
					`+summaryStatusSQL+` IN ? AND
					`+enrichmentStatusSQL+` IN ? AND
					`+wikiStatusSQL+` IN ?
				)
			)`,
			types.ParseStatusFailed,
			types.ParseStatusCompleted,
			derivativeActive,
			derivativeActive,
			derivativeActive,
			derivativeSuccess,
			derivativeSuccess,
			derivativeSuccess,
		)
	default:
		// Preserve access to less common raw states (draft/cancelled) without
		// weakening the four documented workflow projections above.
		return query.Where("knowledges.parse_status = ?", status)
	}
}
