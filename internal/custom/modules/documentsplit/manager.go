package documentsplit

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrStalePart      = errors.New("document split: stale part delivery")
	ErrPartLeased     = errors.New("document split: part is already leased")
	ErrLeaseLost      = errors.New("document split: part lease was lost")
	ErrPlanIncomplete = errors.New("document split: plan is incomplete")
)

type ManagerParams struct {
	dig.In

	DB       *gorm.DB
	Enqueuer interfaces.TaskEnqueuer
	Queue    *documentqueue.Coordinator `optional:"true"`
}

// Manager is the PostgreSQL source of truth for physical split execution.
// Redis/Asynq carries wake-ups only; stable task IDs and database leases make
// every transition safe to replay after a worker or process crash.
type Manager struct {
	db       *gorm.DB
	enqueuer interfaces.TaskEnqueuer
	config   Config
	queue    *documentqueue.Coordinator
	owner    string
	// ownerPrefix is stable for one application-replica identity while owner
	// also includes the process boot. The document-queue identity fence makes
	// it safe for a new boot to reclaim only its own superseded leases.
	ownerPrefix string
	instanceID  string
	bootID      string

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

func NewManager(params ManagerParams) *Manager {
	ownerPrefix, owner := splitWorkerIdentity(params.Queue)
	instanceID := ""
	bootID := ""
	if params.Queue != nil {
		instanceID = params.Queue.InstanceID()
		bootID = params.Queue.BootID()
	}
	return &Manager{
		db:          params.DB,
		enqueuer:    params.Enqueuer,
		config:      LoadConfig(),
		queue:       params.Queue,
		owner:       owner,
		ownerPrefix: ownerPrefix,
		instanceID:  instanceID,
		bootID:      bootID,
	}
}

func NewManagerWithConfig(db *gorm.DB, enqueuer interfaces.TaskEnqueuer, cfg Config) *Manager {
	return &Manager{
		db: db, enqueuer: enqueuer, config: cfg.normalized(),
		owner: "split-worker-" + uuid.NewString(),
	}
}

func splitWorkerIdentity(queue *documentqueue.Coordinator) (string, string) {
	if queue == nil || strings.TrimSpace(queue.InstanceID()) == "" ||
		strings.TrimSpace(queue.BootID()) == "" {
		return "", "split-worker-" + uuid.NewString()
	}
	instanceID := strings.TrimSpace(queue.InstanceID())
	label := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '.', r == '_', r == '-':
			return r
		default:
			return '-'
		}
	}, instanceID)
	label = strings.Trim(label, "-")
	if label == "" {
		label = "instance"
	}
	if len(label) > 32 {
		label = label[:32]
	}
	sum := sha256.Sum256([]byte(instanceID))
	prefix := fmt.Sprintf("split:%s:%x:", label, sum[:6])
	return prefix, prefix + queue.BootID() + ":" + uuid.NewString()
}

func (m *Manager) Config() Config {
	if m == nil {
		return LoadConfig()
	}
	return m.config
}

func (m *Manager) Owner() string {
	if m == nil {
		return ""
	}
	return m.owner
}

// RegisterPartExecution keeps this process incarnation non-takeoverable until
// the part handler has fully returned. It is lifecycle tracking rather than a
// second concurrency limit: the root document workflow already owns the
// configured per-instance document slot.
func (m *Manager) RegisterPartExecution(cancel context.CancelFunc) (func(), error) {
	if m == nil {
		return nil, ErrLeaseLost
	}
	if m.queue == nil {
		return func() {}, nil
	}
	return m.queue.RegisterAuxiliaryExecution(cancel)
}

func (m *Manager) Migrate(ctx context.Context) error {
	if m == nil || m.db == nil {
		return errors.New("document split: database is unavailable")
	}
	if m.db.Dialector.Name() == "sqlite" {
		db := m.db.Session(&gorm.Session{NewDB: true})
		cfg := *db.Config
		cfg.DisableForeignKeyConstraintWhenMigrating = true
		db.Config = &cfg
		return db.WithContext(ctx).AutoMigrate(&Plan{}, &Part{}, &types.Chunk{})
	}
	migrator := m.db.Migrator()
	if !migrator.HasTable(&Plan{}) || !migrator.HasTable(&Part{}) {
		return errors.New("document split schema is missing; run versioned migration 000077")
	}
	for _, field := range []string{"lease_instance_id", "lease_boot_id"} {
		if !migrator.HasColumn(&Part{}, field) {
			return fmt.Errorf(
				"document split part column %s is missing; run versioned migration 000080",
				field,
			)
		}
	}
	for _, field := range []string{
		"processing_generation", "split_part_index", "source_locator",
	} {
		if !migrator.HasColumn(&types.Chunk{}, field) {
			return fmt.Errorf("document split chunk column %s is missing; run versioned migration 000077", field)
		}
	}
	if m.db.Dialector.Name() == "postgres" {
		columnTypes, err := migrator.ColumnTypes(&types.Chunk{})
		if err != nil {
			return fmt.Errorf("inspect document split chunk coordinates: %w", err)
		}
		requiredBigInt := map[string]bool{
			"chunk_index": false,
			"start_at":    false,
			"end_at":      false,
		}
		for _, column := range columnTypes {
			name := strings.ToLower(column.Name())
			if _, required := requiredBigInt[name]; !required {
				continue
			}
			dataType := strings.ToLower(column.DatabaseTypeName())
			if dataType != "bigint" && dataType != "int8" {
				return fmt.Errorf(
					"document split chunk coordinate %s is %s, expected BIGINT; run versioned migration 000077",
					name, dataType,
				)
			}
			requiredBigInt[name] = true
		}
		for name, found := range requiredBigInt {
			if !found {
				return fmt.Errorf(
					"document split chunk coordinate %s is missing; run versioned migration 000077",
					name,
				)
			}
		}
	}
	return nil
}

