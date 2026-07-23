// Package knowledgeaux owns the durable lifecycle of object-storage artifacts
// that are derived from a knowledge row but are not represented by their own
// database table.  task_pending_ops is used as an ownership ledger, not as a
// work queue: a row is removed only after the corresponding object was deleted.
package knowledgeaux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	TaskType       = "knowledge:aux_object"
	operationOwned = "owned"

	KindFanoutImage     = "fanout_image"
	KindFAQEntries      = "faq_entries"
	KindFAQFailedExport = "faq_failed_export"
	KindSourceFile      = "source_file"
	KindFileURLTemp     = "file_url_temp"
	KindCloneSourceFile = "clone_source_file"
	KindCloneImage      = "clone_image"

	referenceFingerprintPrefix = "ref-sha256:"

	quarantineReasonSharedPhysical = "shared_physical_object"
	quarantineReasonLegacyIdentity = "legacy_identity_mismatch"
)

var (
	ErrInvalidObject      = errors.New("knowledge auxiliary object has an invalid identity")
	ErrKnowledgeFence     = errors.New("knowledge auxiliary object generation fence rejected the write")
	ErrProviderRouting    = errors.New("knowledge auxiliary object storage provider cannot be resolved")
	ErrReservationLost    = errors.New("knowledge auxiliary object reservation no longer exists")
	ErrBindingMissing     = errors.New("knowledge auxiliary object storage binding is missing")
	ErrBindingMismatch    = errors.New("knowledge auxiliary object storage binding does not match its path or service")
	ErrBindingQuarantined = errors.New("knowledge auxiliary object storage binding is quarantined")
)

// Object is the durable ownership proof stored in task_pending_ops.Payload.
// Reference is an optional one-way identity of a client-facing reference; a
// signed URL itself is never persisted here. Path is always the provider path
// that DeleteFile must receive.
type Object struct {
	TenantID             uint64                  `json:"tenant_id"`
	KnowledgeBaseID      string                  `json:"knowledge_base_id"`
	KnowledgeID          string                  `json:"knowledge_id"`
	ProcessingGeneration string                  `json:"processing_generation"`
	Path                 string                  `json:"path"`
	Reference            string                  `json:"reference,omitempty"`
	FallbackProvider     string                  `json:"fallback_provider,omitempty"`
	Kind                 string                  `json:"kind"`
	Binding              *storagebinding.Binding `json:"storage_binding,omitempty"`
	Quarantined          bool                    `json:"binding_quarantined,omitempty"`
	QuarantineReason     string                  `json:"binding_quarantine_reason,omitempty"`
}

type registeredObject struct {
	row    *types.TaskPendingOp
	object Object
}

func PayloadBelongsToKnowledgeBase(payload []byte, tenantID uint64, knowledgeBaseID string) (bool, error) {
	object, err := decodeObject(payload)
	if err != nil {
		return false, err
	}
	return object.TenantID == tenantID && object.KnowledgeBaseID == strings.TrimSpace(knowledgeBaseID), nil
}

// ServiceResolver is injectable so routing and partial-failure behavior can be
// tested without contacting a real object store.
type ServiceResolver func(
	ctx context.Context,
	tenant *types.Tenant,
	provider string,
) (interfaces.FileService, string, error)

type BindingServiceResolver func(
	ctx context.Context,
	tenant *types.Tenant,
	binding storagebinding.Binding,
	path string,
) (interfaces.FileService, error)

type Registry struct {
	db              *gorm.DB
	resolver        ServiceResolver
	bindingResolver BindingServiceResolver
	injectedGlobal  interfaces.FileService
}

func New(db *gorm.DB, injectedGlobal interfaces.FileService) *Registry {
	resolver := func(
		_ context.Context,
		tenant *types.Tenant,
		provider string,
	) (interfaces.FileService, string, error) {
		if tenant == nil {
			return nil, "", errors.New("tenant is unavailable")
		}
		return filesvc.NewReadOnlyFileServiceFromStorageConfig(
			provider,
			tenant.StorageEngineConfig,
			strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR")),
		)
	}
	return &Registry{
		db: db, resolver: resolver, injectedGlobal: injectedGlobal,
		bindingResolver: func(
			_ context.Context, tenant *types.Tenant, binding storagebinding.Binding, _ string,
		) (interfaces.FileService, error) {
			if tenant == nil {
				return nil, errors.New("tenant is unavailable")
			}
			return filesvc.NewFileServiceForBinding(binding, tenant.StorageEngineConfig, injectedGlobal)
		},
	}
}

func NewWithResolver(db *gorm.DB, resolver ServiceResolver) *Registry {
	return &Registry{
		db: db, resolver: resolver,
		bindingResolver: func(
			ctx context.Context, tenant *types.Tenant, binding storagebinding.Binding, path string,
		) (interfaces.FileService, error) {
			if resolver == nil {
				return nil, errors.New("knowledge auxiliary object provider resolver is unavailable")
			}
			service, resolved, err := resolver(ctx, tenant, string(binding.Provider))
			if err != nil {
				return nil, err
			}
			if service == nil || normalizeProvider(resolved) != string(binding.Provider) {
				return nil, ErrBindingMismatch
			}
			provider, ok := service.(storagebinding.BindingProvider)
			if !ok || provider == nil {
				return nil, ErrBindingMismatch
			}
			actual, err := provider.BindingForPath(path)
			if err != nil || actual.Fingerprint != binding.Fingerprint {
				return nil, ErrBindingMismatch
			}
			return service, nil
		},
	}
}

