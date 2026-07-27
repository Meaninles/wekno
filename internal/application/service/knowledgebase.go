package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/application/service/retriever"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbdeletequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/datasource"
	apperrors "github.com/Tencent/WeKnora/internal/errors"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	secutils "github.com/Tencent/WeKnora/internal/utils"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
)

// ErrInvalidTenantID represents an error for invalid tenant ID
var ErrInvalidTenantID = errors.New("invalid tenant ID")

// knowledgeBaseService implements the knowledge base service interface
type knowledgeBaseService struct {
	repo            interfaces.KnowledgeBaseRepository
	kgRepo          interfaces.KnowledgeRepository
	chunkRepo       interfaces.ChunkRepository
	shareRepo       interfaces.KBShareRepository
	kbShareService  interfaces.KBShareService
	modelService    interfaces.ModelService
	retrieveEngine  interfaces.RetrieveEngineRegistry
	ownership       retriever.TenantStoreOwnership
	tenantRepo      interfaces.TenantRepository
	fileSvc         interfaces.FileService
	graphEngine     interfaces.RetrieveGraphRepository
	asynqClient     interfaces.TaskEnqueuer
	taskInspector   interfaces.TaskInspector
	dsRepo          interfaces.DataSourceRepository
	syncLogRepo     interfaces.SyncLogRepository
	dsScheduler     *datasource.Scheduler
	kbDeleteQueue   *kbdeletequeue.Coordinator
	wikiDeleteCoord *wikidelete.Coordinator
	auxObjects      *knowledgeaux.Registry
}

// NewKnowledgeBaseService creates a new knowledge base service
func NewKnowledgeBaseService(repo interfaces.KnowledgeBaseRepository,
	kgRepo interfaces.KnowledgeRepository,
	chunkRepo interfaces.ChunkRepository,
	shareRepo interfaces.KBShareRepository,
	kbShareService interfaces.KBShareService,
	modelService interfaces.ModelService,
	retrieveEngine interfaces.RetrieveEngineRegistry,
	ownership retriever.TenantStoreOwnership,
	tenantRepo interfaces.TenantRepository,
	fileSvc interfaces.FileService,
	graphEngine interfaces.RetrieveGraphRepository,
	asynqClient interfaces.TaskEnqueuer,
	taskInspector interfaces.TaskInspector,
	dsRepo interfaces.DataSourceRepository,
	syncLogRepo interfaces.SyncLogRepository,
	dsScheduler *datasource.Scheduler,
	kbDeleteQueue *kbdeletequeue.Coordinator,
	wikiDeleteCoord *wikidelete.Coordinator,
	auxObjects *knowledgeaux.Registry,
) interfaces.KnowledgeBaseService {
	return &knowledgeBaseService{
		repo:            repo,
		kgRepo:          kgRepo,
		chunkRepo:       chunkRepo,
		shareRepo:       shareRepo,
		kbShareService:  kbShareService,
		modelService:    modelService,
		retrieveEngine:  retrieveEngine,
		ownership:       ownership,
		tenantRepo:      tenantRepo,
		fileSvc:         fileSvc,
		graphEngine:     graphEngine,
		asynqClient:     asynqClient,
		taskInspector:   taskInspector,
		dsRepo:          dsRepo,
		syncLogRepo:     syncLogRepo,
		dsScheduler:     dsScheduler,
		kbDeleteQueue:   kbDeleteQueue,
		wikiDeleteCoord: wikiDeleteCoord,
		auxObjects:      auxObjects,
	}
}

// GetRepository gets the knowledge base repository
// Parameters:
//   - ctx: Context with authentication and request information
//
// Returns:
//   - interfaces.KnowledgeBaseRepository: Knowledge base repository
func (s *knowledgeBaseService) GetRepository() interfaces.KnowledgeBaseRepository {
	return s.repo
}

// CreateKnowledgeBase creates a new knowledge base.
//
// When VectorStoreID is set, the binding is validated against the caller's
// tenant scope and the engine registry before persisting. A nil or
// empty-string VectorStoreID is normalized to nil ("use the tenant's
// effective engines") to match the retrieve-engine factory's pre-condition.
func (s *knowledgeBaseService) CreateKnowledgeBase(ctx context.Context,
	kb *types.KnowledgeBase,
) (*types.KnowledgeBase, error) {
	// Generate UUID and set creation timestamps
	if kb.ID == "" {
		kb.ID = uuid.New().String()
	}
	kb.CreatedAt = time.Now()
	kb.TenantID = types.MustTenantIDFromContext(ctx)
	kb.UpdatedAt = time.Now()
	// Record the creator so RBAC's RequireOwnershipOrRole can let
	// Contributors edit their own KBs without granting them tenant-wide
	// edit rights. The X-API-Key auth path attaches a synthetic
	// `system-<tenantID>` user; we deliberately skip those so the KB
	// stays tenant-owned (CreatorID == ""), which matches the original
	// API-key semantics (any human Admin can manage it) and prevents a
	// later "list KBs by creator" feature from surfacing rows nobody can
	// re-attribute.
	if uid, ok := types.UserIDFromContext(ctx); ok && !types.IsSyntheticUserID(uid) {
		kb.CreatorID = uid
	}
	kb.EnsureDefaults()
	applyTenantDefaultStorageProvider(ctx, kb)

	// Fold empty-string vector_store_id into nil so this path and the
	// retrieve-engine factory's pre-condition share a single representation.
	wasEmpty := kb.VectorStoreID != nil && *kb.VectorStoreID == ""
	kb.Normalize()
	if wasEmpty {
		logger.Debugf(ctx,
			"[kb.create] empty vector_store_id normalized to nil for tenant=%d",
			kb.TenantID)
	}

	if kb.HasVectorStore() {
		if err := s.validateVectorStoreBinding(ctx, kb.TenantID, *kb.VectorStoreID); err != nil {
			return nil, err
		}
	}

	logger.Infof(ctx, "Creating knowledge base, ID: %s, tenant ID: %d, name: %s", kb.ID, kb.TenantID, kb.Name)

	if err := s.repo.CreateKnowledgeBase(ctx, kb); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kb.ID,
			"tenant_id":         kb.TenantID,
		})
		return nil, err
	}

	logger.Infof(ctx, "Knowledge base created successfully, ID: %s, name: %s", kb.ID, kb.Name)
	return kb, nil
}

// applyTenantDefaultStorageProvider fills an empty KB storage provider from the
// tenant's global default (Settings → Storage engine). Frontend should send the
// same value; this keeps API clients and legacy UIs consistent.
func applyTenantDefaultStorageProvider(ctx context.Context, kb *types.KnowledgeBase) {
	if kb == nil || strings.TrimSpace(kb.GetStorageProvider()) != "" {
		return
	}
	tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant)
	provider := "local"
	if tenant != nil && tenant.StorageEngineConfig != nil {
		if p := strings.ToLower(strings.TrimSpace(tenant.StorageEngineConfig.DefaultProvider)); p != "" {
			provider = p
		}
	}
	kb.SetStorageProvider(provider)
}

