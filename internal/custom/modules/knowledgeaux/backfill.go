package knowledgeaux

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/plannedfile"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/storagebinding"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const backfillBatchSize = 200

var errBackfillTargetChanged = errors.New("knowledge auxiliary backfill storage target changed during adoption")

// BackfillReport is deliberately aggregate-first. Samples are scrubbed of URL
// userinfo/query/fragment data so presigned credentials never reach logs.
type BackfillReport struct {
	Scanned           int
	Adopted           int
	AlreadyRegistered int
	Quarantined       int
	SkippedDisplayURL int
	QuarantineSamples []string
	QuarantineReasons map[string]int
}

type legacyCandidate struct {
	path      string
	kind      string
	reference string
}

type legacyChunkImage struct {
	ID          string         `gorm:"column:id;primaryKey"`
	TenantID    uint64         `gorm:"column:tenant_id"`
	KnowledgeID string         `gorm:"column:knowledge_id"`
	ImageInfo   string         `gorm:"column:image_info"`
	DeletedAt   gorm.DeletedAt `gorm:"column:deleted_at"`
}

type provenLegacyCandidate struct {
	object                 Object
	binding                storagebinding.Binding
	objectKey              string
	existingBound          bool
	ledgerIdentityMismatch bool
}

func scrubDiagnosticPath(raw string) string {
	raw = strings.TrimSpace(raw)
	digest := sha256.Sum256([]byte(raw))
	return fmt.Sprintf("path_sha256=%x", digest[:])
}

func (report *BackfillReport) quarantine(knowledgeID, path, reason string) {
	report.Quarantined++
	if report.QuarantineReasons == nil {
		report.QuarantineReasons = make(map[string]int)
	}
	report.QuarantineReasons[reason]++
	if len(report.QuarantineSamples) >= 50 {
		return
	}
	report.QuarantineSamples = append(report.QuarantineSamples,
		fmt.Sprintf("knowledge=%s path=%s reason=%s", strings.TrimSpace(knowledgeID), scrubDiagnosticPath(path), reason),
	)
}

func tenantHasProviderConfig(config *types.StorageEngineConfig, provider string) bool {
	if config == nil {
		return false
	}
	switch normalizeProvider(provider) {
	case "local":
		return config.Local != nil
	case "minio":
		return config.MinIO != nil
	case "cos":
		return config.COS != nil
	case "tos":
		return config.TOS != nil
	case "s3":
		return config.S3 != nil
	case "oss":
		return config.OSS != nil
	case "ks3":
		return config.KS3 != nil
	case "obs":
		return config.OBS != nil
	default:
		return false
	}
}

func fallbackProviderForBackfill(kb *types.KnowledgeBase, tenant *types.Tenant) string {
	fallback := ""
	if kb != nil {
		fallback = strings.TrimSpace(kb.EffectiveStorageProvider(""))
	}
	if fallback == "" && tenant != nil && tenant.StorageEngineConfig != nil {
		fallback = strings.TrimSpace(tenant.StorageEngineConfig.DefaultProvider)
	}
	if fallback == "" {
		fallback = strings.TrimSpace(os.Getenv("STORAGE_TYPE"))
	}
	return fallback
}

// serviceForBackfill chooses the same current profile that owns new writes,
// then requires that concrete service to prove the complete provider/bucket/
// region/prefix path. It never probes storage and never guesses another root.
func (r *Registry) serviceForBackfill(
	ctx context.Context, tenant *types.Tenant, provider, path string,
) (interfaces.FileService, error) {
	provider = normalizeProvider(provider)
	if provider == "" {
		return nil, ErrProviderRouting
	}
	if tenant != nil && tenantHasProviderConfig(tenant.StorageEngineConfig, provider) {
		if r == nil || r.resolver == nil {
			return nil, errors.New("tenant storage resolver is unavailable")
		}
		service, resolved, err := r.resolver(ctx, tenant, provider)
		if err != nil {
			return nil, err
		}
		if service == nil || normalizeProvider(resolved) != provider {
			return nil, ErrProviderRouting
		}
		if _, err := bindObjectToService(Object{
			TenantID: 1, KnowledgeBaseID: "probe", KnowledgeID: "probe",
			Path: path, FallbackProvider: provider, Kind: KindSourceFile,
		}, service); err != nil {
			return nil, err
		}
		return service, nil
	}
	if r != nil && r.injectedGlobal != nil {
		if _, err := bindObjectToService(Object{
			TenantID: 1, KnowledgeBaseID: "probe", KnowledgeID: "probe",
			Path: path, FallbackProvider: provider, Kind: KindSourceFile,
		}, r.injectedGlobal); err == nil {
			return r.injectedGlobal, nil
		}
	}
	// NewWithResolver is used by tests and embedding applications that inject
	// their own already-constructed service. Production Registry.New reaches
	// this only when no global service was supplied.
	if r != nil && r.resolver != nil {
		service, resolved, err := r.resolver(ctx, tenant, provider)
		if err != nil {
			return nil, err
		}
		if service == nil || normalizeProvider(resolved) != provider {
			return nil, ErrProviderRouting
		}
		if _, err := bindObjectToService(Object{
			TenantID: 1, KnowledgeBaseID: "probe", KnowledgeID: "probe",
			Path: path, FallbackProvider: provider, Kind: KindSourceFile,
		}, service); err != nil {
			return nil, err
		}
		return service, nil
	}
	return nil, ErrProviderRouting
}

func candidatesForKnowledge(knowledge *types.Knowledge, imageInfo []string) []legacyCandidate {
	if knowledge == nil {
		return nil
	}
	result := make([]legacyCandidate, 0, len(imageInfo)+2)
	if path := strings.TrimSpace(knowledge.FilePath); path != "" {
		result = append(result, legacyCandidate{path: path, kind: KindSourceFile})
	}
	if len(knowledge.ProcessingFanout) > 0 {
		if plan, err := processownership.ParseFanoutPlan(knowledge.ProcessingFanout); err == nil {
			for _, image := range plan.Images {
				result = append(result, legacyCandidate{path: image.ImageURL, kind: KindFanoutImage})
			}
		}
	}
	for _, raw := range imageInfo {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		var images []*types.ImageInfo
		if err := json.Unmarshal([]byte(raw), &images); err != nil {
			continue
		}
		for _, image := range images {
			if image != nil && strings.TrimSpace(image.URL) != "" {
				result = append(result, legacyCandidate{path: image.URL, kind: KindFanoutImage})
			}
		}
	}
	seen := make(map[string]struct{}, len(result))
	deduped := make([]legacyCandidate, 0, len(result))
	for _, candidate := range result {
		candidate.path = strings.TrimSpace(candidate.path)
		if candidate.path == "" {
			continue
		}
		if _, exists := seen[candidate.path]; exists {
			continue
		}
		seen[candidate.path] = struct{}{}
		deduped = append(deduped, candidate)
	}
	return deduped
}

