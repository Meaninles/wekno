package derivativequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/types"
)

var (
	ErrStaleDispatch      = errors.New("stale derivative dispatch")
	ErrLeaseLost          = errors.New("derivative work-item lease lost")
	ErrGenerationFence    = errors.New("derivative processing generation changed")
	ErrInvalidState       = errors.New("invalid derivative work-item state transition")
	ErrDispatchWindowFull = errors.New("derivative dispatch window is full")
)

type Repository struct {
	db         *gorm.DB
	admission  *modeladmission.Manager
	fair       fairSelector
	dispatchMu sync.Mutex
}

func NewRepository(db *gorm.DB) *Repository { return &Repository{db: db} }

// NewRepositoryWithAdmission is the production DI constructor. Tests and
// migration-only callers can keep using NewRepository.
func NewRepositoryWithAdmission(
	db *gorm.DB,
	admission *modeladmission.Manager,
) *Repository {
	return &Repository{db: db, admission: admission}
}

func (r *Repository) Migrate(ctx context.Context) error {
	if r == nil || r.db == nil {
		return errors.New("derivative queue database is unavailable")
	}
	db := r.db.WithContext(ctx)
	if db.Dialector.Name() != "postgres" {
		return db.AutoMigrate(&WorkItem{}, &Result{}, &ProviderCall{})
	}
	if err := db.AutoMigrate(&WorkItem{}, &Result{}); err != nil {
		return err
	}
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_derivative_pool_active_window
		ON custom_derivative_work_items (resource_pool_id, state, dispatch_lease_until)
		WHERE work_kind <> 'finalizer'`).Error; err != nil {
		return fmt.Errorf("migrate derivative dispatch window index: %w", err)
	}
	if !db.Migrator().HasTable(&ProviderCall{}) {
		if err := db.AutoMigrate(&ProviderCall{}); err != nil {
			return err
		}
	}
	return migratePostgresProviderCalls(db)
}

// migratePostgresProviderCalls is the executable migration boundary for the
// provider-response contract. Files under migrations/custom document the SQL
// for operators, but the custom bootstrap does not scan those files; it calls
// Repository.Migrate. Keep the real, idempotent upgrade here so an old
// (work_item_id, request_hash) uniqueness constraint cannot silently prevent
// durable attempt N+1 after a contract-invalid response.
func migratePostgresProviderCalls(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		statements := []string{
			`SELECT pg_advisory_xact_lock(hashtext('weknora.derivative_provider_calls.v2'))`,
			`ALTER TABLE custom_derivative_provider_calls
				ADD COLUMN IF NOT EXISTS attempt integer NOT NULL DEFAULT 1,
				ADD COLUMN IF NOT EXISTS provider_request_id varchar(160) NOT NULL DEFAULT '',
				ADD COLUMN IF NOT EXISTS processing_generation varchar(64) NOT NULL DEFAULT '',
				ADD COLUMN IF NOT EXISTS disposition varchar(32) NOT NULL DEFAULT 'checkpointed',
				ADD COLUMN IF NOT EXISTS validation_error text NOT NULL DEFAULT '',
				ADD COLUMN IF NOT EXISTS validated_at timestamptz`,
			`UPDATE custom_derivative_provider_calls AS calls
			 SET processing_generation = items.processing_generation
			 FROM custom_derivative_work_items AS items
			 WHERE calls.work_item_id = items.id
			   AND calls.processing_generation = ''`,
			// Early builds used GORM's shorter index name for a two-column
			// uniqueness rule. PostgreSQL permits that index to coexist with the
			// newer three-column constraint, so merely upgrading the constraint
			// leaves attempt N+1 blocked. Remove both possible legacy object forms
			// before establishing the canonical constraint below.
			`ALTER TABLE custom_derivative_provider_calls
				DROP CONSTRAINT IF EXISTS uq_derivative_provider_call`,
			`DROP INDEX IF EXISTS uq_derivative_provider_call`,
			`DO $$
			 DECLARE current_definition text;
			 BEGIN
			   SELECT pg_get_constraintdef(oid)
			   INTO current_definition
			   FROM pg_constraint
			   WHERE conrelid = 'custom_derivative_provider_calls'::regclass
			     AND conname = 'uq_custom_derivative_provider_call';
			   IF current_definition IS DISTINCT FROM
			      'UNIQUE (work_item_id, request_hash, attempt)' THEN
			     ALTER TABLE custom_derivative_provider_calls
			       DROP CONSTRAINT IF EXISTS uq_custom_derivative_provider_call;
			     DROP INDEX IF EXISTS uq_custom_derivative_provider_call;
			     ALTER TABLE custom_derivative_provider_calls
			       ADD CONSTRAINT uq_custom_derivative_provider_call
			       UNIQUE (work_item_id, request_hash, attempt);
			   END IF;
			 END $$`,
			`DO $$
			 BEGIN
			   IF NOT EXISTS (
			     SELECT 1 FROM pg_constraint
			     WHERE conrelid = 'custom_derivative_provider_calls'::regclass
			       AND conname = 'chk_custom_derivative_provider_call_disposition'
			   ) THEN
			     ALTER TABLE custom_derivative_provider_calls
			       ADD CONSTRAINT chk_custom_derivative_provider_call_disposition
			       CHECK (disposition IN ('checkpointed', 'accepted', 'invalid_contract'));
			   END IF;
			 END $$`,
			`CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_calls_replay
				ON custom_derivative_provider_calls
				(work_item_id, request_hash, disposition, attempt DESC)`,
			`CREATE INDEX IF NOT EXISTS idx_custom_derivative_provider_calls_generation
				ON custom_derivative_provider_calls
				(processing_generation, disposition, created_at)`,
		}
		for _, statement := range statements {
			if err := tx.Exec(statement).Error; err != nil {
				return fmt.Errorf("migrate derivative provider calls: %w", err)
			}
		}
		return nil
	})
}

func (r *Repository) UpsertPlan(
	ctx context.Context,
	items []PlanItem,
) ([]WorkItem, error) {
	if r == nil || r.db == nil {
		return nil, errors.New("derivative queue database is unavailable")
	}
	now := time.Now().UTC()
	rows := make([]WorkItem, 0, len(items))
	for _, item := range items {
		if err := validatePlanItem(item); err != nil {
			return nil, err
		}
		payload := append(types.JSON(nil), item.Payload...)
		if len(payload) == 0 {
			payload = types.JSON(`{}`)
		}
		payloadSum := sha256.Sum256(payload)
		lane := strings.TrimSpace(item.QueueLane)
		if lane == "" {
			lane = DefaultLane
		}
		attempt := item.ProcessingAttempt
		if attempt < 1 {
			attempt = 1
		}
		rows = append(rows, WorkItem{
			ID:       deterministicWorkItemID(item),
			TenantID: item.TenantID, KnowledgeBaseID: item.KnowledgeBaseID,
			KnowledgeID: item.KnowledgeID, ProcessingGeneration: item.ProcessingGeneration,
			ProcessingAttempt: attempt, ItemID: item.ItemID, WorkKind: item.WorkKind,
			Payload: payload, PayloadHash: hex.EncodeToString(payloadSum[:]),
			ModelID: item.ModelID, ModelTenantID: item.ModelTenantID,
			ResourcePoolID: item.ResourcePoolID, QuotaPoolID: item.QuotaPoolID,
			GatewayPoolID: item.GatewayPoolID, PolicyVersion: item.PolicyVersion,
			State: StateQueued, Priority: item.Priority, QueueLane: lane,
			NextAttemptAt: now, Version: 1, CreatedAt: now, UpdatedAt: now,
		})
	}
	if len(rows) == 0 {
		return rows, nil
	}
	err := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"}, {Name: "knowledge_id"},
			{Name: "processing_generation"}, {Name: "item_id"},
		},
		DoNothing: true,
	}).Create(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("upsert derivative plan: %w", err)
	}
	var persisted []WorkItem
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	if err := r.db.WithContext(ctx).Where("id IN ?", ids).Order("created_at, id").Find(&persisted).Error; err != nil {
		return nil, err
	}
	return persisted, nil
}

func (r *Repository) MarkDispatched(
	ctx context.Context,
	id string,
	expectedVersion uint64,
) (WakePayload, error) {
	return r.markDispatched(ctx, id, expectedVersion, "", 0, 0, 2*time.Minute)
}

func (r *Repository) markDispatched(
	ctx context.Context,
	id string,
	expectedVersion uint64,
	expectedPool string,
	totalWindow int,
	laneShare int,
	dispatchLease time.Duration,
) (WakePayload, error) {
	if dispatchLease <= 0 {
		dispatchLease = 2 * time.Minute
	}
	if r.db.Dialector.Name() != "postgres" {
		r.dispatchMu.Lock()
		defer r.dispatchMu.Unlock()
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(dispatchLease)
	var payload WakePayload
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if totalWindow > 0 && strings.TrimSpace(expectedPool) != "" && tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(
				"SELECT pg_advisory_xact_lock(?)", modeladmission.DispatchAdvisoryKey(expectedPool),
			).Error; err != nil {
				return err
			}
		}
		var row WorkItem
		if err := lockRow(tx, id, &row); err != nil {
			return err
		}
		if row.Version != expectedVersion ||
			(row.State != StateQueued && row.State != StateRetryWait &&
				row.State != StateMaterializeWait && row.State != StateFinalizeWait) ||
			row.NextAttemptAt.After(now) ||
			(row.DispatchLeaseUntil != nil && row.DispatchLeaseUntil.After(now)) {
			return ErrInvalidState
		}
		if totalWindow > 0 && strings.TrimSpace(expectedPool) != "" {
			if row.ResourcePoolID != expectedPool {
				return ErrInvalidState
			}
			var derivativeActive int64
			if err := tx.Model(&WorkItem{}).
				Where(
					`resource_pool_id = ? AND work_kind <> ? AND (
						state IN ? OR (
							state IN ? AND dispatch_lease_until IS NOT NULL AND dispatch_lease_until > ?
						)
					)`,
					expectedPool, WorkFinalizer,
					[]string{StateLeased, StateAdmitted, StateProviderRunning, StateProviderSucceeded, StateMaterializing},
					[]string{StateQueued, StateRetryWait}, now,
				).Count(&derivativeActive).Error; err != nil {
				return err
			}
			wikiActive := int64(0)
			wikiDemand := int64(0)
			if tx.Migrator().HasTable("task_pending_ops") &&
				tx.Migrator().HasColumn("task_pending_ops", "map_dispatch_lease_until") &&
				tx.Migrator().HasColumn("task_pending_ops", "map_resource_pool_id") {
				if err := tx.Table("task_pending_ops").Where(
					"task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL AND map_resource_pool_id = ? AND map_dispatch_lease_until > ?",
					types.TypeWikiIngest, "knowledge_base", "ingest", expectedPool, now,
				).Count(&wikiActive).Error; err != nil {
					return err
				}
				if err := tx.Table("task_pending_ops").Where(
					"task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL AND map_resource_pool_id = ? AND (next_attempt_at IS NULL OR next_attempt_at <= ?) AND (map_dispatch_lease_until IS NULL OR map_dispatch_lease_until <= ?)",
					types.TypeWikiIngest, "knowledge_base", "ingest", expectedPool, now, now,
				).Count(&wikiDemand).Error; err != nil {
					return err
				}
			}
			if derivativeActive+wikiActive >= int64(totalWindow) {
				return ErrDispatchWindowFull
			}
			if laneShare > 0 && derivativeActive >= int64(laneShare) && wikiDemand > 0 {
				return ErrDispatchWindowFull
			}
		}
		epoch := row.DispatchEpoch + 1
		taskID := fmt.Sprintf("derivative:%s:%d", row.ID, epoch)
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ?", row.ID, row.Version).
			Updates(map[string]any{
				"dispatch_epoch": epoch, "dispatch_task_id": taskID,
				"dispatch_lease_until": leaseUntil,
				"version":              row.Version + 1, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvalidState
		}
		payload = WakePayload{WorkItemID: row.ID, DispatchEpoch: epoch}
		return nil
	})
	return payload, err
}

func (r *Repository) releaseDispatchReservation(
	ctx context.Context,
	id string,
	epoch uint64,
) error {
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where("id = ? AND dispatch_epoch = ? AND state IN ?", id, epoch,
			[]string{StateQueued, StateRetryWait, StateMaterializeWait, StateFinalizeWait}).
		Updates(map[string]any{
			"dispatch_lease_until": nil,
			"version":              gorm.Expr("version + 1"),
			"updated_at":           time.Now().UTC(),
		})
	return result.Error
}

func (r *Repository) Claim(
	ctx context.Context,
	wake WakePayload,
	owner string,
	ttl time.Duration,
) (*WorkItem, error) {
	if ttl <= 0 {
		ttl = 45 * time.Second
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(ttl)
	token := uuid.NewString()
	var claimed WorkItem
	generationStale := false
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row WorkItem
		if err := lockRow(tx, wake.WorkItemID, &row); err != nil {
			return err
		}
		if row.DispatchEpoch != wake.DispatchEpoch {
			return ErrStaleDispatch
		}
		if row.NextAttemptAt.After(now) {
			return ErrInvalidState
		}
		switch row.State {
		case StateQueued, StateRetryWait, StateMaterializeWait, StateFinalizeWait:
		default:
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, row); err != nil {
			if errors.Is(err, ErrGenerationFence) {
				if cancelErr := cancelStaleRow(tx, row, now); cancelErr != nil {
					return cancelErr
				}
				generationStale = true
				return nil
			}
			return err
		}
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ?", row.ID, row.Version).
			Updates(map[string]any{
				"state": StateLeased, "owner_instance_id": strings.TrimSpace(owner),
				"lease_token": token, "lease_until": leaseUntil,
				"dispatch_lease_until": nil,
				"last_heartbeat_at":    now, "version": row.Version + 1,
				"updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		if err := tx.Where("id = ?", row.ID).First(&claimed).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if generationStale {
		return nil, ErrGenerationFence
	}
	return &claimed, nil
}

func (r *Repository) DeferWithoutProviderAttempt(
	ctx context.Context,
	id, leaseToken, errorClass, errorCode, message string,
	retryAfter time.Duration,
) error {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	now := time.Now().UTC()
	next := now.Add(retryAfter)
	result := r.db.WithContext(ctx).Model(&WorkItem{}).
		Where("id = ? AND lease_token = ? AND state = ?", id, leaseToken, StateLeased).
		Updates(map[string]any{
			"state": StateQueued, "next_attempt_at": next,
			"owner_instance_id": "", "lease_token": "", "lease_until": nil,
			"last_error_class":   truncate(errorClass, 32),
			"last_error_code":    truncate(errorCode, 64),
			"last_error_message": truncate(message, 4096),
			"version":            gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

// DeferForAdmission yields a claimed work item without consuming any
// provider, materialization, finalization, Asynq, or document-attempt budget.
// It also supports a multi-call handler that already checkpointed an earlier
// provider response: waiting for capacity before its next call is scheduling,
// not a failed materialization attempt.
func (r *Repository) DeferForAdmission(
	ctx context.Context,
	id, leaseToken, errorCode, message string,
	retryAfter time.Duration,
) error {
	if retryAfter < time.Second {
		retryAfter = time.Second
	}
	now := time.Now().UTC()
	next := now.Add(retryAfter)
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", id, leaseToken).
			First(&row).Error; err != nil {
			return ErrLeaseLost
		}
		switch row.State {
		case StateLeased:
		case StateProviderRunning, StateProviderSucceeded, StateMaterializing:
			var checkpoints int64
			if err := tx.Model(&ProviderCall{}).
				Where("work_item_id = ? AND disposition IN ?", row.ID,
					[]string{ProviderCallCheckpointed, ProviderCallAccepted}).
				Count(&checkpoints).Error; err != nil {
				return err
			}
			if checkpoints == 0 {
				return ErrInvalidState
			}
		default:
			return ErrInvalidState
		}
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ? AND lease_token = ?", row.ID, row.Version, leaseToken).
			Updates(map[string]any{
				"state": StateQueued, "next_attempt_at": next,
				"owner_instance_id": "", "lease_token": "", "lease_until": nil,
				"last_error_class":   "admission",
				"last_error_code":    truncate(errorCode, 64),
				"last_error_message": truncate(message, 4096),
				"version":            gorm.Expr("version + 1"), "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return nil
	})
}

func (r *Repository) BeginProvider(
	ctx context.Context,
	id, leaseToken string,
) (*WorkItem, error) {
	now := time.Now().UTC()
	var row WorkItem
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", id, leaseToken).First(&row).Error; err != nil {
			return ErrLeaseLost
		}
		if row.State != StateLeased || row.ProviderAttempts >= 4 {
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, row); err != nil {
			return err
		}
		requestKey := row.ProviderRequestKey
		if requestKey == "" {
			requestKey = "derivative-" + row.ID
		}
		result := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ? AND lease_token = ?", row.ID, row.Version, leaseToken).
			Updates(map[string]any{
				"state":                StateProviderRunning,
				"provider_request_key": requestKey,
				"provider_attempts":    row.ProviderAttempts + 1,
				"version":              row.Version + 1, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return tx.Where("id = ?", row.ID).First(&row).Error
	})
	if err == nil {
		pipelineobs.DerivativeProviderAttempt(row.WorkKind, row.ResourcePoolID)
	}
	return &row, err
}

// SaveProviderResult is the provider idempotency boundary. It persists the
// immutable response before the item can enter materialization. Re-delivery
// loads the existing Result and never requires another provider call.
func (r *Repository) SaveProviderResult(
	ctx context.Context,
	id, leaseToken string,
	provider ProviderResult,
) (*Result, error) {
	now := time.Now().UTC()
	var saved Result
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var row WorkItem
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ? AND lease_token = ?", id, leaseToken).First(&row).Error; err != nil {
			return ErrLeaseLost
		}
		if row.State != StateProviderRunning && row.State != StateProviderSucceeded {
			return ErrInvalidState
		}
		if err := verifyGeneration(tx, row); err != nil {
			return err
		}
		if len(provider.Content) > MaxPayloadSize && strings.TrimSpace(provider.URI) == "" {
			return errors.New("provider result exceeds 256 KiB and requires an object URI")
		}
		checksum := sha256.Sum256([]byte(provider.Content))
		resultID := uuid.NewSHA1(uuid.NameSpaceOID, []byte(row.ProviderRequestKey)).String()
		saved = Result{
			ID: resultID, WorkItemID: row.ID, ProviderRequestKey: row.ProviderRequestKey,
			ModelID: row.ModelID, ResourcePoolID: row.ResourcePoolID,
			ResponseContent: provider.Content, ResponseURI: provider.URI,
			ResponseSize: provider.Size, ResponseUsage: jsonOrEmpty(provider.Usage),
			ResponseMetadata:  jsonOrEmpty(provider.Metadata),
			ContentChecksum:   hex.EncodeToString(checksum[:]),
			ProviderRequestID: provider.ProviderRequestID,
			CreatedAt:         now, ExpiresAt: now.Add(7 * 24 * time.Hour),
		}
		if saved.ResponseSize <= 0 {
			saved.ResponseSize = int64(len([]byte(provider.Content)))
		}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&saved).Error; err != nil {
			return err
		}
		if err := tx.Where("work_item_id = ?", row.ID).First(&saved).Error; err != nil {
			return err
		}
		update := tx.Model(&WorkItem{}).
			Where("id = ? AND version = ?", row.ID, row.Version).
			Updates(map[string]any{
				"state": StateProviderSucceeded, "result_id": saved.ID,
				"version": row.Version + 1, "updated_at": now,
			})
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return ErrLeaseLost
		}
		return nil
	})
	return &saved, err
}

func deterministicWorkItemID(item PlanItem) string {
	material := fmt.Sprintf(
		"%d\x00%s\x00%s\x00%s",
		item.TenantID, item.KnowledgeID, item.ProcessingGeneration, item.ItemID,
	)
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte(material)).String()
}

func validatePlanItem(item PlanItem) error {
	switch {
	case item.TenantID == 0:
		return errors.New("derivative plan tenant_id is required")
	case strings.TrimSpace(item.KnowledgeBaseID) == "":
		return errors.New("derivative plan knowledge_base_id is required")
	case strings.TrimSpace(item.KnowledgeID) == "":
		return errors.New("derivative plan knowledge_id is required")
	case strings.TrimSpace(item.ProcessingGeneration) == "":
		return errors.New("derivative plan processing_generation is required")
	case strings.TrimSpace(item.ItemID) == "":
		return errors.New("derivative plan item_id is required")
	}
	switch item.WorkKind {
	case WorkSummary, WorkQuestion, WorkGraph, WorkDataTable, WorkFinalizer:
		return nil
	default:
		return fmt.Errorf("unsupported derivative work kind %q", item.WorkKind)
	}
}

func lockRow(tx *gorm.DB, id string, row *WorkItem) error {
	return tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("id = ?", id).First(row).Error
}

func verifyGeneration(tx *gorm.DB, row WorkItem) error {
	var knowledge struct {
		TenantID             uint64
		KnowledgeBaseID      string
		ProcessingGeneration string
		ParseStatus          string
		DeletedAt            gorm.DeletedAt
	}
	err := tx.Table("knowledges").
		Select("tenant_id", "knowledge_base_id", "processing_generation", "parse_status", "deleted_at").
		Where("id = ?", row.KnowledgeID).Take(&knowledge).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrGenerationFence
		}
		return err
	}
	if knowledge.TenantID != row.TenantID ||
		knowledge.KnowledgeBaseID != row.KnowledgeBaseID ||
		knowledge.ProcessingGeneration != row.ProcessingGeneration ||
		knowledge.DeletedAt.Valid {
		return ErrGenerationFence
	}
	switch knowledge.ParseStatus {
	case types.ParseStatusFinalizing, types.ParseStatusProcessing, types.ParseStatusCompleted:
		return nil
	default:
		return ErrGenerationFence
	}
}

func cancelStaleRow(tx *gorm.DB, row WorkItem, now time.Time) error {
	result := tx.Model(&WorkItem{}).Where("id = ? AND version = ?", row.ID, row.Version).
		Updates(map[string]any{
			"state": StateCancelled, "completed_at": now,
			"owner_instance_id": "", "lease_token": "", "lease_until": nil,
			"last_error_class":   "generation_fence",
			"last_error_code":    "generation_changed",
			"last_error_message": "processing generation or document lifecycle changed",
			"version":            row.Version + 1, "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

func jsonOrEmpty(value types.JSON) types.JSON {
	if len(value) == 0 || !json.Valid(value) {
		return types.JSON(`{}`)
	}
	return value
}

func truncate(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) <= max {
		return value
	}
	return value[:max]
}