func validKind(kind string) bool {
	switch kind {
	case KindFanoutImage, KindFAQEntries, KindFAQFailedExport,
		KindSourceFile, KindFileURLTemp, KindCloneSourceFile, KindCloneImage:
		return true
	default:
		return false
	}
}

func normalizeProvider(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

// ProviderForPath gives a recognized provider:// path absolute precedence.
// Only paths without a provider identity use the durable KB/provider snapshot.
func ProviderForPath(path, fallbackProvider string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", ErrProviderRouting
	}
	lowerPath := strings.ToLower(path)
	if provider := types.ParseProviderScheme(lowerPath); provider != "" {
		return provider, nil
	}
	// Historical COS rows used an HTTPS object URL rather than cos://.
	if provider := types.InferStorageFromFilePath(lowerPath); provider != "" {
		return provider, nil
	}
	if marker := strings.Index(lowerPath, "://"); marker > 0 {
		scheme := lowerPath[:marker]
		if scheme != "http" && scheme != "https" {
			return "", fmt.Errorf("%w: unsupported explicit scheme %q", ErrProviderRouting, scheme)
		}
	}
	provider := normalizeProvider(fallbackProvider)
	if provider == "" {
		return "", fmt.Errorf("%w: legacy path %q has no provider snapshot", ErrProviderRouting, path)
	}
	return provider, nil
}

func pathKey(path string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(path)))
	return hex.EncodeToString(digest[:])
}

func referenceIdentity(reference string) string {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(reference), referenceFingerprintPrefix) {
		digest := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(
			strings.ToLower(reference), referenceFingerprintPrefix,
		)))
		if len(digest) == sha256.Size*2 {
			if _, err := hex.DecodeString(digest); err == nil {
				return referenceFingerprintPrefix + digest
			}
		}
	}
	digest := sha256.Sum256([]byte(reference))
	return referenceFingerprintPrefix + hex.EncodeToString(digest[:])
}

func objectKey(knowledgeID, path string) string {
	return strings.TrimSpace(knowledgeID) + ":" + pathKey(path)
}

func objectKeyPrefix(knowledgeID string) string {
	return strings.TrimSpace(knowledgeID) + ":"
}

func validateObject(object Object) error {
	if object.TenantID == 0 || strings.TrimSpace(object.KnowledgeBaseID) == "" ||
		strings.TrimSpace(object.KnowledgeID) == "" || strings.TrimSpace(object.Path) == "" ||
		!validKind(object.Kind) {
		return ErrInvalidObject
	}
	if len(objectKey(object.KnowledgeID, object.Path)) > 128 {
		return fmt.Errorf("%w: knowledge ID is too long for the ownership key", ErrInvalidObject)
	}
	if _, err := ProviderForPath(object.Path, object.FallbackProvider); err != nil {
		return err
	}
	if object.Binding != nil {
		normalized, err := storagebinding.Normalize(*object.Binding)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrBindingMismatch, err)
		}
		provider, err := ProviderForPath(object.Path, object.FallbackProvider)
		if err != nil || provider != string(normalized.Provider) {
			return ErrBindingMismatch
		}
	}
	if object.Quarantined {
		switch object.QuarantineReason {
		case quarantineReasonSharedPhysical, quarantineReasonLegacyIdentity:
		default:
			return ErrInvalidObject
		}
	} else if object.QuarantineReason != "" {
		return ErrInvalidObject
	}
	return nil
}

func normalizeObject(object Object) (Object, error) {
	object.KnowledgeBaseID = strings.TrimSpace(object.KnowledgeBaseID)
	object.KnowledgeID = strings.TrimSpace(object.KnowledgeID)
	object.ProcessingGeneration = strings.TrimSpace(object.ProcessingGeneration)
	object.Path = strings.TrimSpace(object.Path)
	object.Reference = referenceIdentity(object.Reference)
	object.FallbackProvider = normalizeProvider(object.FallbackProvider)
	object.QuarantineReason = strings.ToLower(strings.TrimSpace(object.QuarantineReason))
	if object.Binding != nil {
		normalized, err := storagebinding.Normalize(*object.Binding)
		if err != nil {
			return Object{}, fmt.Errorf("%w: %v", ErrBindingMismatch, err)
		}
		object.Binding = &normalized
	}
	if err := validateObject(object); err != nil {
		return Object{}, err
	}
	return object, nil
}

func decodeObject(payload []byte) (Object, error) {
	var object Object
	if err := json.Unmarshal(payload, &object); err != nil {
		return Object{}, err
	}
	return normalizeObject(object)
}

func sameBinding(left, right *storagebinding.Binding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return left.Fingerprint == right.Fingerprint
}

func sameObject(left, right Object) bool {
	leftBinding, rightBinding := left.Binding, right.Binding
	left.Binding, right.Binding = nil, nil
	return left == right && sameBinding(leftBinding, rightBinding)
}

func bindObjectToService(object Object, service interfaces.FileService) (Object, error) {
	if service == nil {
		return Object{}, ErrBindingMissing
	}
	provider, ok := service.(storagebinding.BindingProvider)
	if !ok || provider == nil {
		return Object{}, fmt.Errorf("%w: file service does not expose an exact binding", ErrBindingMissing)
	}
	binding, err := provider.BindingForPath(object.Path)
	if err != nil {
		return Object{}, fmt.Errorf("%w: %v", ErrBindingMismatch, err)
	}
	if object.Binding != nil {
		normalized, err := storagebinding.Normalize(*object.Binding)
		if err != nil || normalized.Fingerprint != binding.Fingerprint {
			return Object{}, ErrBindingMismatch
		}
	}
	object.Binding = &binding
	return normalizeObject(object)
}

