package knowledgepurge

import (
	"errors"
	"fmt"
	"sort"

	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type wikiLogKnowledgeKey struct {
	tenantID    uint64
	knowledgeID string
}

type wikiLogKnowledgeState struct {
	ID          string
	TenantID    uint64
	ParseStatus string
	DeletedAt   gorm.DeletedAt
}

// RetainWikiLogsForActiveKnowledge removes events whose source document has
// entered deletion from a batch before it is inserted. The read and eventual
// INSERT must run in the caller's transaction. On PostgreSQL, shared row locks
// serialize this check with the deleting-row update:
//
//   - a log committed first is subsequently removed by deletion finalization;
//   - a deletion claim committed first makes this function suppress the log.
//
// This closes the late Wiki-retract/Map writer race without waiting for the
// slow KB-scoped Wiki pipeline. Missing source rows are retained for legacy
// and administrative callers; an actual document deletion keeps a soft
// tombstone, so deleting/deleted state remains authoritative.
func RetainWikiLogsForActiveKnowledge(
	tx *gorm.DB,
	entries []*types.WikiLogEntry,
) ([]*types.WikiLogEntry, error) {
	if tx == nil {
		return nil, errors.New("filter Wiki logs: nil database transaction")
	}
	if len(entries) == 0 {
		return entries, nil
	}

	idsByTenant := make(map[uint64]map[string]struct{})
	for _, entry := range entries {
		if entry == nil || entry.TenantID == 0 || entry.KnowledgeID == "" {
			continue
		}
		if idsByTenant[entry.TenantID] == nil {
			idsByTenant[entry.TenantID] = make(map[string]struct{})
		}
		idsByTenant[entry.TenantID][entry.KnowledgeID] = struct{}{}
	}
	if len(idsByTenant) == 0 {
		return entries, nil
	}

	tenantIDs := make([]uint64, 0, len(idsByTenant))
	for tenantID := range idsByTenant {
		tenantIDs = append(tenantIDs, tenantID)
	}
	sort.Slice(tenantIDs, func(i, j int) bool { return tenantIDs[i] < tenantIDs[j] })

	states := make(map[wikiLogKnowledgeKey]wikiLogKnowledgeState)
	for _, tenantID := range tenantIDs {
		ids := make([]string, 0, len(idsByTenant[tenantID]))
		for knowledgeID := range idsByTenant[tenantID] {
			ids = append(ids, knowledgeID)
		}
		sort.Strings(ids)

		query := tx.Unscoped().Table("knowledges").
			Select("id", "tenant_id", "parse_status", "deleted_at").
			Where("tenant_id = ? AND id IN ?", tenantID, ids).
			Order("id ASC")
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "SHARE"})
		}
		var rows []wikiLogKnowledgeState
		if err := query.Find(&rows).Error; err != nil {
			return nil, fmt.Errorf(
				"filter Wiki logs: load source state for tenant %d: %w",
				tenantID,
				err,
			)
		}
		for _, row := range rows {
			states[wikiLogKnowledgeKey{
				tenantID:    row.TenantID,
				knowledgeID: row.ID,
			}] = row
		}
	}

	retained := make([]*types.WikiLogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry == nil || entry.TenantID == 0 || entry.KnowledgeID == "" {
			retained = append(retained, entry)
			continue
		}
		state, found := states[wikiLogKnowledgeKey{
			tenantID:    entry.TenantID,
			knowledgeID: entry.KnowledgeID,
		}]
		if found && (state.DeletedAt.Valid || state.ParseStatus == types.ParseStatusDeleting) {
			continue
		}
		retained = append(retained, entry)
	}
	return retained, nil
}