// validateVectorStoreBinding routes through retriever.VerifyBinding so the
// ownership + registry sentinel hierarchy stays the single source of truth.
// The service layer's responsibility is to:
//
//  1. fast-reject malformed UUIDs (cheap pre-flight that also avoids a DB
//     round trip for type-confusion inputs like "' OR 1=1 --"),
//  2. translate retriever sentinels into user-facing AppErrors with
//     generic messages and the typed error codes.
//
// UUID parse failures map to the same "vector store not found" message as
// cross-tenant attempts to avoid an enumeration oracle that distinguishes
// "malformed input" from "non-existent UUID".
func (s *knowledgeBaseService) validateVectorStoreBinding(
	ctx context.Context, tenantID uint64, storeID string,
) error {
	sanitized := secutils.SanitizeForLog(storeID)

	if _, err := uuid.Parse(storeID); err != nil {
		logger.WarnWithFields(ctx, logger.Fields{
			"tenant_id": tenantID,
			"store_id":  sanitized,
			"reason":    "malformed vector_store_id",
		}, "[kb.create] vector store id is not a valid UUID")
		return apperrors.NewVectorStoreBindingInvalidError("vector store not found")
	}

	switch err := retriever.VerifyBinding(
		ctx, s.retrieveEngine, s.ownership, tenantID, storeID,
	); {
	case err == nil:
		return nil
	case errors.Is(err, retriever.ErrVectorStoreForbidden):
		logger.WarnWithFields(ctx, logger.Fields{
			"tenant_id": tenantID,
			"store_id":  sanitized,
			"reason":    "cross-tenant or unknown store",
		}, "[kb.create] vector store not owned by tenant")
		return apperrors.NewVectorStoreBindingInvalidError("vector store not found")
	case errors.Is(err, retriever.ErrVectorStoreNotFound):
		logger.WarnWithFields(ctx, logger.Fields{
			"tenant_id": tenantID,
			"store_id":  sanitized,
			"reason":    "store registered in DB but missing in registry",
		}, "[kb.create] vector store currently unavailable")
		return apperrors.NewVectorStoreUnavailableError(
			"vector store is currently unavailable; check its connection configuration")
	default:
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
			"store_id":  sanitized,
			"reason":    "binding verification failed",
		})
		return apperrors.NewInternalServerError("failed to verify vector store binding")
	}
}

// GetKnowledgeBaseByID retrieves a knowledge base by its ID
func (s *knowledgeBaseService) GetKnowledgeBaseByID(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	kb.EnsureDefaults()
	return kb, nil
}

type knowledgeBaseMoveRecoveryRepository interface {
	GetKnowledgeBaseByIDUnscoped(ctx context.Context, id string) (*types.KnowledgeBase, error)
}

// GetKnowledgeBaseByIDForMoveRecovery returns the immutable KB snapshot even
// after a tombstone. It is intentionally outside the broad service interface:
// only an exact-attempt move marker may use it to finish target-side recovery,
// never to authorize a new child write.
func (s *knowledgeBaseService) GetKnowledgeBaseByIDForMoveRecovery(
	ctx context.Context,
	id string,
	tenantID uint64,
) (*types.KnowledgeBase, error) {
	if strings.TrimSpace(id) == "" || tenantID == 0 {
		return nil, errors.New("move recovery knowledge base identity is incomplete")
	}
	repo, ok := s.repo.(knowledgeBaseMoveRecoveryRepository)
	if !ok || repo == nil {
		return nil, errors.New("move recovery unscoped knowledge base repository is unavailable")
	}
	kb, err := repo.GetKnowledgeBaseByIDUnscoped(ctx, id)
	if err != nil {
		return nil, err
	}
	if kb == nil || kb.ID != id || kb.TenantID != tenantID {
		return nil, errors.New("move recovery knowledge base tenant identity mismatch")
	}
	kb.EnsureDefaults()
	return kb, nil
}

// GetKnowledgeBaseByIDOnly retrieves knowledge base by ID without tenant filter
// Used for cross-tenant shared KB access where permission is checked elsewhere
func (s *knowledgeBaseService) GetKnowledgeBaseByIDOnly(ctx context.Context, id string) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	kb.EnsureDefaults()
	return kb, nil
}

// GetKnowledgeBasesByIDsOnly retrieves knowledge bases by IDs without tenant filter (batch).
func (s *knowledgeBaseService) GetKnowledgeBasesByIDsOnly(ctx context.Context, ids []string) ([]*types.KnowledgeBase, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	kbs, err := s.repo.GetKnowledgeBaseByIDs(ctx, ids)
	if err != nil {
		return nil, err
	}
	for _, kb := range kbs {
		if kb != nil {
			kb.EnsureDefaults()
		}
	}
	return kbs, nil
}

// ListKnowledgeBases returns all knowledge bases for a tenant
func (s *knowledgeBaseService) ListKnowledgeBases(ctx context.Context) ([]*types.KnowledgeBase, error) {
	tenantID := types.MustTenantIDFromContext(ctx)

	kbs, err := s.repo.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		for _, kb := range kbs {
			kb.EnsureDefaults()
		}

		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
		})
		return nil, err
	}

	// Query knowledge count and chunk count for each knowledge base
	for _, kb := range kbs {
		kb.EnsureDefaults()

		// Get knowledge count
		switch kb.Type {
		case types.KnowledgeBaseTypeDocument:
			knowledgeCount, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get knowledge count for knowledge base %s: %v", kb.ID, err)
			} else {
				kb.KnowledgeCount = knowledgeCount
			}
		case types.KnowledgeBaseTypeFAQ:
			// Get chunk count
			chunkCount, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID)
			if err != nil {
				logger.Warnf(ctx, "Failed to get chunk count for knowledge base %s: %v", kb.ID, err)
			} else {
				kb.ChunkCount = chunkCount
			}
		}

		// Check if there is a processing import task
		processingCount, err := s.kgRepo.CountKnowledgeByStatus(
			ctx,
			tenantID,
			kb.ID,
			[]string{"pending", "processing"},
		)
		if err != nil {
			logger.Warnf(ctx, "Failed to check processing status for knowledge base %s: %v", kb.ID, err)
		} else {
			kb.IsProcessing = processingCount > 0
			kb.ProcessingCount = processingCount
		}
	}

	// Per-user pin stamping + ordering. The "main" list view is the
	// only path that needs to honour the caller's personal pin set;
	// agent/share/IM callers go through ListKnowledgeBasesByTenantID
	// which also enriches but keys off the user in their own context.
	if userID, ok := types.UserIDFromContext(ctx); ok && userID != "" {
		s.applyUserKBPins(ctx, tenantID, userID, kbs)
	}
	return kbs, nil
}