func hasFAQDisplayReference(knowledge *types.Knowledge) bool {
	if knowledge == nil || len(knowledge.LastFAQImportResult) == 0 {
		return false
	}
	var result types.FAQImportResult
	return json.Unmarshal(knowledge.LastFAQImportResult, &result) == nil &&
		strings.TrimSpace(result.FailedEntriesURL) != ""
}

func bindingObjectKey(binding storagebinding.Binding, filePath string) (string, error) {
	normalized, err := storagebinding.Normalize(binding)
	if err != nil {
		return "", err
	}
	provider := string(normalized.Provider)
	switch normalized.Provider {
	case storagebinding.ProviderLocal:
		if !strings.HasPrefix(filePath, "local://") {
			return "", ErrBindingMismatch
		}
		key := strings.TrimPrefix(filePath, "local://")
		if err := plannedfile.ValidateKey(key, ""); err != nil {
			return "", err
		}
		return key, nil
	case storagebinding.ProviderDummy:
		if !strings.HasPrefix(filePath, "dummy://") {
			return "", ErrBindingMismatch
		}
		key := strings.TrimPrefix(filePath, "dummy://")
		if err := plannedfile.ValidateKey(key, ""); err != nil {
			return "", err
		}
		return key, nil
	case storagebinding.ProviderCOS:
		if strings.HasPrefix(filePath, "cos://") {
			return plannedfile.ParseRegionPath(
				filePath, provider, normalized.Bucket, normalized.Region, normalized.PathPrefix,
			)
		}
		legacyPrefix := strings.TrimRight(normalized.Endpoint, "/") + "/"
		if !strings.HasPrefix(filePath, legacyPrefix) {
			return "", ErrBindingMismatch
		}
		key := strings.TrimPrefix(filePath, legacyPrefix)
		if err := plannedfile.ValidateKey(key, normalized.PathPrefix); err != nil {
			return "", err
		}
		return key, nil
	case storagebinding.ProviderOBS:
		if strings.HasPrefix(filePath, "obs://") {
			return plannedfile.ParseBucketPath(
				filePath, provider, normalized.Bucket, normalized.PathPrefix,
			)
		}
		proxyPrefix := strings.TrimRight(normalized.ProxyDomain, "/") + "/"
		if normalized.ProxyDomain == "" || !strings.HasPrefix(filePath, proxyPrefix) {
			return "", ErrBindingMismatch
		}
		key := strings.TrimPrefix(filePath, proxyPrefix)
		if err := plannedfile.ValidateKey(key, normalized.PathPrefix); err != nil {
			return "", err
		}
		return key, nil
	default:
		return plannedfile.ParseBucketPath(
			filePath, provider, normalized.Bucket, normalized.PathPrefix,
		)
	}
}