func (r *Registry) prepareBoundObject(
	ctx context.Context, object Object, services []interfaces.FileService,
) (Object, error) {
	object, err := normalizeObject(object)
	if err != nil {
		return Object{}, err
	}
	if len(services) > 1 {
		return Object{}, ErrInvalidObject
	}
	if len(services) == 1 {
		return bindObjectToService(object, services[0])
	}
	if object.Binding != nil {
		return object, nil
	}
	// Creating ownership without either a caller-proven concrete service or an
	// already signed binding is forbidden. In particular this path must never
	// call a provider constructor that probes or creates a bucket.
	return Object{}, ErrBindingMissing
}

// Register writes ownership in the same KB/knowledge/generation serialization
// domain as deletion.  FAQ containers created before processing generations
// existed are assigned one while both parent and child rows are locked.
func (r *Registry) Register(
	ctx context.Context, object Object, service ...interfaces.FileService,
) (Object, error) {
	object, err := r.prepareBoundObject(ctx, object, service)
	if err != nil {
		return Object{}, err
	}
	return r.register(ctx, object, false)
}

// Reserve persists a final provider path before its corresponding provider
// Commit call. allowMissingOwner supports source/clone writes whose destination
// knowledge insert has not committed yet; the KB is still locked and proven
// active, and a non-empty pre-generated generation is mandatory.
func (r *Registry) Reserve(
	ctx context.Context, object Object, allowMissingOwner bool, service ...interfaces.FileService,
) (Object, error) {
	object, err := r.prepareBoundObject(ctx, object, service)
	if err != nil {
		return Object{}, err
	}
	return r.register(ctx, object, allowMissingOwner)
}

// Confirm re-validates that a planned object has been adopted by the exact
// active knowledge generation. It is optional for an already-existing owner
// and required after a pre-create source/clone knowledge insert.
func (r *Registry) Confirm(
	ctx context.Context, object Object, service ...interfaces.FileService,
) (Object, error) {
	object, err := r.prepareBoundObject(ctx, object, service)
	if err != nil {
		return Object{}, err
	}
	return r.register(ctx, object, false)
}

// AdoptReservedKnowledge is the source/clone creation boundary. The exact
// pre-created ownership row and the knowledge INSERT are serialized under the
// same exclusive KB lock and transaction. Recovery takes the same locks in the
// same order, so it can never consume a missing-owner reservation and then let
// a late knowledge row commit with a path that it already deleted.
func (r *Registry) AdoptReservedKnowledge(
	ctx context.Context,
	object Object,
	knowledge *types.Knowledge,
	creator interfaces.KnowledgeTransactionalCreator,
) error {
	if r == nil || r.db == nil || knowledge == nil || creator == nil {
		return ErrInvalidObject
	}
	object, err := normalizeObject(object)
	if err != nil {
		return err
	}
	if object.Binding == nil {
		return ErrBindingMissing
	}
	if knowledge.TenantID != object.TenantID || knowledge.KnowledgeBaseID != object.KnowledgeBaseID ||
		knowledge.ID != object.KnowledgeID || knowledge.ProcessingGeneration != object.ProcessingGeneration ||
		strings.TrimSpace(knowledge.FilePath) != object.Path {
		return fmt.Errorf("%w: knowledge does not exactly adopt the reservation", ErrKnowledgeFence)
	}

	return kbwritefence.WithActive(ctx, r.db, object.TenantID, object.KnowledgeBaseID, func(tx *gorm.DB) error {
		query := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			object.TenantID, TaskType, types.TaskScopeKnowledgeBase, object.KnowledgeBaseID,
			operationOwned, objectKey(object.KnowledgeID, object.Path),
		)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var rows []*types.TaskPendingOp
		if err := query.Find(&rows).Error; err != nil {
			return fmt.Errorf("adopt reserved auxiliary object: lock ledger: %w", err)
		}
		if len(rows) == 0 {
			return ErrReservationLost
		}
		for _, row := range rows {
			persisted, err := decodeObject(row.Payload)
			if err == nil && persisted.Quarantined {
				return ErrBindingQuarantined
			}
			if err != nil || !sameObject(persisted, object) {
				return fmt.Errorf("adopt reserved auxiliary object: corrupt/mismatched ledger row %d", row.ID)
			}
		}
		if err := creator.CreateKnowledgeTx(ctx, tx, knowledge); err != nil {
			return fmt.Errorf("adopt reserved auxiliary object: create knowledge: %w", err)
		}
		var adopted types.Knowledge
		if err := tx.Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL",
			object.TenantID, object.KnowledgeBaseID, object.KnowledgeID,
		).Take(&adopted).Error; err != nil {
			return fmt.Errorf("adopt reserved auxiliary object: verify knowledge insert: %w", err)
		}
		if adopted.ProcessingGeneration != object.ProcessingGeneration ||
			strings.TrimSpace(adopted.FilePath) != object.Path {
			return fmt.Errorf("%w: inserted knowledge did not adopt exact reservation", ErrKnowledgeFence)
		}
		return nil
	})
}

