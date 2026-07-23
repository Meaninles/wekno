package wikidelete

import (
	"fmt"
	"sort"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

const knowledgeBaseDeleteOperation = "delete"

type knowledgeBaseWriteLock struct {
	tenantID             uint64
	knowledgeBaseID      string
	allowDeleteTombstone bool
}

// lockKnowledgeBasesForWikiMove locks both move endpoints in the same stable
// order used by every other Wiki coordinator and reports each endpoint's
// authoritative tombstone state.  A move settlement is recovery work: unlike
// an ordinary Wiki write it must be able to clear the exact document marker
// after either KB has been soft-deleted, but it must never recreate queue work
// for that deleted endpoint.
func lockKnowledgeBasesForWikiMove(
	tx *gorm.DB,
	tenantID uint64,
	sourceKnowledgeBaseID, targetKnowledgeBaseID string,
) (sourceDeleted, targetDeleted bool, err error) {
	if tx == nil || tenantID == 0 || sourceKnowledgeBaseID == "" ||
		targetKnowledgeBaseID == "" || sourceKnowledgeBaseID == targetKnowledgeBaseID {
		return false, false, kbwritefence.ErrInvalidIdentity
	}

	ordered := []string{sourceKnowledgeBaseID, targetKnowledgeBaseID}
	sort.Strings(ordered)
	deletedByID := make(map[string]bool, len(ordered))
	for _, knowledgeBaseID := range ordered {
		deleted, lockErr := kbwritefence.LockExisting(tx, tenantID, knowledgeBaseID)
		if lockErr != nil {
			return false, false, fmt.Errorf("lock Wiki move parent KB %q: %w", knowledgeBaseID, lockErr)
		}
		deletedByID[knowledgeBaseID] = deleted
	}
	return deletedByID[sourceKnowledgeBaseID], deletedByID[targetKnowledgeBaseID], nil
}

// lockKnowledgeBasesForWikiWrite establishes one global lock order for Wiki
// deletion/move coordinators before they lock knowledge rows. Ordinary writes
// require an active parent. A source retract may run after a whole-KB
// tombstone only while the exact durable KB-delete intent still exists; once
// Complete consumes that intent, no retry can recreate Wiki queue state.
func lockKnowledgeBasesForWikiWrite(tx *gorm.DB, requested ...knowledgeBaseWriteLock) error {
	if tx == nil {
		return kbwritefence.ErrInvalidIdentity
	}
	type key struct {
		tenantID        uint64
		knowledgeBaseID string
	}
	locks := make(map[key]knowledgeBaseWriteLock, len(requested))
	for _, request := range requested {
		if request.tenantID == 0 || request.knowledgeBaseID == "" {
			return kbwritefence.ErrInvalidIdentity
		}
		k := key{tenantID: request.tenantID, knowledgeBaseID: request.knowledgeBaseID}
		if current, exists := locks[k]; exists {
			// Active-only is the stricter contract if two operations in the same
			// transaction refer to one KB with different permissions.
			current.allowDeleteTombstone = current.allowDeleteTombstone && request.allowDeleteTombstone
			locks[k] = current
			continue
		}
		locks[k] = request
	}
	ordered := make([]knowledgeBaseWriteLock, 0, len(locks))
	for _, request := range locks {
		ordered = append(ordered, request)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].tenantID != ordered[j].tenantID {
			return ordered[i].tenantID < ordered[j].tenantID
		}
		return ordered[i].knowledgeBaseID < ordered[j].knowledgeBaseID
	})

	for _, request := range ordered {
		deleted, err := kbwritefence.LockExisting(tx, request.tenantID, request.knowledgeBaseID)
		if err != nil {
			return fmt.Errorf("lock Wiki parent KB %q: %w", request.knowledgeBaseID, err)
		}
		if !deleted {
			continue
		}
		if !request.allowDeleteTombstone {
			return fmt.Errorf("lock Wiki parent KB %q: %w", request.knowledgeBaseID, kbwritefence.ErrKnowledgeBaseUnavailable)
		}

		var count int64
		err = tx.Model(&types.TaskPendingOp{}).
			Where(
				"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
				request.tenantID,
				types.TypeKBDelete,
				types.TaskScopeKnowledgeBase,
				request.knowledgeBaseID,
				knowledgeBaseDeleteOperation,
				request.knowledgeBaseID,
			).
			Count(&count).Error
		if err != nil {
			return fmt.Errorf("verify KB-delete intent for Wiki cleanup %q: %w", request.knowledgeBaseID, err)
		}
		if count != 1 {
			return fmt.Errorf(
				"verify KB-delete intent for Wiki cleanup %q: %w (found %d exact intents)",
				request.knowledgeBaseID,
				kbwritefence.ErrKnowledgeBaseUnavailable,
				count,
			)
		}
	}
	return nil
}