func validateOwnerNamespace(
	binding storagebinding.Binding, filePath string, tenantID uint64, knowledgeID, kind string,
) error {
	key, err := bindingObjectKey(binding, filePath)
	if err != nil {
		return err
	}
	prefix, err := plannedfile.NormalizePrefix(binding.PathPrefix)
	if err != nil {
		return err
	}
	relative := key
	if prefix != "" {
		marker := prefix + "/"
		if !strings.HasPrefix(key, marker) {
			return ErrBindingMismatch
		}
		relative = strings.TrimPrefix(key, marker)
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 3 || parts[0] != strconv.FormatUint(tenantID, 10) {
		return fmt.Errorf("%w: object key is outside the exact owner namespace", ErrBindingMismatch)
	}
	switch kind {
	case KindSourceFile, KindCloneSourceFile:
		if parts[1] != strings.TrimSpace(knowledgeID) {
			return fmt.Errorf("%w: source key is outside the exact knowledge namespace", ErrBindingMismatch)
		}
	case KindFanoutImage, KindCloneImage:
		// Legacy planned image writes used the tenant-scoped exports namespace.
		// Cross-knowledge uniqueness is checked globally before this function.
		// New writes use the knowledge namespace and are accepted as well.
		if parts[1] != "exports" && parts[1] != strings.TrimSpace(knowledgeID) {
			return fmt.Errorf("%w: image key is outside the exact application namespace", ErrBindingMismatch)
		}
	default:
		return fmt.Errorf("%w: object kind has no backfill namespace rule", ErrBindingMismatch)
	}
	return nil
}

func (r *Registry) existingOwnership(
	ctx context.Context, object Object,
) ([]*types.TaskPendingOp, error) {
	var rows []*types.TaskPendingOp
	err := r.db.WithContext(ctx).Where(
		"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
		object.TenantID, TaskType, types.TaskScopeKnowledgeBase, object.KnowledgeBaseID,
		operationOwned, objectKey(object.KnowledgeID, object.Path),
	).Order("id ASC").Find(&rows).Error
	return rows, err
}

func (r *Registry) proveLegacyCandidate(
	ctx context.Context,
	tenant *types.Tenant,
	kb *types.KnowledgeBase,
	knowledge *types.Knowledge,
	candidate legacyCandidate,
	services map[string]interfaces.FileService,
) (provenLegacyCandidate, string, error) {
	fallback := fallbackProviderForBackfill(kb, tenant)
	provider, err := ProviderForPath(candidate.path, fallback)
	if err != nil {
		return provenLegacyCandidate{}, "provider-unresolved", err
	}
	object := Object{
		TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
		KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
		Path: candidate.path, Reference: candidate.reference,
		FallbackProvider: provider, Kind: candidate.kind,
	}
	rows, err := r.existingOwnership(ctx, object)
	if err != nil {
		return provenLegacyCandidate{}, "ledger-read-failed", err
	}
	var persistedBinding *storagebinding.Binding
	allBound := len(rows) > 0
	ledgerIdentityMismatch := false
	if len(rows) > 0 {
		for _, row := range rows {
			persisted, decodeErr := decodeObject(row.Payload)
			if decodeErr != nil || persisted.TenantID != object.TenantID ||
				persisted.KnowledgeBaseID != object.KnowledgeBaseID ||
				persisted.KnowledgeID != object.KnowledgeID || persisted.Path != object.Path {
				return provenLegacyCandidate{}, "ledger-conflict", ErrInvalidObject
			}
			if persisted.Binding == nil {
				allBound = false
			} else if persistedBinding == nil {
				binding := *persisted.Binding
				persistedBinding = &binding
			} else if persistedBinding.Fingerprint != persisted.Binding.Fingerprint {
				return provenLegacyCandidate{}, "ledger-binding-conflict", ErrBindingMismatch
			}
			if !migrationLedgerIdentityMatches(persisted, object) {
				allBound = false
				ledgerIdentityMismatch = true
			}
			// A quarantined row is never an already-complete migration result.
			// It must pass the locked adoption path below, which may repair only
			// the narrowly defined legacy-identity quarantine after proving the
			// current owner, path and physical binding again.
			if persisted.Quarantined {
				allBound = false
			}
		}
	}
	if persistedBinding == nil {
		serviceKey := fmt.Sprintf("%d:%s", knowledge.TenantID, provider)
		service := services[serviceKey]
		if service == nil {
			service, err = r.serviceForBackfill(ctx, tenant, provider, candidate.path)
			if err != nil {
				return provenLegacyCandidate{}, "path-not-provable", err
			}
			services[serviceKey] = service
		}
		bound, err := bindObjectToService(object, service)
		if err != nil || bound.Binding == nil {
			return provenLegacyCandidate{}, "binding-not-provable", errors.Join(err, ErrBindingMissing)
		}
		persistedBinding = bound.Binding
	}
	if err := validateOwnerNamespace(
		*persistedBinding, candidate.path, knowledge.TenantID, knowledge.ID, candidate.kind,
	); err != nil {
		return provenLegacyCandidate{}, "owner-namespace-mismatch", err
	}
	key, err := bindingObjectKey(*persistedBinding, candidate.path)
	if err != nil {
		return provenLegacyCandidate{}, "canonical-key-failed", err
	}
	binding := *persistedBinding
	object.Binding = &binding
	return provenLegacyCandidate{
		object: object, binding: binding, objectKey: key, existingBound: allBound,
		ledgerIdentityMismatch: ledgerIdentityMismatch,
	}, "", nil
}

func migrationLedgerIdentityMatches(persisted, current Object) bool {
	if strings.TrimSpace(persisted.Kind) != strings.TrimSpace(current.Kind) {
		return false
	}
	if isPersistentSourceKind(current.Kind) {
		return true
	}
	// A pre-migration completed document legitimately has no processing
	// generation. The derived path is still exactly attributable when both the
	// persisted ledger and the locked current owner carry that same empty legacy
	// identity. Requiring a non-empty value made the first backfill succeed and
	// every later startup quarantine that same valid row, so the migration was
	// not idempotent.
	return persisted.ProcessingGeneration == current.ProcessingGeneration
}

func lockTenantForBackfill(tx *gorm.DB, tenantID uint64) (*types.Tenant, error) {
	if tx == nil || tenantID == 0 {
		return nil, ErrInvalidObject
	}
	if tx.Dialector.Name() == "sqlite" {
		result := tx.Exec("UPDATE tenants SET id = id WHERE id = ?", tenantID)
		if result.Error != nil {
			return nil, result.Error
		}
		if result.RowsAffected != 1 {
			return nil, gorm.ErrRecordNotFound
		}
	}
	query := tx.Unscoped().Where("id = ?", tenantID)
	if tx.Dialector.Name() != "sqlite" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var tenant types.Tenant
	if err := query.Take(&tenant).Error; err != nil {
		return nil, err
	}
	return &tenant, nil
}

// revalidateMigrationTarget is the commit-time proof for legacy adoption.
// It locks the tenant configuration before the KB/knowledge/ledger lock chain,
// rebuilds a no-provision service from the current configuration, and accepts
// credential/profile rotation only when the provider and physical target are
// still exactly the same as discovery proved.
func (r *Registry) revalidateMigrationTarget(
	ctx context.Context, tx *gorm.DB, proven provenLegacyCandidate,
) (storagebinding.Binding, string, error) {
	object := proven.object
	tenant, err := lockTenantForBackfill(tx, object.TenantID)
	if err != nil {
		return storagebinding.Binding{}, "", err
	}
	if _, err := kbwritefence.LockExisting(tx, object.TenantID, object.KnowledgeBaseID); err != nil {
		return storagebinding.Binding{}, "", err
	}
	kbQuery := tx.Unscoped().Where(
		"tenant_id = ? AND id = ?", object.TenantID, object.KnowledgeBaseID,
	)
	if tx.Dialector.Name() != "sqlite" {
		kbQuery = kbQuery.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var kb types.KnowledgeBase
	if err := kbQuery.Take(&kb).Error; err != nil {
		return storagebinding.Binding{}, "", err
	}
	provider, err := ProviderForPath(object.Path, fallbackProviderForBackfill(&kb, tenant))
	if err != nil || provider != string(proven.binding.Provider) {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	service, err := r.serviceForBackfill(ctx, tenant, provider, object.Path)
	if err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	current, err := bindObjectToService(object, service)
	if err != nil || current.Binding == nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err, ErrBindingMissing)
	}
	if err := validateOwnerNamespace(
		*current.Binding, object.Path, object.TenantID, object.KnowledgeID, object.Kind,
	); err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	currentKey, err := bindingObjectKey(*current.Binding, object.Path)
	if err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	if physicalObjectDigest(proven.binding, proven.objectKey) !=
		physicalObjectDigest(*current.Binding, currentKey) {
		return storagebinding.Binding{}, "", errBackfillTargetChanged
	}
	return *current.Binding, currentKey, nil
}

func migrationOwnerReferencesImage(tx *gorm.DB, owner *types.Knowledge, filePath string) (bool, error) {
	if owner == nil {
		return false, nil
	}
	if len(owner.ProcessingFanout) > 0 {
		if plan, err := processownership.ParseFanoutPlan(owner.ProcessingFanout); err == nil {
			for _, image := range plan.Images {
				if strings.TrimSpace(image.ImageURL) == filePath {
					return true, nil
				}
			}
		}
	}
	if !tx.Migrator().HasTable(&types.Chunk{}) {
		return false, nil
	}
	var rows []legacyChunkImage
	query := tx.Table("chunks").Select("knowledge_id", "image_info", "deleted_at").
		Where("tenant_id = ? AND knowledge_id = ? AND image_info <> ''", owner.TenantID, owner.ID)
	allowDeletedChunks := owner.DeletedAt.Valid || owner.ParseStatus == types.ParseStatusDeleting ||
		owner.ParseStatus == types.ParseStatusCancelling
	if !allowDeletedChunks {
		query = query.Where("deleted_at IS NULL")
	}
	if err := query.Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		var images []*types.ImageInfo
		if json.Unmarshal([]byte(row.ImageInfo), &images) != nil {
			continue
		}
		for _, image := range images {
			if image != nil && strings.TrimSpace(image.URL) == filePath {
				return true, nil
			}
		}
	}
	return false, nil
}

func imageInfoReferencesPath(raw, filePath string) bool {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(filePath) == "" {
		return false
	}
	var images []*types.ImageInfo
	if json.Unmarshal([]byte(raw), &images) != nil {
		return false
	}
	for _, image := range images {
		if image != nil && strings.TrimSpace(image.URL) == strings.TrimSpace(filePath) {
			return true
		}
	}
	return false
}