func (m *Manager) CreatePlan(ctx context.Context, plan *Plan, parts []*Part) (*Plan, error) {
	if m == nil || m.db == nil || plan == nil || len(parts) == 0 {
		return nil, ErrPlanIncomplete
	}
	now := time.Now()
	if plan.ID == "" {
		plan.ID = uuid.NewString()
	}
	if plan.TenantID == 0 || strings.TrimSpace(plan.KnowledgeBaseID) == "" ||
		strings.TrimSpace(plan.KnowledgeID) == "" ||
		strings.TrimSpace(plan.ProcessingGeneration) == "" ||
		strings.TrimSpace(plan.ProcessingOwner) == "" ||
		plan.SourceSize <= 0 || strings.TrimSpace(plan.SourceSHA256) == "" {
		return nil, ErrPlanIncomplete
	}
	if len(parts) > m.config.ArchiveMaxParts {
		return nil, fmt.Errorf("document split: %d parts exceeds configured maximum %d",
			len(parts), m.config.ArchiveMaxParts)
	}
	plan.State = PlanQueued
	plan.PartCount = len(parts)
	plan.CompletedParts = 0
	plan.FailedParts = 0
	plan.LastProgressAt = now
	plan.TargetRatio = m.config.TargetRatio
	plan.Attempt = 1
	plan.Version = 1
	plan.CreatedAt = now
	plan.UpdatedAt = now

	namespace, err := uuid.Parse(plan.ID)
	if err != nil {
		return nil, fmt.Errorf("document split: invalid plan ID: %w", err)
	}
	var totalBytes int64
	for index, part := range parts {
		if part == nil || part.PartIndex != index || part.InputSize <= 0 ||
			strings.TrimSpace(part.InputPath) == "" || strings.TrimSpace(part.InputSHA256) == "" {
			return nil, fmt.Errorf("%w: invalid part %d", ErrPlanIncomplete, index)
		}
		part.ID = uuid.NewSHA1(namespace, []byte(fmt.Sprintf("part:%08d", index))).String()
		part.PlanID = plan.ID
		part.TenantID = plan.TenantID
		part.KnowledgeBaseID = plan.KnowledgeBaseID
		part.KnowledgeID = plan.KnowledgeID
		part.ProcessingGeneration = plan.ProcessingGeneration
		part.State = PartPreparing
		part.Attempt = 0
		part.LastProgressAt = now
		part.Version = 1
		part.CreatedAt = now
		part.UpdatedAt = now
		totalBytes += part.InputSize
	}
	if plan.TotalPartBytes != 0 && plan.TotalPartBytes != totalBytes {
		return nil, fmt.Errorf("document split: part byte total mismatch: manifest=%d actual=%d",
			plan.TotalPartBytes, totalBytes)
	}
	plan.TotalPartBytes = totalBytes

	err = m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing Plan
		find := tx.Where(
			"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
			plan.TenantID, plan.KnowledgeID, plan.ProcessingGeneration,
		).First(&existing)
		switch {
		case find.Error == nil:
			if existing.SourceSHA256 != plan.SourceSHA256 ||
				existing.PartCount != plan.PartCount ||
				existing.PlannerVersion != plan.PlannerVersion {
				return errors.New("document split: immutable generation plan conflicts with existing row")
			}
			*plan = existing
			return nil
		case !errors.Is(find.Error, gorm.ErrRecordNotFound):
			return find.Error
		}
		if err := tx.Create(plan).Error; err != nil {
			return err
		}
		return tx.CreateInBatches(parts, 100).Error
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (m *Manager) GetPlan(ctx context.Context, planID string) (*Plan, error) {
	var plan Plan
	if err := m.db.WithContext(ctx).First(&plan, "id = ?", planID).Error; err != nil {
		return nil, err
	}
	return &plan, nil
}

func (m *Manager) GetPlanForGeneration(
	ctx context.Context, tenantID uint64, knowledgeID, generation string,
) (*Plan, error) {
	var plan Plan
	if err := m.db.WithContext(ctx).Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
		tenantID, knowledgeID, generation,
	).First(&plan).Error; err != nil {
		return nil, err
	}
	return &plan, nil
}

func (m *Manager) ListParts(ctx context.Context, planID string) ([]*Part, error) {
	var parts []*Part
	err := m.db.WithContext(ctx).Where("plan_id = ?", planID).
		Order("part_index ASC").Find(&parts).Error
	return parts, err
}