// ListKnowledgeBasesByTenantID returns all knowledge bases for the given tenant (e.g. for shared agent context).
func (s *knowledgeBaseService) ListKnowledgeBasesByTenantID(ctx context.Context, tenantID uint64) ([]*types.KnowledgeBase, error) {
	kbs, err := s.repo.ListKnowledgeBasesByTenantID(ctx, tenantID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"tenant_id": tenantID,
		})
		return nil, err
	}
	for _, kb := range kbs {
		kb.EnsureDefaults()
		switch kb.Type {
		case types.KnowledgeBaseTypeDocument:
			if cnt, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
				kb.KnowledgeCount = cnt
			}
		case types.KnowledgeBaseTypeFAQ:
			if cnt, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
				kb.ChunkCount = cnt
			}
		}
		if processingCount, err := s.kgRepo.CountKnowledgeByStatus(ctx, tenantID, kb.ID, []string{"pending", "processing"}); err == nil {
			kb.IsProcessing = processingCount > 0
			kb.ProcessingCount = processingCount
		}
	}

	// Stamp pin state from the caller's perspective. The tenantID
	// argument may not match the caller's own tenant (this method is
	// also used to list a shared-agent's source-tenant KBs); we still
	// scope user_kb_pins by `tenantID` since a pin tied to one tenant
	// shouldn't surface when browsing another tenant's KBs.
	if userID, ok := types.UserIDFromContext(ctx); ok && userID != "" {
		s.applyUserKBPins(ctx, tenantID, userID, kbs)
	}
	return kbs, nil
}

// FillKnowledgeBaseCounts fills KnowledgeCount, ChunkCount, IsProcessing, ProcessingCount for the given KB using kb.TenantID.
func (s *knowledgeBaseService) FillKnowledgeBaseCounts(ctx context.Context, kb *types.KnowledgeBase) error {
	if kb == nil {
		return nil
	}
	tenantID := kb.TenantID
	kb.EnsureDefaults()
	switch kb.Type {
	case types.KnowledgeBaseTypeDocument:
		if cnt, err := s.kgRepo.CountKnowledgeByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
			kb.KnowledgeCount = cnt
		}
	case types.KnowledgeBaseTypeFAQ:
		if cnt, err := s.chunkRepo.CountChunksByKnowledgeBaseID(ctx, tenantID, kb.ID); err == nil {
			kb.ChunkCount = cnt
		}
	}
	if processingCount, err := s.kgRepo.CountKnowledgeByStatus(ctx, tenantID, kb.ID, []string{"pending", "processing"}); err == nil {
		kb.IsProcessing = processingCount > 0
		kb.ProcessingCount = processingCount
	}
	return nil
}

// UpdateKnowledgeBase updates a knowledge base's mutable properties.
//
// IMPORTANT — vector_store_id immutability contract:
// The vector_store_id binding is deliberately not accepted by this method.
// Two layers enforce immutability:
//
//  1. ORM layer: the GORM tag `<-:create` on KnowledgeBase.VectorStoreID
//     makes every UPDATE path (Save / Updates / Select-Updates) a no-op for
//     that column. Verified by repository/knowledgebase_sqlite_test.go.
//  2. Service layer: this method intentionally omits VectorStoreID from its
//     parameter list, and the matching handler DTO UpdateKnowledgeBaseRequest
//     omits the field as well. A reflection-based regression test
//     (handler/knowledgebase_request_test.go) fails if either DTO field
//     is added back, alerting future maintainers.
//
// Any future cross-store rebind workflow must use raw SQL through a
// dedicated repository method — the only sanctioned write path post-creation.
func (s *knowledgeBaseService) UpdateKnowledgeBase(ctx context.Context,
	id string,
	name string,
	description string,
	config *types.KnowledgeBaseConfig,
) (*types.KnowledgeBase, error) {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return nil, errors.New("knowledge base ID cannot be empty")
	}

	logger.Infof(ctx, "Updating knowledge base, ID: %s, name: %s", id, name)

	// Get existing knowledge base
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	// Update the knowledge base properties
	kb.Name = name
	kb.Description = description
	if config != nil {
		kb.ChunkingConfig = config.ChunkingConfig
		kb.ImageProcessingConfig = config.ImageProcessingConfig
		if config.FAQConfig != nil {
			kb.FAQConfig = config.FAQConfig
		}
		if config.WikiConfig != nil {
			kb.WikiConfig = config.WikiConfig
		}
		// Update indexing strategy — syncs to ExtractConfig for backward compat
		if config.IndexingStrategy != nil {
			if !config.IndexingStrategy.HasAnyIndexing() {
				return nil, errors.New("at least one indexing strategy must be enabled")
			}
			kb.IndexingStrategy = *config.IndexingStrategy
			// Ensure WikiConfig exists when wiki indexing is enabled so that
			// wiki-specific tunables (synthesis model, granularity, …) have a home.
			if kb.WikiConfig == nil && config.IndexingStrategy.WikiEnabled {
				kb.WikiConfig = &types.WikiConfig{}
			}
			// Sync GraphEnabled → ExtractConfig
			if kb.ExtractConfig != nil {
				kb.ExtractConfig.Enabled = config.IndexingStrategy.GraphEnabled
			} else if config.IndexingStrategy.GraphEnabled {
				kb.ExtractConfig = &types.ExtractConfig{Enabled: true}
			}
		}
	}
	kb.UpdatedAt = time.Now()
	kb.EnsureDefaults()

	logger.Info(ctx, "Saving knowledge base update")
	if err := s.repo.UpdateKnowledgeBase(ctx, kb); err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return nil, err
	}

	logger.Infof(ctx, "Knowledge base updated successfully, ID: %s, name: %s", kb.ID, kb.Name)
	return kb, nil
}

// TogglePinKnowledgeBase toggles whether the calling user has pinned
// this knowledge base. Pin state is per-(user, kb) as of migration
// 000050; previously this method flipped a tenant-wide column on the
// KB row which broke down under RBAC (only Admin/creator could pin,
// and the pin reordered the list for everyone in the tenant). The
// public signature is unchanged so the HTTP handler / CLI / SDK don't
// move.
//
// The KB still has to belong to the caller's tenant — the route is
// already gated behind KBAccessRead, but we re-check via
// GetKnowledgeBaseByIDAndTenant so a stale param survives a tenant
// switch cleanly.
func (s *knowledgeBaseService) TogglePinKnowledgeBase(
	ctx context.Context, id string,
) (*types.KnowledgeBase, error) {
	if id == "" {
		return nil, errors.New("knowledge base ID cannot be empty")
	}
	tenantID := types.MustTenantIDFromContext(ctx)
	userID, ok := types.UserIDFromContext(ctx)
	if !ok || userID == "" {
		// API-key callers without a user identity can't have a personal
		// pin set. We surface this rather than silently flipping a
		// shared-tenant flag like the old behaviour.
		return nil, errors.New("pin requires an authenticated user")
	}

	// Look the KB up without a tenant filter: the route's KBAccessRead
	// guard already validated that this caller can see this KB (own,
	// org-shared, or agent-shared). Filtering by the caller's tenant
	// here would 404 every legitimate pin against a shared KB whose
	// owning tenant differs from the caller's active tenant.
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
			"tenant_id":         tenantID,
		})
		return nil, err
	}

	// Read current pin state to decide direction. ListUserKBPinIDs is
	// already optimised for the "many KBs at once" path; for a single-id
	// check the round-trip is acceptable and avoids leaking a second
	// repository method just for this.
	pins, err := s.repo.ListUserKBPinIDs(ctx, tenantID, userID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
			"tenant_id":         tenantID,
			"user_id":           userID,
		})
		return nil, err
	}
	_, currentlyPinned := pins[id]

	pinnedAt, err := s.repo.SetUserKBPin(ctx, tenantID, userID, id, !currentlyPinned)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
			"tenant_id":         tenantID,
			"user_id":           userID,
			"target_pinned":     !currentlyPinned,
		})
		return nil, err
	}

	kb.EnsureDefaults()
	kb.IsPinned = !currentlyPinned
	kb.PinnedAt = pinnedAt
	logger.Infof(ctx, "Knowledge base pin toggled, ID: %s, user: %s, is_pinned: %v",
		id, userID, kb.IsPinned)
	return kb, nil
}