// validateExactDerivedOwnerNamespace is intentionally stricter than the
// repository-wide migration rule. The bulk migration may prove historical
// tenant-scoped exports after a global alias/owner scan; a live reparse is only
// allowed to adopt a path whose canonical key names this exact knowledge.
func validateExactDerivedOwnerNamespace(
	binding storagebinding.Binding,
	filePath string,
	tenantID uint64,
	knowledgeID string,
) error {
	if err := validateOwnerNamespace(
		binding, filePath, tenantID, knowledgeID, KindFanoutImage,
	); err != nil {
		return err
	}
	key, err := bindingObjectKey(binding, filePath)
	if err != nil {
		return err
	}
	prefix, err := plannedfile.NormalizePrefix(binding.PathPrefix)
	if err != nil {
		return err
	}
	relative := key
	if prefix != "" {
		marker := prefix + "/"
		if !strings.HasPrefix(key, marker) {
			return ErrBindingMismatch
		}
		relative = strings.TrimPrefix(key, marker)
	}
	parts := strings.Split(relative, "/")
	if len(parts) != 3 ||
		parts[0] != strconv.FormatUint(tenantID, 10) ||
		parts[1] != strings.TrimSpace(knowledgeID) {
		return fmt.Errorf(
			"%w: live cleanup adoption requires the exact tenant/knowledge namespace",
			ErrBindingMismatch,
		)
	}
	return nil
}

// cleanupOwnerReferencesImage always includes soft-deleted chunks. Reparse
// cleanup needs those rows as immutable evidence after an earlier generation
// has already soft-deleted its active chunk set.
func cleanupOwnerReferencesImage(
	tx *gorm.DB,
	owner *types.Knowledge,
	filePath string,
) (bool, error) {
	if owner == nil {
		return false, nil
	}
	if len(owner.ProcessingFanout) > 0 {
		if plan, err := processownership.ParseFanoutPlan(owner.ProcessingFanout); err == nil {
			for _, image := range plan.Images {
				if strings.TrimSpace(image.ImageURL) == strings.TrimSpace(filePath) {
					return true, nil
				}
			}
		}
	}
	if !tx.Migrator().HasTable(&types.Chunk{}) {
		return false, nil
	}
	var rows []legacyChunkImage
	if err := tx.Unscoped().Model(&types.Chunk{}).
		Select("tenant_id", "knowledge_id", "image_info", "deleted_at").
		Where(
			"tenant_id = ? AND knowledge_id = ? AND image_info <> ''",
			owner.TenantID, owner.ID,
		).
		Find(&rows).Error; err != nil {
		return false, err
	}
	for _, row := range rows {
		if imageInfoReferencesPath(row.ImageInfo, filePath) {
			return true, nil
		}
	}
	return false, nil
}

func fanoutReferencesPath(raw types.JSON, filePath string) bool {
	if len(raw) == 0 {
		return false
	}
	plan, err := processownership.ParseFanoutPlan(raw)
	if err != nil {
		return false
	}
	for _, image := range plan.Images {
		if strings.TrimSpace(image.ImageURL) == strings.TrimSpace(filePath) {
			return true
		}
	}
	return false
}

// ensureCleanupDerivedPathHasOneOwner rejects legacy corruption before signing
// it into the durable ledger. The scan is deliberately bounded and only runs
// for an unregistered legacy path; ordinary registered document processing
// never pays this cost.
func (r *Registry) ensureCleanupDerivedPathHasOneOwner(
	tx *gorm.DB,
	object Object,
	binding storagebinding.Binding,
	objectKeyValue string,
) error {
	if tx == nil {
		return ErrInvalidObject
	}
	if tx.Migrator().HasTable(&types.Chunk{}) {
		var batch []legacyChunkImage
		err := tx.Unscoped().Model(&types.Chunk{}).
			Select("id", "tenant_id", "knowledge_id", "image_info", "deleted_at").
			Where("image_info <> '' AND (tenant_id <> ? OR knowledge_id <> ?)",
				object.TenantID, object.KnowledgeID).
			Order("id ASC").
			FindInBatches(&batch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
				for _, row := range batch {
					if imageInfoReferencesPath(row.ImageInfo, object.Path) {
						return fmt.Errorf(
							"%w: derived path is referenced by another knowledge",
							ErrBindingMismatch,
						)
					}
				}
				return nil
			}).Error
		if err != nil {
			return err
		}
	}

	type fanoutOwner struct {
		ID               string `gorm:"primaryKey"`
		TenantID         uint64
		ProcessingFanout types.JSON
	}
	var knowledgeBatch []fanoutOwner
	if err := tx.Unscoped().Table("knowledges").
		Select("id", "tenant_id", "processing_fanout").
		Where(
			"processing_fanout IS NOT NULL AND (tenant_id <> ? OR id <> ?)",
			object.TenantID, object.KnowledgeID,
		).
		Order("id ASC").
		FindInBatches(&knowledgeBatch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
			for _, owner := range knowledgeBatch {
				if fanoutReferencesPath(owner.ProcessingFanout, object.Path) {
					return fmt.Errorf(
						"%w: derived path is present in another knowledge fanout",
						ErrBindingMismatch,
					)
				}
			}
			return nil
		}).Error; err != nil {
		return err
	}

	targetPhysical := physicalObjectDigest(binding, objectKeyValue)
	var ledgerBatch []*types.TaskPendingOp
	return tx.Where("task_type = ?", TaskType).
		Order("id ASC").
		FindInBatches(&ledgerBatch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
			for _, row := range ledgerBatch {
				persisted, err := decodeObject(row.Payload)
				if err != nil {
					return fmt.Errorf(
						"decode auxiliary ownership row %d during cleanup adoption: %w",
						row.ID, err,
					)
				}
				if persisted.TenantID == object.TenantID &&
					persisted.KnowledgeBaseID == object.KnowledgeBaseID &&
					persisted.KnowledgeID == object.KnowledgeID &&
					persisted.Path == object.Path {
					continue
				}
				if persisted.Path == object.Path {
					return fmt.Errorf(
						"%w: derived path already belongs to another knowledge",
						ErrBindingMismatch,
					)
				}
				if persisted.Binding == nil {
					continue
				}
				key, keyErr := bindingObjectKey(*persisted.Binding, persisted.Path)
				if keyErr == nil &&
					physicalObjectDigest(*persisted.Binding, key) == targetPhysical {
					return fmt.Errorf(
						"%w: derived physical object already belongs to another knowledge",
						ErrBindingMismatch,
					)
				}
			}
			return nil
		}).Error
}