func (m *Manager) enqueuePart(part *Part) error {
	payload, err := json.Marshal(PartPayload{
		TenantID: part.TenantID, KnowledgeBaseID: part.KnowledgeBaseID,
		KnowledgeID: part.KnowledgeID, ProcessingGeneration: part.ProcessingGeneration,
		PlanID: part.PlanID, PartID: part.ID, PartIndex: part.PartIndex,
		Attempt: part.Attempt + 1, DeliveryEpoch: part.LeaseEpoch + 1,
	})
	if err != nil {
		return err
	}
	_, err = m.enqueuer.Enqueue(
		asynq.NewTask(TypePartProcess, payload),
		asynq.Queue(QueuePart),
		asynq.TaskID(PartTaskID(part.PlanID, part.PartIndex, part.LeaseEpoch+1)),
		asynq.MaxRetry(m.config.MaxRetry),
		asynq.Timeout(m.config.TaskTimeout),
		asynq.Retention(7*24*time.Hour),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	return err
}

func (m *Manager) enqueueFinalize(plan *Plan) error {
	payload, err := json.Marshal(FinalizePayload{
		TenantID: plan.TenantID, KnowledgeBaseID: plan.KnowledgeBaseID,
		KnowledgeID: plan.KnowledgeID, ProcessingGeneration: plan.ProcessingGeneration,
		ProcessingOwner: plan.ProcessingOwner, PlanID: plan.ID, Attempt: plan.Attempt,
	})
	if err != nil {
		return err
	}
	_, err = m.enqueuer.Enqueue(
		asynq.NewTask(TypeFinalize, payload),
		asynq.Queue(QueuePart),
		asynq.TaskID(FinalizeTaskID(plan.ID)),
		asynq.MaxRetry(m.config.MaxRetry),
		asynq.Timeout(2*m.config.TaskTimeout),
		asynq.Retention(7*24*time.Hour),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	return err
}

// DispatchPlan admits only a bounded number of parts for one logical document.
// The plan row lock serializes several replicas; plan ordering in recoverOnce
// supplies round-robin fairness across documents.
func (m *Manager) DispatchPlan(ctx context.Context, planID string) error {
	if m == nil || m.enqueuer == nil {
		return errors.New("document split: task enqueuer is unavailable")
	}
	var selected []*Part
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var plan Plan
		query := tx.Where("id = ?", planID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&plan).Error; err != nil {
			return err
		}
		if plan.State != PlanQueued && plan.State != PlanParsing {
			return nil
		}
		var active int64
		if err := tx.Model(&Part{}).Where(
			"plan_id = ? AND state IN ?", planID, []PartState{PartQueued, PartLeased},
		).Count(&active).Error; err != nil {
			return err
		}
		window := m.config.PerDocumentWindow
		var activeRetries int64
		if err := tx.Model(&Part{}).Where(
			"plan_id = ? AND state NOT IN ? AND attempt > 0 AND last_error <> ''",
			planID, []PartState{PartCompleted, PartFailed},
		).Count(&activeRetries).Error; err != nil {
			return err
		}
		var completedRetries int64
		if err := tx.Model(&Part{}).Where(
			"plan_id = ? AND state = ? AND attempt > 1",
			planID, PartCompleted,
		).Count(&completedRetries).Error; err != nil {
			return err
		}
		// A transient provider failure (most notably an embedding TPM 429)
		// moves a part back to preparing with an attempt count. Resume that
		// logical document with one probe at a time. Keep the circuit breaker
		// closed for the rest of this document after a probe succeeds as well:
		// immediately reopening the full window would recreate the same burst
		// every few parts and eventually exhaust the retry budget.
		if (activeRetries > 0 || completedRetries > 0) && window > 1 {
			window = 1
		}
		available := window - int(active)
		if available <= 0 {
			return nil
		}
		now := time.Now()
		q := tx.Where(
			"plan_id = ? AND state = ? AND (lease_until IS NULL OR lease_until <= ?)",
			planID, PartPreparing, now,
		).
			Order("part_index ASC").Limit(available)
		if tx.Dialector.Name() != "sqlite" {
			q = q.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := q.Find(&selected).Error; err != nil {
			return err
		}
		if len(selected) == 0 {
			return nil
		}
		ids := make([]string, 0, len(selected))
		for _, part := range selected {
			ids = append(ids, part.ID)
		}
		if err := tx.Model(&Part{}).Where("id IN ? AND state = ?", ids, PartPreparing).
			Updates(map[string]interface{}{
				"state": PartQueued, "lease_until": nil,
				"last_progress_at": now, "updated_at": now,
			}).Error; err != nil {
			return err
		}
		return tx.Model(&Plan{}).Where("id = ?", planID).Updates(map[string]interface{}{
			"state": PlanParsing, "last_progress_at": now, "updated_at": now,
		}).Error
	})
	if err != nil {
		return err
	}
	for _, part := range selected {
		part.State = PartQueued
		if err := m.enqueuePart(part); err != nil {
			_ = m.db.WithContext(ctx).Model(&Part{}).
				Where("id = ? AND state = ?", part.ID, PartQueued).
				Updates(map[string]interface{}{
					"state": PartPreparing, "last_error": err.Error(), "updated_at": time.Now(),
				}).Error
			return fmt.Errorf("enqueue split part %d: %w", part.PartIndex, err)
		}
	}
	return nil
}

func (m *Manager) ClaimPart(ctx context.Context, payload PartPayload) (*Part, int64, error) {
	if m.queue != nil {
		if err := m.queue.AssertCurrentBoot(ctx, false); err != nil {
			return nil, 0, err
		}
	}
	var claimed Part
	now := time.Now()
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("id = ? AND plan_id = ?", payload.PartID, payload.PlanID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&claimed).Error; err != nil {
			return err
		}
		if claimed.TenantID != payload.TenantID ||
			claimed.KnowledgeBaseID != payload.KnowledgeBaseID ||
			claimed.KnowledgeID != payload.KnowledgeID ||
			claimed.ProcessingGeneration != payload.ProcessingGeneration ||
			claimed.PartIndex != payload.PartIndex {
			return ErrStalePart
		}
		if payload.DeliveryEpoch != claimed.LeaseEpoch+1 {
			return ErrStalePart
		}
		switch claimed.State {
		case PartCompleted, PartCancelled, PartFailed:
			return ErrStalePart
		case PartLeased:
			if claimed.LeaseUntil != nil && claimed.LeaseUntil.After(now) {
				return ErrPartLeased
			}
		case PartQueued:
		default:
			return ErrStalePart
		}
		if claimed.Attempt >= m.config.MaxRetry {
			return errors.New("document split: part retry budget exhausted")
		}
		until := now.Add(m.config.LeaseDuration)
		claimed.State = PartLeased
		claimed.Attempt++
		claimed.LeaseEpoch++
		claimed.LeaseOwner = m.owner
		claimed.LeaseInstanceID = m.instanceID
		claimed.LeaseBootID = m.bootID
		claimed.LeaseUntil = &until
		claimed.LastProgressAt = now
		claimed.UpdatedAt = now
		return tx.Model(&Part{}).Where("id = ? AND version = ?", claimed.ID, claimed.Version).
			Updates(map[string]interface{}{
				"state": PartLeased, "attempt": claimed.Attempt,
				"lease_epoch": claimed.LeaseEpoch, "lease_owner": claimed.LeaseOwner,
				"lease_instance_id": claimed.LeaseInstanceID,
				"lease_boot_id":     claimed.LeaseBootID,
				"lease_until":       until, "last_progress_at": now, "updated_at": now,
				"version": gorm.Expr("version + 1"),
			}).Error
	})
	if err != nil {
		return nil, 0, err
	}
	return &claimed, claimed.LeaseEpoch, nil
}

func (m *Manager) HeartbeatPart(ctx context.Context, partID string, epoch int64) error {
	if m.queue != nil {
		if err := m.queue.AssertCurrentBoot(ctx, true); err != nil {
			return errors.Join(ErrLeaseLost, err)
		}
	}
	now := time.Now()
	res := m.db.WithContext(ctx).Model(&Part{}).Where(
		"id = ? AND state = ? AND lease_owner = ? AND lease_instance_id = ? AND lease_boot_id = ? AND lease_epoch = ?",
		partID, PartLeased, m.owner, m.instanceID, m.bootID, epoch,
	).Updates(map[string]interface{}{
		"lease_until":      now.Add(m.config.LeaseDuration),
		"last_progress_at": now, "updated_at": now,
	})
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected != 1 {
		return ErrLeaseLost
	}
	return nil
}