// applyUserKBPins stamps IsPinned / PinnedAt onto each KB in the slice
// from the caller's perspective and sorts the slice so pinned rows
// float to the top (newest pin first, ties broken by created_at desc).
// Safe to call with an empty userID (no-op stamp; default sort by
// created_at preserved).
func (s *knowledgeBaseService) applyUserKBPins(
	ctx context.Context, tenantID uint64, userID string, kbs []*types.KnowledgeBase,
) {
	if len(kbs) == 0 || userID == "" {
		return
	}
	pins, err := s.repo.ListUserKBPinIDs(ctx, tenantID, userID)
	if err != nil {
		// Pin enrichment is best-effort: a transient DB blip here
		// should not break listing KBs. Log and bail without altering
		// the slice — caller still gets a valid list, just unsorted by
		// pin.
		logger.Warnf(ctx, "applyUserKBPins: failed to load pins for tenant=%d user=%s: %v",
			tenantID, userID, err)
		return
	}
	if len(pins) == 0 {
		return
	}
	for _, kb := range kbs {
		if ts, ok := pins[kb.ID]; ok {
			kb.IsPinned = true
			t := ts
			kb.PinnedAt = &t
		}
	}
	sort.SliceStable(kbs, func(i, j int) bool {
		a, b := kbs[i], kbs[j]
		if a.IsPinned != b.IsPinned {
			return a.IsPinned
		}
		if a.IsPinned && b.IsPinned {
			at, bt := a.PinnedAt, b.PinnedAt
			if at != nil && bt != nil && !at.Equal(*bt) {
				return at.After(*bt)
			}
		}
		return a.CreatedAt.After(b.CreatedAt)
	})
}

// DeleteKnowledgeBase deletes a knowledge base by its ID
// This method marks the knowledge base as deleted and enqueues an async task
// to handle the heavy cleanup operations (embeddings, chunks, files, graph data)
func (s *knowledgeBaseService) DeleteKnowledgeBase(ctx context.Context, id string) error {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return errors.New("knowledge base ID cannot be empty")
	}

	logger.Infof(ctx, "Deleting knowledge base, ID: %s", id)

	// Get tenant identity and the complete routing snapshot before the KB is
	// hidden by GORM's soft-delete scope.
	tenantID := types.MustTenantIDFromContext(ctx)
	tenantInfo, ok := types.TenantInfoFromContext(ctx)
	if !ok || tenantInfo == nil {
		return errors.New("delete knowledge base: tenant info is unavailable")
	}

	// Load the KB before soft-delete so we can snapshot its VectorStoreID
	// into the async cleanup payload. GORM's soft-delete filter hides the
	// row from subsequent reads, so this read must happen first.
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return err
	}
	if kb == nil {
		return errors.New("delete knowledge base: repository returned nil without error")
	}
	if kb.TenantID != tenantID {
		return fmt.Errorf("delete knowledge base: KB %s belongs to tenant %d, not %d", id, kb.TenantID, tenantID)
	}
	var vectorStoreIDSnapshot *string
	if kb.VectorStoreID != nil {
		storeID := *kb.VectorStoreID
		vectorStoreIDSnapshot = &storeID
	}
	defaultProvider := ""
	if tenantInfo.StorageEngineConfig != nil {
		defaultProvider = tenantInfo.StorageEngineConfig.DefaultProvider
	}
	storageProviderSnapshot := strings.ToLower(strings.TrimSpace(kb.EffectiveStorageProvider(defaultProvider)))
	if storageProviderSnapshot == "" {
		return fmt.Errorf("delete knowledge base: KB %s has no resolvable storage provider", id)
	}

	// Persist the soft-delete and cleanup outbox atomically. Redis is only a
	// disposable wake-up: a failed enqueue cannot strand an invisible KB,
	// because the recovery loop republishes every retained PostgreSQL intent.
	payload := types.KBDeletePayload{
		TenantID:         tenantID,
		KnowledgeBaseID:  id,
		EffectiveEngines: tenantInfo.GetEffectiveEngines(),
		VectorStoreID:    vectorStoreIDSnapshot, // snapshot taken before soft-delete
		StorageProvider:  storageProviderSnapshot,
	}
	langfuse.InjectTracing(ctx, &payload)

	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal KB delete payload: %w", err)
	}

	if s.kbDeleteQueue == nil {
		return errors.New("delete knowledge base: durable outbox coordinator is unavailable")
	}
	if err := s.kbDeleteQueue.Prepare(ctx, tenantID, id, payloadBytes); err != nil {
		return err
	}
	if err := kbdeletequeue.EnqueueTrigger(s.asynqClient, payloadBytes, 0); err != nil {
		// The operation is already durably accepted. Reporting an API failure
		// here would be misleading (the KB is hidden and a retry would 404);
		// startup/periodic recovery republishes this exact retained payload.
		logger.Warnf(ctx, "KB delete trigger degraded; durable outbox will retry KB %s: %v", id, err)
	}

	logger.Infof(ctx, "Knowledge base deleted successfully, ID: %s", id)
	return nil
}

type unscopedKnowledgeBaseDeleteInspector interface {
	GetKnowledgeBaseByIDUnscoped(ctx context.Context, id string) (*types.KnowledgeBase, error)
}

func (s *knowledgeBaseService) loadCommittedKBDelete(
	ctx context.Context,
	payload types.KBDeletePayload,
) (*types.KnowledgeBase, error) {
	inspector, ok := s.repo.(unscopedKnowledgeBaseDeleteInspector)
	if !ok || inspector == nil {
		return nil, errors.New("KB delete worker: unscoped KB inspector is unavailable")
	}
	kb, err := inspector.GetKnowledgeBaseByIDUnscoped(ctx, payload.KnowledgeBaseID)
	if err != nil {
		return nil, fmt.Errorf("KB delete worker: load KB tombstone: %w", err)
	}
	if kb == nil {
		return nil, errors.New("KB delete worker: unscoped inspector returned nil without error")
	}
	if kb.TenantID != payload.TenantID {
		return nil, fmt.Errorf("KB delete worker: KB tenant %d does not match payload tenant %d", kb.TenantID, payload.TenantID)
	}
	if !kb.DeletedAt.Valid {
		return nil, fmt.Errorf("KB delete worker: KB %s is still active; waiting for soft-delete commit", kb.ID)
	}
	if strings.TrimSpace(payload.StorageProvider) == "" {
		return nil, errors.New("KB delete worker: storage provider snapshot is required")
	}
	if explicitProvider := strings.ToLower(strings.TrimSpace(kb.GetStorageProvider())); explicitProvider != "" && explicitProvider != strings.ToLower(strings.TrimSpace(payload.StorageProvider)) {
		return nil, errors.New("KB delete worker: storage provider snapshot does not match KB tombstone")
	}
	if !equalOptionalString(kb.VectorStoreID, payload.VectorStoreID) {
		return nil, errors.New("KB delete worker: vector store snapshot does not match KB tombstone")
	}
	return kb, nil
}