func (r *Registry) revalidateCleanupTargetWithinMoveFence(
	ctx context.Context,
	tx *gorm.DB,
	proven provenLegacyCandidate,
) (storagebinding.Binding, string, error) {
	object := proven.object
	var tenant types.Tenant
	if err := tx.Where("id = ?", object.TenantID).Take(&tenant).Error; err != nil {
		return storagebinding.Binding{}, "", err
	}
	var kb types.KnowledgeBase
	if err := tx.Unscoped().Where(
		"tenant_id = ? AND id = ?", object.TenantID, object.KnowledgeBaseID,
	).Take(&kb).Error; err != nil {
		return storagebinding.Binding{}, "", err
	}
	provider, err := ProviderForPath(
		object.Path, fallbackProviderForBackfill(&kb, &tenant),
	)
	if err != nil || provider != string(proven.binding.Provider) {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	service, err := r.serviceForBackfill(ctx, &tenant, provider, object.Path)
	if err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	current, err := bindObjectToService(object, service)
	if err != nil || current.Binding == nil {
		return storagebinding.Binding{}, "", errors.Join(
			errBackfillTargetChanged, err, ErrBindingMissing,
		)
	}
	if err := validateExactDerivedOwnerNamespace(
		*current.Binding, object.Path, object.TenantID, object.KnowledgeID,
	); err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	currentKey, err := bindingObjectKey(*current.Binding, object.Path)
	if err != nil {
		return storagebinding.Binding{}, "", errors.Join(errBackfillTargetChanged, err)
	}
	if physicalObjectDigest(proven.binding, proven.objectKey) !=
		physicalObjectDigest(*current.Binding, currentKey) {
		return storagebinding.Binding{}, "", errBackfillTargetChanged
	}
	return *current.Binding, currentKey, nil
}

// adoptLegacyDerivedCleanupPath signs one narrowly provable pre-registry image
// into the ownership ledger. It never deletes provider data. The normal path
// follows the tenant -> KB -> knowledge -> ledger lock order. A knowledge move
// already owns the parent KB shared fence, so its variant skips an impossible
// lock upgrade and still serializes on the knowledge/ledger rows.
func (r *Registry) adoptLegacyDerivedCleanupPath(
	ctx context.Context,
	tenantID uint64,
	knowledgeBaseID string,
	knowledgeID string,
	fallbackProvider string,
	filePath string,
	parentMoveFenceHeld bool,
) error {
	if r == nil || r.db == nil || tenantID == 0 ||
		strings.TrimSpace(knowledgeBaseID) == "" ||
		strings.TrimSpace(knowledgeID) == "" ||
		strings.TrimSpace(filePath) == "" {
		return ErrInvalidObject
	}

	var tenant types.Tenant
	if err := r.db.WithContext(ctx).Where("id = ?", tenantID).Take(&tenant).Error; err != nil {
		return err
	}
	var kb types.KnowledgeBase
	if err := r.db.WithContext(ctx).Unscoped().Where(
		"tenant_id = ? AND id = ?", tenantID, knowledgeBaseID,
	).Take(&kb).Error; err != nil {
		return err
	}
	var owner types.Knowledge
	if err := r.db.WithContext(ctx).Unscoped().Where(
		"tenant_id = ? AND knowledge_base_id = ? AND id = ?",
		tenantID, knowledgeBaseID, knowledgeID,
	).Take(&owner).Error; err != nil {
		return err
	}
	if owner.DeletedAt.Valid {
		return ErrKnowledgeFence
	}
	provider, err := ProviderForPath(filePath, fallbackProvider)
	if err != nil {
		return err
	}
	service, err := r.serviceForBackfill(ctx, &tenant, provider, filePath)
	if err != nil {
		return err
	}
	object := Object{
		TenantID: tenantID, KnowledgeBaseID: strings.TrimSpace(knowledgeBaseID),
		KnowledgeID:          strings.TrimSpace(knowledgeID),
		ProcessingGeneration: owner.ProcessingGeneration,
		Path:                 strings.TrimSpace(filePath), FallbackProvider: provider,
		Kind: KindFanoutImage,
	}
	bound, err := bindObjectToService(object, service)
	if err != nil || bound.Binding == nil {
		return errors.Join(err, ErrBindingMissing)
	}
	if err := validateExactDerivedOwnerNamespace(
		*bound.Binding, bound.Path, tenantID, knowledgeID,
	); err != nil {
		return err
	}
	key, err := bindingObjectKey(*bound.Binding, bound.Path)
	if err != nil {
		return err
	}
	proven := provenLegacyCandidate{
		object: bound, binding: *bound.Binding, objectKey: key,
	}

	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var (
			currentBinding storagebinding.Binding
			currentKey     string
			err            error
		)
		if parentMoveFenceHeld && tx.Dialector.Name() != "sqlite" {
			currentBinding, currentKey, err =
				r.revalidateCleanupTargetWithinMoveFence(ctx, tx, proven)
		} else {
			currentBinding, currentKey, err =
				r.revalidateMigrationTarget(ctx, tx, proven)
			if err == nil {
				err = validateExactDerivedOwnerNamespace(
					currentBinding, bound.Path, tenantID, knowledgeID,
				)
			}
		}
		if err != nil {
			return err
		}

		ownerQuery := tx.Unscoped().Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id = ?",
			tenantID, knowledgeBaseID, knowledgeID,
		)
		if tx.Dialector.Name() != "sqlite" {
			ownerQuery = ownerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var currentOwner types.Knowledge
		if err := ownerQuery.Take(&currentOwner).Error; err != nil {
			return err
		}
		if currentOwner.DeletedAt.Valid ||
			currentOwner.ProcessingGeneration != owner.ProcessingGeneration {
			return errBackfillTargetChanged
		}
		referenced, err := cleanupOwnerReferencesImage(tx, &currentOwner, bound.Path)
		if err != nil {
			return err
		}
		if !referenced {
			return fmt.Errorf(
				"%w: derived path is not referenced by the locked owner",
				ErrBindingMismatch,
			)
		}
		if err := r.ensureCleanupDerivedPathHasOneOwner(
			tx, bound, currentBinding, currentKey,
		); err != nil {
			return err
		}

		bound.Binding = &currentBinding
		bound.ProcessingGeneration = currentOwner.ProcessingGeneration
		ledgerQuery := tx.Where(
			"tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			tenantID, TaskType, types.TaskScopeKnowledgeBase, knowledgeBaseID,
			operationOwned, objectKey(knowledgeID, bound.Path),
		)
		if tx.Dialector.Name() != "sqlite" {
			ledgerQuery = ledgerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var rows []*types.TaskPendingOp
		if err := ledgerQuery.Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > 0 {
			for _, row := range rows {
				persisted, decodeErr := decodeObject(row.Payload)
				if decodeErr != nil ||
					persisted.TenantID != bound.TenantID ||
					persisted.KnowledgeBaseID != bound.KnowledgeBaseID ||
					persisted.KnowledgeID != bound.KnowledgeID ||
					persisted.Path != bound.Path ||
					persisted.Kind != KindFanoutImage ||
					persisted.Quarantined ||
					!sameBinding(persisted.Binding, bound.Binding) {
					return ErrBindingMismatch
				}
			}
			return nil
		}
		payload, err := json.Marshal(bound)
		if err != nil {
			return err
		}
		return tx.Create(&types.TaskPendingOp{
			TenantID: tenantID, TaskType: TaskType,
			Scope: types.TaskScopeKnowledgeBase, ScopeID: knowledgeBaseID,
			Op: operationOwned, DedupKey: objectKey(knowledgeID, bound.Path),
			Payload: payload, EnqueuedAt: time.Now().UTC(),
		}).Error
	})
}