type PartCompletion struct {
	MarkdownChars int64
	ChunkCount    int
	StorageBytes  int64
	FirstChunkID  string
	LastChunkID   string
	ImageMappings types.JSON
}

func (m *Manager) CompletePart(
	ctx context.Context, part *Part, epoch int64, completion PartCompletion,
) (bool, error) {
	if part == nil || completion.ChunkCount < 0 || completion.StorageBytes < 0 {
		return false, ErrPlanIncomplete
	}
	if m.queue != nil {
		if err := m.queue.AssertCurrentBoot(ctx, true); err != nil {
			return false, errors.Join(ErrLeaseLost, err)
		}
	}
	allComplete := false
	completionMetrics := append(types.JSON(nil), part.Metrics...)
	var metrics map[string]interface{}
	if len(completionMetrics) == 0 || json.Unmarshal(completionMetrics, &metrics) != nil {
		metrics = make(map[string]interface{})
	}
	metrics["execution_owner"] = m.owner
	metrics["execution_lease_epoch"] = epoch
	if encoded, err := json.Marshal(metrics); err == nil {
		completionMetrics = encoded
	}
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		now := time.Now()
		res := tx.Model(&Part{}).Where(
			"id = ? AND state = ? AND lease_owner = ? AND lease_instance_id = ? AND lease_boot_id = ? AND lease_epoch = ?",
			part.ID, PartLeased, m.owner, m.instanceID, m.bootID, epoch,
		).Updates(map[string]interface{}{
			"state": PartCompleted, "lease_owner": "", "lease_instance_id": "",
			"lease_boot_id": "", "lease_until": nil,
			"markdown_chars": completion.MarkdownChars, "draft_chunks": completion.ChunkCount,
			"storage_bytes":  completion.StorageBytes,
			"first_chunk_id": completion.FirstChunkID, "last_chunk_id": completion.LastChunkID,
			"image_mappings": completion.ImageMappings,
			"metrics":        completionMetrics,
			"last_error":     "", "completed_at": now, "last_progress_at": now,
			"updated_at": now, "version": gorm.Expr("version + 1"),
		})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected != 1 {
			var current Part
			if err := tx.First(&current, "id = ?", part.ID).Error; err == nil &&
				current.State == PartCompleted {
				return nil
			}
			return ErrLeaseLost
		}
		var completed int64
		if err := tx.Model(&Part{}).Where(
			"plan_id = ? AND state = ?", part.PlanID, PartCompleted,
		).Count(&completed).Error; err != nil {
			return err
		}
		var plan Plan
		query := tx.Where("id = ?", part.PlanID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&plan).Error; err != nil {
			return err
		}
		updates := map[string]interface{}{
			"completed_parts": int(completed), "last_progress_at": now, "updated_at": now,
			"version": gorm.Expr("version + 1"),
		}
		if int(completed) == plan.PartCount {
			allComplete = true
			updates["state"] = PlanFinalizing
			updates["finalizer_task_id"] = FinalizeTaskID(plan.ID)
		} else {
			var completedRetries int64
			if err := tx.Model(&Part{}).Where(
				"plan_id = ? AND state = ? AND attempt > 1",
				part.PlanID, PartCompleted,
			).Count(&completedRetries).Error; err != nil {
				return err
			}
			if completedRetries > 0 {
				// A successful probe commonly consumes most of a provider's
				// rolling TPM allowance. Pace the next physical part by one
				// two base backoff windows; otherwise a single-concurrency circuit
				// breaker still burns an avoidable retry immediately after
				// every success.
				resumeAt := now.Add(m.retryBackoff(2))
				if err := tx.Model(&Part{}).Where(
					"plan_id = ? AND state = ? AND (lease_until IS NULL OR lease_until < ?)",
					part.PlanID, PartPreparing, resumeAt,
				).Updates(map[string]interface{}{
					"lease_until": resumeAt, "updated_at": now,
				}).Error; err != nil {
					return err
				}
			}
		}
		return tx.Model(&Plan{}).Where("id = ?", plan.ID).Updates(updates).Error
	})
	if err != nil {
		return false, err
	}
	if !allComplete {
		if err := m.DispatchPlan(ctx, part.PlanID); err != nil {
			// The part completion is already durable. Redis is only a wake-up
			// channel; returning an error here would retry a completed part and
			// could incorrectly spend its business retry budget. Recovery
			// republishes the next bounded window from PostgreSQL.
			logger.Warnf(ctx, "[document split] deferred next-part dispatch plan=%s: %v",
				part.PlanID, err)
		}
		return false, nil
	}
	plan, err := m.GetPlan(ctx, part.PlanID)
	if err != nil {
		logger.Warnf(ctx, "[document split] finalizer wake-up deferred; reload plan=%s: %v",
			part.PlanID, err)
		return true, nil
	}
	if err := m.enqueueFinalize(plan); err != nil {
		// PlanFinalizing is the durable state. The recovery loop republishes
		// this stable task ID, so a transient Redis outage after the commit
		// must not turn a successfully parsed part into a failure.
		logger.Warnf(ctx, "[document split] finalizer wake-up deferred plan=%s: %v",
			part.PlanID, err)
	}
	return true, nil
}

