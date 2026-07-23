// Package wikiingestguard provides the database serialization boundary
// between a durable Wiki ingest operation and lifecycle changes to its source
// knowledge. Service code carries the exact tenant/KB/knowledge/generation in
// context; repositories validate it in the same transaction as every Wiki
// side effect.
package wikiingestguard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/wikilease"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const appliedPageSlugsPayloadKey = "applied_page_slugs"

var ErrInvalidIdentity = errors.New("wiki ingest guard requires a complete identity")

// Identity is the authoritative lifecycle identity of one durable ingest op.
type Identity struct {
	TenantID             uint64
	KnowledgeBaseID      string
	KnowledgeID          string
	ProcessingGeneration string
}

// Operation binds an identity to its task_pending_ops row. PendingOpID may be
// zero for legacy/direct callers that still need lifecycle validation but do
// not have a durable page-application checkpoint.
type Operation struct {
	PendingOpID int64
	Identity    Identity
}

type guardState struct {
	identities []Identity
	operations []Operation
	pageSlug   string
}

type contextKey struct{}

// StaleIdentityError is terminal for the listed durable operations: their
// source disappeared, moved, changed generation, or entered a terminal state.
// Database/query and tenant-integrity failures use ordinary errors and must be
// retried/dead-lettered rather than silently acknowledged.
type StaleIdentityError struct {
	Identities []Identity
}

func (e *StaleIdentityError) Error() string {
	if e == nil || len(e.Identities) == 0 {
		return "wiki ingest identity is stale"
	}
	parts := make([]string, 0, len(e.Identities))
	for _, identity := range e.Identities {
		parts = append(parts, identity.KnowledgeID+":"+identity.ProcessingGeneration)
	}
	return "wiki ingest identity is stale: " + strings.Join(parts, ", ")
}

// NewStaleIdentityError constructs a typed terminal result after a service-
// level recheck. Invalid/duplicate identities are discarded defensively.
func NewStaleIdentityError(identities ...Identity) error {
	identities = normalizeIdentities(identities)
	if len(identities) == 0 {
		return errors.New("wiki ingest identity is stale")
	}
	return &StaleIdentityError{Identities: identities}
}

// StaleIdentities extracts a detached identity list through wrapped errors.
func StaleIdentities(err error) []Identity {
	var stale *StaleIdentityError
	if !errors.As(err, &stale) || stale == nil {
		return nil
	}
	return append([]Identity(nil), stale.Identities...)
}

// WithValidation attaches identities that every repository write using this
// context must validate transactionally.
func WithValidation(ctx context.Context, identities ...Identity) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, contextKey{}, guardState{identities: normalizeIdentities(identities)})
}

// WithPageApplication attaches both the lifecycle fence and the exact page
// application checkpoint. A Wiki page repository records pageSlug into every
// positive PendingOpID in the same transaction as the page mutation.
func WithPageApplication(ctx context.Context, pageSlug string, operations ...Operation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	operations = normalizeOperations(operations)
	identities := make([]Identity, 0, len(operations))
	for _, operation := range operations {
		identities = append(identities, operation.Identity)
	}
	return context.WithValue(ctx, contextKey{}, guardState{
		identities: normalizeIdentities(identities),
		operations: operations,
		pageSlug:   strings.TrimSpace(pageSlug),
	})
}

// Identities returns the detached identities carried by ctx.
func Identities(ctx context.Context) []Identity {
	state, _ := stateFromContext(ctx)
	return append([]Identity(nil), state.identities...)
}

// Scope returns the common tenant/KB carried by the guard. Mixed scopes are
// rejected by returning ok=false; Wiki ingest batches must never cross them.
func Scope(ctx context.Context) (tenantID uint64, knowledgeBaseID string, ok bool) {
	identities := Identities(ctx)
	if len(identities) == 0 {
		lease, leaseOK := wikilease.IdentityFromContext(ctx)
		if !leaseOK {
			return 0, "", false
		}
		return lease.TenantID, lease.KnowledgeBaseID, true
	}
	tenantID = identities[0].TenantID
	knowledgeBaseID = identities[0].KnowledgeBaseID
	for _, identity := range identities[1:] {
		if identity.TenantID != tenantID || identity.KnowledgeBaseID != knowledgeBaseID {
			return 0, "", false
		}
	}
	return tenantID, knowledgeBaseID, true
}