// CommitReserved is the only safe boundary for a planned provider write. It
// reacquires the same parent-KB lock used by deletion, revalidates the optional
// knowledge owner and exact ledger row, and keeps that lock across commit. If
// deletion wins first the active fence rejects before commit is called; if this
// method wins, deletion waits until the provider outcome is durable.
func (r *Registry) CommitReserved(
	ctx context.Context,
	object Object,
	allowMissingOwner bool,
	commit func() error,
) error {
	if r == nil || r.db == nil || commit == nil {
		return ErrInvalidObject
	}
	object, err := normalizeObject(object)
	if err != nil {
		return err
	}
	if object.Binding == nil {
		return ErrBindingMissing
	}
	return kbwritefence.WithActiveShared(ctx, r.db, object.TenantID, object.KnowledgeBaseID, func(tx *gorm.DB) error {
		query := tx.Table("knowledges").
			Select("id", "processing_generation", "parse_status").
			Where("tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL",
				object.TenantID, object.KnowledgeBaseID, object.KnowledgeID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "SHARE"})
		}
		var owner struct {
			ID                   string
			ProcessingGeneration string
			ParseStatus          string
		}
		ownerErr := query.Take(&owner).Error
		if ownerErr != nil && !errors.Is(ownerErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("commit reserved auxiliary object: inspect owner: %w", ownerErr)
		}
		if errors.Is(ownerErr, gorm.ErrRecordNotFound) {
			if !allowMissingOwner || object.ProcessingGeneration == "" {
				return ErrKnowledgeFence
			}
		} else {
			if owner.ProcessingGeneration != object.ProcessingGeneration {
				return ErrKnowledgeFence
			}
			switch owner.ParseStatus {
			case types.ParseStatusDeleting, types.ParseStatusCancelling, types.ParseStatusCancelled:
				return ErrKnowledgeFence
			}
		}

		ledgerQuery := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			object.TenantID, TaskType, types.TaskScopeKnowledgeBase, object.KnowledgeBaseID,
			operationOwned, objectKey(object.KnowledgeID, object.Path),
		)
		if tx.Dialector.Name() != "sqlite" {
			ledgerQuery = ledgerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var rows []*types.TaskPendingOp
		if err := ledgerQuery.Find(&rows).Error; err != nil {
			return fmt.Errorf("commit reserved auxiliary object: lock ledger: %w", err)
		}
		if len(rows) == 0 {
			return ErrReservationLost
		}
		for _, row := range rows {
			persisted, err := decodeObject(row.Payload)
			if err == nil && persisted.Quarantined {
				return ErrBindingQuarantined
			}
			if err != nil ||
				persisted.TenantID != object.TenantID || persisted.KnowledgeBaseID != object.KnowledgeBaseID ||
				persisted.KnowledgeID != object.KnowledgeID || persisted.ProcessingGeneration != object.ProcessingGeneration ||
				persisted.Path != object.Path || !sameBinding(persisted.Binding, object.Binding) {
				return fmt.Errorf("commit reserved auxiliary object: corrupt/mismatched ledger row %d", row.ID)
			}
		}
		if err := commit(); err != nil {
			return fmt.Errorf("commit reserved auxiliary object %q: %w", object.Path, err)
		}
		return nil
	})
}

func (r *Registry) register(ctx context.Context, object Object, allowMissingOwner bool) (Object, error) {
	if r == nil || r.db == nil {
		return Object{}, errors.New("knowledge auxiliary object registry is unavailable")
	}
	var err error
	object, err = normalizeObject(object)
	if err != nil {
		return Object{}, err
	}
	if object.Binding == nil {
		return Object{}, ErrBindingMissing
	}

	err = kbwritefence.WithActive(ctx, r.db, object.TenantID, object.KnowledgeBaseID, func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "sqlite" && !allowMissingOwner {
			result := tx.Exec(
				`UPDATE knowledges SET id = id
				 WHERE tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL`,
				object.TenantID, object.KnowledgeBaseID, object.KnowledgeID,
			)
			if result.Error != nil {
				return fmt.Errorf("lock knowledge auxiliary owner: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrKnowledgeFence
			}
		}
		query := tx.Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id", "processing_generation", "parse_status").
			Where("tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL",
				object.TenantID, object.KnowledgeBaseID, object.KnowledgeID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var owner struct {
			ID                   string
			TenantID             uint64
			KnowledgeBaseID      string
			ProcessingGeneration string
			ParseStatus          string
		}
		ownerErr := query.Take(&owner).Error
		if ownerErr != nil && !errors.Is(ownerErr, gorm.ErrRecordNotFound) {
			return fmt.Errorf("read knowledge auxiliary owner: %w", ownerErr)
		}
		ownerMissing := errors.Is(ownerErr, gorm.ErrRecordNotFound)
		if ownerMissing && !allowMissingOwner {
			return ErrKnowledgeFence
		}
		if ownerMissing && object.ProcessingGeneration == "" {
			return fmt.Errorf("%w: a pre-create reservation requires a generation", ErrKnowledgeFence)
		}
		if !ownerMissing {
			switch owner.ParseStatus {
			case types.ParseStatusDeleting, types.ParseStatusCancelling, types.ParseStatusCancelled:
				return fmt.Errorf("%w: owner status is %s", ErrKnowledgeFence, owner.ParseStatus)
			}
		}
		if ownerMissing {
			// The KB lock above serializes this intent with a concurrent KB
			// tombstone. Recovery grants the destination insert a bounded grace.
		} else if object.ProcessingGeneration == "" {
			if owner.ProcessingGeneration == "" {
				owner.ProcessingGeneration = uuid.NewString()
				result := tx.Table("knowledges").
					Where("tenant_id = ? AND knowledge_base_id = ? AND id = ? AND deleted_at IS NULL AND processing_generation = ''",
						object.TenantID, object.KnowledgeBaseID, object.KnowledgeID).
					Update("processing_generation", owner.ProcessingGeneration)
				if result.Error != nil {
					return fmt.Errorf("assign FAQ auxiliary generation: %w", result.Error)
				}
				if result.RowsAffected != 1 {
					return ErrKnowledgeFence
				}
			}
			object.ProcessingGeneration = owner.ProcessingGeneration
		} else if object.ProcessingGeneration != owner.ProcessingGeneration {
			return fmt.Errorf("%w: expected generation %q, found %q",
				ErrKnowledgeFence, object.ProcessingGeneration, owner.ProcessingGeneration)
		}

		key := objectKey(object.KnowledgeID, object.Path)
		var existing []*types.TaskPendingOp
		if err := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			object.TenantID, TaskType, types.TaskScopeKnowledgeBase, object.KnowledgeBaseID, operationOwned, key,
		).Find(&existing).Error; err != nil {
			return fmt.Errorf("find knowledge auxiliary ownership: %w", err)
		}
		if len(existing) > 0 {
			for _, row := range existing {
				persisted, err := decodeObject(row.Payload)
				if err == nil && persisted.Quarantined {
					return ErrBindingQuarantined
				}
				if err != nil || persisted.Path != object.Path {
					return fmt.Errorf("knowledge auxiliary ownership hash collision or corrupt payload for %s", object.Path)
				}
				if !sameBinding(persisted.Binding, object.Binding) {
					return fmt.Errorf("%w: repeated registration for %s", ErrBindingMismatch, object.Path)
				}
			}
			payload, err := json.Marshal(object)
			if err != nil {
				return fmt.Errorf("marshal knowledge auxiliary ownership: %w", err)
			}
			ids := make([]int64, 0, len(existing))
			for _, row := range existing {
				ids = append(ids, row.ID)
			}
			if err := tx.Model(&types.TaskPendingOp{}).Where("id IN ?", ids).
				Updates(map[string]interface{}{
					"payload": payload, "fail_count": 0, "enqueued_at": time.Now().UTC(), "claimed_at": nil,
				}).Error; err != nil {
				return fmt.Errorf("refresh knowledge auxiliary ownership: %w", err)
			}
			return nil
		}
		payload, err := json.Marshal(object)
		if err != nil {
			return fmt.Errorf("marshal knowledge auxiliary ownership: %w", err)
		}
		return tx.Create(&types.TaskPendingOp{
			TenantID: object.TenantID, TaskType: TaskType,
			Scope: types.TaskScopeKnowledgeBase, ScopeID: object.KnowledgeBaseID,
			Op: operationOwned, DedupKey: key, Payload: payload, EnqueuedAt: time.Now().UTC(),
		}).Error
	})
	if err != nil {
		return Object{}, err
	}
	return object, nil
}

