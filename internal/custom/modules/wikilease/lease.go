// Package wikilease provides a durable fencing token for the per-KB Wiki
// ingest coordinator. Redis (or the Lite in-process lock) is only a liveness
// hint: every successful owner must advance this PostgreSQL/SQLite epoch, and
// every durable side effect validates the exact epoch/token in its transaction.
package wikilease

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const Table = "custom_wiki_ingest_leases"

var (
	// ErrFenced is returned when a former owner attempts a durable mutation
	// after a newer owner has advanced the database epoch.
	ErrFenced = errors.New("wiki ingest database lease is fenced")
	// ErrLeaseRequired means a durable ingest context reached a repository
	// without first acquiring its mandatory database lease.
	ErrLeaseRequired   = errors.New("wiki ingest database lease is required")
	ErrInvalidIdentity = errors.New("wiki ingest database lease identity is invalid")
	// ErrTombstoneDrained is a terminal no-op for a disposable wake-up: the KB
	// is tombstoned and no Wiki queue row remains to drain, so a lease row must
	// not be recreated after KB-delete completion.
	ErrTombstoneDrained = errors.New("wiki ingest tombstone queue is already drained")
)

// Identity is an unforgeable, generation-specific coordinator identity. Token
// is deliberately omitted from error messages and logs.
type Identity struct {
	TenantID        uint64
	KnowledgeBaseID string
	Epoch           int64
	Token           string
}

func (i Identity) valid() bool {
	return i.TenantID != 0 && strings.TrimSpace(i.KnowledgeBaseID) != "" &&
		i.Epoch > 0 && strings.TrimSpace(i.Token) != ""
}

// Lease is the persisted coordinator row. There is exactly one row per
// tenant-scoped KB; epoch is monotonically increasing for the row's lifetime.
type Lease struct {
	TenantID        uint64    `gorm:"primaryKey;column:tenant_id"`
	KnowledgeBaseID string    `gorm:"primaryKey;column:knowledge_base_id;type:varchar(64)"`
	Epoch           int64     `gorm:"column:epoch;not null"`
	Token           string    `gorm:"column:lease_token;type:varchar(64);not null"`
	AcquiredAt      time.Time `gorm:"column:acquired_at;not null"`
	UpdatedAt       time.Time `gorm:"column:updated_at;not null"`
}

func (Lease) TableName() string { return Table }

type contextState struct {
	required bool
	identity Identity
}

type contextKey struct{}

// Require marks a context as a durable Wiki ingest path. Repositories fail
// closed if the caller has not subsequently attached an acquired Identity.
// Ordinary administrative/direct calls intentionally carry no marker and
// retain their existing behavior.
func Require(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	state, _ := stateFromContext(ctx)
	state.required = true
	return context.WithValue(ctx, contextKey{}, state)
}

// WithIdentity attaches the authoritative database lease and also marks the
// context required. Callers may safely derive cancellation/deadline contexts;
// the value remains immutable.
func WithIdentity(ctx context.Context, identity Identity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, contextState{required: true, identity: identity})
}

// IdentityFromContext returns a detached copy of the current lease identity.
func IdentityFromContext(ctx context.Context) (Identity, bool) {
	state, ok := stateFromContext(ctx)
	return state.identity, ok && state.identity.valid()
}

// Required reports whether ctx belongs to the durable ingest coordinator.
func Required(ctx context.Context) bool {
	state, ok := stateFromContext(ctx)
	return ok && state.required
}

func stateFromContext(ctx context.Context) (contextState, bool) {
	if ctx == nil {
		return contextState{}, false
	}
	state, ok := ctx.Value(contextKey{}).(contextState)
	return state, ok
}

// FencedError is terminal for the obsolete worker but not a business failure:
// its durable rows remain owned by the newer coordinator and must not accrue a
// fail_count or dead-letter entry.
type FencedError struct {
	ExpectedTenantID        uint64
	ExpectedKnowledgeBaseID string
	ExpectedEpoch           int64
	CurrentEpoch            int64
	Reason                  string
}

func (e *FencedError) Error() string {
	if e == nil {
		return ErrFenced.Error()
	}
	reason := strings.TrimSpace(e.Reason)
	if reason == "" {
		reason = "owner identity changed"
	}
	return fmt.Sprintf("%s: tenant=%d kb=%s expected_epoch=%d current_epoch=%d (%s)",
		ErrFenced, e.ExpectedTenantID, e.ExpectedKnowledgeBaseID,
		e.ExpectedEpoch, e.CurrentEpoch, reason)
}

func (e *FencedError) Unwrap() error { return ErrFenced }

func newToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate Wiki database lease token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// Acquire advances the database epoch after the caller has acquired its
// Redis/Lite coordination lock. The parent KB lock serializes acquisition with
// KB deletion and establishes the global KB -> lease -> knowledge lock order.
//
// Tombstoned KBs may acquire only while Wiki rows still need terminal draining.
// Once the queue is empty, acquisition returns ErrTombstoneDrained so a late
// disposable trigger cannot recreate an orphan lease after delete completion.
func Acquire(ctx context.Context, db *gorm.DB, tenantID uint64, knowledgeBaseID string) (Identity, error) {
	if db == nil || tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return Identity{}, ErrInvalidIdentity
	}
	var acquired Identity
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		deleted, err := kbwritefence.LockExisting(tx, tenantID, knowledgeBaseID)
		if err != nil {
			return fmt.Errorf("acquire Wiki database lease: lock KB: %w", err)
		}
		if deleted {
			var pending int64
			if err := tx.Model(&types.TaskPendingOp{}).Where(
				"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
				tenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase, knowledgeBaseID,
			).Count(&pending).Error; err != nil {
				return fmt.Errorf("acquire Wiki database lease: inspect tombstone queue: %w", err)
			}
			if pending == 0 {
				return ErrTombstoneDrained
			}
		}

		token, err := newToken()
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		var current Lease
		query := tx.Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, knowledgeBaseID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		err = query.Take(&current).Error
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			current = Lease{
				TenantID: tenantID, KnowledgeBaseID: knowledgeBaseID,
				Epoch: 1, Token: token, AcquiredAt: now, UpdatedAt: now,
			}
			if err := tx.Create(&current).Error; err != nil {
				return fmt.Errorf("acquire Wiki database lease: insert epoch: %w", err)
			}
		case err != nil:
			return fmt.Errorf("acquire Wiki database lease: lock epoch: %w", err)
		default:
			if current.Epoch <= 0 || current.Epoch == math.MaxInt64 {
				return fmt.Errorf("acquire Wiki database lease: invalid/exhausted epoch %d", current.Epoch)
			}
			nextEpoch := current.Epoch + 1
			result := tx.Model(&Lease{}).
				Where("tenant_id = ? AND knowledge_base_id = ? AND epoch = ?", tenantID, knowledgeBaseID, current.Epoch).
				Updates(map[string]interface{}{
					"epoch": nextEpoch, "lease_token": token,
					"acquired_at": now, "updated_at": now,
				})
			if result.Error != nil {
				return fmt.Errorf("acquire Wiki database lease: advance epoch: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return errors.New("acquire Wiki database lease: epoch compare-and-swap lost under KB lock")
			}
			current.Epoch = nextEpoch
			current.Token = token
		}
		acquired = Identity{
			TenantID: tenantID, KnowledgeBaseID: knowledgeBaseID,
			Epoch: current.Epoch, Token: token,
		}
		return nil
	})
	if err != nil {
		return Identity{}, err
	}
	return acquired, nil
}

// Validate locks and validates the current epoch/token inside the caller's
// side-effect transaction. The caller must already own the target KB lock.
// PostgreSQL uses FOR SHARE so concurrent writes by the same owner coexist,
// while the next acquisition's FOR UPDATE waits for all of them to commit.
func Validate(
	ctx context.Context,
	tx *gorm.DB,
	expectedTenantID uint64,
	expectedKnowledgeBaseID string,
) error {
	state, hasState := stateFromContext(ctx)
	if !hasState || (!state.required && !state.identity.valid()) {
		return nil // explicit compatibility policy for ordinary admin/direct calls
	}
	if tx == nil {
		return errors.New("validate Wiki database lease: nil transaction")
	}
	if !state.identity.valid() {
		return ErrLeaseRequired
	}
	identity := state.identity
	if expectedTenantID == 0 || strings.TrimSpace(expectedKnowledgeBaseID) == "" {
		return ErrInvalidIdentity
	}
	if identity.TenantID != expectedTenantID || identity.KnowledgeBaseID != expectedKnowledgeBaseID {
		return &FencedError{
			ExpectedTenantID: identity.TenantID, ExpectedKnowledgeBaseID: identity.KnowledgeBaseID,
			ExpectedEpoch: identity.Epoch, Reason: "target scope differs from lease scope",
		}
	}

	var current Lease
	query := tx.Where("tenant_id = ? AND knowledge_base_id = ?", identity.TenantID, identity.KnowledgeBaseID)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "SHARE"})
	}
	err := query.Take(&current).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &FencedError{
			ExpectedTenantID: identity.TenantID, ExpectedKnowledgeBaseID: identity.KnowledgeBaseID,
			ExpectedEpoch: identity.Epoch, Reason: "lease row was removed",
		}
	}
	if err != nil {
		return fmt.Errorf("validate Wiki database lease: load current epoch: %w", err)
	}
	if current.Epoch != identity.Epoch ||
		subtle.ConstantTimeCompare([]byte(current.Token), []byte(identity.Token)) != 1 {
		return &FencedError{
			ExpectedTenantID: identity.TenantID, ExpectedKnowledgeBaseID: identity.KnowledgeBaseID,
			ExpectedEpoch: identity.Epoch, CurrentEpoch: current.Epoch,
			Reason: "a newer coordinator owns the KB",
		}
	}
	return nil
}

// DeleteLocked removes the lease row during KB-delete finalization. The caller
// must already hold kbwritefence.LockTombstone in tx.
func DeleteLocked(tx *gorm.DB, tenantID uint64, knowledgeBaseID string) error {
	if tx == nil || tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return ErrInvalidIdentity
	}
	if err := tx.Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, knowledgeBaseID).
		Delete(&Lease{}).Error; err != nil {
		return fmt.Errorf("delete Wiki database lease: %w", err)
	}
	return nil
}