func equalOptionalString(left, right *string) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return strings.TrimSpace(*left) == strings.TrimSpace(*right)
}

type knowledgeBaseWikiTaskInspector interface {
	CancelWikiTasksForKnowledgeBase(
		ctx context.Context, knowledgeBaseID string,
	) (deleted int, cancelled int, err error)
	HasWikiTasksForKnowledgeBase(ctx context.Context, knowledgeBaseID string) (bool, error)
}

// quiesceKnowledgeBaseWikiTasks is the queue-side half of the KB tombstone
// fence. Page/log/folder repositories provide the database-side half, so even
// a worker missed by a queue snapshot cannot commit after deletion. Two empty
// snapshots close the cross-queue hand-off window where a running trigger
// schedules its follow-up while the first scan is in progress.
func quiesceKnowledgeBaseWikiTasks(
	ctx context.Context,
	taskInspector interfaces.TaskInspector,
	knowledgeBaseID string,
) error {
	inspector, ok := taskInspector.(knowledgeBaseWikiTaskInspector)
	if !ok || inspector == nil {
		return errors.New("KB delete worker: Wiki task inspector is unavailable")
	}
	waitCtx, cancel := context.WithTimeout(ctx, knowledgeDeleteQuiesceTimeout)
	defer cancel()
	ticker := time.NewTicker(knowledgeDeleteQuiescePoll)
	defer ticker.Stop()
	emptySnapshots := 0
	for {
		if _, _, err := inspector.CancelWikiTasksForKnowledgeBase(waitCtx, knowledgeBaseID); err != nil {
			return fmt.Errorf("KB delete worker: cancel Wiki tasks for KB %s: %w", knowledgeBaseID, err)
		}
		live, err := inspector.HasWikiTasksForKnowledgeBase(waitCtx, knowledgeBaseID)
		if err != nil {
			return fmt.Errorf("KB delete worker: inspect Wiki task quiescence for KB %s: %w", knowledgeBaseID, err)
		}
		if !live {
			emptySnapshots++
			if emptySnapshots >= 2 {
				return nil
			}
		} else {
			emptySnapshots = 0
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("KB delete worker: Wiki tasks did not quiesce for KB %s: %w", knowledgeBaseID, waitCtx.Err())
		case <-ticker.C:
		}
	}
}

func (s *knowledgeBaseService) resolveKBDeleteFileService(
	tenant *types.Tenant,
	payload types.KBDeletePayload,
) (interfaces.FileService, error) {
	if tenant == nil {
		return nil, errors.New("KB delete worker: tenant is unavailable for storage routing")
	}
	provider := strings.ToLower(strings.TrimSpace(payload.StorageProvider))
	if provider == "" {
		return nil, errors.New("KB delete worker: storage provider snapshot is empty")
	}
	fileService, resolvedProvider, err := filesvc.NewFileServiceFromStorageConfig(
		provider, tenant.StorageEngineConfig, strings.TrimSpace(os.Getenv("LOCAL_STORAGE_BASE_DIR")),
	)
	if err != nil {
		return nil, fmt.Errorf("KB delete worker: resolve snapshotted storage provider %s: %w", provider, err)
	}
	if fileService == nil || resolvedProvider != provider {
		return nil, fmt.Errorf("KB delete worker: storage provider resolved as %q, expected %q", resolvedProvider, provider)
	}
	return fileService, nil
}