// adoptMigrationBinding is intentionally separate from Register: it may lock
// and sign an exact deleting/cancelling or soft-deleted owner so an already
// durable deletion intent can converge after upgrade. It never performs a
// provider operation and never relaxes the active-writer fence.
func (r *Registry) adoptMigrationBinding(ctx context.Context, proven provenLegacyCandidate) error {
	object := proven.object
	identityQuarantined := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		currentBinding, currentKey, err := r.revalidateMigrationTarget(ctx, tx, proven)
		if err != nil {
			return err
		}
		object.Binding = &currentBinding
		proven.binding = currentBinding
		proven.objectKey = currentKey
		query := tx.Unscoped().Where(
			"tenant_id = ? AND knowledge_base_id = ? AND id = ?",
			object.TenantID, object.KnowledgeBaseID, object.KnowledgeID,
		)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var owner types.Knowledge
		if err := query.Take(&owner).Error; err != nil {
			return err
		}
		switch object.Kind {
		case KindSourceFile, KindCloneSourceFile:
			if strings.TrimSpace(owner.FilePath) != object.Path {
				return ErrBindingMismatch
			}
		case KindFanoutImage, KindCloneImage:
			referenced, err := migrationOwnerReferencesImage(tx, &owner, object.Path)
			if err != nil {
				return err
			}
			if !referenced {
				return ErrBindingMismatch
			}
		default:
			return ErrInvalidObject
		}
		if !isPersistentSourceKind(object.Kind) &&
			owner.ProcessingGeneration != object.ProcessingGeneration {
			return errBackfillTargetChanged
		}
		object.ProcessingGeneration = owner.ProcessingGeneration
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
			return err
		}
		if len(rows) == 0 {
			payload, err := json.Marshal(object)
			if err != nil {
				return err
			}
			return tx.Create(&types.TaskPendingOp{
				TenantID: object.TenantID, TaskType: TaskType,
				Scope: types.TaskScopeKnowledgeBase, ScopeID: object.KnowledgeBaseID,
				Op: operationOwned, DedupKey: objectKey(object.KnowledgeID, object.Path),
				Payload: payload, EnqueuedAt: owner.UpdatedAt.UTC(),
			}).Error
		}
		identityMismatch := proven.ledgerIdentityMismatch
		for _, row := range rows {
			persisted, decodeErr := decodeObject(row.Payload)
			if decodeErr != nil || persisted.TenantID != object.TenantID ||
				persisted.KnowledgeBaseID != object.KnowledgeBaseID ||
				persisted.KnowledgeID != object.KnowledgeID || persisted.Path != object.Path {
				return ErrInvalidObject
			}
			if !migrationLedgerIdentityMatches(persisted, object) {
				identityMismatch = true
			}
		}
		if identityMismatch {
			for _, row := range rows {
				persisted, decodeErr := decodeObject(row.Payload)
				if decodeErr != nil {
					return decodeErr
				}
				persisted.Quarantined = true
				persisted.QuarantineReason = quarantineReasonLegacyIdentity
				payload, marshalErr := json.Marshal(persisted)
				if marshalErr != nil {
					return marshalErr
				}
				if updateErr := tx.Model(&types.TaskPendingOp{}).Where("id = ?", row.ID).
					Update("payload", payload).Error; updateErr != nil {
					return updateErr
				}
			}
			identityQuarantined = true
			return nil
		}
		for _, row := range rows {
			persisted, err := decodeObject(row.Payload)
			if err != nil || persisted.TenantID != object.TenantID ||
				persisted.KnowledgeBaseID != object.KnowledgeBaseID ||
				persisted.KnowledgeID != object.KnowledgeID || persisted.Path != object.Path {
				return ErrInvalidObject
			}
			if persisted.Quarantined {
				if persisted.QuarantineReason != quarantineReasonLegacyIdentity ||
					!migrationLedgerIdentityMatches(persisted, object) {
					return ErrBindingQuarantined
				}
				// This row was quarantined by the old non-idempotent migration
				// rule. The transaction has re-proved its current owner reference,
				// exact physical target and ledger tuple, so it is safe to repair.
				persisted.Quarantined = false
				persisted.QuarantineReason = ""
			}
			if persisted.Binding != nil {
				key, keyErr := bindingObjectKey(*persisted.Binding, persisted.Path)
				if keyErr != nil || physicalObjectDigest(*persisted.Binding, key) !=
					physicalObjectDigest(proven.binding, proven.objectKey) {
					return errors.Join(ErrBindingMismatch, keyErr)
				}
			} else {
				binding := proven.binding
				persisted.Binding = &binding
			}
			persisted.Reference = referenceIdentity(persisted.Reference)
			payload, err := json.Marshal(persisted)
			if err != nil {
				return err
			}
			if err := tx.Model(&types.TaskPendingOp{}).Where("id = ?", row.ID).
				Update("payload", payload).Error; err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if identityQuarantined {
		return ErrBindingQuarantined
	}
	return nil
}

func (r *Registry) sanitizePersistedReferences(ctx context.Context) error {
	var batch []*types.TaskPendingOp
	return r.db.WithContext(ctx).Where("task_type = ?", TaskType).Order("id ASC").
		FindInBatches(&batch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
			for _, snapshot := range batch {
				var identity Object
				if err := json.Unmarshal(snapshot.Payload, &identity); err != nil ||
					strings.TrimSpace(identity.Reference) == "" ||
					referenceIdentity(identity.Reference) == identity.Reference {
					continue
				}
				err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
					// Preserve the repository-wide KB -> ledger lock order. The
					// snapshot is used only to choose that lock; payload mutation is
					// based exclusively on the row re-read under FOR UPDATE.
					_, lockErr := kbwritefence.LockExisting(
						tx, identity.TenantID, identity.KnowledgeBaseID,
					)
					if lockErr != nil && !errors.Is(lockErr, kbwritefence.ErrKnowledgeBaseUnavailable) {
						return lockErr
					}
					query := tx.Where("id = ? AND task_type = ?", snapshot.ID, TaskType)
					if tx.Dialector.Name() != "sqlite" {
						query = query.Clauses(clause.Locking{Strength: "UPDATE"})
					}
					var current types.TaskPendingOp
					if err := query.Take(&current).Error; err != nil {
						if errors.Is(err, gorm.ErrRecordNotFound) {
							return nil
						}
						return err
					}
					var object Object
					if err := json.Unmarshal(current.Payload, &object); err != nil {
						return err
					}
					if object.TenantID != identity.TenantID ||
						object.KnowledgeBaseID != identity.KnowledgeBaseID {
						return errBackfillTargetChanged
					}
					normalizedReference := referenceIdentity(object.Reference)
					if strings.TrimSpace(object.Reference) == "" || normalizedReference == object.Reference {
						return nil
					}
					// Patch only the reference field in the raw JSON object. This
					// preserves fields introduced by newer deployments while removing
					// every legacy signed/display URL from durable storage.
					var fields map[string]json.RawMessage
					if err := json.Unmarshal(current.Payload, &fields); err != nil {
						return err
					}
					encodedReference, err := json.Marshal(normalizedReference)
					if err != nil {
						return err
					}
					fields["reference"] = encodedReference
					payload, err := json.Marshal(fields)
					if err != nil {
						return err
					}
					if bytes.Equal(bytes.TrimSpace(current.Payload), payload) {
						return nil
					}
					update := tx.Model(&types.TaskPendingOp{}).Where("id = ?", current.ID)
					if tx.Dialector.Name() == "postgres" {
						update = update.Where("payload = CAST(? AS jsonb)", string(current.Payload))
					} else {
						update = update.Where("payload = ?", current.Payload)
					}
					result := update.Update("payload", payload)
					if result.Error != nil {
						return result.Error
					}
					if result.RowsAffected != 1 {
						return errBackfillTargetChanged
					}
					return nil
				})
				if err != nil {
					return err
				}
			}
			return nil
		}).Error
}