func (m *Manager) ReleasePart(ctx context.Context, part *Part, epoch int64, cause error) error {
	if part == nil {
		return ErrLeaseLost
	}
	if m.queue != nil {
		if err := m.queue.AssertCurrentBoot(ctx, true); err != nil {
			return errors.Join(ErrLeaseLost, err)
		}
	}
	message := "part processing failed"
	if cause != nil {
		message = cause.Error()
	}
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current Part
		query := tx.Where("id = ?", part.ID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&current).Error; err != nil {
			return err
		}
		if current.State != PartLeased ||
			current.LeaseOwner != m.owner ||
			current.LeaseInstanceID != m.instanceID ||
			current.LeaseBootID != m.bootID ||
			current.LeaseEpoch != epoch {
			return ErrLeaseLost
		}
		now := time.Now()
		nextState := PartQueued
		var retryAt *time.Time
		refundAttempt := false
		if providerDelay, deferred := modeladmission.ModelRetryAfter(cause); deferred {
			// Provider outages are shared external backpressure, not failed
			// document-part attempts. Keep the part durable, pause its siblings
			// and refund the attempt claimed by this delivery.
			nextState = PartPreparing
			if providerDelay < m.config.RetryBackoffBase {
				providerDelay = m.config.RetryBackoffBase
			}
			value := now.Add(providerDelay)
			retryAt = &value
			refundAttempt = true
		} else if errors.Is(cause, context.Canceled) {
			nextState = PartPreparing
			value := now.Add(m.config.RetryBackoffBase)
			retryAt = &value
			refundAttempt = true
		} else if current.Attempt >= m.config.MaxRetry {
			nextState = PartFailed
		} else if isTransientPartError(cause) {
			nextState = PartPreparing
			value := now.Add(m.retryBackoff(current.Attempt))
			retryAt = &value
		}
		updates := map[string]interface{}{
			"state": nextState, "lease_owner": "", "lease_instance_id": "",
			"lease_boot_id": "", "lease_until": retryAt,
			"last_error": message, "last_progress_at": now, "updated_at": now,
			"version": gorm.Expr("version + 1"),
		}
		if refundAttempt {
			updates["attempt"] = gorm.Expr("CASE WHEN attempt > 0 THEN attempt - 1 ELSE 0 END")
		}
		if err := tx.Model(&Part{}).Where("id = ?", part.ID).Updates(updates).Error; err != nil {
			return err
		}
		if nextState == PartFailed {
			return tx.Model(&Plan{}).Where("id = ?", part.PlanID).Updates(map[string]interface{}{
				"state": PlanFailed, "failed_parts": gorm.Expr("failed_parts + 1"),
				"last_error": message, "last_progress_at": now, "updated_at": now,
			}).Error
		}
		if retryAt != nil {
			// Pause not-yet-admitted siblings too. Otherwise one provider 429
			// immediately opens the document window for another burst.
			if err := tx.Model(&Part{}).Where(
				"plan_id = ? AND state = ? AND (lease_until IS NULL OR lease_until < ?)",
				part.PlanID, PartPreparing, *retryAt,
			).Updates(map[string]interface{}{
				"lease_until": *retryAt, "updated_at": now,
			}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func isTransientPartError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"too many requests", "status 429", "rate limit", "temporarily unavailable",
		"status 502", "status 503", "status 504", "connection reset",
		"i/o timeout", "deadline exceeded",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func (m *Manager) retryBackoff(attempt int) time.Duration {
	delay := m.config.RetryBackoffBase
	for step := 1; step < attempt && delay < m.config.RetryBackoffMax; step++ {
		if delay > m.config.RetryBackoffMax/2 {
			return m.config.RetryBackoffMax
		}
		delay *= 2
	}
	if delay > m.config.RetryBackoffMax {
		return m.config.RetryBackoffMax
	}
	return delay
}

func (m *Manager) ClaimFinalize(ctx context.Context, payload FinalizePayload) (*Plan, error) {
	var plan Plan
	err := m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Where("id = ?", payload.PlanID)
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE"})
		}
		if err := query.First(&plan).Error; err != nil {
			return err
		}
		if plan.TenantID != payload.TenantID ||
			plan.KnowledgeBaseID != payload.KnowledgeBaseID ||
			plan.KnowledgeID != payload.KnowledgeID ||
			plan.ProcessingGeneration != payload.ProcessingGeneration ||
			plan.ProcessingOwner != payload.ProcessingOwner {
			return ErrStalePart
		}
		if plan.State == PlanCompleted || plan.State == PlanCancelled || plan.State == PlanSuperseded {
			return ErrStalePart
		}
		if plan.State != PlanFinalizing || plan.CompletedParts != plan.PartCount {
			return ErrPlanIncomplete
		}
		now := time.Now()
		plan.Attempt++
		return tx.Model(&Plan{}).Where("id = ? AND state = ?", plan.ID, PlanFinalizing).
			Updates(map[string]interface{}{
				"attempt": plan.Attempt, "last_progress_at": now, "updated_at": now,
				"version": gorm.Expr("version + 1"),
			}).Error
	})
	return &plan, err
}

func (m *Manager) CompletePlan(ctx context.Context, planID string) error {
	now := time.Now()
	return m.db.WithContext(ctx).Model(&Plan{}).
		Where("id = ? AND state = ?", planID, PlanFinalizing).
		Updates(map[string]interface{}{
			"state": PlanCompleted, "last_error": "", "last_progress_at": now,
			"updated_at": now, "version": gorm.Expr("version + 1"),
		}).Error
}

func (m *Manager) FailPlan(ctx context.Context, planID string, cause error) error {
	message := "document split finalization failed"
	if cause != nil {
		message = cause.Error()
	}
	now := time.Now()
	return m.db.WithContext(ctx).Model(&Plan{}).
		Where("id = ? AND state NOT IN ?", planID,
			[]PlanState{PlanCompleted, PlanCancelled, PlanSuperseded}).
		Updates(map[string]interface{}{
			"state": PlanFailed, "last_error": message, "last_progress_at": now,
			"updated_at": now, "version": gorm.Expr("version + 1"),
		}).Error
}

func (m *Manager) DeletePartChunks(
	ctx context.Context, tenantID uint64, knowledgeID, generation string, partIndex int,
) error {
	return m.db.WithContext(ctx).Unscoped().Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND split_part_index = ?",
		tenantID, knowledgeID, generation, partIndex,
	).Delete(&types.Chunk{}).Error
}

func (m *Manager) ListPartChunkIDs(
	ctx context.Context, tenantID uint64, knowledgeID, generation string, partIndex int,
) ([]string, error) {
	var ids []string
	err := m.db.WithContext(ctx).Model(&types.Chunk{}).Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND split_part_index = ?",
		tenantID, knowledgeID, generation, partIndex,
	).Order("id ASC").Pluck("id", &ids).Error
	return ids, err
}