// ProcessKBDelete handles async knowledge base deletion task
// This method performs heavy cleanup operations: deleting embeddings, chunks, files, and graph data
func (s *knowledgeBaseService) ProcessKBDelete(ctx context.Context, t *asynq.Task) error {
	var payload types.KBDeletePayload
	if err := json.Unmarshal(t.Payload(), &payload); err != nil {
		logger.Errorf(ctx, "Failed to unmarshal KB delete payload: %v", err)
		return err
	}
	if payload.TenantID == 0 || strings.TrimSpace(payload.KnowledgeBaseID) == "" {
		return errors.New("KB delete payload: tenant_id and knowledge_base_id are required")
	}

	tenantID := payload.TenantID
	kbID := payload.KnowledgeBaseID

	// Set tenant context for downstream services
	ctx = context.WithValue(ctx, types.TenantIDContextKey, tenantID)

	logger.Infof(ctx, "Processing KB delete task for knowledge base: %s", kbID)
	if _, err := s.loadCommittedKBDelete(ctx, payload); err != nil {
		// Prepare commits the tombstone and durable outbox atomically. Seeing an
		// active KB therefore means this trigger is stale/forged or the database
		// state is unknown, never permission to ACK irreversible cleanup.
		return err
	}
	tenant, err := s.tenantRepo.GetTenantByID(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("KB delete worker: load tenant %d: %w", tenantID, err)
	}
	if tenant == nil {
		return fmt.Errorf("KB delete worker: tenant %d repository returned nil without error", tenantID)
	}
	ctx = context.WithValue(ctx, types.TenantInfoContextKey, tenant)

	// Step 1: Get all knowledge entries in this knowledge base
	logger.Infof(ctx, "Fetching all knowledge entries in knowledge base, ID: %s", kbID)
	knowledgeList, err := s.kgRepo.ListKnowledgeByKnowledgeBaseID(ctx, tenantID, kbID)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": kbID,
		})
		return err
	}
	logger.Infof(ctx, "Found %d knowledge entries to delete", len(knowledgeList))
	if s.kbDeleteQueue == nil {
		return errors.New("KB delete worker: durable outbox coordinator is unavailable")
	}
	intentExists, err := s.kbDeleteQueue.IntentExists(ctx, tenantID, kbID, t.Payload())
	if err != nil {
		return err
	}
	if !intentExists {
		if len(knowledgeList) == 0 {
			logger.Infof(ctx, "KB delete task is an already-completed duplicate for KB %s", kbID)
			return nil
		}
		return errors.New("KB delete worker: durable outbox intent is missing")
	}

	// Step 2: claim every document for durable deletion and take the retry
	// snapshots before touching any external system.
	if len(knowledgeList) > 0 {
		if s.wikiDeleteCoord == nil {
			return errors.New("KB delete worker: deletion coordinator is unavailable")
		}
		knowledgeIDs := make([]string, 0, len(knowledgeList))
		intents := make([]wikidelete.Intent, 0, len(knowledgeList))
		for _, knowledge := range knowledgeList {
			if knowledge == nil || knowledge.ID == "" || knowledge.KnowledgeBaseID != kbID || knowledge.TenantID != tenantID {
				return errors.New("KB delete worker: knowledge snapshot contains an invalid identity")
			}
			knowledgeIDs = append(knowledgeIDs, knowledge.ID)
			retractPayload, marshalErr := json.Marshal(map[string]string{
				"op": "retract", "knowledge_id": knowledge.ID,
			})
			if marshalErr != nil {
				return fmt.Errorf("KB delete worker: marshal Wiki retract for %s: %w", knowledge.ID, marshalErr)
			}
			intents = append(intents, wikidelete.Intent{
				TenantID: tenantID, KnowledgeID: knowledge.ID, KnowledgeBaseID: kbID,
				PendingOp: &types.TaskPendingOp{
					TenantID: tenantID, TaskType: types.TypeWikiIngest,
					Scope: types.TaskScopeKnowledgeBase, ScopeID: kbID,
					Op: "retract", DedupKey: knowledge.ID, Payload: retractPayload,
				},
			})
		}
		if err := s.wikiDeleteCoord.Begin(ctx, intents); err != nil {
			return fmt.Errorf("KB delete worker: begin durable document deletion: %w", err)
		}
		// Whole-KB deletion purges every Wiki page, so running retract synthesis
		// would be wasted work and would open a late-writer race. The durable
		// retract rows remain a crash marker until the final purge below, while
		// this barrier cancels all old/current KB-scoped wake-ups.
		if err := quiesceKnowledgeBaseWikiTasks(ctx, s.taskInspector, kbID); err != nil {
			return err
		}
		if err := quiesceKnowledgeDeletionWithInspector(ctx, s.taskInspector, knowledgeList); err != nil {
			return fmt.Errorf("KB delete worker: quiesce document lifecycle: %w", err)
		}

		imageRepo, ok := s.chunkRepo.(unscopedChunkImageInfoRepository)
		if !ok || imageRepo == nil {
			return errors.New("KB delete worker: unscoped image metadata repository is unavailable")
		}
		chunkImageInfos, err := imageRepo.ListImageInfoByKnowledgeIDsUnscoped(ctx, tenantID, knowledgeIDs)
		if err != nil {
			return fmt.Errorf("KB delete worker: snapshot extracted image metadata: %w", err)
		}
		knowledgeImageInfo := make(map[string][]string, len(knowledgeIDs))
		knowledgeSet := make(map[string]struct{}, len(knowledgeIDs))
		for _, id := range knowledgeIDs {
			knowledgeSet[id] = struct{}{}
		}
		for _, imageInfo := range chunkImageInfos {
			if _, belongs := knowledgeSet[imageInfo.KnowledgeID]; !belongs {
				return fmt.Errorf("KB delete worker: image metadata belongs to unexpected knowledge %s", imageInfo.KnowledgeID)
			}
			knowledgeImageInfo[imageInfo.KnowledgeID] = append(
				knowledgeImageInfo[imageInfo.KnowledgeID], imageInfo.ImageInfo,
			)
		}
		knowledgeObjectPaths := make(map[string][]string, len(knowledgeList))
		for _, knowledge := range knowledgeList {
			imageURLs, decodeErr := collectImageURLsStrict(knowledgeImageInfo[knowledge.ID])
			if decodeErr != nil {
				return fmt.Errorf("KB delete worker: decode extracted image metadata for %s: %w", knowledge.ID, decodeErr)
			}
			metadataPaths, decodeErr := auxiliaryPathsFromKnowledge(knowledge)
			if decodeErr != nil {
				return decodeErr
			}
			paths := append(imageURLs, metadataPaths...)
			paths = append(paths, knowledge.FilePath)
			knowledgeObjectPaths[knowledge.ID] = uniqueNonEmptyStrings(paths)
		}

		// Step 3: clean external systems.  Keep chunks and knowledge rows intact
		// until every operation succeeds so a retry retains its complete source
		// of truth and can never ACK a partial cleanup.
		var externalErr error
		type groupKey struct {
			EmbeddingModelID string
			Type             string
		}
		embeddingGroups := make(map[groupKey][]string)
		for _, knowledge := range knowledgeList {
			if strings.TrimSpace(knowledge.EmbeddingModelID) == "" {
				continue
			}
			key := groupKey{EmbeddingModelID: knowledge.EmbeddingModelID, Type: knowledge.Type}
			embeddingGroups[key] = append(embeddingGroups[key], knowledge.ID)
		}
		if len(embeddingGroups) > 0 {
			logger.Infof(ctx, "Deleting embeddings from vector store")
			retrieveEngine, resolveErr := retriever.CreateRetrieveEngineFromPayload(
				ctx,
				s.retrieveEngine,
				s.ownership,
				payload.TenantID,
				payload.EffectiveEngines,
				payload.VectorStoreID,
			)
			if resolveErr != nil {
				externalErr = errors.Join(externalErr, fmt.Errorf("resolve snapshotted vector store: %w", resolveErr))
			} else {
				for key, knowledgeGroup := range embeddingGroups {
					embeddingModel, err := s.modelService.GetEmbeddingModel(ctx, key.EmbeddingModelID)
					if err != nil {
						externalErr = errors.Join(externalErr,
							fmt.Errorf("load embedding model %s: %w", key.EmbeddingModelID, err))
						continue
					}
					if err := retrieveEngine.DeleteByKnowledgeIDList(ctx, knowledgeGroup, embeddingModel.GetDimensions(), key.Type); err != nil {
						externalErr = errors.Join(externalErr,
							fmt.Errorf("delete embeddings for model %s: %w", key.EmbeddingModelID, err))
					}
				}
			}
		}

		// Delete source files and every derived-object evidence source through
		// one lifecycle. Each explicit provider:// path wins over the snapshotted
		// KB provider; only legacy paths use payload.StorageProvider.
		logger.Infof(ctx, "Deleting physical files and extracted images")
		if s.auxObjects == nil {
			externalErr = errors.Join(externalErr,
				errors.New("KB delete worker: auxiliary object registry is unavailable"))
		} else {
			for _, knowledge := range knowledgeList {
				externalErr = errors.Join(externalErr, s.auxObjects.CleanupForDelete(
					ctx, tenantID, kbID, knowledge.ID, payload.StorageProvider, knowledgeObjectPaths[knowledge.ID],
				))
			}
			// Also catches planned source/copy rows whose knowledge insert never
			// committed and therefore was absent from knowledgeList.
			externalErr = errors.Join(externalErr, s.auxObjects.CleanupKnowledgeBase(
				ctx, tenantID, kbID, payload.StorageProvider,
			))
		}

		// Delete knowledge graph data
		logger.Infof(ctx, "Deleting knowledge graph data")
		namespaces := make([]types.NameSpace, 0, len(knowledgeList))
		for _, knowledge := range knowledgeList {
			namespaces = append(namespaces, types.NameSpace{
				KnowledgeBase: knowledge.KnowledgeBaseID,
				Knowledge:     knowledge.ID,
			})
		}
		if len(namespaces) > 0 && s.graphEngine == nil {
			externalErr = errors.Join(externalErr, errors.New("delete knowledge graph: graph repository is unavailable"))
		} else if len(namespaces) > 0 {
			if err := s.graphEngine.DelGraph(ctx, namespaces); err != nil {
				externalErr = errors.Join(externalErr, fmt.Errorf("delete knowledge graph: %w", err))
			}
		}

		if s.shareRepo != nil {
			if err := s.shareRepo.DeleteByKnowledgeBaseID(ctx, kbID); err != nil {
				externalErr = errors.Join(externalErr, fmt.Errorf("delete KB shares: %w", err))
			}
		}
		externalErr = errors.Join(externalErr, s.deleteDataSourcesForKnowledgeBase(ctx, kbID))
		if externalErr != nil {
			return externalErr
		}

		logger.Infof(ctx, "Deleting all chunks in knowledge base")
		var chunkDeleteErr error
		for _, knowledgeID := range knowledgeIDs {
			if err := s.chunkRepo.DeleteChunksByKnowledgeID(ctx, tenantID, knowledgeID); err != nil {
				chunkDeleteErr = errors.Join(chunkDeleteErr,
					fmt.Errorf("delete chunks for knowledge %s: %w", knowledgeID, err))
			}
		}
		if chunkDeleteErr != nil {
			return chunkDeleteErr
		}

		// Every document-owned worker has crossed the live-task quiescence
		// barrier above, so no new terminal record can be published now. Keep
		// whole-KB deletion aligned with the single-document path by removing
		// completed/archived Asynq metadata before the durable document
		// tombstones are finalized. PostgreSQL workflow rows intentionally stay
		// behind as the authoritative audit trail.
		taskHistoryPurger, ok := s.taskInspector.(interfaces.TaskHistoryPurger)
		if !ok {
			return errors.New("KB delete worker: terminal task-history purger is unavailable")
		}
		for _, knowledgeID := range knowledgeIDs {
			if _, err := taskHistoryPurger.PurgeTaskHistoryForKnowledge(ctx, knowledgeID); err != nil {
				return fmt.Errorf(
					"KB delete worker: purge terminal task history for knowledge %s: %w",
					knowledgeID,
					err,
				)
			}
		}

		removedStorage, err := s.wikiDeleteCoord.Finalize(ctx, tenantID, knowledgeIDs)
		if err != nil {
			return fmt.Errorf("KB delete worker: atomically finalize knowledge deletion: %w", err)
		}
		tenant.StorageUsed -= removedStorage
		if tenant.StorageUsed < 0 {
			tenant.StorageUsed = 0
		}
	} else {
		// A KB can be empty. Its non-document dependants still belong to the
		// durable worker and failures must not be acknowledged. Auxiliary
		// planned writes may exist even when their knowledge insert crashed.
		var cleanupErr error
		if s.auxObjects == nil {
			cleanupErr = errors.Join(cleanupErr,
				errors.New("KB delete worker: auxiliary object registry is unavailable"))
		} else {
			cleanupErr = errors.Join(cleanupErr, s.auxObjects.CleanupKnowledgeBase(
				ctx, tenantID, kbID, payload.StorageProvider,
			))
		}
		if s.shareRepo != nil {
			cleanupErr = errors.Join(cleanupErr, s.shareRepo.DeleteByKnowledgeBaseID(ctx, kbID))
		}
		cleanupErr = errors.Join(cleanupErr, s.deleteDataSourcesForKnowledgeBase(ctx, kbID))
		if cleanupErr != nil {
			return cleanupErr
		}
	}
	// Close the final recovery/follow-up window. The first barrier cancels any
	// trigger published during slow external cleanup. Purge then removes the
	// PostgreSQL source that recovery scans; the second barrier proves a signal
	// already published just before that commit has also gone away.
	if err := quiesceKnowledgeBaseWikiTasks(ctx, s.taskInspector, kbID); err != nil {
		return err
	}
	if err := s.kbDeleteQueue.PurgeWikiState(ctx, tenantID, kbID); err != nil {
		return fmt.Errorf("KB delete worker: purge Wiki state: %w", err)
	}
	if err := quiesceKnowledgeBaseWikiTasks(ctx, s.taskInspector, kbID); err != nil {
		return err
	}
	if s.auxObjects == nil {
		return errors.New("KB delete worker: auxiliary object registry is unavailable")
	}
	if err := s.auxObjects.PurgeKnowledgeBaseDeleteProofs(ctx, tenantID, kbID); err != nil {
		return fmt.Errorf("KB delete worker: purge auxiliary delete completion proofs: %w", err)
	}
	if err := s.kbDeleteQueue.Complete(ctx, tenantID, kbID); err != nil {
		return fmt.Errorf("KB delete worker: consume durable outbox intent: %w", err)
	}

	logger.Infof(ctx, "KB delete task completed successfully, knowledge base ID: %s", kbID)
	return nil
}

