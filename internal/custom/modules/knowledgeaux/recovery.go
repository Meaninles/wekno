package knowledgeaux

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	filesvc "github.com/Tencent/WeKnora/internal/application/service/file"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultRecoveryInterval = time.Minute
	defaultRecoveryTimeout  = 30 * time.Second
	// Zero means the one-time migration is bounded by shutdown/caller
	// cancellation only. A fixed default timeout can make a large database
	// restart the full multi-pass scan forever without ever completing.
	defaultBackfillTimeout   time.Duration = 0
	defaultFAQEntriesMaxAge                = 24 * time.Hour
	defaultFAQExportMaxAge                 = 7 * 24 * time.Hour
	defaultPendingOwnerGrace               = time.Hour
)

type RecoveryConfig struct {
	ScanInterval      time.Duration
	ScanTimeout       time.Duration
	BackfillTimeout   time.Duration
	PendingOwnerGrace time.Duration
	FAQEntriesMaxAge  time.Duration
	FAQExportMaxAge   time.Duration
}

func DefaultRecoveryConfig() RecoveryConfig {
	backfillTimeout := defaultBackfillTimeout
	if configured := strings.TrimSpace(os.Getenv("KNOWLEDGE_AUX_BACKFILL_TIMEOUT")); configured != "" {
		if parsed, err := time.ParseDuration(configured); err == nil && parsed > 0 {
			backfillTimeout = parsed
		}
	}
	return RecoveryConfig{
		ScanInterval:      defaultRecoveryInterval,
		ScanTimeout:       defaultRecoveryTimeout,
		BackfillTimeout:   backfillTimeout,
		PendingOwnerGrace: defaultPendingOwnerGrace,
		FAQEntriesMaxAge:  defaultFAQEntriesMaxAge,
		FAQExportMaxAge:   defaultFAQExportMaxAge,
	}
}

func (config RecoveryConfig) normalized() RecoveryConfig {
	defaults := DefaultRecoveryConfig()
	if config.ScanInterval <= 0 {
		config.ScanInterval = defaults.ScanInterval
	}
	if config.ScanTimeout <= 0 {
		config.ScanTimeout = defaults.ScanTimeout
	}
	if config.BackfillTimeout < 0 {
		config.BackfillTimeout = defaults.BackfillTimeout
	}
	if config.PendingOwnerGrace <= 0 {
		config.PendingOwnerGrace = defaults.PendingOwnerGrace
	}
	if config.FAQEntriesMaxAge <= 0 {
		config.FAQEntriesMaxAge = defaults.FAQEntriesMaxAge
	}
	if config.FAQExportMaxAge <= 0 {
		config.FAQExportMaxAge = defaults.FAQExportMaxAge
	}
	return config
}

type Recovery struct {
	registry *Registry
	config   RecoveryConfig

	backfillMu       sync.Mutex
	backfillComplete bool
	backfillReport   BackfillReport

	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	scanMu         sync.Mutex
	recoveryCursor int64
}

func NewRecovery(registry *Registry) *Recovery {
	return NewRecoveryWithConfig(registry, DefaultRecoveryConfig())
}

func NewRecoveryWithConfig(registry *Registry, config RecoveryConfig) *Recovery {
	return &Recovery{registry: registry, config: config.normalized()}
}

func (r *Recovery) Start(parent context.Context) {
	if r == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	r.mu.Lock()
	if r.cancel != nil {
		r.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	r.cancel, r.done = cancel, make(chan struct{})
	done := r.done
	r.mu.Unlock()
	go func() {
		defer func() {
			close(done)
			r.mu.Lock()
			if r.done == done {
				r.cancel, r.done = nil, nil
			}
			r.mu.Unlock()
		}()
		r.runCycle(ctx)
		ticker := time.NewTicker(r.config.ScanInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				r.runCycle(ctx)
			}
		}
	}()
}

func (r *Recovery) Stop() {
	if r == nil {
		return
	}
	r.mu.Lock()
	cancel, done := r.cancel, r.done
	r.mu.Unlock()
	if cancel == nil || done == nil {
		return
	}
	cancel()
	<-done
}

