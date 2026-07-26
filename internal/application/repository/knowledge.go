package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgepurge"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeworkflowfilter"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/questiondedup"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrKnowledgeNotFound = errors.New("knowledge not found")
	// ErrKnowledgeDeleting is the repository-wide write fence. Once the
	// durable delete coordinator claims a row, no stale full-row or column
	// update may resurrect it by replacing parse_status=deleting.
	ErrKnowledgeDeleting = errors.New("knowledge is being deleted")
)

// escapeLikeKeyword escapes SQL LIKE wildcards (%, _) in a keyword
// so they are treated as literal characters.
func escapeLikeKeyword(keyword string) string {
	keyword = strings.ReplaceAll(keyword, `\`, `\\`)
	keyword = strings.ReplaceAll(keyword, "%", `\%`)
	keyword = strings.ReplaceAll(keyword, "_", `\_`)
	return keyword
}

// genericOmitFieldsOnUpdate defines lifecycle fields a full-row metadata save
// is never allowed to write. Lifecycle transitions have dedicated CAS helpers;
// omitting these fields prevents a slow summary/question worker from saving an
// old snapshot over a newer processing generation or terminal state.
//
// PendingSubtasksCount is deliberately omitted from every full-row Save:
// it is an orchestration counter owned exclusively by the atomic helpers
// SetFinalizing (seed), FinalizeSubtask (decrement+promote) and the
// explicit UpdateKnowledgeColumns resets (cancel/reparse). A generic
// UpdateKnowledge call persists the WHOLE in-memory struct, so any
// concurrent enrichment subtask that loaded the row, did slow work
// (e.g. an LLM call), then saved an unrelated field would otherwise
// write back the STALE counter it read at load time — clobbering the
// decrements other subtasks performed in the meantime. That made the
// counter jump back up and never reach zero (the "stuck
// pending_subtasks_count / never promoted to completed" bug). Omitting
// the column here means Save can never touch it.
var genericOmitFieldsOnUpdate = []string{
	"DeletedAt",
	"ParseStatus",
	"ProcessingGeneration",
	"ProcessingOwner",
	"ProcessingWorkflowID",
	"ProcessingFanout",
	"PendingSubtasksCount",
	"EnrichmentStatus",
	"WikiStatus",
	"WikiErrorMessage",
	"ProcessedAt",
}

// atomicFinalizeOmitFields is used only by the guarded core finalizer. That
// transaction owns lifecycle fields, but the enrichment counter remains owned
// by its own atomic helpers.
var atomicFinalizeOmitFields = []string{
	"DeletedAt",
	"PendingSubtasksCount",
	"ProcessingWorkflowID",
	"EnrichmentStatus",
	"WikiStatus",
	"WikiErrorMessage",
}

var terminalKnowledgeStatuses = []string{
	types.ParseStatusCompleted,
	types.ParseStatusFailed,
	types.ParseStatusCancelling,
	types.ParseStatusCancelled,
	types.ParseStatusDeleting,
}

// knowledgeRepository implements knowledge base and knowledge repository interface
type knowledgeRepository struct {
	db *gorm.DB
}

// NewKnowledgeRepository creates a new knowledge repository
func NewKnowledgeRepository(db *gorm.DB) interfaces.KnowledgeRepository {
	return &knowledgeRepository{db: db}
}

// CreateKnowledge creates knowledge
func (r *knowledgeRepository) CreateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	if knowledge == nil {
		return errors.New("create knowledge: knowledge is nil")
	}
	return kbwritefence.WithActive(
		ctx, r.db, knowledge.TenantID, knowledge.KnowledgeBaseID,
		func(tx *gorm.DB) error {
			if err := tx.Create(knowledge).Error; err != nil {
				return fmt.Errorf("create knowledge: %w", err)
			}
			if len(knowledge.InitialTagIDs) > 0 {
				if err := replaceKnowledgeTagsTx(tx, knowledge.ID, knowledge.InitialTagIDs); err != nil {
					return fmt.Errorf("create knowledge tags: %w", err)
				}
			}
			return nil
		},
	)
}

func (r *knowledgeRepository) CreateKnowledgeTx(
	ctx context.Context,
	tx *gorm.DB,
	knowledge *types.Knowledge,
) error {
	if tx == nil || knowledge == nil {
		return errors.New("create knowledge in transaction: dependencies are unavailable")
	}
	if err := tx.WithContext(ctx).Create(knowledge).Error; err != nil {
		return fmt.Errorf("create knowledge in transaction: %w", err)
	}
	if len(knowledge.InitialTagIDs) > 0 {
		if err := replaceKnowledgeTagsTx(tx.WithContext(ctx), knowledge.ID, knowledge.InitialTagIDs); err != nil {
			return fmt.Errorf("create knowledge tags in transaction: %w", err)
		}
	}
	return nil
}

// GetKnowledgeByID gets knowledge
func (r *knowledgeRepository) GetKnowledgeByID(
	ctx context.Context,
	tenantID uint64,
	id string,
) (*types.Knowledge, error) {
	var knowledge types.Knowledge
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND id = ?", tenantID, id).First(&knowledge).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeNotFound
		}
		return nil, err
	}
	return &knowledge, nil
}

// GetKnowledgeByIDOnly returns knowledge by ID without tenant filter (for permission resolution).
func (r *knowledgeRepository) GetKnowledgeByIDOnly(ctx context.Context, id string) (*types.Knowledge, error) {
	var knowledge types.Knowledge
	if err := r.db.WithContext(ctx).Where("id = ?", id).First(&knowledge).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrKnowledgeNotFound
		}
		return nil, err
	}
	return &knowledge, nil
}

// ListKnowledgeByKnowledgeBaseID lists all knowledge in a knowledge base
func (r *knowledgeRepository) ListKnowledgeByKnowledgeBaseID(
	ctx context.Context, tenantID uint64, kbID string,
) ([]*types.Knowledge, error) {
	var knowledges []*types.Knowledge
	if err := r.db.WithContext(ctx).Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Order("created_at DESC").Find(&knowledges).Error; err != nil {
		return nil, err
	}
	return knowledges, nil
}

// applyKnowledgeListFilter applies the optional filter dimensions of
// KnowledgeListFilter to a GORM query. Tenant / knowledge base scoping must be
// applied by the caller before invoking this helper.
func applyKnowledgeListFilter(query *gorm.DB, filter types.KnowledgeListFilter) *gorm.DB {
	if len(filter.TagIDs) > 0 {
		query = query.Where(
			"knowledges.id IN (SELECT knowledge_id FROM knowledge_tag_relations WHERE tag_id IN (?))",
			filter.TagIDs,
		)
	}
	if filter.Keyword != "" {
		escaped := strings.ToLower(escapeLikeKeyword(filter.Keyword))
		query = query.Where("(LOWER(file_name) LIKE ? OR LOWER(title) LIKE ?)", "%"+escaped+"%", "%"+escaped+"%")
	}
	// FileType and Source share the same special-case routing onto `type` for
	// the "manual" / "url" values, so callers can pick either control.
	applyTypeOrFileType := func(q *gorm.DB, val string) *gorm.DB {
		switch val {
		case "":
			return q
		case "manual", "url":
			return q.Where("type = ?", val)
		default:
			return q.Where("file_type = ?", val)
		}
	}
	query = applyTypeOrFileType(query, filter.FileType)
	if filter.Source != "" {
		switch filter.Source {
		case "manual", "url":
			query = query.Where("type = ?", filter.Source)
		default:
			query = query.Where("channel = ?", filter.Source)
		}
	}
	if filter.ParseStatus != "" {
		query = query.Where("parse_status = ?", filter.ParseStatus)
	}
	query = knowledgeworkflowfilter.Apply(query, filter.WorkflowStatus)
	if !filter.UpdatedFrom.IsZero() {
		query = query.Where("updated_at >= ?", filter.UpdatedFrom)
	}
	if !filter.UpdatedTo.IsZero() {
		query = query.Where("updated_at <= ?", filter.UpdatedTo)
	}
	return query
}

// ListPagedKnowledgeByKnowledgeBaseID lists all knowledge in a knowledge base with pagination
func (r *knowledgeRepository) ListPagedKnowledgeByKnowledgeBaseID(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	page *types.Pagination,
	filter types.KnowledgeListFilter,
) ([]*types.Knowledge, int64, error) {
	var knowledges []*types.Knowledge
	var total int64

	scope := func(q *gorm.DB) *gorm.DB {
		return applyKnowledgeListFilter(
			q.Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID),
			filter,
		)
	}

	if err := scope(r.db.WithContext(ctx).Model(&types.Knowledge{})).Count(&total).Error; err != nil {
		return nil, 0, err
	}

	if err := scope(r.db.WithContext(ctx)).
		Order("created_at DESC").
		Offset(page.Offset()).
		Limit(page.Limit()).
		Find(&knowledges).Error; err != nil {
		return nil, 0, err
	}

	return knowledges, total, nil
}

// UpdateKnowledge updates knowledge
func (r *knowledgeRepository) UpdateKnowledge(ctx context.Context, knowledge *types.Knowledge) error {
	if knowledge == nil || knowledge.ID == "" || knowledge.TenantID == 0 || knowledge.KnowledgeBaseID == "" {
		return errors.New("update knowledge: complete knowledge identity is required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND COALESCE(processing_generation, '') = ? AND parse_status <> ?",
			knowledge.TenantID,
			knowledge.ID,
			knowledge.KnowledgeBaseID,
			knowledge.ProcessingGeneration,
			types.ParseStatusDeleting,
		).
		Select("*").
		Omit(genericOmitFieldsOnUpdate...).
		Updates(knowledge)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("update knowledge %s: state or generation conflict: %w", knowledge.ID, ErrKnowledgeDeleting)
	}
	return nil
}

// FinalizeKnowledgeWithStorage atomically persists a processing result and
// charges its storage to the owning tenant. The expected identity is part of
// the UPDATE predicate so a concurrent cancel, delete, move, or reparse wins
// cleanly without leaving tenant storage charged for data the knowledge row
// never adopted.
func (r *knowledgeRepository) FinalizeKnowledgeWithStorage(
	ctx context.Context,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	storageDelta int64,
) (bool, error) {
	if knowledge == nil || knowledge.ID == "" || knowledge.TenantID == 0 || knowledge.KnowledgeBaseID == "" {
		return false, errors.New("finalize knowledge storage: complete knowledge identity is required")
	}
	if expectedParseStatus == "" {
		return false, errors.New("finalize knowledge storage: expected parse status is required")
	}
	if storageDelta < 0 {
		return false, errors.New("finalize knowledge storage: storage delta cannot be negative")
	}

	var finalized bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(file_hash, '') = ?",
				knowledge.TenantID,
				knowledge.ID,
				knowledge.KnowledgeBaseID,
				expectedParseStatus,
				knowledge.FileHash,
			).
			Select("*").
			Omit(atomicFinalizeOmitFields...).
			Updates(knowledge)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}

		if storageDelta > 0 {
			result = tx.Model(&types.Tenant{}).
				Where("id = ?", knowledge.TenantID).
				UpdateColumn("storage_used", gorm.Expr("storage_used + ?", storageDelta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("finalize knowledge storage: tenant %d not found", knowledge.TenantID)
			}
		}
		finalized = true
		return nil
	})
	return finalized, err
}

// ResetKnowledgeStorage atomically transfers ownership of an existing storage
// charge away from a knowledge row after its external resources were cleaned.
// The exact row generation and previous size are part of the predicate, so a
// concurrent delete either performs the reset itself or observes storage_size
// zero and cannot double-decrement the tenant.
func (r *knowledgeRepository) ResetKnowledgeStorage(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedStorageSize int64,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || expectedKnowledgeBaseID == "" || expectedParseStatus == "" {
		return false, errors.New("reset knowledge storage: complete expected identity is required")
	}
	if expectedStorageSize < 0 {
		return false, errors.New("reset knowledge storage: expected size cannot be negative")
	}

	var reset bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(processing_generation, '') = ? AND storage_size = ?",
				tenantID,
				knowledgeID,
				expectedKnowledgeBaseID,
				expectedParseStatus,
				expectedGeneration,
				expectedStorageSize,
			).
			UpdateColumn("storage_size", 0)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}

		if expectedStorageSize > 0 {
			result = tx.Model(&types.Tenant{}).
				Where("id = ?", tenantID).
				UpdateColumn(
					"storage_used",
					gorm.Expr(
						"CASE WHEN storage_used < ? THEN 0 ELSE storage_used - ? END",
						expectedStorageSize,
						expectedStorageSize,
					),
				)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("reset knowledge storage: tenant %d not found", tenantID)
			}
		}
		reset = true
		return nil
	})
	return reset, err
}

// UpdateKnowledgeBatch updates knowledge items in batch
func (r *knowledgeRepository) UpdateKnowledgeBatch(ctx context.Context, knowledgeList []*types.Knowledge) error {
	if len(knowledgeList) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txRepo := &knowledgeRepository{db: tx}
		for _, knowledge := range knowledgeList {
			if err := txRepo.UpdateKnowledge(ctx, knowledge); err != nil {
				return err
			}
		}
		return nil
	})
}

// DeleteKnowledge deletes knowledge
func (r *knowledgeRepository) DeleteKnowledge(ctx context.Context, tenantID uint64, id string) error {
	return r.DeleteKnowledgeList(ctx, tenantID, []string{id})
}

// DeleteKnowledge deletes knowledge
func (r *knowledgeRepository) DeleteKnowledgeList(ctx context.Context, tenantID uint64, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Knowledge uses soft delete, so the ledger FK cascade never fires.
		// Keep explicit cleanup and the soft delete atomic for every repository
		// deletion path, including whole-KB deletion which bypasses the Wiki
		// deletion coordinator.
		if err := knowledgepurge.DeleteSoftRowArtifacts(tx, tenantID, ids); err != nil {
			return err
		}
		return tx.Where("tenant_id = ? AND id IN ?", tenantID, ids).
			Delete(&types.Knowledge{}).Error
	})
}

// GetKnowledgeBatch gets knowledge in batch
func (r *knowledgeRepository) GetKnowledgeBatch(
	ctx context.Context, tenantID uint64, ids []string,
) ([]*types.Knowledge, error) {
	var knowledge []*types.Knowledge
	if err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND id IN ?", tenantID, ids).
		Find(&knowledge).Error; err != nil {
		return nil, err
	}
	return knowledge, nil
}

// CheckKnowledgeExists checks if knowledge already exists
func (r *knowledgeRepository) CheckKnowledgeExists(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	params *types.KnowledgeCheckParams,
) (bool, *types.Knowledge, error) {
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ? AND parse_status <> ?", tenantID, kbID, "failed")

	switch params.Type {
	case "file":
		// If file hash exists, prioritize exact match using hash
		if params.FileHash != "" {
			var knowledge types.Knowledge
			err := query.Where("file_hash = ?", params.FileHash).First(&knowledge).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return false, nil, nil
				}
				return false, nil, err
			}
			return true, &knowledge, nil
		}

		// If no hash or hash doesn't match, use filename and size
		if params.FileName != "" && params.FileSize > 0 {
			var knowledge types.Knowledge
			err := query.Where(
				"file_name = ? AND file_size = ?",
				params.FileName, params.FileSize,
			).First(&knowledge).Error
			if err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return false, nil, nil
				}
				return false, nil, err
			}
			return true, &knowledge, nil
		}
	case "url":
		// If file hash exists, prioritize exact match using hash
		if params.FileHash != "" {
			var knowledge types.Knowledge
			err := query.Where("type = 'url' AND file_hash = ?", params.FileHash).First(&knowledge).Error
			if err == nil && knowledge.ID != "" {
				return true, &knowledge, nil
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil, err
			}
		}

		if params.URL != "" {
			var knowledge types.Knowledge
			err := query.Where("type = 'url' AND source = ?", params.URL).First(&knowledge).Error
			if err == nil && knowledge.ID != "" {
				return true, &knowledge, nil
			}
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return false, nil, err
			}
		}
		return false, nil, nil
	}

	// No valid parameters, default to not existing
	return false, nil, nil
}

func (r *knowledgeRepository) AminusB(
	ctx context.Context,
	Atenant uint64, A string,
	Btenant uint64, B string,
) ([]string, error) {
	knowledgeIDs := []string{}
	subQuery := r.db.Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", Btenant, B).Select("file_hash")
	err := r.db.Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", Atenant, A).
		Where("file_hash NOT IN (?)", subQuery).
		Pluck("id", &knowledgeIDs).
		Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return knowledgeIDs, nil
	}
	return knowledgeIDs, err
}

func (r *knowledgeRepository) UpdateKnowledgeColumn(
	ctx context.Context,
	id string,
	column string,
	value interface{},
) error {
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND parse_status <> ?", id, types.ParseStatusDeleting)
	if column == "parse_status" {
		query = query.Where("parse_status NOT IN ? OR parse_status = ?", terminalKnowledgeStatuses, value)
	}
	result := query.Update(column, value)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("update knowledge %s column %s: %w", id, column, ErrKnowledgeDeleting)
	}
	return nil
}

// UpdateKnowledgeColumns writes multiple columns in a single UPDATE so callers
// that flip related fields together (parse_status + error_message after
// dead-letter, for example) cannot leave the row half-updated when the second
// write fails.
func (r *knowledgeRepository) UpdateKnowledgeColumns(
	ctx context.Context,
	id string,
	values map[string]interface{},
) error {
	if len(values) == 0 {
		return nil
	}
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND parse_status <> ?", id, types.ParseStatusDeleting)
	if nextStatus, ok := values["parse_status"]; ok {
		query = query.Where("parse_status NOT IN ? OR parse_status = ?", terminalKnowledgeStatuses, nextStatus)
	}
	result := query.Updates(values)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("update knowledge %s columns: %w", id, ErrKnowledgeDeleting)
	}
	return nil
}

// CompareAndSwapKnowledgeState applies a state transition only while the
// knowledge still has the tenant, knowledge-base and parse-status identity the
// caller observed. Long-running move/reparse flows use this as their write
// fence: once deletion changes parse_status to deleting, a stale worker can no
// longer save an old full-row snapshot over the durable delete intent.
func (r *knowledgeRepository) CompareAndSwapKnowledgeState(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedParseStatus == "" {
		return false, errors.New("compare-and-swap knowledge state: complete expected identity is required")
	}
	if len(values) == 0 {
		return false, errors.New("compare-and-swap knowledge state: update values are required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ?",
			tenantID,
			id,
			expectedKnowledgeBaseID,
			expectedParseStatus,
		).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompareAndSwapKnowledgeGeneration is the generation-aware variant used by
// reparse/manual-processing flows. parse_status alone is not a sufficient
// ownership token: an old worker can observe processing, a newer edit can
// cycle the row through pending back to processing, and the old worker would
// otherwise be able to commit against the newer run. file_hash is a stable
// content identity for uploaded documents and a unique processing generation
// for manual knowledge, so including it in the predicate closes that ABA race.
func (r *knowledgeRepository) CompareAndSwapKnowledgeGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedParseStatus == "" {
		return false, errors.New("compare-and-swap knowledge generation: complete expected identity is required")
	}
	if len(values) == 0 {
		return false, errors.New("compare-and-swap knowledge generation: update values are required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(file_hash, '') = ?",
			tenantID,
			id,
			expectedKnowledgeBaseID,
			expectedParseStatus,
			expectedGeneration,
		).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompareAndSwapDocumentProcessing fences a core document worker by complete
// row identity, durable processing generation and the stable task owner.
func (r *knowledgeRepository) CompareAndSwapDocumentProcessing(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" ||
		expectedParseStatus == "" || expectedGeneration == "" {
		return false, errors.New("compare-and-swap document processing: complete expected identity is required")
	}
	if len(values) == 0 {
		return false, errors.New("compare-and-swap document processing: update values are required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND COALESCE(processing_owner, '') = ?",
			tenantID,
			id,
			expectedKnowledgeBaseID,
			expectedParseStatus,
			expectedGeneration,
			expectedOwner,
		).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompareAndSwapBatchReparseSnapshot claims a deterministic batch-reparse
// generation from the exact immutable snapshot captured before the parent task
// was persisted. UpdatedAt is part of the SQL predicate, rather than a prior
// application-side check, so status/generation ABA and heartbeat races cannot
// let an old batch adopt a newer row incarnation.
func (r *knowledgeRepository) CompareAndSwapBatchReparseSnapshot(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	expectedUpdatedAt time.Time,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" ||
		expectedParseStatus == "" || expectedUpdatedAt.IsZero() {
		return false, errors.New("compare-and-swap batch reparse snapshot: complete expected identity is required")
	}
	if len(values) == 0 {
		return false, errors.New("compare-and-swap batch reparse snapshot: update values are required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND COALESCE(processing_generation, '') = ? AND COALESCE(processing_owner, '') = ? AND updated_at = ?",
			tenantID,
			id,
			expectedKnowledgeBaseID,
			expectedParseStatus,
			expectedGeneration,
			expectedOwner,
			expectedUpdatedAt,
		).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompareAndSwapKnowledgeProcessingGeneration is used after the core owner is
// consumed. It accepts only an explicit set of nonterminal lifecycle states.
func (r *knowledgeRepository) CompareAndSwapKnowledgeProcessingGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	expectedStatuses []string,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedGeneration == "" {
		return false, errors.New("compare-and-swap processing generation: complete expected identity is required")
	}
	if len(expectedStatuses) == 0 || len(values) == 0 {
		return false, errors.New("compare-and-swap processing generation: statuses and values are required")
	}
	for _, status := range expectedStatuses {
		switch status {
		case types.ParseStatusPending, types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCancelling:
		default:
			return false, fmt.Errorf("compare-and-swap processing generation: terminal status %q is not allowed", status)
		}
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ?",
			tenantID,
			id,
			expectedKnowledgeBaseID,
			expectedGeneration,
			expectedStatuses,
		).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// UpdateWikiStatusGeneration records the asynchronous Wiki lane and closes the
// final document gate when every per-document enrichment slot has already
// drained. Wiki remains a separately queued, KB-scoped pipeline, but a document
// must not become completed while its exact generation is still pending Wiki
// materialization.
//
// Status persistence and the guarded finalizing->completed promotion share one
// transaction. This matters when the Wiki worker is the last finisher: a crash
// after committing a terminal Wiki status but before promoting the document
// would otherwise leave a zero-count generation stranded forever, because a
// retry correctly treats that terminal Wiki operation as already settled.
func (r *knowledgeRepository) UpdateWikiStatusGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	status string,
	detail string,
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedGeneration == "" {
		return false, errors.New("update Wiki status generation: complete identity is required")
	}
	switch status {
	case types.WikiStatusNone,
		types.WikiStatusPending,
		types.WikiStatusCompleted,
		types.WikiStatusDegraded,
		types.WikiStatusFailed:
	default:
		return false, fmt.Errorf("update Wiki status generation: invalid status %q", status)
	}
	updated := false
	now := time.Now()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.
			Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ? AND deleted_at IS NULL",
				tenantID,
				id,
				expectedKnowledgeBaseID,
				expectedGeneration,
				[]string{
					types.ParseStatusProcessing,
					types.ParseStatusFinalizing,
					types.ParseStatusCompleted,
				},
			).
			UpdateColumns(map[string]interface{}{
				"wiki_status":        status,
				"wiki_error_message": enrichmentoutcome.NormalizeDetail(detail),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		updated = true

		if status == types.WikiStatusPending {
			return nil
		}
		promotion := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count = 0 AND wiki_status <> ? AND deleted_at IS NULL",
				tenantID,
				id,
				expectedKnowledgeBaseID,
				expectedGeneration,
				types.ParseStatusFinalizing,
				types.WikiStatusPending,
			).
			Updates(map[string]interface{}{
				"parse_status":      types.ParseStatusCompleted,
				"processed_at":      now,
				"processing_owner":  "",
				"processing_fanout": nil,
				"enrichment_status": gorm.Expr(
					"CASE WHEN enrichment_status = ? THEN ? ELSE enrichment_status END",
					types.EnrichmentStatusPending,
					types.EnrichmentStatusDegraded,
				),
				"updated_at": now,
			})
		return promotion.Error
	})
	return updated, err
}

// RecordKnowledgeFanoutCompletion inserts one generation-scoped fan-out item
// exactly once. The composite primary key makes retries and duplicate worker
// deliveries harmless across both PostgreSQL and SQLite.
func (r *knowledgeRepository) RecordKnowledgeFanoutCompletion(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	itemID string,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		processingGeneration == "" || itemID == "" {
		return false, errors.New("record knowledge fanout completion: complete identity is required")
	}
	inserted := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Serialize with a generation swap. If reparse/delete wins first, a
		// delayed old worker cannot recreate ledger rows that retention cleanup
		// has just removed.
		eligibleStatuses := []string{types.ParseStatusPending, types.ParseStatusProcessing}
		if itemID == processownership.PostProcessCompletionItem {
			// The orchestration receipt is written after the processing ->
			// finalizing transition and may race with very fast child workers
			// promoting the exact generation to completed. It is still fenced
			// by the row lock and exact generation, so reparse/delete cannot
			// resurrect a stale receipt.
			eligibleStatuses = []string{
				types.ParseStatusProcessing,
				types.ParseStatusFinalizing,
				types.ParseStatusCompleted,
			}
		}
		query := tx.Model(&types.Knowledge{}).
			Select("id").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ?",
				tenantID, knowledgeID, knowledgeBaseID, processingGeneration,
				eligibleStatuses,
			)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var current struct{ ID string }
		if err := query.Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		row := &types.KnowledgeFanoutCompletion{
			TenantID:             tenantID,
			KnowledgeID:          knowledgeID,
			KnowledgeBaseID:      knowledgeBaseID,
			ProcessingGeneration: processingGeneration,
			ItemID:               itemID,
			CompletedAt:          time.Now(),
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
		if result.Error != nil {
			return result.Error
		}
		inserted = result.RowsAffected == 1
		return nil
	})
	return inserted, err
}

// ListKnowledgeFanoutCompletions returns the durable completion set used to
// rebuild Redis fan-in state after restart/TTL expiry.
func (r *knowledgeRepository) ListKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
) ([]string, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" || processingGeneration == "" {
		return nil, errors.New("list knowledge fanout completions: complete identity is required")
	}
	var itemIDs []string
	err := r.db.WithContext(ctx).
		Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ?",
			tenantID, knowledgeID, knowledgeBaseID, processingGeneration,
		).
		Pluck("item_id", &itemIDs).Error
	return itemIDs, err
}

// CountKnowledgeFanoutCompletions returns the authoritative number of core
// fan-out completions without transferring the whole set on every item. The
// enrichment drain and orchestration receipts share this table but use
// reserved prefixes and therefore do not participate in the image/data-table
// fan-in count.
func (r *knowledgeRepository) CountKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
) (int64, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" || processingGeneration == "" {
		return 0, errors.New("count knowledge fanout completions: complete identity is required")
	}
	var count int64
	err := r.db.WithContext(ctx).
		Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id NOT LIKE ? AND item_id NOT LIKE ?",
			tenantID, knowledgeID, knowledgeBaseID, processingGeneration,
			"enrichment:%", "orchestration:%",
		).
		Count(&count).Error
	return count, err
}

func (r *knowledgeRepository) KnowledgeFanoutCompletionExists(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	itemID string,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		processingGeneration == "" || itemID == "" {
		return false, errors.New("check knowledge fanout completion: complete identity is required")
	}
	var count int64
	err := r.db.WithContext(ctx).
		Model(&types.KnowledgeFanoutCompletion{}).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND item_id = ?",
			tenantID, knowledgeID, knowledgeBaseID, processingGeneration, itemID,
		).
		Limit(1).
		Count(&count).Error
	return count > 0, err
}

// CleanupKnowledgeFanoutCompletions bounds the durable ledger. A non-empty
// keepGeneration removes every older generation for the exact tenant/KB/
// knowledge identity; an empty keepGeneration removes all rows during final
// soft deletion. Callers must treat an error as a lifecycle failure so they do
// not enqueue a new generation while stale completion facts remain.
func (r *knowledgeRepository) CleanupKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	keepGeneration string,
) error {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" {
		return errors.New("cleanup knowledge fanout completions: complete identity is required")
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		completions := tx.Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ?",
			tenantID, knowledgeID, knowledgeBaseID,
		)
		outcomes := tx.Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ?",
			tenantID, knowledgeID, knowledgeBaseID,
		)
		if keepGeneration != "" {
			completions = completions.Where("processing_generation <> ?", keepGeneration)
			outcomes = outcomes.Where("processing_generation <> ?", keepGeneration)
		}
		if err := completions.Delete(&types.KnowledgeFanoutCompletion{}).Error; err != nil {
			return err
		}
		if err := outcomes.Delete(&enrichmentoutcome.Outcome{}).Error; err != nil {
			return err
		}
		questionClaims := tx.Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ?",
			tenantID, knowledgeID, knowledgeBaseID,
		)
		if keepGeneration != "" {
			questionClaims = questionClaims.Where("processing_generation <> ?", keepGeneration)
		}
		return questionClaims.Delete(&questiondedup.Claim{}).Error
	})
}

// ClaimGeneratedQuestions atomically arbitrates semantically identical
// generated questions across concurrent batches and application replicas.
// A retry by the same stable claim ID keeps ownership; a different chunk or
// batch producing the same normalized question loses the claim.
func (r *knowledgeRepository) ClaimGeneratedQuestions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	candidates []questiondedup.Candidate,
) (map[string]string, bool, error) {
	accepted := make(map[string]string, len(candidates))
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		processingGeneration == "" {
		return accepted, false, errors.New("claim generated questions: complete identity is required")
	}
	if len(candidates) == 0 {
		return accepted, true, nil
	}
	current := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&types.Knowledge{}).
			Select("id").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND deleted_at IS NULL",
				tenantID,
				knowledgeID,
				knowledgeBaseID,
				processingGeneration,
				types.ParseStatusFinalizing,
			)
		if tx.Dialector.Name() != "sqlite" {
			// Fuzzy stem arbitration must be serialized per document
			// generation. A shared lock was sufficient for exact hash
			// conflicts, but allowed two concurrent batches to both inspect an
			// empty claim set and insert near-identical wording.
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var identity struct {
			ID string `gorm:"column:id"`
		}
		if err := query.Take(&identity).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		current = true

		for _, candidate := range candidates {
			if candidate.ClaimID == "" || candidate.QuestionHash == "" ||
				candidate.NormalizedQuestion == "" {
				return errors.New("claim generated questions: invalid candidate")
			}
		}

		var existing []questiondedup.Claim
		if err := tx.Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ?",
			tenantID,
			knowledgeID,
			knowledgeBaseID,
			processingGeneration,
		).Order("created_at ASC, question_hash ASC").Find(&existing).Error; err != nil {
			return err
		}
		normalized := make([]string, 0, len(existing)+len(candidates))
		existingByClaimID := make(map[string]string, len(existing))
		for _, row := range existing {
			normalized = append(normalized, row.NormalizedQuestion)
			existingByClaimID[row.ClaimID] = row.Question
		}

		for _, candidate := range candidates {
			// A claim ID is an immutable output slot. Retries must reuse the
			// first accepted wording even when the provider produces a
			// different sentence on the next attempt. Return only slots in
			// this call, not unrelated claims owned by earlier batches.
			if storedQuestion, retrySlot := existingByClaimID[candidate.ClaimID]; retrySlot {
				accepted[candidate.ClaimID] = storedQuestion
				continue
			}
			superficialDuplicate := false
			for _, claimed := range normalized {
				if questiondedup.IsSuperficialParaphrase(candidate.NormalizedQuestion, claimed) {
					superficialDuplicate = true
					break
				}
			}
			if superficialDuplicate {
				continue
			}
			row := &questiondedup.Claim{
				TenantID:             tenantID,
				KnowledgeID:          knowledgeID,
				KnowledgeBaseID:      knowledgeBaseID,
				ProcessingGeneration: processingGeneration,
				QuestionHash:         candidate.QuestionHash,
				ClaimID:              candidate.ClaimID,
				Question:             candidate.Question,
				NormalizedQuestion:   candidate.NormalizedQuestion,
				CreatedAt:            time.Now(),
			}
			result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(row)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected == 1 {
				accepted[candidate.ClaimID] = candidate.Question
				normalized = append(normalized, candidate.NormalizedQuestion)
			}
		}
		return nil
	})
	return accepted, current, err
}

// FailDocumentProcessingGeneration is the dead-letter-only fence. The planned
// owner is persisted before enqueue, so both pending and processing rows must
// match the exact payload owner. processed_at IS NULL makes every core-committed
// row ineligible even if a delayed dead-letter callback arrives afterwards.
func (r *knowledgeRepository) FailDocumentProcessingGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	expectedOwner string,
	values map[string]interface{},
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" ||
		expectedGeneration == "" || expectedOwner == "" || len(values) == 0 {
		return false, errors.New("fail document processing generation: complete expected identity is required")
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where("tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processed_at IS NULL",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration).
		Where("parse_status IN ? AND processing_owner = ?",
			[]string{types.ParseStatusPending, types.ParseStatusProcessing}, expectedOwner).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// CompletePostProcessDeadLetterGeneration degrades a core-committed document
// to completed when its optional post-processing orchestrator exhausts its
// retries. The processed_at predicate is the durable boundary between an
// unfinished core parse (which must still fail) and usable chunks/indexes whose
// summary/question/graph enrichment may safely be abandoned.
func (r *knowledgeRepository) CompletePostProcessDeadLetterGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
) (bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedGeneration == "" {
		return false, errors.New("complete postprocess dead-letter generation: complete identity is required")
	}
	now := time.Now()
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ? AND processed_at IS NOT NULL",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration,
			[]string{types.ParseStatusProcessing, types.ParseStatusFinalizing},
		).
		Updates(map[string]interface{}{
			"parse_status":           types.ParseStatusCompleted,
			"pending_subtasks_count": 0,
			"enrichment_status":      types.EnrichmentStatusDegraded,
			"wiki_status": gorm.Expr(
				"CASE WHEN wiki_status = ? THEN ? ELSE wiki_status END",
				types.WikiStatusPending,
				types.WikiStatusFailed,
			),
			"wiki_error_message": gorm.Expr(
				"CASE WHEN wiki_status = ? THEN ? ELSE wiki_error_message END",
				types.WikiStatusPending,
				"postprocess retry budget exhausted before Wiki handoff",
			),
			"error_message":     "",
			"processing_owner":  "",
			"processing_fanout": nil,
			"summary_status": gorm.Expr(
				"CASE WHEN summary_status IN (?, ?) THEN ? ELSE summary_status END",
				types.SummaryStatusPending,
				types.SummaryStatusProcessing,
				types.SummaryStatusFailed,
			),
			"updated_at": now,
		})
	return result.RowsAffected == 1, result.Error
}

// FinalizeKnowledgeWithStorageOwned is the owner-aware core commit. The owner
// and generation are consumed by the update atomically with tenant charging.
func (r *knowledgeRepository) FinalizeKnowledgeWithStorageOwned(
	ctx context.Context,
	knowledge *types.Knowledge,
	expectedParseStatus string,
	expectedGeneration string,
	expectedOwner string,
	storageDelta int64,
) (bool, error) {
	if knowledge == nil || knowledge.ID == "" || knowledge.TenantID == 0 || knowledge.KnowledgeBaseID == "" ||
		expectedParseStatus == "" || expectedGeneration == "" || expectedOwner == "" {
		return false, errors.New("finalize owned knowledge storage: complete expected identity is required")
	}
	if storageDelta < 0 {
		return false, errors.New("finalize owned knowledge storage: storage delta cannot be negative")
	}

	var finalized bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND parse_status = ? AND processing_generation = ? AND processing_owner = ?",
				knowledge.TenantID,
				knowledge.ID,
				knowledge.KnowledgeBaseID,
				expectedParseStatus,
				expectedGeneration,
				expectedOwner,
			).
			Select("*").
			Omit(atomicFinalizeOmitFields...).
			Updates(knowledge)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		if storageDelta > 0 {
			result = tx.Model(&types.Tenant{}).
				Where("id = ?", knowledge.TenantID).
				UpdateColumn("storage_used", gorm.Expr("storage_used + ?", storageDelta))
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return fmt.Errorf("finalize owned knowledge storage: tenant %d not found", knowledge.TenantID)
			}
		}
		finalized = true
		return nil
	})
	return finalized, err
}

// UpdateActiveDeletingKnowledgeColumns only touches rows that are still visible
// to normal queries and have not moved out of the transient deleting state.
func (r *knowledgeRepository) UpdateActiveDeletingKnowledgeColumns(
	ctx context.Context,
	id string,
	values map[string]interface{},
) (bool, error) {
	if len(values) == 0 {
		return false, nil
	}
	result := r.db.WithContext(ctx).
		Model(&types.Knowledge{}).
		Where("id = ? AND parse_status = ?", id, types.ParseStatusDeleting).
		Updates(values)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected > 0, nil
}

// FinalizeSubtask atomically decrements pending_subtasks_count and, when
// the counter reaches zero while parse_status is still 'finalizing' and Wiki
// is no longer pending, flips the row to 'completed'. Concurrent derivative
// and Wiki completions can arrive in either order; both sides attempt the same
// guarded promotion and at most one wins.
//
// Returns (newCount, promoted, error). promoted is true iff this caller
// was the one whose UPDATE flipped 'finalizing'→'completed'.
//
// The implementation is two statements (atomic decrement, then a guarded
// promote UPDATE) because GORM does not expose a portable RETURNING
// across PostgreSQL and SQLite. The promote UPDATE's WHERE clause
// (parse_status='finalizing' AND pending_subtasks_count=0) makes it
// safe to run from any number of concurrent callers — at most one wins.
func (r *knowledgeRepository) FinalizeSubtask(
	ctx context.Context, id string,
) (int, bool, error) {
	now := time.Now()
	// 1) Atomic decrement, clamped at zero. The `pending_subtasks_count > 0`
	//    guard is purely a safety net for accounting bugs — under normal
	//    operation each subtask handler decrements at most once per task,
	//    so the counter cannot go negative.
	res := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND pending_subtasks_count > 0", id).
		Updates(map[string]interface{}{
			"pending_subtasks_count": gorm.Expr("pending_subtasks_count - 1"),
			"updated_at":             now,
		})
	if res.Error != nil {
		return 0, false, res.Error
	}

	// 2) Settle the enrichment aggregate even when Wiki is still pending. This
	//    keeps the per-document derivative result truthful while the document
	//    remains finalizing on the independent Wiki gate.
	settleRes := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND parse_status = ? AND pending_subtasks_count = 0",
			id, types.ParseStatusFinalizing).
		Updates(map[string]interface{}{
			"enrichment_status": gorm.Expr(
				"CASE WHEN enrichment_status = ? THEN ? ELSE enrichment_status END",
				types.EnrichmentStatusPending,
				types.EnrichmentStatusDegraded,
			),
			"updated_at": now,
		})
	if settleRes.Error != nil {
		return 0, false, settleRes.Error
	}

	// 3) Guarded promote. EVERY caller unconditionally attempts this after
	//    decrementing — we must NOT gate it on a separate SELECT of the
	//    counter. That read can be served by a lagging read-replica (or a
	//    stale connection snapshot) and return a non-zero value even after
	//    the counter has truly reached zero on the primary; if every caller
	//    trusts that stale read, NONE of them runs the promote and the row
	//    is stranded in `finalizing` forever (the observed "stuck
	//    pending_subtasks_count" bug). The promote is a WRITE, so it executes
	//    on the primary and its `pending_subtasks_count = 0` WHERE clause is
	//    the single authoritative, atomic check on the live row: only the
	//    caller whose decrement actually brought the counter to zero matches,
	//    and cancel/delete cannot be clobbered by a late promote.
	promoteRes := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND parse_status = ? AND pending_subtasks_count = 0 AND wiki_status <> ?",
			id, types.ParseStatusFinalizing, types.WikiStatusPending).
		Updates(map[string]interface{}{
			"parse_status": types.ParseStatusCompleted,
			"processed_at": now,
			"updated_at":   now,
		})
	if promoteRes.Error != nil {
		return 0, false, promoteRes.Error
	}
	promoted := promoteRes.RowsAffected > 0

	// 4) Best-effort re-read of the new count for diagnostics/return value
	//    only. This read may be replica-stale and is intentionally NOT used
	//    to decide whether to promote (see above). A read failure here does
	//    not affect correctness, so we don't propagate it as an error.
	var snap struct {
		PendingSubtasksCount int `gorm:"column:pending_subtasks_count"`
	}
	if err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Select("pending_subtasks_count").
		Where("id = ?", id).Take(&snap).Error; err != nil {
		return 0, promoted, nil
	}
	return snap.PendingSubtasksCount, promoted, nil
}

// FinalizeSubtaskGeneration is the generation-scoped variant used by every
// document enrichment descendant. An old summary/question/graph task cannot
// decrement or promote a newer reparse generation that reused the same row.
func (r *knowledgeRepository) FinalizeSubtaskGeneration(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
) (int, bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" || expectedGeneration == "" {
		return 0, false, errors.New("finalize subtask generation: complete identity is required")
	}
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count > 0",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
		).
		Updates(map[string]interface{}{
			"pending_subtasks_count": gorm.Expr("pending_subtasks_count - 1"),
			"updated_at":             now,
		})
	if res.Error != nil {
		return 0, false, res.Error
	}

	settleRes := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count = 0",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
		).
		Updates(map[string]interface{}{
			"enrichment_status": gorm.Expr(
				"CASE WHEN enrichment_status = ? THEN ? ELSE enrichment_status END",
				types.EnrichmentStatusPending,
				types.EnrichmentStatusDegraded,
			),
			"updated_at": now,
		})
	if settleRes.Error != nil {
		return 0, false, settleRes.Error
	}

	promoteRes := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count = 0 AND wiki_status <> ?",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
			types.WikiStatusPending,
		).
		Updates(map[string]interface{}{
			"parse_status":      types.ParseStatusCompleted,
			"processed_at":      now,
			"processing_owner":  "",
			"processing_fanout": nil,
			"updated_at":        now,
		})
	if promoteRes.Error != nil {
		return 0, false, promoteRes.Error
	}
	promoted := promoteRes.RowsAffected > 0

	var snap struct {
		PendingSubtasksCount int `gorm:"column:pending_subtasks_count"`
	}
	if err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Select("pending_subtasks_count").
		Where(
			"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ?",
			tenantID, id, expectedKnowledgeBaseID, expectedGeneration,
		).Take(&snap).Error; err != nil {
		return 0, promoted, nil
	}
	return snap.PendingSubtasksCount, promoted, nil
}

// FinalizeSubtaskGenerationItem is the exactly-once descendant drain. The
// completion ledger insert and counter decrement share one transaction, so an
// Asynq retry of a successful summary/question/graph handler cannot consume a
// second slot. Duplicate calls still attempt the guarded zero-count promotion
// to repair a crash between an earlier decrement and promotion.
func (r *knowledgeRepository) FinalizeSubtaskGenerationItem(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	itemID string,
) (int, bool, error) {
	return r.finalizeSubtaskGenerationItem(
		ctx, tenantID, id, expectedKnowledgeBaseID, expectedGeneration, itemID, "", "",
	)
}

// FinalizeSubtaskGenerationItemOutcome atomically records a terminal
// enrichment outcome, consumes the item's exactly-once counter slot and, when
// the last distinct item finishes, promotes the document with an aggregate
// enrichment_status. Keeping all three writes in one transaction prevents a
// crash or retry from exposing "completed" while losing the failure fact.
func (r *knowledgeRepository) FinalizeSubtaskGenerationItemOutcome(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	itemID string,
	outcomeStatus string,
	outcomeDetail string,
) (int, bool, error) {
	if !enrichmentoutcome.ValidStatus(outcomeStatus) {
		return 0, false, fmt.Errorf("finalize subtask generation item outcome: invalid status %q", outcomeStatus)
	}
	return r.finalizeSubtaskGenerationItem(
		ctx, tenantID, id, expectedKnowledgeBaseID, expectedGeneration, itemID,
		outcomeStatus, enrichmentoutcome.NormalizeDetail(outcomeDetail),
	)
}

// RecordGenerationOutcome persists a terminal generation item without
// consuming a post-process counter slot. Core fan-out work (multimodal images
// and data-table preparation) finishes while the document is still in
// "processing", before pending_subtasks_count exists. Recording its result in
// the same generation-scoped outcome table makes that failure participate in
// the later enrichment aggregate instead of being silently converted to
// success by fan-in completion.
func (r *knowledgeRepository) RecordGenerationOutcome(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
	itemID string,
	outcomeStatus string,
	outcomeDetail string,
) (bool, error) {
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		processingGeneration == "" || itemID == "" {
		return false, errors.New("record generation outcome: complete identity is required")
	}
	if !enrichmentoutcome.ValidStatus(outcomeStatus) {
		return false, fmt.Errorf("record generation outcome: invalid status %q", outcomeStatus)
	}
	inserted := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&types.Knowledge{}).
			Select("id").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status IN ?",
				tenantID,
				knowledgeID,
				knowledgeBaseID,
				processingGeneration,
				[]string{types.ParseStatusProcessing, types.ParseStatusFinalizing},
			)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var current struct{ ID string }
		if err := query.Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		result := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(
			&enrichmentoutcome.Outcome{
				TenantID:             tenantID,
				KnowledgeID:          knowledgeID,
				KnowledgeBaseID:      knowledgeBaseID,
				ProcessingGeneration: processingGeneration,
				ItemID:               itemID,
				Status:               outcomeStatus,
				Detail:               enrichmentoutcome.NormalizeDetail(outcomeDetail),
				CompletedAt:          time.Now(),
			},
		)
		if result.Error != nil {
			return result.Error
		}
		inserted = result.RowsAffected == 1
		return nil
	})
	return inserted, err
}

// GetGenerationOutcomeAggregate returns the authoritative generation result
// used both before and after the processing -> finalizing handoff.
func (r *knowledgeRepository) GetGenerationOutcomeAggregate(
	ctx context.Context,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	processingGeneration string,
) (enrichmentoutcome.Aggregate, error) {
	var aggregate enrichmentoutcome.Aggregate
	if tenantID == 0 || knowledgeID == "" || knowledgeBaseID == "" ||
		processingGeneration == "" {
		return aggregate, errors.New("get generation outcome aggregate: complete identity is required")
	}
	err := r.db.WithContext(ctx).
		Model(&enrichmentoutcome.Outcome{}).
		Select(
			"COUNT(*) AS total, "+
				"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed, "+
				"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS degraded, "+
				"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS completed",
			enrichmentoutcome.StatusFailed,
			enrichmentoutcome.StatusDegraded,
			enrichmentoutcome.StatusCompleted,
		).
		Where(
			"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ?",
			tenantID,
			knowledgeID,
			knowledgeBaseID,
			processingGeneration,
		).
		Scan(&aggregate).Error
	return aggregate, err
}

func (r *knowledgeRepository) finalizeSubtaskGenerationItem(
	ctx context.Context,
	tenantID uint64,
	id string,
	expectedKnowledgeBaseID string,
	expectedGeneration string,
	itemID string,
	outcomeStatus string,
	outcomeDetail string,
) (int, bool, error) {
	if tenantID == 0 || id == "" || expectedKnowledgeBaseID == "" ||
		expectedGeneration == "" || itemID == "" {
		return 0, false, errors.New("finalize subtask generation item: complete identity is required")
	}
	now := time.Now()
	newCount := 0
	promoted := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// Lock the exact live generation before touching the ledger. Besides
		// fencing the counter, this prevents a delayed old task from recreating
		// obsolete rows after a reparse claim cleaned them up.
		query := tx.Model(&types.Knowledge{}).
			Select("id, enrichment_status").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ?",
				tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
			)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var current struct {
			ID               string `gorm:"column:id"`
			EnrichmentStatus string `gorm:"column:enrichment_status"`
		}
		if err := query.Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		if outcomeStatus != "" {
			outcome := &enrichmentoutcome.Outcome{
				TenantID:             tenantID,
				KnowledgeID:          id,
				KnowledgeBaseID:      expectedKnowledgeBaseID,
				ProcessingGeneration: expectedGeneration,
				ItemID:               itemID,
				Status:               outcomeStatus,
				Detail:               outcomeDetail,
				CompletedAt:          now,
			}
			if err := tx.Clauses(clause.OnConflict{
				Columns: []clause.Column{
					{Name: "tenant_id"},
					{Name: "knowledge_id"},
					{Name: "processing_generation"},
					{Name: "item_id"},
				},
				// A terminal delivery is immutable. A duplicate or delayed
				// delivery may ACK the same completion slot, but it must not
				// rewrite the first observed outcome and change the aggregate
				// status after the document was already finalized.
				DoNothing: true,
			}).Create(outcome).Error; err != nil {
				return err
			}
		}
		completion := &types.KnowledgeFanoutCompletion{
			TenantID:             tenantID,
			KnowledgeID:          id,
			KnowledgeBaseID:      expectedKnowledgeBaseID,
			ProcessingGeneration: expectedGeneration,
			ItemID:               "enrichment:" + itemID,
			CompletedAt:          now,
		}
		insert := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(completion)
		if insert.Error != nil {
			return insert.Error
		}
		if insert.RowsAffected == 1 {
			decrement := tx.Model(&types.Knowledge{}).
				Where(
					"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count > 0",
					tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
				).
				Updates(map[string]interface{}{
					"pending_subtasks_count": gorm.Expr("pending_subtasks_count - 1"),
					"updated_at":             now,
				})
			if decrement.Error != nil {
				return decrement.Error
			}
		}
		aggregateStatus := current.EnrichmentStatus
		var outcomeStats struct {
			Total    int64 `gorm:"column:total"`
			Failed   int64 `gorm:"column:failed"`
			Degraded int64 `gorm:"column:degraded"`
		}
		if err := tx.Model(&enrichmentoutcome.Outcome{}).
			Select(
				"COUNT(*) AS total, "+
					"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS failed, "+
					"COALESCE(SUM(CASE WHEN status = ? THEN 1 ELSE 0 END), 0) AS degraded",
				enrichmentoutcome.StatusFailed,
				enrichmentoutcome.StatusDegraded,
			).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND knowledge_base_id = ? AND processing_generation = ?",
				tenantID, id, expectedKnowledgeBaseID, expectedGeneration,
			).
			Scan(&outcomeStats).Error; err != nil {
			return err
		}
		switch {
		case outcomeStats.Total == 0 && current.EnrichmentStatus == types.EnrichmentStatusPending:
			// Legacy/recovery drains without an item outcome must never turn a
			// pending enrichment run into an unexplained success.
			aggregateStatus = types.EnrichmentStatusDegraded
		case outcomeStats.Total == 0:
			aggregateStatus = types.EnrichmentStatusNone
		case outcomeStats.Failed == outcomeStats.Total:
			aggregateStatus = types.EnrichmentStatusFailed
		case outcomeStats.Failed > 0 || outcomeStats.Degraded > 0:
			aggregateStatus = types.EnrichmentStatusDegraded
		default:
			aggregateStatus = types.EnrichmentStatusCompleted
		}

		settlement := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count = 0",
				tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
			).
			Updates(map[string]interface{}{
				"enrichment_status": aggregateStatus,
				"updated_at":        now,
			})
		if settlement.Error != nil {
			return settlement.Error
		}

		promotion := tx.Model(&types.Knowledge{}).
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ? AND pending_subtasks_count = 0 AND wiki_status <> ?",
				tenantID, id, expectedKnowledgeBaseID, expectedGeneration, types.ParseStatusFinalizing,
				types.WikiStatusPending,
			).
			Updates(map[string]interface{}{
				"parse_status":      types.ParseStatusCompleted,
				"processed_at":      now,
				"processing_owner":  "",
				"processing_fanout": nil,
				"updated_at":        now,
			})
		if promotion.Error != nil {
			return promotion.Error
		}
		promoted = promotion.RowsAffected == 1
		var snapshot struct {
			PendingSubtasksCount int `gorm:"column:pending_subtasks_count"`
		}
		if err := tx.Model(&types.Knowledge{}).
			Select("pending_subtasks_count").
			Where(
				"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ?",
				tenantID, id, expectedKnowledgeBaseID, expectedGeneration,
			).
			Take(&snapshot).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}
		newCount = snapshot.PendingSubtasksCount
		return nil
	})
	return newCount, promoted, err
}

// SetFinalizing atomically transitions a row from 'processing' to
// 'finalizing' and seeds pending_subtasks_count. Used by
// KnowledgePostProcess.Handle as the single durable handoff between
// the synchronous parse stage and the asynchronous enrichment fan-out.
//
// The transition is conditional on parse_status='processing' so a row
// that the user cancelled / deleted between ProcessDocument finishing
// and post-process starting will NOT get hijacked into finalizing.
// Returns whether the transition happened.
func (r *knowledgeRepository) SetFinalizing(
	ctx context.Context, id string, expectedSubtasks int,
) (bool, error) {
	if expectedSubtasks < 0 {
		expectedSubtasks = 0
	}
	now := time.Now()
	res := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("id = ? AND parse_status = ?", id, types.ParseStatusProcessing).
		Updates(map[string]interface{}{
			"parse_status":           types.ParseStatusFinalizing,
			"pending_subtasks_count": expectedSubtasks,
			"enrichment_status": func() string {
				if expectedSubtasks > 0 {
					return types.EnrichmentStatusPending
				}
				return types.EnrichmentStatusNone
			}(),
			"updated_at": now,
		})
	if res.Error != nil {
		return false, res.Error
	}
	return res.RowsAffected > 0, nil
}

// CountKnowledgeByKnowledgeBaseID counts the number of knowledge items in a knowledge base
func (r *knowledgeRepository) CountKnowledgeByKnowledgeBaseID(
	ctx context.Context,
	tenantID uint64,
	kbID string,
) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Count(&count).Error
	return count, err
}

// CountKnowledgeByStatus counts the number of knowledge items with the specified parse status
func (r *knowledgeRepository) CountKnowledgeByStatus(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	parseStatuses []string,
) (int64, error) {
	if len(parseStatuses) == 0 {
		return 0, nil
	}

	var count int64
	query := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Where("tenant_id = ? AND knowledge_base_id = ?", tenantID, kbID).
		Where("parse_status IN ?", parseStatuses)

	if err := query.Count(&count).Error; err != nil {
		return 0, err
	}

	return count, nil
}

// SearchKnowledge searches knowledge items by keyword across the tenant
// If keyword is empty, returns recent files
// Only returns documents from document-type knowledge bases (excludes FAQ)
// Returns (results, hasMore, error)
// FindByMetadataKey finds a knowledge item by a key-value pair in the metadata JSON column.
// Uses Postgres jsonb operator: metadata->>'key' = 'value'.
func (r *knowledgeRepository) FindByMetadataKey(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	key string,
	value string,
) (*types.Knowledge, error) {
	var knowledge types.Knowledge
	err := r.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_base_id = ? AND deleted_at IS NULL", tenantID, kbID).
		Where("metadata->>? = ?", key, value).
		First(&knowledge).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &knowledge, nil
}

func (r *knowledgeRepository) SearchKnowledge(
	ctx context.Context,
	tenantID uint64,
	keyword string,
	offset, limit int,
	fileTypes []string,
) ([]*types.Knowledge, bool, error) {
	// Use raw query to properly map knowledge_base_name
	type KnowledgeWithKBName struct {
		types.Knowledge
		KnowledgeBaseName string `gorm:"column:knowledge_base_name"`
	}

	var results []KnowledgeWithKBName
	query := r.db.WithContext(ctx).
		Table("knowledges").
		Select("knowledges.id, knowledges.knowledge_base_id, knowledges.type, knowledges.title, knowledges.file_name, knowledges.file_type, knowledge_bases.name as knowledge_base_name").
		Joins("JOIN knowledge_bases ON knowledge_bases.id = knowledges.knowledge_base_id").
		Where("knowledges.tenant_id = ?", tenantID).
		Where("knowledge_bases.type = ?", types.KnowledgeBaseTypeDocument).
		Where("knowledges.deleted_at IS NULL")

	// If keyword is provided, filter by file_name or title
	if keyword != "" {
		escaped := strings.ToLower(escapeLikeKeyword(keyword))
		query = query.Where("(LOWER(knowledges.file_name) LIKE ? OR LOWER(knowledges.title) LIKE ?)", "%"+escaped+"%", "%"+escaped+"%")
	}

	// If fileTypes is provided, filter by file extension or type
	if len(fileTypes) > 0 {
		seen := make(map[string]bool)
		var uniquePatterns []string
		includeURL := false
		for _, ft := range fileTypes {
			ft = strings.ToLower(strings.TrimPrefix(ft, "."))
			if ft == "url" || ft == "html" {
				includeURL = true
				continue
			}
			pattern := "%." + ft
			if !seen[pattern] {
				seen[pattern] = true
				uniquePatterns = append(uniquePatterns, pattern)
			}
			// Handle common aliases
			var aliases []string
			switch ft {
			case "xlsx":
				aliases = []string{"%.xls"}
			case "xls":
				aliases = []string{"%.xlsx"}
			case "docx":
				aliases = []string{"%.doc"}
			case "doc":
				aliases = []string{"%.docx"}
			case "jpg":
				aliases = []string{"%.jpeg", "%.png"}
			case "jpeg":
				aliases = []string{"%.jpg", "%.png"}
			case "png":
				aliases = []string{"%.jpg", "%.jpeg"}
			}
			for _, alias := range aliases {
				if !seen[alias] {
					seen[alias] = true
					uniquePatterns = append(uniquePatterns, alias)
				}
			}
		}
		var orConditions []string
		var args []interface{}
		for _, p := range uniquePatterns {
			orConditions = append(orConditions, "LOWER(knowledges.file_name) LIKE ?")
			args = append(args, p)
		}
		if includeURL {
			orConditions = append(orConditions, "knowledges.type = ?")
			args = append(args, "url")
		}
		if len(orConditions) > 0 {
			query = query.Where("("+strings.Join(orConditions, " OR ")+")", args...)
		}
	}

	// Fetch limit+1 to check if there are more results
	err := query.Order("knowledges.created_at DESC").
		Offset(offset).
		Limit(limit + 1).
		Scan(&results).Error
	if err != nil {
		return nil, false, err
	}

	// Check if there are more results
	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	// Convert to []*types.Knowledge
	knowledges := make([]*types.Knowledge, len(results))
	for i, r := range results {
		k := r.Knowledge
		k.KnowledgeBaseName = r.KnowledgeBaseName
		knowledges[i] = &k
	}
	return knowledges, hasMore, nil
}

// SearchKnowledgeInScopes searches knowledge items by keyword within the given (tenant_id, kb_id) scopes (e.g. own + shared KBs).
func (r *knowledgeRepository) SearchKnowledgeInScopes(
	ctx context.Context,
	scopes []types.KnowledgeSearchScope,
	keyword string,
	offset, limit int,
	fileTypes []string,
	includeTotal bool,
) ([]*types.Knowledge, bool, int64, error) {
	if len(scopes) == 0 {
		return nil, false, 0, nil
	}

	type KnowledgeWithKBName struct {
		types.Knowledge
		KnowledgeBaseName string `gorm:"column:knowledge_base_name"`
	}

	placeholders := make([]string, len(scopes))
	args := make([]interface{}, 0, len(scopes)*2)
	for i, s := range scopes {
		placeholders[i] = "(?,?)"
		args = append(args, s.TenantID, s.KBID)
	}
	scopeCondition := "(knowledges.tenant_id, knowledges.knowledge_base_id) IN (" + strings.Join(placeholders, ",") + ")"

	query := r.db.WithContext(ctx).
		Table("knowledges").
		Select("knowledges.id, knowledges.knowledge_base_id, knowledges.type, knowledges.title, knowledges.file_name, knowledges.file_type, knowledge_bases.name as knowledge_base_name").
		Joins("JOIN knowledge_bases ON knowledge_bases.id = knowledges.knowledge_base_id AND knowledge_bases.tenant_id = knowledges.tenant_id").
		Where(scopeCondition, args...).
		Where("knowledge_bases.type = ?", types.KnowledgeBaseTypeDocument).
		Where("knowledges.deleted_at IS NULL")

	if keyword != "" {
		escaped := strings.ToLower(escapeLikeKeyword(keyword))
		query = query.Where("(LOWER(knowledges.file_name) LIKE ? OR LOWER(knowledges.title) LIKE ?)", "%"+escaped+"%", "%"+escaped+"%")
	}

	if len(fileTypes) > 0 {
		seen := make(map[string]bool)
		var uniquePatterns []string
		includeURL := false
		for _, ft := range fileTypes {
			ft = strings.ToLower(strings.TrimPrefix(ft, "."))
			if ft == "url" || ft == "html" {
				includeURL = true
				continue
			}
			pattern := "%." + ft
			if !seen[pattern] {
				seen[pattern] = true
				uniquePatterns = append(uniquePatterns, pattern)
			}
			var aliases []string
			switch ft {
			case "xlsx":
				aliases = []string{"%.xls"}
			case "xls":
				aliases = []string{"%.xlsx"}
			case "docx":
				aliases = []string{"%.doc"}
			case "doc":
				aliases = []string{"%.docx"}
			case "jpg":
				aliases = []string{"%.jpeg", "%.png"}
			case "jpeg":
				aliases = []string{"%.jpg", "%.png"}
			case "png":
				aliases = []string{"%.jpg", "%.jpeg"}
			}
			for _, alias := range aliases {
				if !seen[alias] {
					seen[alias] = true
					uniquePatterns = append(uniquePatterns, alias)
				}
			}
		}
		var orConditions []string
		var ftArgs []interface{}
		for _, p := range uniquePatterns {
			orConditions = append(orConditions, "LOWER(knowledges.file_name) LIKE ?")
			ftArgs = append(ftArgs, p)
		}
		if includeURL {
			orConditions = append(orConditions, "knowledges.type = ?")
			ftArgs = append(ftArgs, "url")
		}
		if len(orConditions) > 0 {
			query = query.Where("("+strings.Join(orConditions, " OR ")+")", ftArgs...)
		}
	}

	var total int64
	if includeTotal {
		if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
			return nil, false, 0, err
		}
	}

	var results []KnowledgeWithKBName
	err := query.Order("knowledges.created_at DESC").
		Offset(offset).
		Limit(limit + 1).
		Scan(&results).Error
	if err != nil {
		return nil, false, 0, err
	}

	hasMore := len(results) > limit
	if hasMore {
		results = results[:limit]
	}

	knowledges := make([]*types.Knowledge, len(results))
	for i, r := range results {
		k := r.Knowledge
		k.KnowledgeBaseName = r.KnowledgeBaseName
		knowledges[i] = &k
	}
	return knowledges, hasMore, total, nil
}

// ListIDsByTagIDs returns all knowledge IDs that have any of the specified tag IDs (OR semantics)
func (r *knowledgeRepository) ListIDsByTagIDs(
	ctx context.Context,
	tenantID uint64,
	kbID string,
	tagIDs []string,
) ([]string, error) {
	if len(tagIDs) == 0 {
		return nil, nil
	}
	var ids []string
	err := r.db.WithContext(ctx).Model(&types.Knowledge{}).
		Joins("JOIN knowledge_tag_relations ktr ON knowledges.id = ktr.knowledge_id").
		Where("knowledges.tenant_id = ? AND knowledges.knowledge_base_id = ? AND ktr.tag_id IN (?)",
			tenantID, kbID, tagIDs).
		Distinct("knowledges.id").
		Pluck("knowledges.id", &ids).Error
	return ids, err
}
