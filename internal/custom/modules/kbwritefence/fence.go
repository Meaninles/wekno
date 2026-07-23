// Package kbwritefence provides the database serialization boundary between
// knowledge-base deletion and child-row writers.  Every child insert/update
// that can outlive an API request locks the parent KB first and proves it is
// still active in the same transaction as the write.
package kbwritefence

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// SQLite is a single-process deployment in this project. A long-running
// knowledge move cannot keep a SQLite write transaction open while its normal
// repository calls use other connections, so Lite mode uses this process-wide
// reader/writer gate for the same interval that PostgreSQL protects with
// parent-row SHARE/UPDATE locks. KB deletion takes the writer side below.
var sqliteLongOperationFence sync.RWMutex

var (
	// ErrKnowledgeBaseUnavailable intentionally covers missing, cross-tenant,
	// and tombstoned rows. Callers must not learn another tenant's identity,
	// and none of those states permits a child write.
	ErrKnowledgeBaseUnavailable = errors.New("knowledge base is unavailable for writes")
	ErrInvalidIdentity          = errors.New("knowledge base write fence requires a complete identity")
)

type identity struct {
	ID        string
	TenantID  uint64
	DeletedAt gorm.DeletedAt
}

// WithActive runs write in a transaction that owns the parent KB lock and has
// proved the exact tenant-scoped row is not tombstoned. PostgreSQL uses FOR
// UPDATE. SQLite has no row-lock syntax, so a harmless self-assignment first
// acquires its database write lock; this preserves the same ordering in Lite
// mode and in deterministic concurrency tests.
func WithActive(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	kbID string,
	write func(tx *gorm.DB) error,
) error {
	if db == nil || tenantID == 0 || strings.TrimSpace(kbID) == "" || write == nil {
		return ErrInvalidIdentity
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := LockActive(tx, tenantID, kbID); err != nil {
			return err
		}
		return write(tx)
	})
}

// WithActiveShared is the high-throughput variant for callbacks that only
// need to prove the KB remains active while performing an external commit.
// PostgreSQL writers share FOR SHARE locks with one another, while a KB delete
// still requires FOR UPDATE and therefore waits for every commit to finish.
// SQLite has no shared row locks and deliberately reuses the write-lock path.
func WithActiveShared(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	kbID string,
	write func(tx *gorm.DB) error,
) error {
	if db == nil || tenantID == 0 || strings.TrimSpace(kbID) == "" || write == nil {
		return ErrInvalidIdentity
	}
	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := LockActiveShared(tx, tenantID, kbID); err != nil {
			return err
		}
		return write(tx)
	})
}

// WithActiveSharedSet keeps every named KB active for the complete callback.
// PostgreSQL holds deterministic parent-row SHARE locks while work runs; the
// KB-delete path takes UPDATE on the same rows. SQLite uses the process-wide
// gate documented above, validates the rows once under that gate, and then
// runs work without holding a database transaction so nested repository
// writes can use their normal connections.
func WithActiveSharedSet(
	ctx context.Context,
	db *gorm.DB,
	tenantID uint64,
	kbIDs []string,
	work func() error,
) error {
	if db == nil || tenantID == 0 || work == nil {
		return ErrInvalidIdentity
	}
	ordered, err := orderedKnowledgeBaseIDs(kbIDs)
	if err != nil {
		return ErrInvalidIdentity
	}

	if db.Dialector.Name() == "sqlite" {
		sqliteLongOperationFence.RLock()
		defer sqliteLongOperationFence.RUnlock()
		if err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			return LockActiveSharedSet(tx, tenantID, ordered...)
		}); err != nil {
			return err
		}
		return work()
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := LockActiveSharedSet(tx, tenantID, ordered...); err != nil {
			return err
		}
		return work()
	})
}

// LockActiveSharedSet acquires parent locks in one stable order before a
// caller locks or mutates any child row. This is the common KB -> knowledge
// lock order for move finalization and other multi-KB operations.
func LockActiveSharedSet(tx *gorm.DB, tenantID uint64, kbIDs ...string) error {
	if tx == nil || tenantID == 0 {
		return ErrInvalidIdentity
	}
	ordered, err := orderedKnowledgeBaseIDs(kbIDs)
	if err != nil {
		return err
	}
	for _, kbID := range ordered {
		if err := LockActiveShared(tx, tenantID, kbID); err != nil {
			return err
		}
	}
	return nil
}