func (m *Manager) ListGenerationChunks(
	ctx context.Context, tenantID uint64, knowledgeID, generation string, afterIndex int, limit int,
) ([]*types.Chunk, error) {
	return m.ListGenerationChunksAfter(
		ctx, tenantID, knowledgeID, generation,
		GenerationChunkCursor{ChunkIndex: afterIndex}, limit,
	)
}

// GenerationChunkCursor is a stable keyset cursor over the logical chunk
// sequence. ChunkIndex alone is not unique: image OCR/caption chunks and
// parent/child strategies can legitimately share the text chunk's index.
// Pairing it with the immutable chunk ID prevents page-boundary omissions.
type GenerationChunkCursor struct {
	ChunkIndex int
	ChunkID    string
}

func (m *Manager) ListGenerationChunksAfter(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, generation string,
	after GenerationChunkCursor,
	limit int,
) ([]*types.Chunk, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var chunks []*types.Chunk
	query := m.db.WithContext(ctx).Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
		tenantID, knowledgeID, generation,
	)
	if after.ChunkID == "" {
		query = query.Where("chunk_index > ?", after.ChunkIndex)
	} else {
		query = query.Where(
			"(chunk_index > ?) OR (chunk_index = ? AND id > ?)",
			after.ChunkIndex, after.ChunkIndex, after.ChunkID,
		)
	}
	err := query.Order("chunk_index ASC, id ASC").Limit(limit).Find(&chunks).Error
	return chunks, err
}

func (m *Manager) CountGenerationChunks(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, generation string,
	chunkTypes []types.ChunkType,
) (int64, error) {
	query := m.db.WithContext(ctx).Model(&types.Chunk{}).Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
		tenantID, knowledgeID, generation,
	)
	if len(chunkTypes) > 0 {
		query = query.Where("chunk_type IN ?", chunkTypes)
	}
	var count int64
	return count, query.Count(&count).Error
}

func (m *Manager) ListGenerationChunksByType(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, generation string,
	chunkTypes []types.ChunkType,
	afterIndex int,
	limit int,
) ([]*types.Chunk, error) {
	return m.ListGenerationChunksByTypeAfter(
		ctx, tenantID, knowledgeID, generation, chunkTypes,
		GenerationChunkCursor{ChunkIndex: afterIndex}, limit,
	)
}

func (m *Manager) ListGenerationChunksByTypeAfter(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, generation string,
	chunkTypes []types.ChunkType,
	after GenerationChunkCursor,
	limit int,
) ([]*types.Chunk, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	query := m.db.WithContext(ctx).Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
		tenantID, knowledgeID, generation,
	)
	if after.ChunkID == "" {
		query = query.Where("chunk_index > ?", after.ChunkIndex)
	} else {
		query = query.Where(
			"(chunk_index > ?) OR (chunk_index = ? AND id > ?)",
			after.ChunkIndex, after.ChunkIndex, after.ChunkID,
		)
	}
	if len(chunkTypes) > 0 {
		query = query.Where("chunk_type IN ?", chunkTypes)
	}
	var chunks []*types.Chunk
	err := query.Order("chunk_index ASC, id ASC").Limit(limit).Find(&chunks).Error
	return chunks, err
}

func (m *Manager) ListOldChunkIDs(
	ctx context.Context, tenantID uint64, knowledgeID, generation, afterID string, limit int,
) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	var ids []string
	query := m.db.WithContext(ctx).Model(&types.Chunk{}).
		Where("tenant_id = ? AND knowledge_id = ? AND processing_generation <> ?",
			tenantID, knowledgeID, generation)
	if afterID != "" {
		query = query.Where("id > ?", afterID)
	}
	err := query.Order("id ASC").Limit(limit).Pluck("id", &ids).Error
	return ids, err
}

func (m *Manager) DeleteOldChunksByIDs(
	ctx context.Context, tenantID uint64, knowledgeID, generation string, ids []string,
) error {
	if len(ids) == 0 {
		return nil
	}
	return m.db.WithContext(ctx).Unscoped().Where(
		"tenant_id = ? AND knowledge_id = ? AND processing_generation <> ? AND id IN ?",
		tenantID, knowledgeID, generation, ids,
	).Delete(&types.Chunk{}).Error
}

// NormalizeGenerationTextChunkIndexes turns the sparse coordinates used while
// parts are parsed concurrently into the same contiguous 0-based sequence an
// unsplit document exposes. SourceLocator and SplitPartIndex retain the
// physical provenance; ChunkIndex is the public logical document position.
func (m *Manager) NormalizeGenerationTextChunkIndexes(
	ctx context.Context, tenantID uint64, knowledgeID, generation string,
) error {
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var ids []string
		if err := tx.Model(&types.Chunk{}).
			Where(
				"tenant_id = ? AND knowledge_id = ? AND processing_generation = ? AND chunk_type = ?",
				tenantID, knowledgeID, generation, types.ChunkTypeText,
			).
			Order("split_part_index ASC, chunk_index ASC, id ASC").
			Pluck("id", &ids).Error; err != nil {
			return err
		}

		const updateBatchSize = 500
		for start := 0; start < len(ids); start += updateBatchSize {
			end := start + updateBatchSize
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[start:end]
			var expression strings.Builder
			expression.WriteString("CASE id")
			args := make([]interface{}, 0, len(batch)*2)
			for offset, id := range batch {
				expression.WriteString(" WHEN ? THEN ?")
				args = append(args, id, start+offset)
			}
			expression.WriteString(" ELSE chunk_index END")
			if err := tx.Model(&types.Chunk{}).
				Where("id IN ?", batch).
				UpdateColumn(
					"chunk_index",
					gorm.Expr(expression.String(), args...),
				).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (m *Manager) PublishGeneration(
	ctx context.Context, tenantID uint64, knowledgeID, generation string, parts []*Part,
) error {
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for index := 1; index < len(parts); index++ {
			previous := parts[index-1]
			current := parts[index]
			if previous.LastChunkID == "" || current.FirstChunkID == "" {
				continue
			}
			if err := tx.Model(&types.Chunk{}).Where(
				"tenant_id = ? AND id = ? AND processing_generation = ?",
				tenantID, previous.LastChunkID, generation,
			).Update("next_chunk_id", current.FirstChunkID).Error; err != nil {
				return err
			}
			if err := tx.Model(&types.Chunk{}).Where(
				"tenant_id = ? AND id = ? AND processing_generation = ?",
				tenantID, current.FirstChunkID, generation,
			).Update("pre_chunk_id", previous.LastChunkID).Error; err != nil {
				return err
			}
		}
		// The logical document switches generations in one database
		// transaction. New vectors may already be ready, but retrieval cannot
		// expose them before this point; old rows remain available until the
		// same commit disables them.
		if err := tx.Model(&types.Chunk{}).Where(
			"tenant_id = ? AND knowledge_id = ? AND processing_generation <> ?",
			tenantID, knowledgeID, generation,
		).Updates(map[string]interface{}{
			"is_enabled": false, "updated_at": time.Now(),
		}).Error; err != nil {
			return err
		}
		return tx.Model(&types.Chunk{}).Where(
			"tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
			tenantID, knowledgeID, generation,
		).Updates(map[string]interface{}{"is_enabled": true, "updated_at": time.Now()}).Error
	})
}