func (r *Registry) walkLegacyCandidates(
	ctx context.Context,
	visit func(*types.Knowledge, legacyCandidate) error,
) (int, error) {
	hasChunks := r.db.Migrator().HasTable(&types.Chunk{})
	skippedDisplayURLs := 0
	var batch []*types.Knowledge
	err := r.db.WithContext(ctx).Unscoped().
		// GORM's FindInBatches advances by the model primary key. Ordering by
		// tenant/KB first would combine that hidden id cursor with a different
		// sort order and silently skip interleaved UUIDs across tenants.
		Order("id ASC").
		FindInBatches(&batch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
			imageInfo := make(map[string][]legacyChunkImage)
			if hasChunks && len(batch) > 0 {
				ids := make([]string, 0, len(batch))
				for _, knowledge := range batch {
					ids = append(ids, knowledge.ID)
				}
				var images []legacyChunkImage
				if err := r.db.WithContext(ctx).Unscoped().Table("chunks").
					Select("tenant_id", "knowledge_id", "image_info", "deleted_at").
					Where("knowledge_id IN ? AND image_info <> ''", ids).
					Find(&images).Error; err != nil {
					return fmt.Errorf("read legacy chunk image paths: %w", err)
				}
				for _, image := range images {
					key := fmt.Sprintf("%d:%s", image.TenantID, image.KnowledgeID)
					imageInfo[key] = append(imageInfo[key], image)
				}
			}
			for _, knowledge := range batch {
				if hasFAQDisplayReference(knowledge) {
					skippedDisplayURLs++
				}
				key := fmt.Sprintf("%d:%s", knowledge.TenantID, knowledge.ID)
				allowDeletedChunks := knowledge.DeletedAt.Valid ||
					knowledge.ParseStatus == types.ParseStatusDeleting ||
					knowledge.ParseStatus == types.ParseStatusCancelling
				chunkImages := make([]string, 0, len(imageInfo[key]))
				for _, image := range imageInfo[key] {
					if !image.DeletedAt.Valid || allowDeletedChunks {
						chunkImages = append(chunkImages, image.ImageInfo)
					}
				}
				for _, candidate := range candidatesForKnowledge(knowledge, chunkImages) {
					if err := visit(knowledge, candidate); err != nil {
						return err
					}
				}
			}
			return nil
		}).Error
	return skippedDisplayURLs, err
}

func (r *Registry) walkAuxiliaryLedgerRows(
	ctx context.Context, visit func(*types.TaskPendingOp) error,
) error {
	var batch []*types.TaskPendingOp
	return r.db.WithContext(ctx).Where("task_type = ?", TaskType).Order("id ASC").
		FindInBatches(&batch, backfillBatchSize, func(_ *gorm.DB, _ int) error {
			for _, row := range batch {
				if err := visit(row); err != nil {
					return err
				}
			}
			return nil
		}).Error
}

func physicalObjectDigest(binding storagebinding.Binding, objectKey string) [sha256.Size]byte {
	endpointAuthority := ""
	if parsed, err := url.Parse(binding.Endpoint); err == nil {
		endpointAuthority = strings.ToLower(parsed.Host)
	}
	identity := strings.Join([]string{
		string(binding.Provider), endpointAuthority, binding.Region, binding.Bucket,
		binding.CanonicalLocalBase, binding.LocalRootIdentity, objectKey,
	}, "\x00")
	return sha256.Sum256([]byte(identity))
}

func rawPathDigest(path string) [sha256.Size]byte {
	return sha256.Sum256([]byte(strings.TrimSpace(path)))
}

func logicalOwnerDigest(object Object) [sha256.Size]byte {
	semantic := "derived"
	generation := strings.TrimSpace(object.ProcessingGeneration)
	if isPersistentSourceKind(object.Kind) {
		semantic = "persistent"
		generation = ""
	}
	identity := strings.Join([]string{
		strconv.FormatUint(object.TenantID, 10), strings.TrimSpace(object.KnowledgeBaseID),
		strings.TrimSpace(object.KnowledgeID), semantic, strings.TrimSpace(object.Kind), generation,
	}, "\x00")
	return sha256.Sum256([]byte(identity))
}

func addPhysicalOwner(
	first map[[sha256.Size]byte][sha256.Size]byte,
	shared map[[sha256.Size]byte]struct{},
	physical, owner [sha256.Size]byte,
) {
	if existing, ok := first[physical]; !ok {
		first[physical] = owner
	} else if existing != owner {
		shared[physical] = struct{}{}
	}
}

func (r *Registry) quarantineLedgerRow(ctx context.Context, rowID int64) error {
	var snapshot types.TaskPendingOp
	if err := r.db.WithContext(ctx).Where("id = ? AND task_type = ?", rowID, TaskType).Take(&snapshot).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		return err
	}
	var identity Object
	if err := json.Unmarshal(snapshot.Payload, &identity); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		_, lockErr := kbwritefence.LockExisting(tx, identity.TenantID, identity.KnowledgeBaseID)
		if lockErr != nil && !errors.Is(lockErr, kbwritefence.ErrKnowledgeBaseUnavailable) {
			return lockErr
		}
		query := tx.Where("id = ? AND task_type = ?", rowID, TaskType)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var row types.TaskPendingOp
		if err := query.Take(&row).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		object, err := decodeObject(row.Payload)
		if err != nil {
			return err
		}
		object.Quarantined = true
		object.QuarantineReason = quarantineReasonSharedPhysical
		payload, err := json.Marshal(object)
		if err != nil {
			return err
		}
		return tx.Model(&types.TaskPendingOp{}).Where("id = ?", row.ID).Update("payload", payload).Error
	})
}