// Validate locks and validates all source rows. It is a no-op when no guard is
// attached, so repositories can call it for every write path.
func Validate(ctx context.Context, tx *gorm.DB) error {
	state, ok := stateFromContext(ctx)
	if ok && len(state.identities) > 0 {
		tenantID := state.identities[0].TenantID
		knowledgeBaseID := state.identities[0].KnowledgeBaseID
		for _, identity := range state.identities[1:] {
			if identity.TenantID != tenantID || identity.KnowledgeBaseID != knowledgeBaseID {
				return ErrInvalidIdentity
			}
		}
		return ValidateScope(ctx, tx, tenantID, knowledgeBaseID)
	}
	lease, leaseOK := wikilease.IdentityFromContext(ctx)
	if leaseOK {
		return ValidateScope(ctx, tx, lease.TenantID, lease.KnowledgeBaseID)
	}
	// A durable ingest path without an identity must fail closed. Ordinary
	// direct/admin calls carry neither marker and remain a no-op here.
	if wikilease.Required(ctx) {
		return wikilease.ErrLeaseRequired
	}
	return nil
}

// ValidateScope enforces the complete KB -> lease -> knowledge lock order.
// The repository caller must already own the exact target KB lock. Lease
// validation runs first so a former coordinator is rejected before it locks
// source rows or mutates any page/log/checkpoint/queue state.
func ValidateScope(ctx context.Context, tx *gorm.DB, tenantID uint64, knowledgeBaseID string) error {
	state, _ := stateFromContext(ctx)
	if tx == nil {
		return errors.New("wiki ingest guard: nil transaction")
	}
	if tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return ErrInvalidIdentity
	}
	if err := wikilease.Validate(ctx, tx, tenantID, knowledgeBaseID); err != nil {
		return err
	}
	if len(state.identities) == 0 {
		return nil
	}

	stale := make([]Identity, 0)
	for _, identity := range state.identities {
		if identity.TenantID != tenantID || identity.KnowledgeBaseID != knowledgeBaseID {
			return ErrInvalidIdentity
		}
		var row knowledgeIdentityRow
		query := tx.Unscoped().Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id", "processing_generation", "parse_status", "processed_at", "deleted_at").
			Where("id = ?", identity.KnowledgeID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "SHARE"})
		}
		err := query.Take(&row).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			stale = append(stale, identity)
			continue
		}
		if err != nil {
			return fmt.Errorf("wiki ingest guard: load knowledge %s: %w", identity.KnowledgeID, err)
		}
		if row.TenantID != identity.TenantID {
			return fmt.Errorf(
				"wiki ingest guard: tenant mismatch for knowledge %s (database=%d expected=%d)",
				identity.KnowledgeID, row.TenantID, identity.TenantID,
			)
		}
		if row.DeletedAt.Valid || row.KnowledgeBaseID != identity.KnowledgeBaseID ||
			row.ProcessingGeneration != identity.ProcessingGeneration || terminalParseStatus(row.ParseStatus) {
			stale = append(stale, identity)
			continue
		}
		if row.ProcessedAt == nil {
			return fmt.Errorf(
				"wiki ingest guard: core generation %s for knowledge %s is not committed",
				identity.ProcessingGeneration, identity.KnowledgeID,
			)
		}
		if row.ParseStatus == types.ParseStatusPending {
			return fmt.Errorf(
				"wiki ingest guard: committed generation %s unexpectedly returned to pending for knowledge %s",
				identity.ProcessingGeneration, identity.KnowledgeID,
			)
		}
		if !allowedParseStatus(row.ParseStatus) {
			return fmt.Errorf(
				"wiki ingest guard: unknown parse status %q for knowledge %s",
				row.ParseStatus, identity.KnowledgeID,
			)
		}
	}
	if len(stale) > 0 {
		return &StaleIdentityError{Identities: stale}
	}
	return nil
}

// RecordPageApplication patches task_pending_ops.payload after the page write.
// Callers must invoke it inside the same transaction and after the mutation;
// any failure rolls both changes back. It is a no-op for validation-only
// contexts, non-matching slugs, and legacy operations with PendingOpID == 0.
func RecordPageApplication(ctx context.Context, tx *gorm.DB, pageSlug string) error {
	state, ok := stateFromContext(ctx)
	if !ok || len(state.operations) == 0 || strings.TrimSpace(pageSlug) == "" ||
		strings.TrimSpace(pageSlug) != state.pageSlug {
		return nil
	}
	if tx == nil {
		return errors.New("wiki ingest guard: nil transaction")
	}

	for _, operation := range state.operations {
		if operation.PendingOpID <= 0 {
			continue
		}
		var row types.TaskPendingOp
		query := tx.Where("id = ?", operation.PendingOpID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return &StaleIdentityError{Identities: []Identity{operation.Identity}}
			}
			return fmt.Errorf("wiki ingest guard: lock pending op %d: %w", operation.PendingOpID, err)
		}
		if err := validatePendingRow(row, operation); err != nil {
			return err
		}
		payload, changed, err := appendAppliedPageSlug(row.Payload, state.pageSlug)
		if err != nil {
			return fmt.Errorf("wiki ingest guard: decode pending op %d payload: %w", operation.PendingOpID, err)
		}
		if !changed {
			continue
		}
		result := tx.Model(&types.TaskPendingOp{}).
			Where("id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ?",
				row.ID, operation.Identity.TenantID, types.TypeWikiIngest,
				types.TaskScopeKnowledgeBase, operation.Identity.KnowledgeBaseID, "ingest").
			Update("payload", payload)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return &StaleIdentityError{Identities: []Identity{operation.Identity}}
		}
	}
	return nil
}