func (r *Registry) list(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeIDs []string,
) ([]registeredObject, error) {
	knowledgeBaseID = strings.TrimSpace(knowledgeBaseID)
	if r == nil || r.db == nil || tenantID == 0 || knowledgeBaseID == "" || len(knowledgeIDs) == 0 {
		return nil, nil
	}
	var rows []*types.TaskPendingOp
	query := r.db.WithContext(ctx).Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ?",
		tenantID, TaskType, types.TaskScopeKnowledgeBase, knowledgeBaseID, operationOwned,
	)
	if len(knowledgeIDs) == 1 {
		prefix := objectKeyPrefix(knowledgeIDs[0])
		// Do not use a Unicode high-sentinel range here. PostgreSQL applies the
		// database collation to varchar comparisons and common locale collations
		// may sort U+FFFF before the ASCII hex suffix used by objectKey. That made
		// freshly committed source-file ledgers invisible to the first worker
		// lookup even though the exact row existed. substr/length has identical
		// semantics on PostgreSQL and SQLite and is independent of collation.
		query = query.Where("substr(dedup_key, 1, length(?)) = ?", prefix, prefix)
	}
	if err := query.Order("id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list knowledge auxiliary ownership: %w", err)
	}
	wanted := make(map[string]struct{}, len(knowledgeIDs))
	for _, knowledgeID := range knowledgeIDs {
		wanted[strings.TrimSpace(knowledgeID)] = struct{}{}
	}
	result := make([]registeredObject, 0, len(rows))
	for _, row := range rows {
		object, err := decodeObject(row.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode knowledge auxiliary ownership row %d: %w", row.ID, err)
		}
		if err := validateObject(object); err != nil || object.TenantID != tenantID ||
			object.KnowledgeBaseID != knowledgeBaseID || row.ScopeID != knowledgeBaseID ||
			objectKey(object.KnowledgeID, object.Path) != row.DedupKey {
			return nil, fmt.Errorf("knowledge auxiliary ownership row %d is corrupt: %w", row.ID, errors.Join(err, ErrInvalidObject))
		}
		if _, ok := wanted[object.KnowledgeID]; !ok {
			continue
		}
		result = append(result, registeredObject{row: row, object: object})
	}
	return result, nil
}

func (r *Registry) tenant(ctx context.Context, tenantID uint64) (*types.Tenant, error) {
	if r == nil || r.db == nil || tenantID == 0 {
		return nil, errors.New("knowledge auxiliary object tenant identity is unavailable")
	}
	var tenant types.Tenant
	if err := r.db.WithContext(ctx).First(&tenant, "id = ?", tenantID).Error; err != nil {
		return nil, fmt.Errorf("load knowledge auxiliary object tenant %d: %w", tenantID, err)
	}
	return &tenant, nil
}

func (r *Registry) resolveLegacy(ctx context.Context, tenant *types.Tenant, path, fallbackProvider string) (interfaces.FileService, error) {
	if r == nil || r.resolver == nil {
		return nil, errors.New("knowledge auxiliary object provider resolver is unavailable")
	}
	provider, err := ProviderForPath(path, fallbackProvider)
	if err != nil {
		return nil, err
	}
	service, resolved, err := r.resolver(ctx, tenant, provider)
	if err != nil {
		return nil, fmt.Errorf("%w: build provider %s for %q: %v", ErrProviderRouting, provider, path, err)
	}
	if service == nil || normalizeProvider(resolved) != provider {
		return nil, fmt.Errorf("%w: requested %s but resolver returned %q", ErrProviderRouting, provider, resolved)
	}
	return service, nil
}