func (m *Manager) recoverOnce(ctx context.Context) error {
	now := time.Now()
	if err := m.recoverSupersededLocalBootLeases(ctx, now); err != nil {
		return err
	}
	if err := m.recoverExpiredLeases(ctx, now); err != nil {
		return err
	}

	var plans []*Plan
	if err := m.db.WithContext(ctx).Where(
		"state IN ?", []PlanState{PlanQueued, PlanParsing, PlanFinalizing},
	).Order("last_progress_at ASC, id ASC").Limit(m.config.RecoveryBatchSize).
		Find(&plans).Error; err != nil {
		return err
	}
	for _, plan := range plans {
		var knowledge types.Knowledge
		err := m.db.WithContext(ctx).Select(
			"id", "tenant_id", "knowledge_base_id", "processing_generation",
			"processing_owner", "parse_status",
		).First(&knowledge, "id = ? AND tenant_id = ?", plan.KnowledgeID, plan.TenantID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			_ = m.db.WithContext(ctx).Model(&Plan{}).Where("id = ?", plan.ID).
				Update("state", PlanCancelled).Error
			continue
		}
		if err != nil {
			return err
		}
		if knowledge.KnowledgeBaseID != plan.KnowledgeBaseID ||
			knowledge.ProcessingGeneration != plan.ProcessingGeneration ||
			knowledge.ProcessingOwner != plan.ProcessingOwner {
			_ = m.db.WithContext(ctx).Model(&Plan{}).Where("id = ?", plan.ID).
				Update("state", PlanSuperseded).Error
			continue
		}
		if knowledge.ParseStatus != types.ParseStatusProcessing {
			if knowledge.ParseStatus == types.ParseStatusCancelled ||
				knowledge.ParseStatus == types.ParseStatusDeleting {
				_ = m.db.WithContext(ctx).Model(&Plan{}).Where("id = ?", plan.ID).
					Update("state", PlanCancelled).Error
			}
			continue
		}
		if plan.State == PlanFinalizing {
			if err := m.enqueueFinalize(plan); err != nil {
				logger.Warnf(ctx, "[document split] recover finalizer plan=%s: %v", plan.ID, err)
			}
			continue
		}
		// Re-publish queued wake-ups (stable TaskID conflict is success), then
		// admit more preparing parts up to the per-document window.
		var queued []*Part
		if err := m.db.WithContext(ctx).Where(
			"plan_id = ? AND state = ?", plan.ID, PartQueued,
		).Order("part_index ASC").Limit(m.config.PerDocumentWindow).Find(&queued).Error; err != nil {
			return err
		}
		for _, part := range queued {
			if err := m.enqueuePart(part); err != nil {
				logger.Warnf(ctx, "[document split] recover part plan=%s index=%d: %v",
					plan.ID, part.PartIndex, err)
			}
		}
		if err := m.DispatchPlan(ctx, plan.ID); err != nil {
			logger.Warnf(ctx, "[document split] dispatch plan=%s: %v", plan.ID, err)
		}
	}
	return nil
}

func (m *Manager) recoverSupersededLocalBootLeases(ctx context.Context, now time.Time) error {
	if m == nil || m.db == nil || m.ownerPrefix == "" {
		return nil
	}
	// Coordinator.Start has registered this exact boot before split recovery
	// starts. Its stable-identity fence proves another live boot with this
	// prefix cannot exist. Other replicas still observe the normal timeout.
	query := m.db.WithContext(ctx).Model(&Part{}).Where("state = ?", PartLeased)
	if m.instanceID != "" && m.bootID != "" {
		query = query.Where(
			"lease_instance_id = ? AND lease_boot_id <> '' AND lease_boot_id <> ?",
			m.instanceID,
			m.bootID,
		)
	} else {
		// SQLite-only deterministic fixtures predate orchestration identity.
		query = query.Where(
			"lease_owner LIKE ? AND lease_owner <> ?",
			m.ownerPrefix+"%",
			m.owner,
		)
	}
	return query.Updates(map[string]interface{}{
		"state": PartQueued, "lease_owner": "", "lease_instance_id": "",
		"lease_boot_id": "", "lease_until": nil,
		// Planned restarts are not business parse failures. LeaseEpoch still
		// advances on the next claim and fences any delayed old completion.
		"attempt":          gorm.Expr("CASE WHEN attempt > 0 THEN attempt - 1 ELSE 0 END"),
		"last_error":       "worker boot superseded; recovered",
		"last_progress_at": now, "updated_at": now,
		"version": gorm.Expr("version + 1"),
	}).Error
}