type knowledgeIdentityRow struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	ParseStatus          string
	ProcessedAt          *time.Time
	DeletedAt            gorm.DeletedAt
}

func allowedParseStatus(status string) bool {
	switch status {
	case types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted:
		return true
	default:
		return false
	}
}

func terminalParseStatus(status string) bool {
	switch status {
	case types.ParseStatusFailed, types.ParseStatusCancelling, types.ParseStatusCancelled, types.ParseStatusDeleting:
		return true
	default:
		return false
	}
}

func stateFromContext(ctx context.Context) (guardState, bool) {
	if ctx == nil {
		return guardState{}, false
	}
	state, ok := ctx.Value(contextKey{}).(guardState)
	return state, ok
}

func validateIdentity(identity Identity) bool {
	return identity.TenantID != 0 && strings.TrimSpace(identity.KnowledgeBaseID) != "" &&
		strings.TrimSpace(identity.KnowledgeID) != "" && strings.TrimSpace(identity.ProcessingGeneration) != ""
}

func normalizeIdentities(identities []Identity) []Identity {
	seen := make(map[string]struct{}, len(identities))
	result := make([]Identity, 0, len(identities))
	for _, identity := range identities {
		if !validateIdentity(identity) {
			continue
		}
		key := fmt.Sprintf("%d\x00%s\x00%s\x00%s", identity.TenantID, identity.KnowledgeBaseID,
			identity.KnowledgeID, identity.ProcessingGeneration)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, identity)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].KnowledgeID != result[j].KnowledgeID {
			return result[i].KnowledgeID < result[j].KnowledgeID
		}
		return result[i].ProcessingGeneration < result[j].ProcessingGeneration
	})
	return result
}

func normalizeOperations(operations []Operation) []Operation {
	seen := make(map[int64]struct{}, len(operations))
	result := make([]Operation, 0, len(operations))
	for _, operation := range operations {
		if !validateIdentity(operation.Identity) {
			continue
		}
		if operation.PendingOpID > 0 {
			if _, exists := seen[operation.PendingOpID]; exists {
				continue
			}
			seen[operation.PendingOpID] = struct{}{}
		}
		result = append(result, operation)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PendingOpID < result[j].PendingOpID })
	return result
}

func validatePendingRow(row types.TaskPendingOp, operation Operation) error {
	identity := operation.Identity
	if row.TenantID != identity.TenantID || row.TaskType != types.TypeWikiIngest ||
		row.Scope != types.TaskScopeKnowledgeBase || row.ScopeID != identity.KnowledgeBaseID || row.Op != "ingest" {
		return fmt.Errorf("wiki ingest guard: pending op %d identity mismatch", operation.PendingOpID)
	}
	var payload struct {
		KnowledgeID          string          `json:"knowledge_id"`
		ProcessingGeneration string          `json:"processing_generation"`
		Prepared             json.RawMessage `json:"prepared"`
	}
	if err := json.Unmarshal(row.Payload, &payload); err != nil {
		return fmt.Errorf("wiki ingest guard: invalid pending op %d payload: %w", operation.PendingOpID, err)
	}
	if payload.KnowledgeID != identity.KnowledgeID || payload.ProcessingGeneration != identity.ProcessingGeneration {
		return fmt.Errorf("wiki ingest guard: pending op %d source identity mismatch", operation.PendingOpID)
	}
	if len(payload.Prepared) == 0 || string(payload.Prepared) == "null" {
		return fmt.Errorf("wiki ingest guard: pending op %d has no durable prepared plan", operation.PendingOpID)
	}
	return nil
}

func appendAppliedPageSlug(raw json.RawMessage, slug string) (json.RawMessage, bool, error) {
	payload := make(map[string]json.RawMessage)
	if len(raw) > 0 && string(raw) != "null" {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return nil, false, err
		}
	}
	var slugs []string
	if encoded := payload[appliedPageSlugsPayloadKey]; len(encoded) > 0 {
		if err := json.Unmarshal(encoded, &slugs); err != nil {
			return nil, false, err
		}
	}
	for _, existing := range slugs {
		if existing == slug {
			return raw, false, nil
		}
	}
	slugs = append(slugs, slug)
	sort.Strings(slugs)
	encodedSlugs, err := json.Marshal(slugs)
	if err != nil {
		return nil, false, err
	}
	payload[appliedPageSlugsPayloadKey] = encodedSlugs
	encodedPayload, err := json.Marshal(payload)
	return encodedPayload, true, err
}