func (r *Registry) resolveBound(
	ctx context.Context, tenant *types.Tenant, path string, binding *storagebinding.Binding,
) (interfaces.FileService, error) {
	if binding == nil {
		return nil, ErrBindingMissing
	}
	normalized, err := storagebinding.Normalize(*binding)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBindingMismatch, err)
	}
	provider, err := ProviderForPath(path, string(normalized.Provider))
	if err != nil || provider != string(normalized.Provider) {
		return nil, ErrBindingMismatch
	}
	if r == nil || r.bindingResolver == nil {
		return nil, errors.New("knowledge auxiliary object binding resolver is unavailable")
	}
	service, err := r.bindingResolver(ctx, tenant, normalized, path)
	if err != nil {
		return nil, fmt.Errorf("%w: resolve exact %s target for %q: %v", ErrProviderRouting, provider, path, err)
	}
	if service == nil {
		return nil, ErrBindingMismatch
	}
	actualProvider, ok := service.(storagebinding.BindingProvider)
	if !ok || actualProvider == nil {
		return nil, ErrBindingMismatch
	}
	actual, err := actualProvider.BindingForPath(path)
	if err != nil || actual.Fingerprint != normalized.Fingerprint {
		return nil, ErrBindingMismatch
	}
	return service, nil
}

// FileServiceForPath resolves a path through its persisted ownership snapshot
// when available. It is used by FAQ workers so a KB/provider config change
// between enqueue and execution cannot make the payload unreadable.
func (r *Registry) FileServiceForPath(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	path string,
	fallbackProvider string,
) (interfaces.FileService, error) {
	path = strings.TrimSpace(path)
	if tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" ||
		strings.TrimSpace(knowledgeID) == "" || path == "" {
		return nil, ErrInvalidObject
	}
	objects, err := r.list(ctx, tenantID, knowledgeBaseID, []string{knowledgeID})
	if err != nil {
		return nil, err
	}
	var found *Object
	for _, registered := range objects {
		if registered.object.Path == path {
			object := registered.object
			found = &object
			break
		}
	}
	if found == nil {
		return nil, ErrReservationLost
	}
	if found.Quarantined {
		return nil, ErrBindingQuarantined
	}
	tenant, err := r.tenant(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return r.resolveBound(ctx, tenant, path, found.Binding)
}

type deleteTarget struct {
	path             string
	fallbackProvider string
	registered       bool
	binding          *storagebinding.Binding
	quarantined      bool
}

func (r *Registry) deleteTargets(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	targets []deleteTarget,
	parentMoveFenceHeld bool,
) error {
	if len(targets) == 0 {
		return nil
	}
	tenant, err := r.tenant(ctx, tenantID)
	if err != nil {
		return err
	}
	var errs []error
	for _, target := range targets {
		if !target.registered {
			errs = append(errs, fmt.Errorf("%w: refusing unregistered legacy path %q", ErrBindingMissing, target.path))
			continue
		}
		if target.quarantined {
			errs = append(errs, fmt.Errorf("%w: refusing quarantined path", ErrBindingQuarantined))
			continue
		}
		service, routeErr := r.resolveBound(ctx, tenant, target.path, target.binding)
		if routeErr != nil {
			errs = append(errs, routeErr)
			continue
		}
		deleteErr := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			// The exclusive parent lock conflicts with CommitReserved's shared
			// lock. Provider deletion and ledger consume therefore form one
			// serialization interval with every planned provider commit.
			//
			// A reparse move already holds the source+target parent rows for the
			// complete destructive callback. Re-locking the source FOR UPDATE from
			// this separate transaction would wait on our own outer FOR SHARE lock.
			// In that narrow mode the ledger's UPDATE lock still serializes an
			// already-reserved CommitReserved, while the outer parent fence blocks
			// KB deletion and any new reservation. SQLite's outer scope has no
			// long-lived database transaction, so it keeps the ordinary lock here.
			if !parentMoveFenceHeld || tx.Dialector.Name() == "sqlite" {
				if _, err := kbwritefence.LockExisting(tx, tenantID, knowledgeBaseID); err != nil {
					return err
				}
			}

			ids := make([]int64, 0, 1)
			if target.registered {
				query := tx.Where(
					"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
					tenantID, TaskType, types.TaskScopeKnowledgeBase, knowledgeBaseID,
					operationOwned, objectKey(knowledgeID, target.path),
				)
				if tx.Dialector.Name() != "sqlite" {
					query = query.Clauses(clause.Locking{Strength: "UPDATE"})
				}
				var rows []*types.TaskPendingOp
				if err := query.Find(&rows).Error; err != nil {
					return fmt.Errorf("lock auxiliary ownership for %q: %w", target.path, err)
				}
				// A concurrent abort/recovery already proved deletion and consumed
				// the reservation. Never issue an unowned second delete.
				if len(rows) == 0 {
					return nil
				}
				for _, row := range rows {
					object, err := decodeObject(row.Payload)
					if err != nil ||
						object.TenantID != tenantID || object.KnowledgeBaseID != knowledgeBaseID ||
						object.KnowledgeID != knowledgeID || object.Path != target.path ||
						!sameBinding(object.Binding, target.binding) {
						return fmt.Errorf("auxiliary ownership row %d is corrupt while deleting %q", row.ID, target.path)
					}
					if object.Quarantined {
						return ErrBindingQuarantined
					}
					ids = append(ids, row.ID)
				}
			}

			if err := service.DeleteFile(ctx, target.path); err != nil {
				return fmt.Errorf("delete auxiliary object %q: %w", target.path, err)
			}
			if len(ids) > 0 {
				if err := tx.Where("id IN ?", ids).Delete(&types.TaskPendingOp{}).Error; err != nil {
					return fmt.Errorf("consume auxiliary ownership for %q: %w", target.path, err)
				}
			}
			return nil
		})
		if deleteErr != nil {
			errs = append(errs, deleteErr)
		}
	}
	return errors.Join(errs...)
}