func (r *Recovery) runCycle(parent context.Context) {
	if !r.bindingBackfillComplete() {
		if _, err := r.RunBackfill(parent); err != nil && parent.Err() == nil {
			logger.Errorf(parent, "[knowledge aux] storage binding backfill retry failed: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(parent, r.config.ScanTimeout)
	defer cancel()
	if err := r.RecoverNow(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[knowledge aux] recovery failed: %v", err)
	}
}

func (r *Recovery) bindingBackfillComplete() bool {
	if r == nil {
		return false
	}
	r.backfillMu.Lock()
	defer r.backfillMu.Unlock()
	return r.backfillComplete
}

// RunBackfill uses its own long, configurable timeout and is safe to retry.
// A timeout/error is never cached as completion; the periodic recovery loop
// attempts the idempotent batched migration again.
func (r *Recovery) RunBackfill(parent context.Context) (BackfillReport, error) {
	if r == nil || r.registry == nil || r.registry.db == nil {
		return BackfillReport{}, errors.New("knowledge auxiliary backfill dependencies are unavailable")
	}
	if parent == nil {
		parent = context.Background()
	}
	r.backfillMu.Lock()
	defer r.backfillMu.Unlock()
	if r.backfillComplete {
		return r.backfillReport, nil
	}
	ctx := parent
	cancel := func() {}
	if r.config.BackfillTimeout > 0 {
		ctx, cancel = context.WithTimeout(parent, r.config.BackfillTimeout)
	}
	defer cancel()
	report, err := r.registry.BackfillLegacyBindings(ctx)
	r.backfillReport = report
	logger.Infof(ctx,
		"[knowledge aux] storage binding backfill scanned=%d adopted=%d existing=%d quarantined=%d skipped_display_urls=%d",
		report.Scanned, report.Adopted, report.AlreadyRegistered,
		report.Quarantined, report.SkippedDisplayURL,
	)
	reasons := make([]string, 0, len(report.QuarantineReasons))
	for reason := range report.QuarantineReasons {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		logger.Warnf(ctx, "[knowledge aux] binding backfill quarantine reason=%s count=%d",
			reason, report.QuarantineReasons[reason])
	}
	for _, sample := range report.QuarantineSamples {
		logger.Warnf(ctx, "[knowledge aux] binding backfill quarantined: %s", sample)
	}
	if err == nil {
		r.backfillComplete = true
	}
	return report, err
}

type recoveryKnowledge struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	ParseStatus          string
	LastFAQImportResult  types.JSON
	DeletedAt            gorm.DeletedAt
}

func faqResultReference(raw types.JSON) string {
	if len(raw) == 0 {
		return ""
	}
	var result types.FAQImportResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return ""
	}
	return referenceIdentity(result.FailedEntriesURL)
}

func (r *Recovery) shouldDelete(now time.Time, row *types.TaskPendingOp, object Object, owner *recoveryKnowledge) bool {
	if owner == nil {
		return now.Sub(row.EnqueuedAt) >= r.config.PendingOwnerGrace
	}
	if owner.DeletedAt.Valid || owner.TenantID != object.TenantID ||
		owner.KnowledgeBaseID != object.KnowledgeBaseID {
		return true
	}
	// Source objects belong to the knowledge lifetime, not one processing
	// generation. Failed/cancelled documents must retain them for retry and a
	// reparse generation change must not delete the only source file.
	if isPersistentSourceKind(object.Kind) {
		return false
	}
	if owner.ProcessingGeneration != object.ProcessingGeneration {
		return true
	}
	age := now.Sub(row.EnqueuedAt)
	switch object.Kind {
	case KindFAQEntries:
		// FAQ containers normally remain completed while an import task owns
		// this payload, so owner status cannot identify task completion. The
		// worker deletes explicitly; recovery uses the bounded payload age.
		return age >= r.config.FAQEntriesMaxAge
	case KindFileURLTemp:
		switch owner.ParseStatus {
		case types.ParseStatusDeleting, types.ParseStatusCancelling,
			types.ParseStatusCancelled, types.ParseStatusFailed, types.ParseStatusCompleted:
			return true
		default:
			return false
		}
	case KindFAQFailedExport:
		// A committed result owns the export until it is replaced or its
		// knowledge is deleted. Dry-run/uncommitted exports are age-bounded.
		return (object.Reference == "" ||
			object.Reference != faqResultReference(owner.LastFAQImportResult)) && age >= r.config.FAQExportMaxAge
	default:
		return false
	}
}

func (r *Recovery) recoverRow(ctx context.Context, row *types.TaskPendingOp, now time.Time) error {
	if row == nil {
		return errors.New("knowledge auxiliary recovery: nil ownership row")
	}
	object, err := decodeObject(row.Payload)
	if err != nil {
		return fmt.Errorf("knowledge auxiliary recovery: decode row %d: %w", row.ID, err)
	}
	if err := validateObject(object); err != nil || object.TenantID != row.TenantID ||
		row.Scope != types.TaskScopeKnowledgeBase || object.KnowledgeBaseID != row.ScopeID ||
		objectKey(object.KnowledgeID, object.Path) != row.DedupKey {
		return fmt.Errorf("knowledge auxiliary recovery: row %d is corrupt: %w", row.ID, errors.Join(err, ErrInvalidObject))
	}
	if object.Quarantined {
		return fmt.Errorf("knowledge auxiliary recovery: row %d: %w", row.ID, ErrBindingQuarantined)
	}

	return r.registry.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		kbDeleted, kbErr := kbwritefence.LockExisting(tx, object.TenantID, object.KnowledgeBaseID)
		kbMissing := errors.Is(kbErr, kbwritefence.ErrKnowledgeBaseUnavailable)
		if kbErr != nil && !kbMissing {
			return fmt.Errorf("knowledge auxiliary recovery: lock KB: %w", kbErr)
		}

		query := tx.Unscoped().Table("knowledges").
			Select("id", "tenant_id", "knowledge_base_id", "processing_generation", "parse_status", "last_faq_import_result", "deleted_at").
			Where("tenant_id = ? AND id = ?", object.TenantID, object.KnowledgeID)
		if tx.Dialector.Name() == "sqlite" {
			if err := tx.Exec(
				"UPDATE knowledges SET id = id WHERE tenant_id = ? AND id = ?",
				object.TenantID, object.KnowledgeID,
			).Error; err != nil {
				return fmt.Errorf("knowledge auxiliary recovery: lock owner: %w", err)
			}
		} else {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var owner recoveryKnowledge
		err := query.Take(&owner).Error
		var ownerPtr *recoveryKnowledge
		switch {
		case err == nil:
			ownerPtr = &owner
		case errors.Is(err, gorm.ErrRecordNotFound):
			ownerPtr = nil
		default:
			return fmt.Errorf("knowledge auxiliary recovery: inspect owner: %w", err)
		}

		ledgerQuery := tx.Where(
			"id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			row.ID, object.TenantID, TaskType, types.TaskScopeKnowledgeBase, object.KnowledgeBaseID,
			operationOwned, row.DedupKey,
		)
		if tx.Dialector.Name() != "sqlite" {
			ledgerQuery = ledgerQuery.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		var current types.TaskPendingOp
		if err := ledgerQuery.Take(&current).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return fmt.Errorf("knowledge auxiliary recovery: lock row %d: %w", row.ID, err)
		}
		currentObject, err := decodeObject(current.Payload)
		if err != nil || !sameObject(currentObject, object) {
			return fmt.Errorf("knowledge auxiliary recovery: row %d changed identity while locked", row.ID)
		}

		if !kbMissing && !kbDeleted && !r.shouldDelete(now, &current, object, ownerPtr) {
			return nil
		}

		var tenant types.Tenant
		if err := tx.First(&tenant, "id = ?", object.TenantID).Error; err != nil {
			return fmt.Errorf("knowledge auxiliary recovery: load tenant: %w", err)
		}
		service, err := r.registry.resolveBound(ctx, &tenant, object.Path, object.Binding)
		if err != nil {
			return err
		}
		// The knowledge lock remains held across the external delete. This is
		// deliberate: a new generation cannot re-register/reuse this path while
		// recovery is deciding that the old generation owns an orphan.
		if err := service.DeleteFile(ctx, object.Path); err != nil {
			return fmt.Errorf("knowledge auxiliary recovery: delete %q: %w", object.Path, err)
		}
		result := tx.Where(
			"id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			row.ID, object.TenantID, TaskType, types.TaskScopeKnowledgeBase,
			object.KnowledgeBaseID, operationOwned, row.DedupKey,
		).Delete(&types.TaskPendingOp{})
		if result.Error != nil {
			return fmt.Errorf("knowledge auxiliary recovery: consume row %d: %w", row.ID, result.Error)
		}
		return nil
	})
}