// BackfillLegacyBindings signs ownership only for current, strictly
// verifiable application-owned source/image paths. Every database scan is
// batched; cross-pass state contains fixed-size hashes and bounded config
// caches, never full Knowledge rows, metadata, paths, or signed URLs.
//
// Rollout invariant: this startup pass runs after old writers are stopped and
// before new workers/HTTP are enabled. Periodic retries are safe because new
// writers register knowledge-scoped paths before commit, while every legacy
// adoption re-locks tenant+KB+owner and revalidates generation, live reference,
// current no-provision service, and physical target in one transaction.
func (r *Registry) BackfillLegacyBindings(ctx context.Context) (BackfillReport, error) {
	var report BackfillReport
	if r == nil || r.db == nil {
		return report, errors.New("knowledge auxiliary binding backfill: registry is unavailable")
	}
	if err := r.reconcileMovedPersistentOwnership(ctx); err != nil {
		return report, fmt.Errorf("reconcile moved persistent auxiliary ownership: %w", err)
	}
	if err := r.sanitizePersistedReferences(ctx); err != nil {
		return report, fmt.Errorf("sanitize auxiliary reference identities: %w", err)
	}
	physicalFirstOwner := make(map[[sha256.Size]byte][sha256.Size]byte)
	sharedPhysicalPaths := make(map[[sha256.Size]byte]struct{})
	rawFirstOwner := make(map[[sha256.Size]byte][sha256.Size]byte)
	sharedRawPaths := make(map[[sha256.Size]byte]struct{})
	kbCache := make(map[string]*types.KnowledgeBase)
	tenantCache := make(map[uint64]*types.Tenant)
	services := make(map[string]interfaces.FileService)
	loadContext := func(knowledge *types.Knowledge) (*types.Tenant, *types.KnowledgeBase, error) {
		kbKey := fmt.Sprintf("%d:%s", knowledge.TenantID, knowledge.KnowledgeBaseID)
		const maxContextCacheEntries = backfillBatchSize * 4
		if _, exists := kbCache[kbKey]; !exists && len(kbCache) >= maxContextCacheEntries {
			clear(kbCache)
			clear(tenantCache)
			clear(services)
		}
		kb := kbCache[kbKey]
		if kb == nil {
			var loaded types.KnowledgeBase
			if err := r.db.WithContext(ctx).Unscoped().Where(
				"tenant_id = ? AND id = ?", knowledge.TenantID, knowledge.KnowledgeBaseID,
			).Take(&loaded).Error; err != nil {
				return nil, nil, err
			}
			kb = &loaded
			kbCache[kbKey] = kb
		}
		tenant := tenantCache[knowledge.TenantID]
		if tenant == nil {
			var loaded types.Tenant
			if err := r.db.WithContext(ctx).Take(&loaded, "id = ?", knowledge.TenantID).Error; err != nil {
				return nil, nil, err
			}
			tenant = &loaded
			tenantCache[knowledge.TenantID] = tenant
		}
		return tenant, kb, nil
	}

	// Every ledger row first participates in exact raw-path grouping, even
	// when its historical binding is absent/corrupt. Rows with a valid binding
	// additionally participate in alias-aware physical grouping.
	if err := r.walkAuxiliaryLedgerRows(ctx, func(row *types.TaskPendingOp) error {
		var rawObject Object
		if err := json.Unmarshal(row.Payload, &rawObject); err != nil ||
			strings.TrimSpace(rawObject.Path) == "" {
			return nil
		}
		owner := logicalOwnerDigest(rawObject)
		addPhysicalOwner(rawFirstOwner, sharedRawPaths, rawPathDigest(rawObject.Path), owner)
		object, err := decodeObject(row.Payload)
		if err != nil {
			return nil
		}
		if object.Binding == nil {
			return nil
		}
		key, err := bindingObjectKey(*object.Binding, object.Path)
		if err != nil {
			return nil
		}
		addPhysicalOwner(physicalFirstOwner, sharedPhysicalPaths,
			physicalObjectDigest(*object.Binding, key), owner)
		return nil
	}); err != nil {
		return report, fmt.Errorf("scan existing auxiliary bindings: %w", err)
	}

	skipped, err := r.walkLegacyCandidates(ctx, func(knowledge *types.Knowledge, candidate legacyCandidate) error {
		report.Scanned++
		candidateObject := Object{
			TenantID: knowledge.TenantID, KnowledgeBaseID: knowledge.KnowledgeBaseID,
			KnowledgeID: knowledge.ID, ProcessingGeneration: knowledge.ProcessingGeneration,
			Path: candidate.path, Kind: candidate.kind,
		}
		addPhysicalOwner(rawFirstOwner, sharedRawPaths,
			rawPathDigest(candidate.path), logicalOwnerDigest(candidateObject))
		tenant, kb, err := loadContext(knowledge)
		if err != nil {
			report.quarantine(knowledge.ID, candidate.path, "owner-context-unavailable")
			return nil
		}
		proven, reason, err := r.proveLegacyCandidate(ctx, tenant, kb, knowledge, candidate, services)
		if err != nil {
			report.quarantine(knowledge.ID, candidate.path, reason)
			return nil
		}
		addPhysicalOwner(physicalFirstOwner, sharedPhysicalPaths,
			physicalObjectDigest(proven.binding, proven.objectKey),
			logicalOwnerDigest(proven.object))
		return nil
	})
	report.SkippedDisplayURL = skipped
	if err != nil {
		return report, fmt.Errorf("knowledge auxiliary binding backfill discovery: %w", err)
	}
	if err := r.walkAuxiliaryLedgerRows(ctx, func(row *types.TaskPendingOp) error {
		var rawObject Object
		if err := json.Unmarshal(row.Payload, &rawObject); err != nil ||
			strings.TrimSpace(rawObject.Path) == "" {
			return nil
		}
		_, shared := sharedRawPaths[rawPathDigest(rawObject.Path)]
		object, decodeErr := decodeObject(row.Payload)
		if !shared && decodeErr == nil && object.Binding != nil {
			if key, keyErr := bindingObjectKey(*object.Binding, object.Path); keyErr == nil {
				_, shared = sharedPhysicalPaths[physicalObjectDigest(*object.Binding, key)]
			}
		}
		if shared && decodeErr == nil {
			return r.quarantineLedgerRow(ctx, row.ID)
		}
		return nil
	}); err != nil {
		return report, fmt.Errorf("persist shared-object quarantine: %w", err)
	}

	_, err = r.walkLegacyCandidates(ctx, func(knowledge *types.Knowledge, candidate legacyCandidate) error {
		tenant, kb, contextErr := loadContext(knowledge)
		if contextErr != nil {
			return nil // already counted during pass one
		}
		proven, _, proofErr := r.proveLegacyCandidate(ctx, tenant, kb, knowledge, candidate, services)
		if proofErr != nil {
			return nil // already counted during pass one
		}
		physical := physicalObjectDigest(proven.binding, proven.objectKey)
		if _, shared := sharedRawPaths[rawPathDigest(candidate.path)]; shared {
			report.quarantine(knowledge.ID, candidate.path, "shared-raw-path")
			return nil
		}
		if _, shared := sharedPhysicalPaths[physical]; shared {
			report.quarantine(knowledge.ID, candidate.path, "shared-physical-object")
			return nil
		}
		if proven.existingBound {
			report.AlreadyRegistered++
			return nil
		}
		if err := r.adoptMigrationBinding(ctx, proven); err != nil {
			report.quarantine(knowledge.ID, candidate.path, "migration-adoption-rejected")
			if errors.Is(err, errBackfillTargetChanged) {
				return err
			}
			return nil
		}
		report.Adopted++
		return nil
	})
	if err != nil {
		return report, fmt.Errorf("knowledge auxiliary binding backfill adoption: %w", err)
	}
	return report, nil
}