func dedupeTargets(targets []deleteTarget) []deleteTarget {
	seen := make(map[string]int, len(targets))
	result := make([]deleteTarget, 0, len(targets))
	for _, target := range targets {
		target.path = strings.TrimSpace(target.path)
		target.fallbackProvider = normalizeProvider(target.fallbackProvider)
		if target.path == "" {
			continue
		}
		if index, exists := seen[target.path]; exists {
			// A persisted per-object snapshot is more precise than a caller's
			// legacy KB fallback, so retain the first non-empty snapshot.
			if result[index].fallbackProvider == "" && target.fallbackProvider != "" {
				result[index].fallbackProvider = target.fallbackProvider
			}
			result[index].registered = result[index].registered || target.registered
			if result[index].binding == nil && target.binding != nil {
				binding := *target.binding
				result[index].binding = &binding
			} else if result[index].binding != nil && target.binding != nil &&
				result[index].binding.Fingerprint != target.binding.Fingerprint {
				// Preserve an impossible mismatch so the subsequent exact resolver
				// fails closed instead of silently choosing one identity.
				result[index].binding = nil
				result[index].registered = true
			}
			result[index].quarantined = result[index].quarantined || target.quarantined
			continue
		}
		seen[target.path] = len(result)
		result = append(result, target)
	}
	return result
}

// DeletePaths deletes only the supplied paths. Registered ownership supplies
// the historical provider snapshot and is consumed per successful path.
func (r *Registry) DeletePaths(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	paths []string,
) error {
	objects, err := r.list(ctx, tenantID, knowledgeBaseID, []string{knowledgeID})
	if err != nil {
		return err
	}
	registeredByPath := make(map[string]Object, len(objects))
	for _, registered := range objects {
		registeredByPath[registered.object.Path] = registered.object
	}
	targets := make([]deleteTarget, 0, len(paths))
	for _, path := range paths {
		target := deleteTarget{path: path, fallbackProvider: fallbackProvider}
		if object, ok := registeredByPath[strings.TrimSpace(path)]; ok {
			target.fallbackProvider = object.FallbackProvider
			target.registered = true
			target.binding = object.Binding
			target.quarantined = object.Quarantined
		}
		targets = append(targets, target)
	}
	return r.deleteTargets(ctx, tenantID, knowledgeBaseID, knowledgeID, dedupeTargets(targets), false)
}

// Abort is the compensating half of Reserve. It deletes the exact planned path
// and consumes its ledger row only after provider deletion succeeds.
func (r *Registry) Abort(ctx context.Context, object Object) error {
	object, err := normalizeObject(object)
	if err != nil {
		return err
	}
	if object.Binding == nil {
		return ErrBindingMissing
	}
	objects, err := r.list(ctx, object.TenantID, object.KnowledgeBaseID, []string{object.KnowledgeID})
	if err != nil {
		return err
	}
	matched := false
	for _, registered := range objects {
		if registered.object.Path == object.Path {
			matched = sameBinding(registered.object.Binding, object.Binding)
			break
		}
	}
	if !matched {
		return ErrBindingMismatch
	}
	return r.DeletePaths(
		ctx, object.TenantID, object.KnowledgeBaseID, object.KnowledgeID,
		object.FallbackProvider, []string{object.Path},
	)
}

func isPersistentSourceKind(kind string) bool {
	return kind == KindSourceFile || kind == KindCloneSourceFile
}

func (r *Registry) cleanupKnowledge(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	legacyPaths []string,
	includePersistent bool,
	parentMoveFenceHeld bool,
) error {
	objects, err := r.list(ctx, tenantID, knowledgeBaseID, []string{knowledgeID})
	if err != nil {
		return err
	}
	targets := make([]deleteTarget, 0, len(objects)+len(legacyPaths))
	references := make(map[string]struct{}, len(objects))
	for _, registered := range objects {
		if reference := strings.TrimSpace(registered.object.Reference); reference != "" {
			references[reference] = struct{}{}
		}
		if !includePersistent && isPersistentSourceKind(registered.object.Kind) {
			continue
		}
		targets = append(targets, deleteTarget{
			path: registered.object.Path, fallbackProvider: registered.object.FallbackProvider,
			registered: true, binding: registered.object.Binding, quarantined: registered.object.Quarantined,
		})
	}
	for _, path := range legacyPaths {
		if _, isDisplayReference := references[referenceIdentity(path)]; isDisplayReference {
			continue
		}
		targets = append(targets, deleteTarget{path: path, fallbackProvider: fallbackProvider})
	}
	return r.deleteTargets(
		ctx, tenantID, knowledgeBaseID, knowledgeID, dedupeTargets(targets), parentMoveFenceHeld,
	)
}