func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil || r.registry == nil || r.registry.db == nil {
		return errors.New("knowledge auxiliary recovery dependencies are unavailable")
	}
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	cleanupErr := filesvc.CleanupStaleBoundCopyStages(time.Now(), r.config.PendingOwnerGrace)
	readPage := func(after int64) ([]*types.TaskPendingOp, error) {
		var rows []*types.TaskPendingOp
		err := r.registry.db.WithContext(ctx).Where(
			"task_type = ? AND scope = ? AND op = ? AND id > ?",
			TaskType, types.TaskScopeKnowledgeBase, operationOwned, after,
		).Order("id ASC").Limit(1000).Find(&rows).Error
		return rows, err
	}
	rows, err := readPage(r.recoveryCursor)
	if err != nil {
		return fmt.Errorf("knowledge auxiliary recovery: scan ownership: %w", err)
	}
	if len(rows) == 0 && r.recoveryCursor != 0 {
		r.recoveryCursor = 0
		rows, err = readPage(0)
		if err != nil {
			return fmt.Errorf("knowledge auxiliary recovery: wrap ownership scan: %w", err)
		}
	}
	if len(rows) > 0 {
		// Advance even when one row is corrupt/quarantined or its provider is
		// temporarily unavailable; otherwise that row can starve every higher ID.
		r.recoveryCursor = rows[len(rows)-1].ID
	}
	now := time.Now().UTC()
	var errs []error
	for _, row := range rows {
		if err := r.recoverRow(ctx, row, now); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(cleanupErr, errors.Join(errs...))
}