func (m *Manager) expiredPartTakeoverProven(ctx context.Context, part *Part) (bool, error) {
	if m == nil || part == nil {
		return false, nil
	}
	if m.queue == nil {
		// The no-coordinator mode is only used by the single-process SQLite
		// state-machine harness. Production construction always injects the
		// durable document coordinator and therefore fails closed.
		return m.db != nil && m.db.Dialector.Name() == "sqlite", nil
	}
	if part.LeaseInstanceID == "" || part.LeaseBootID == "" {
		return false, nil
	}
	if part.LeaseInstanceID == m.instanceID {
		// Same boot may still have a paused handler. A different boot is safe
		// only because Coordinator registration already replaced/fenced the
		// exact stable instance identity.
		return part.LeaseBootID != m.bootID, nil
	}
	return m.queue.ProveInstanceBootTermination(
		ctx,
		part.LeaseInstanceID,
		part.LeaseBootID,
	)
}

func (m *Manager) recoverExpiredLeases(ctx context.Context, now time.Time) error {
	var candidates []*Part
	if err := m.db.WithContext(ctx).Where(
		"state = ? AND lease_until < ?",
		PartLeased,
		now,
	).Order("lease_until ASC, plan_id ASC, part_index ASC").
		Limit(m.config.RecoveryBatchSize).
		Find(&candidates).Error; err != nil {
		return err
	}
	recoverable := make(map[string]*Part, len(candidates))
	for _, part := range candidates {
		proven, err := m.expiredPartTakeoverProven(ctx, part)
		if err != nil {
			return err
		}
		if proven {
			recoverable[part.ID] = part
		}
	}
	if len(recoverable) == 0 {
		return nil
	}
	ids := make([]string, 0, len(recoverable))
	for id := range recoverable {
		ids = append(ids, id)
	}

	// A crash on the final business attempt must not be returned to queued
	// forever: no future ClaimPart is allowed to spend a fourth attempt.
	// Resolve terminal expirations and their parent knowledge in the same
	// transaction; retryable expirations are made claimable again afterwards.
	return m.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var locked []*Part
		query := tx.Where(
			"id IN ? AND state = ? AND lease_until < ?",
			ids,
			PartLeased,
			now,
		).Order("plan_id ASC, part_index ASC")
		if tx.Dialector.Name() != "sqlite" {
			query = query.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"})
		}
		if err := query.Find(&locked).Error; err != nil {
			return err
		}
		terminal := make([]*Part, 0, len(locked))
		retryable := make([]*Part, 0, len(locked))
		for _, current := range locked {
			expected := recoverable[current.ID]
			if expected == nil ||
				current.LeaseEpoch != expected.LeaseEpoch ||
				current.LeaseOwner != expected.LeaseOwner ||
				current.LeaseInstanceID != expected.LeaseInstanceID ||
				current.LeaseBootID != expected.LeaseBootID {
				continue
			}
			if current.Attempt >= m.config.MaxRetry {
				terminal = append(terminal, current)
			} else {
				retryable = append(retryable, current)
			}
		}
		planParts := make(map[string][]int)
		if len(terminal) > 0 {
			terminalIDs := make([]string, 0, len(terminal))
			for _, part := range terminal {
				terminalIDs = append(terminalIDs, part.ID)
				planParts[part.PlanID] = append(planParts[part.PlanID], part.PartIndex)
			}
			if err := tx.Model(&Part{}).Where(
				"id IN ? AND state = ? AND lease_until < ? AND attempt >= ?",
				terminalIDs, PartLeased, now, m.config.MaxRetry,
			).Updates(map[string]interface{}{
				"state": PartFailed, "lease_owner": "", "lease_instance_id": "",
				"lease_boot_id": "", "lease_until": nil,
				"last_error":       "worker lease expired after retry budget was exhausted",
				"last_progress_at": now, "updated_at": now,
				"version": gorm.Expr("version + 1"),
			}).Error; err != nil {
				return err
			}
		}

		for planID, indexes := range planParts {
			var plan Plan
			planQuery := tx.Where("id = ?", planID)
			if tx.Dialector.Name() != "sqlite" {
				planQuery = planQuery.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			if err := planQuery.First(&plan).Error; err != nil {
				return err
			}
			if plan.State != PlanQueued && plan.State != PlanParsing {
				continue
			}
			var failed int64
			if err := tx.Model(&Part{}).Where(
				"plan_id = ? AND state = ?", planID, PartFailed,
			).Count(&failed).Error; err != nil {
				return err
			}
			message := fmt.Sprintf(
				"physical document part %d failed: worker lease expired after %d attempts",
				indexes[0]+1, m.config.MaxRetry,
			)
			if err := tx.Model(&Plan{}).Where(
				"id = ? AND state IN ?", planID, []PlanState{PlanQueued, PlanParsing},
			).Updates(map[string]interface{}{
				"state": PlanFailed, "failed_parts": int(failed),
				"last_error": message, "last_progress_at": now, "updated_at": now,
				"version": gorm.Expr("version + 1"),
			}).Error; err != nil {
				return err
			}
			if err := tx.Model(&types.Knowledge{}).Where(
				"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processing_owner = ? AND parse_status = ?",
				plan.KnowledgeID, plan.TenantID, plan.KnowledgeBaseID,
				plan.ProcessingGeneration, plan.ProcessingOwner,
				types.ParseStatusProcessing,
			).Updates(map[string]interface{}{
				"parse_status": types.ParseStatusFailed, "error_message": message,
				"pending_subtasks_count": 0, "processing_owner": "",
				"enrichment_status":  types.EnrichmentStatusNone,
				"wiki_status":        types.WikiStatusNone,
				"wiki_error_message": "",
				"updated_at":         now,
			}).Error; err != nil {
				return err
			}
		}

		if len(retryable) == 0 {
			return nil
		}
		retryableIDs := make([]string, 0, len(retryable))
		for _, part := range retryable {
			retryableIDs = append(retryableIDs, part.ID)
		}
		// A delivery still running with a live heartbeat or whose exact owner
		// was not proven terminated never matches this bounded CAS.
		return tx.Model(&Part{}).Where(
			"id IN ? AND state = ? AND lease_until < ? AND attempt < ?",
			retryableIDs, PartLeased, now, m.config.MaxRetry,
		).Updates(map[string]interface{}{
			"state": PartQueued, "lease_owner": "", "lease_instance_id": "",
			"lease_boot_id": "", "lease_until": nil,
			"last_error":       "worker lease expired; recovered",
			"last_progress_at": now, "updated_at": now,
			"version": gorm.Expr("version + 1"),
		}).Error
	})
}