// CleanupDerived removes generation-derived and temporary artifacts while
// preserving the original uploaded/copied source across reparse and retries.
func (r *Registry) CleanupDerived(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	legacyPaths []string,
) error {
	return r.cleanupKnowledge(
		ctx, tenantID, knowledgeBaseID, knowledgeID, fallbackProvider, legacyPaths, false, false,
	)
}

// CleanupDerivedWithinMoveFence is the reparse-move variant of CleanupDerived.
// The caller must hold both move endpoint KB rows through
// kbwritefence.WithActiveSharedSet for the entire call. This avoids attempting
// to upgrade that outer parent lock from a second database connection while
// retaining exact ledger-row serialization with in-flight planned commits.
func (r *Registry) CleanupDerivedWithinMoveFence(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	legacyPaths []string,
) error {
	return r.cleanupKnowledge(
		ctx, tenantID, knowledgeBaseID, knowledgeID, fallbackProvider, legacyPaths, false, true,
	)
}

// CleanupForDelete removes every owned object, including persistent source
// files. It is the only document-level lifecycle API allowed to do so.
func (r *Registry) CleanupForDelete(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	legacyPaths []string,
) error {
	return r.cleanupKnowledge(
		ctx, tenantID, knowledgeBaseID, knowledgeID, fallbackProvider, legacyPaths, true, false,
	)
}

func (r *Registry) objectsForKnowledgeBase(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (map[string][]registeredObject, error) {
	if r == nil || r.db == nil || tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return nil, ErrInvalidObject
	}
	var rows []*types.TaskPendingOp
	if err := r.db.WithContext(ctx).Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ?",
		tenantID, TaskType, types.TaskScopeKnowledgeBase, knowledgeBaseID, operationOwned,
	).Order("id ASC").Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("list KB auxiliary ownership: %w", err)
	}
	grouped := make(map[string][]registeredObject)
	for _, row := range rows {
		object, err := decodeObject(row.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode KB auxiliary ownership row %d: %w", row.ID, err)
		}
		if err := validateObject(object); err != nil || object.TenantID != tenantID ||
			object.KnowledgeBaseID != knowledgeBaseID || row.ScopeID != knowledgeBaseID ||
			objectKey(object.KnowledgeID, object.Path) != row.DedupKey {
			return nil, fmt.Errorf("KB auxiliary ownership row %d is corrupt: %w", row.ID, errors.Join(err, ErrInvalidObject))
		}
		grouped[object.KnowledgeID] = append(grouped[object.KnowledgeID], registeredObject{row: row, object: object})
	}
	return grouped, nil
}

// CleanupKnowledgeBase also sees planned source/copy intents whose knowledge
// row was never committed, so whole-KB deletion cannot strand pre-create
// uploads merely because ListKnowledgeByKnowledgeBaseID cannot return them.
func (r *Registry) CleanupKnowledgeBase(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	fallbackProvider string,
) error {
	grouped, err := r.objectsForKnowledgeBase(ctx, tenantID, knowledgeBaseID)
	if err != nil {
		return err
	}
	var errs []error
	for knowledgeID, objects := range grouped {
		targets := make([]deleteTarget, 0, len(objects))
		for _, registered := range objects {
			targets = append(targets, deleteTarget{
				path: registered.object.Path, fallbackProvider: registered.object.FallbackProvider,
				registered: true, binding: registered.object.Binding, quarantined: registered.object.Quarantined,
			})
		}
		errs = append(errs, r.deleteTargets(
			ctx, tenantID, knowledgeBaseID, knowledgeID, dedupeTargets(targets), false,
		))
	}
	return errors.Join(errs...)
}

func (r *Registry) CountKnowledgeBase(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
) (int64, error) {
	if r == nil || r.db == nil || tenantID == 0 || strings.TrimSpace(knowledgeBaseID) == "" {
		return 0, ErrInvalidObject
	}
	var count int64
	err := r.db.WithContext(ctx).Model(&types.TaskPendingOp{}).Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ?",
		tenantID, TaskType, types.TaskScopeKnowledgeBase, strings.TrimSpace(knowledgeBaseID), operationOwned,
	).Count(&count).Error
	if err != nil {
		return 0, fmt.Errorf("count KB auxiliary ownership: %w", err)
	}
	return count, nil
}

// DeleteSupersededFAQExports removes registered CSV objects not referenced by
// the newly committed LastFAQImportResult. It is safe to retry after a partial
// failure because successful paths consume their own ownership rows.
func (r *Registry) DeleteSupersededFAQExports(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	keepReference string,
) error {
	objects, err := r.list(ctx, tenantID, knowledgeBaseID, []string{knowledgeID})
	if err != nil {
		return err
	}
	paths := make([]string, 0)
	for _, registered := range objects {
		object := registered.object
		if object.Kind == KindFAQFailedExport && object.Reference != referenceIdentity(keepReference) {
			paths = append(paths, object.Path)
		}
	}
	return r.DeletePaths(ctx, tenantID, knowledgeBaseID, knowledgeID, "", paths)
}

// DeleteUnregistered is the compensating action for an object that was stored
// but whose durable ownership transaction failed. The caller must surface any
// returned error: nil is the proof that rollback reached the exact provider.
func (r *Registry) DeleteUnregistered(
	ctx context.Context,
	tenantID uint64,
	fallbackProvider string,
	paths []string,
) error {
	targets := make([]deleteTarget, 0, len(paths))
	for _, path := range paths {
		targets = append(targets, deleteTarget{path: path, fallbackProvider: fallbackProvider})
	}
	targets = dedupeTargets(targets)
	if len(targets) == 0 {
		return nil
	}
	return fmt.Errorf("%w: refusing to delete %d unregistered auxiliary path(s)", ErrBindingMissing, len(targets))
}