func collectImageURLsStrict(imageInfos []string) ([]string, error) {
	seen := make(map[string]struct{})
	urls := make([]string, 0)
	for _, info := range imageInfos {
		if strings.TrimSpace(info) == "" {
			continue
		}
		var images []*types.ImageInfo
		if err := json.Unmarshal([]byte(info), &images); err != nil {
			return nil, err
		}
		for _, image := range images {
			if image == nil || strings.TrimSpace(image.URL) == "" {
				continue
			}
			if _, exists := seen[image.URL]; exists {
				continue
			}
			seen[image.URL] = struct{}{}
			urls = append(urls, image.URL)
		}
	}
	return urls, nil
}

// deleteDataSourcesForKnowledgeBase mirrors DataSourceService.DeleteDataSource
// for every data source attached to the KB.  Cancellation precedes the data
// source soft-delete so a failed cancellation remains discoverable on retry.
func (s *knowledgeBaseService) deleteDataSourcesForKnowledgeBase(ctx context.Context, kbID string) error {
	if s.dsRepo == nil {
		return nil
	}

	dataSources, err := s.dsRepo.FindByKnowledgeBase(ctx, kbID)
	if err != nil {
		return fmt.Errorf("list data sources for deleted KB %s: %w", kbID, err)
	}
	var cleanupErr error
	for _, ds := range dataSources {
		if ds == nil || ds.ID == "" {
			cleanupErr = errors.Join(cleanupErr, errors.New("delete KB data sources: invalid data source identity"))
			continue
		}
		if s.syncLogRepo != nil {
			if err := s.syncLogRepo.CancelPendingByDataSource(ctx, ds.ID); err != nil {
				cleanupErr = errors.Join(cleanupErr,
					fmt.Errorf("cancel pending sync logs for ds=%s kb=%s: %w", ds.ID, kbID, err))
				continue
			}
		}
		if err := s.dsRepo.Delete(ctx, ds.ID); err != nil {
			cleanupErr = errors.Join(cleanupErr,
				fmt.Errorf("delete data source %s for KB %s: %w", ds.ID, kbID, err))
			continue
		}
		if s.dsScheduler != nil {
			s.dsScheduler.Remove(ds.ID)
		}
		logger.Infof(ctx, "Data source deleted with knowledge base: ds=%s kb=%s", ds.ID, kbID)
	}
	return cleanupErr
}