func orderedKnowledgeBaseIDs(kbIDs []string) ([]string, error) {
	ordered := make([]string, 0, len(kbIDs))
	seen := make(map[string]struct{}, len(kbIDs))
	for _, raw := range kbIDs {
		kbID := strings.TrimSpace(raw)
		if kbID == "" {
			return nil, ErrInvalidIdentity
		}
		if _, exists := seen[kbID]; exists {
			continue
		}
		seen[kbID] = struct{}{}
		ordered = append(ordered, kbID)
	}
	if len(ordered) == 0 {
		return nil, ErrInvalidIdentity
	}
	sort.Strings(ordered)
	return ordered, nil
}

// WithDeleteTransaction is the delete-side counterpart to
// WithActiveSharedSet. PostgreSQL already serializes through LockExisting's
// UPDATE row lock inside fn. SQLite additionally takes the process-wide writer
// gate so a long-running move cannot validate an active target and then race a
// tombstone before publishing its authoritative knowledge row.
func WithDeleteTransaction(
	ctx context.Context,
	db *gorm.DB,
	fn func(tx *gorm.DB) error,
) error {
	if db == nil || fn == nil {
		return ErrInvalidIdentity
	}
	if db.Dialector.Name() == "sqlite" {
		sqliteLongOperationFence.Lock()
		defer sqliteLongOperationFence.Unlock()
	}
	return db.WithContext(ctx).Transaction(fn)
}

// LockActive locks and validates an active KB inside the caller's transaction.
// It is exported for coordinators that must combine the fence with several
// child-table operations atomically.
func LockActive(tx *gorm.DB, tenantID uint64, kbID string) error {
	deleted, err := LockExisting(tx, tenantID, kbID)
	if err != nil {
		return err
	}
	if deleted {
		return ErrKnowledgeBaseUnavailable
	}
	return nil
}

// LockActiveShared validates an active KB while acquiring a PostgreSQL FOR
// SHARE lock. It is compatible with other shared writers and conflicts with
// the delete-side FOR UPDATE lock. SQLite falls back to LockActive so its
// validation/write window remains serialized at database level.
func LockActiveShared(tx *gorm.DB, tenantID uint64, kbID string) error {
	if tx == nil || tenantID == 0 || strings.TrimSpace(kbID) == "" {
		return ErrInvalidIdentity
	}
	if tx.Dialector.Name() == "sqlite" {
		return LockActive(tx, tenantID, kbID)
	}
	query := tx.Unscoped().Table("knowledge_bases").
		Select("id", "tenant_id", "deleted_at").
		Where("id = ? AND tenant_id = ?", kbID, tenantID).
		Clauses(clause.Locking{Strength: "SHARE"})
	var kb identity
	if err := query.Take(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrKnowledgeBaseUnavailable
		}
		return fmt.Errorf("lock shared knowledge base write fence: %w", err)
	}
	if kb.DeletedAt.Valid {
		return ErrKnowledgeBaseUnavailable
	}
	return nil
}

// LockTombstone is the symmetric delete-side primitive. It takes the same
// parent lock as WithActive and proves deletion has committed before cleanup
// assertions or outbox acknowledgement proceed.
func LockTombstone(tx *gorm.DB, tenantID uint64, kbID string) error {
	deleted, err := LockExisting(tx, tenantID, kbID)
	if err != nil {
		return err
	}
	if !deleted {
		return fmt.Errorf("%w: knowledge base is still active", ErrKnowledgeBaseUnavailable)
	}
	return nil
}

// LockExisting locks the exact tenant-scoped KB regardless of deletion state
// and returns whether it is tombstoned. Delete coordinators use it for
// idempotent retries while sharing precisely the same SQLite/PostgreSQL lock
// protocol as child writers.
func LockExisting(tx *gorm.DB, tenantID uint64, kbID string) (bool, error) {
	if tx == nil || tenantID == 0 || strings.TrimSpace(kbID) == "" {
		return false, ErrInvalidIdentity
	}
	if tx.Dialector.Name() == "sqlite" {
		// A qualified no-op UPDATE is intentional: SELECT alone does not stop a
		// concurrent SQLite writer from committing between validation and the
		// child INSERT. Unscoped Table avoids GORM soft-delete predicates.
		result := tx.Exec(
			"UPDATE knowledge_bases SET id = id WHERE id = ? AND tenant_id = ?",
			kbID, tenantID,
		)
		if result.Error != nil {
			return false, fmt.Errorf("lock knowledge base write fence: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return false, ErrKnowledgeBaseUnavailable
		}
	}

	query := tx.Unscoped().Table("knowledge_bases").
		Select("id", "tenant_id", "deleted_at").
		Where("id = ? AND tenant_id = ?", kbID, tenantID)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var kb identity
	if err := query.Take(&kb).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return false, ErrKnowledgeBaseUnavailable
		}
		return false, fmt.Errorf("lock knowledge base write fence: %w", err)
	}
	return kb.DeletedAt.Valid, nil
}
