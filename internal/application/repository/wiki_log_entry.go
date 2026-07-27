package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgepurge"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiingestguard"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// wikiLogEntryRepository implements interfaces.WikiLogEntryRepository.
type wikiLogEntryRepository struct {
	db *gorm.DB
}

// NewWikiLogEntryRepository constructs a GORM-backed WikiLogEntryRepository.
func NewWikiLogEntryRepository(db *gorm.DB) interfaces.WikiLogEntryRepository {
	return &wikiLogEntryRepository{db: db}
}

// AppendBatch inserts every entry in one statement. Empty batches are a
// no-op so callers can invoke unconditionally at the end of a wiki ingest
// batch without guarding against the "no events this round" case.
//
// Queue-backed entries carry task_pending_ops.id in source_op_id. The unique
// index on that nullable column plus ON CONFLICT makes this write durable and
// idempotent across retries after a later index/publication/queue-settlement
// failure. Legacy and administrative entries have a NULL source_op_id; SQL
// unique semantics deliberately allow more than one of those rows.
func (r *wikiLogEntryRepository) AppendBatch(ctx context.Context, entries []*types.WikiLogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	type kbKey struct {
		tenantID uint64
		kbID     string
	}
	keySet := make(map[kbKey]struct{})
	for index, entry := range entries {
		if entry == nil || entry.TenantID == 0 || entry.KnowledgeBaseID == "" {
			return fmt.Errorf("append wiki log entries: entry %d has an incomplete knowledge-base identity", index)
		}
		keySet[kbKey{tenantID: entry.TenantID, kbID: entry.KnowledgeBaseID}] = struct{}{}
	}
	keys := make([]kbKey, 0, len(keySet))
	for key := range keySet {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].tenantID != keys[j].tenantID {
			return keys[i].tenantID < keys[j].tenantID
		}
		return keys[i].kbID < keys[j].kbID
	})
	// Keep the original plain-INSERT path for batches that have no durable
	// source key (legacy/admin callers). Besides avoiding an unnecessary
	// conflict clause, GORM reliably scans every generated ID back into such
	// slices. Queue-backed production batches take the idempotent path below;
	// their caller never relies on returned log IDs.
	queueBacked := false
	for _, entry := range entries {
		if entry != nil && entry.SourceOpID != nil {
			queueBacked = true
			break
		}
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, key := range keys {
			if err := kbwritefence.LockActive(tx, key.tenantID, key.kbID); err != nil {
				return fmt.Errorf("append wiki log entries: lock tenant=%d KB=%s: %w", key.tenantID, key.kbID, err)
			}
		}
		if tenantID, knowledgeBaseID, ok := wikiingestguard.Scope(ctx); ok {
			if len(keys) != 1 || keys[0].tenantID != tenantID || keys[0].kbID != knowledgeBaseID {
				return wikiingestguard.ErrInvalidIdentity
			}
			if err := wikiingestguard.ValidateScope(ctx, tx, tenantID, knowledgeBaseID); err != nil {
				return err
			}
		} else if err := wikiingestguard.Validate(ctx, tx); err != nil {
			return err
		}
		retained, err := knowledgepurge.RetainWikiLogsForActiveKnowledge(tx, entries)
		if err != nil {
			return err
		}
		if len(retained) == 0 {
			return nil
		}
		if !queueBacked {
			return tx.Create(&retained).Error
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "source_op_id"}},
			DoNothing: true,
		}).Create(&retained).Error
	})
}

// List returns up to `limit` entries strictly older than `cursor`, newest
// first. `cursor` is the stringified ID of the oldest entry from the
// previous page; an empty string starts from the newest. Callers get back
// a nextCursor string to pass on the next request — empty means no more.
//
// `limit` is clamped to [1, 200]. Values outside that range are coerced
// silently to keep the handler simple.
func (r *wikiLogEntryRepository) List(
	ctx context.Context,
	kbID string,
	cursor string,
	limit int,
) ([]*types.WikiLogEntry, string, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}

	q := r.db.WithContext(ctx).
		Where("knowledge_base_id = ?", kbID).
		Order("id DESC").
		Limit(limit)

	if cursor != "" {
		cursorID, err := strconv.ParseUint(cursor, 10, 64)
		if err != nil {
			return nil, "", fmt.Errorf("invalid cursor %q: %w", cursor, err)
		}
		q = q.Where("id < ?", cursorID)
	}

	var entries []*types.WikiLogEntry
	if err := q.Find(&entries).Error; err != nil {
		return nil, "", err
	}

	nextCursor := ""
	// Only emit a cursor when we actually filled the page — a short page
	// is guaranteed to be the tail, so returning a cursor would just cause
	// the frontend to fire a final empty request.
	if len(entries) == limit {
		nextCursor = strconv.FormatUint(entries[len(entries)-1].ID, 10)
	}
	return entries, nextCursor, nil
}

// DeleteByKB drops every log entry for a KB. No "affected rows" signal is
// surfaced — missing rows are a legitimate state (e.g., a KB that was
// created before wiki_log_entries existed) and not a failure condition.
func (r *wikiLogEntryRepository) DeleteByKB(ctx context.Context, kbID string) error {
	if kbID == "" {
		return errors.New("wiki log entries: empty kb id")
	}
	return r.db.WithContext(ctx).
		Where("knowledge_base_id = ?", kbID).
		Delete(&types.WikiLogEntry{}).Error
}