// SetEmbeddingModel sets the embedding model for a knowledge base
func (s *knowledgeBaseService) SetEmbeddingModel(ctx context.Context, id string, modelID string) error {
	if id == "" {
		logger.Error(ctx, "Knowledge base ID is empty")
		return errors.New("knowledge base ID cannot be empty")
	}

	if modelID == "" {
		logger.Error(ctx, "Model ID is empty")
		return errors.New("model ID cannot be empty")
	}

	logger.Infof(ctx, "Setting embedding model for knowledge base, knowledge base ID: %s, model ID: %s", id, modelID)

	// Get the knowledge base
	kb, err := s.repo.GetKnowledgeBaseByID(ctx, id)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id": id,
		})
		return err
	}

	// Update the knowledge base's embedding model
	kb.EmbeddingModelID = modelID
	kb.UpdatedAt = time.Now()

	logger.Info(ctx, "Saving knowledge base embedding model update")
	err = s.repo.UpdateKnowledgeBase(ctx, kb)
	if err != nil {
		logger.ErrorWithFields(ctx, err, map[string]interface{}{
			"knowledge_base_id":  id,
			"embedding_model_id": modelID,
		})
		return err
	}

	logger.Infof(
		ctx,
		"Knowledge base embedding model set successfully, knowledge base ID: %s, model ID: %s",
		id,
		modelID,
	)
	return nil
}

// CopyKnowledgeBase copies a knowledge base to a new knowledge base (shallow copy).
// Source and target must belong to the tenant in context; cross-tenant access is rejected.
//
// Defensive checks:
//
//   - When dstKB != "" (clone into an existing target), the source's
//     EmbeddingModelID and VectorStoreID must match the target's. Mismatched
//     embedding models would silently mix incompatible vector spaces;
//     mismatched vector stores would require copying physical vector data
//     between stores, which is not yet supported.
//   - When dstKB == "" (create a new target), VectorStoreID is copied from
//     the source so the new KB shares the same physical vector index. GORM
//     `<-:create` allows INSERT, so the new row is well-formed.
//
// The handler's CopyKnowledgeBase endpoint runs the same checks synchronously
// before enqueueing the async clone task, so the 400 errors here are
// defense-in-depth for the worker entry point.
func (s *knowledgeBaseService) CopyKnowledgeBase(ctx context.Context,
	srcKB string, dstKB string,
) (*types.KnowledgeBase, *types.KnowledgeBase, error) {
	tenantID := types.MustTenantIDFromContext(ctx)
	// Load source KB with tenant scope to prevent cross-tenant cloning
	sourceKB, err := s.repo.GetKnowledgeBaseByIDAndTenant(ctx, srcKB, tenantID)
	if err != nil {
		logger.Errorf(ctx, "Get source knowledge base failed: %v", err)
		return nil, nil, err
	}
	sourceKB.EnsureDefaults()
	var targetKB *types.KnowledgeBase
	if dstKB != "" {
		// Load target KB with tenant scope so we only clone into the caller's tenant
		targetKB, err = s.repo.GetKnowledgeBaseByIDAndTenant(ctx, dstKB, tenantID)
		if err != nil {
			return nil, nil, err
		}

		// Defense 1: embedding model must match. Mixing incompatible
		// vector spaces would produce semantically broken search results.
		if sourceKB.EmbeddingModelID != targetKB.EmbeddingModelID {
			return nil, nil, apperrors.NewBadRequestError(
				"source and target knowledge bases use different embedding models; " +
					"clone into a target with the same embedding model")
		}

		// Defense 2: vector store binding must match. Cross-store cloning
		// would require copying physical vector data between stores.
		// (both nil → equal; both same UUID → equal; otherwise → rejected)
		if !sourceKB.SharesStoreWith(targetKB) {
			return nil, nil, apperrors.NewBadRequestError(
				"source and target knowledge bases are bound to different vector stores; " +
					"cross-store cloning is not yet supported")
		}

		// Defense 3: storage backend must match — only meaningful when the
		// tenant has a StorageEngineConfig. Without it, resolveFileService
		// ignores per-KB provider pins and routes ALL KBs to the global
		// storage service, so a clone can never span two real backends and
		// the pins must NOT be used to reject (that would be a false positive).
		// When a tenant config exists, pins are honored, so compare effective
		// providers and reject a genuine cross-backend clone up front (it would
		// otherwise fail mid-clone with ErrCrossBackendCopy).
		if tenant, _ := ctx.Value(types.TenantInfoContextKey).(*types.Tenant); tenant != nil && tenant.StorageEngineConfig != nil {
			tenantDefault := tenant.StorageEngineConfig.DefaultProvider
			srcProvider := sourceKB.EffectiveStorageProvider(tenantDefault)
			dstProvider := targetKB.EffectiveStorageProvider(tenantDefault)
			if srcProvider != "" && dstProvider != "" && srcProvider != dstProvider {
				return nil, nil, apperrors.NewBadRequestError(fmt.Sprintf(
					"source and target knowledge bases use different storage backends (%s vs %s); "+
						"cross-storage-backend cloning is not supported", srcProvider, dstProvider))
			}
		}
	} else {
		var faqConfig *types.FAQConfig
		if sourceKB.FAQConfig != nil {
			cfg := *sourceKB.FAQConfig
			faqConfig = &cfg
		}
		// Preserve VectorStoreID so the cloned KB lands on the same
		// physical index. GORM `<-:create` permits the value at INSERT.
		targetKB = &types.KnowledgeBase{
			ID:                    uuid.New().String(),
			Name:                  sourceKB.Name,
			Type:                  sourceKB.Type,
			Description:           sourceKB.Description,
			TenantID:              tenantID,
			ChunkingConfig:        sourceKB.ChunkingConfig,
			ImageProcessingConfig: sourceKB.ImageProcessingConfig,
			EmbeddingModelID:      sourceKB.EmbeddingModelID,
			SummaryModelID:        sourceKB.SummaryModelID,
			VLMConfig:             sourceKB.VLMConfig,
			StorageProviderConfig: sourceKB.StorageProviderConfig,
			StorageConfig:         sourceKB.StorageConfig,
			FAQConfig:             faqConfig,
			VectorStoreID:         sourceKB.VectorStoreID,
		}
		// The clone is owned by the caller, not the original creator —
		// otherwise a Contributor copying someone else's KB would still
		// not be able to edit the result. Skip synthetic API-key users
		// (see CreateKnowledgeBase for the same reasoning).
		if uid, ok := types.UserIDFromContext(ctx); ok && !types.IsSyntheticUserID(uid) {
			targetKB.CreatorID = uid
		}
		targetKB.EnsureDefaults()
		if err := s.repo.CreateKnowledgeBase(ctx, targetKB); err != nil {
			return nil, nil, err
		}
	}
	return sourceKB, targetKB, nil
}
