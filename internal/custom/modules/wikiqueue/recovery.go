// Package wikiqueue contains the production safety net for WeKnora's durable
// Wiki pending-op queue.
//
// A Wiki operation is durable once it is written to task_pending_ops, while an
// asynq wiki:ingest task is only a wake-up signal. If Redis is unavailable at
// the moment the row is inserted, that wake-up can fail even though the work
// itself is safely stored in Postgres. Recovery periodically republishes one
// stable, KB-scoped wake-up signal for every non-empty Wiki queue.
package wikiqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	wikiTaskScope       = types.TaskScopeKnowledgeBase
	wikiActiveKeyPrefix = "wiki:active:"

	defaultScanInterval = time.Minute
	defaultScanTimeout  = 15 * time.Second
	defaultProcessDelay = time.Second
	// A recovery signal uses a longer uniqueness lease than an upload's
	// short debounce signal. If Redis loses the worker's active marker, this
	// still caps duplicate lock-conflict tasks at one per 15 minutes. A stale
	// uniqueness lease can delay a missing wake-up by at most UniqueTTL plus
	// one ScanInterval (about 16 minutes with defaults).
	defaultUniqueTTL   = 15 * time.Minute
	defaultTaskTimeout = 2 * time.Hour
	defaultMaxRetry    = 10
	defaultMapLimit    = 1000
)

var errMapDispatchWindowFull = errors.New("wiki Map dispatch window is full")

// Config controls the recovery loop. The defaults intentionally mirror the
// primary Wiki enqueue path's low queue, ten retries, and two-hour worker
// deadline. Recovery uses a longer 15-minute uniqueness lease to prevent a
// missing active marker from flooding a long-running batch. ScanInterval and
// ScanTimeout only affect the recovery loop itself.
type Config struct {
	ScanInterval time.Duration
	ScanTimeout  time.Duration
	ProcessDelay time.Duration
	UniqueTTL    time.Duration
	TaskTimeout  time.Duration
	MaxRetry     int
}

// DefaultConfig returns production settings for the Wiki trigger recovery
// loop.
func DefaultConfig() Config {
	return Config{
		ScanInterval: defaultScanInterval,
		ScanTimeout:  defaultScanTimeout,
		ProcessDelay: defaultProcessDelay,
		UniqueTTL:    defaultUniqueTTL,
		TaskTimeout:  defaultTaskTimeout,
		MaxRetry:     defaultMaxRetry,
	}
}

func (c Config) normalized() Config {
	defaults := DefaultConfig()
	if c.ScanInterval <= 0 {
		c.ScanInterval = defaults.ScanInterval
	}
	if c.ScanTimeout <= 0 {
		c.ScanTimeout = defaults.ScanTimeout
	}
	if c.ProcessDelay <= 0 {
		c.ProcessDelay = defaults.ProcessDelay
	}
	if c.UniqueTTL <= 0 {
		c.UniqueTTL = defaults.UniqueTTL
	}
	if c.TaskTimeout <= 0 {
		c.TaskTimeout = defaults.TaskTimeout
	}
	if c.MaxRetry <= 0 {
		c.MaxRetry = defaults.MaxRetry
	}
	return c
}

// Recovery republishes missing KB-control signals and dispatches a bounded
// number of document Map wake-ups from the PostgreSQL outbox. Multiple
// replicas may scan concurrently: KB signals are coalesced by Asynq, while
// Map publication is fenced by a database epoch, lease and pool advisory lock.
type Recovery struct {
	db         *gorm.DB
	enqueuer   interfaces.TaskEnqueuer
	activeKeys activeKeyChecker
	admission  *modeladmission.Manager
	config     Config

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
}

type activeKeyChecker interface {
	Exists(ctx context.Context, keys ...string) *redis.IntCmd
}

// NewRecovery constructs a recovery loop with production defaults.
// redisClient may be nil so the same module remains usable in Lite mode.
func NewRecovery(db *gorm.DB, enqueuer interfaces.TaskEnqueuer, redisClient *redis.Client) *Recovery {
	return NewRecoveryWithConfig(db, enqueuer, redisClient, DefaultConfig())
}

// NewRecoveryWithAdmission is the production constructor. Focused queue tests
// keep using NewRecovery and exercise the legacy recovery-only path without a
// control-plane database; the runtime always uses this constructor so Wiki Map
// publication is bounded by the same model resource-pool policy as derivative
// work.
func NewRecoveryWithAdmission(
	db *gorm.DB,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	admission *modeladmission.Manager,
) *Recovery {
	recovery := NewRecovery(db, enqueuer, redisClient)
	recovery.admission = admission
	return recovery
}

// NewRecoveryWithConfig is primarily useful for deterministic tests and for
// an operator who needs to tune the scan cadence without changing queue
// semantics.
func NewRecoveryWithConfig(
	db *gorm.DB,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	config Config,
) *Recovery {
	recovery := &Recovery{
		db:       db,
		enqueuer: enqueuer,
		config:   config.normalized(),
	}
	if redisClient != nil {
		recovery.activeKeys = redisClient
	}
	return recovery
}

// Start begins the recovery loop. The first scan runs immediately; later
// scans run once per ScanInterval. Repeated or concurrent Start calls while
// the loop is running are no-ops.
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
	runCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	r.cancel = cancel
	r.done = done
	r.mu.Unlock()

	go r.run(runCtx, done)
}

// Stop cancels the recovery loop and waits for its current bounded scan to
// finish. Repeated or concurrent Stop calls are safe.
func (r *Recovery) Stop() {
	if r == nil {
		return
	}

	r.mu.Lock()
	cancel := r.cancel
	done := r.done
	r.mu.Unlock()
	if cancel == nil || done == nil {
		return
	}

	cancel()
	<-done
}

func (r *Recovery) run(ctx context.Context, done chan struct{}) {
	defer func() {
		close(done)
		r.mu.Lock()
		if r.done == done {
			r.cancel = nil
			r.done = nil
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
}

func (r *Recovery) runCycle(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, r.config.ScanTimeout)
	defer cancel()

	if err := r.recoverSafely(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[wiki queue recovery] scan failed; durable rows remain for the next scan: %v", err)
	}
}

func (r *Recovery) recoverSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("wiki queue recovery panic: %v", recovered)
		}
	}()
	return r.RecoverNow(ctx)
}

type pendingScope struct {
	TenantID uint64 `gorm:"column:tenant_id"`
	ScopeID  string `gorm:"column:scope_id"`
}

type pendingMapRow struct {
	ID                    int64      `gorm:"column:id"`
	TenantID              uint64     `gorm:"column:tenant_id"`
	ScopeID               string     `gorm:"column:scope_id"`
	DedupKey              string     `gorm:"column:dedup_key"`
	Payload               []byte     `gorm:"column:payload"`
	MapResourcePoolID     string     `gorm:"column:map_resource_pool_id"`
	MapDispatchEpoch      uint64     `gorm:"column:map_dispatch_epoch"`
	MapDispatchLeaseUntil *time.Time `gorm:"column:map_dispatch_lease_until"`
}

type pendingMapDocument struct {
	KnowledgeID          string `json:"knowledge_id,omitempty"`
	ProcessingGeneration string `json:"processing_generation,omitempty"`
}

type mapTriggerPayload struct {
	TenantID             uint64 `json:"tenant_id"`
	KnowledgeBaseID      string `json:"knowledge_base_id"`
	TaskMode             string `json:"task_mode"`
	MapDedupKey          string `json:"map_dedup_key"`
	MapDispatchEpoch     uint64 `json:"map_dispatch_epoch,omitempty"`
	KnowledgeID          string `json:"knowledge_id,omitempty"`
	ProcessingGeneration string `json:"processing_generation,omitempty"`
}

// triggerPayload deliberately contains only stable queue identity. Request
// tracing and locale belong to individual task_pending_ops rows; including
// either here would defeat asynq.Unique coalescing during bulk uploads.
type triggerPayload struct {
	TenantID        uint64 `json:"tenant_id"`
	KnowledgeBaseID string `json:"knowledge_base_id"`
}

// RecoverNow performs one scan synchronously. It is exported for health
// checks and tests. Every matching KB is attempted even when another enqueue
// fails, and errors are joined so callers retain all failed queue identities.
// In production the Map lane reserves dispatch epochs in task_pending_ops;
// the KB-control lane remains read-only.
func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil {
		return errors.New("wiki queue recovery: receiver is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if r.db == nil {
		return errors.New("wiki queue recovery: database is nil")
	}
	if r.enqueuer == nil {
		return errors.New("wiki queue recovery: task enqueuer is nil")
	}

	var scopes []pendingScope
	err := r.db.WithContext(ctx).
		Table("task_pending_ops").
		Select("tenant_id, scope_id").
		Where("task_type = ? AND scope = ?", types.TypeWikiIngest, wikiTaskScope).
		Group("tenant_id, scope_id").
		Order("tenant_id ASC, scope_id ASC").
		Scan(&scopes).Error
	if err != nil {
		return fmt.Errorf("wiki queue recovery: list pending scopes: %w", err)
	}

	var enqueueErrors []error
	recovered := 0
	active := 0
	for _, scope := range scopes {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(enqueueErrors...), fmt.Errorf("wiki queue recovery: scan canceled: %w", err))
		}
		isActive, activeErr := r.isActive(ctx, scope.ScopeID)
		if activeErr != nil {
			if err := ctx.Err(); err != nil {
				return errors.Join(errors.Join(enqueueErrors...), fmt.Errorf("wiki queue recovery: scan canceled: %w", err))
			}
			// Redis also backs asynq, but a partial/network-path failure may
			// affect EXISTS while enqueue still works. Preserve the diagnostic
			// and attempt the durable wake-up instead of treating uncertainty as
			// evidence that no work is needed.
			enqueueErrors = append(enqueueErrors, fmt.Errorf(
				"tenant=%d knowledge_base=%q: check active worker: %w",
				scope.TenantID,
				scope.ScopeID,
				activeErr,
			))
		} else if isActive {
			active++
			continue
		}

		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(enqueueErrors...), fmt.Errorf("wiki queue recovery: scan canceled: %w", err))
		}
		published, err := r.enqueueTriggerIfActive(ctx, scope)
		if err != nil {
			enqueueErrors = append(enqueueErrors, fmt.Errorf(
				"tenant=%d knowledge_base=%q: %w",
				scope.TenantID,
				scope.ScopeID,
				err,
			))
			continue
		}
		if published {
			recovered++
		}
	}
	mapRecovered, mapErrors := r.recoverPendingMaps(ctx)
	enqueueErrors = append(enqueueErrors, mapErrors...)

	if recovered > 0 {
		logger.Infof(ctx, "[wiki queue recovery] ensured triggers for %d/%d pending knowledge bases", recovered, len(scopes))
	}
	if mapRecovered > 0 {
		logger.Infof(ctx, "[wiki queue recovery] ensured %d distributed Wiki Map wake-ups", mapRecovered)
	}
	if active > 0 {
		logger.Debugf(ctx, "[wiki queue recovery] skipped %d active knowledge-base workers", active)
	}
	return errors.Join(enqueueErrors...)
}

// recoverPendingMaps dispatches document-generation wake-ups whose durable
// Map output is not ready yet. Production uses database epochs and leases;
// the admission-free branch is retained only by focused recovery tests.
func (r *Recovery) recoverPendingMaps(ctx context.Context) (int, []error) {
	if r.admission != nil {
		dispatched, err := r.DispatchMaps(ctx, defaultMapLimit)
		if err != nil {
			return dispatched, []error{err}
		}
		return dispatched, nil
	}
	var rows []pendingMapRow
	err := r.db.WithContext(ctx).
		Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload").
		Where(
			"task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL",
			types.TypeWikiIngest,
			wikiTaskScope,
			"ingest",
		).
		Order("id ASC").
		Limit(defaultMapLimit).
		Scan(&rows).Error
	if err != nil {
		return 0, []error{fmt.Errorf("wiki queue recovery: list pending distributed Maps: %w", err)}
	}

	recovered := 0
	var errs []error
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("wiki queue recovery: Map scan canceled: %w", err))
			break
		}
		var document pendingMapDocument
		if err := json.Unmarshal(row.Payload, &document); err != nil {
			// Still publish the exact durable key. The worker will account the
			// malformed row against its bounded per-op retry/dead-letter budget
			// instead of letting it become an invisible queue head.
			document = pendingMapDocument{}
		}
		payload, err := json.Marshal(mapTriggerPayload{
			TenantID:             row.TenantID,
			KnowledgeBaseID:      row.ScopeID,
			TaskMode:             "map",
			MapDedupKey:          row.DedupKey,
			KnowledgeID:          document.KnowledgeID,
			ProcessingGeneration: document.ProcessingGeneration,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("wiki queue recovery: marshal Map row %d: %w", row.ID, err))
			continue
		}
		published, err := r.enqueueMapIfActive(ctx, row, payload)
		if err == nil && published {
			recovered++
			continue
		}
		if err == nil {
			continue
		}
		errs = append(errs, fmt.Errorf(
			"wiki queue recovery: enqueue Map row %d tenant=%d kb=%q: %w",
			row.ID, row.TenantID, row.ScopeID, err,
		))
	}
	return recovered, errs
}

type mapDispatchSetting struct {
	total int
	share int
	lease time.Duration
	err   error
}

// DispatchMaps is the latency-sensitive production Wiki Map dispatcher. It
// turns a globally bounded, weighted share of PostgreSQL rows per resource
// pool into disposable Asynq wake-ups. Idle derivative capacity is borrowable;
// the database epoch/lease is the durable publication proof and Redis contains
// no unbounded copy of the Wiki backlog.
func (r *Recovery) DispatchMaps(ctx context.Context, limit int) (int, error) {
	if r == nil || r.db == nil || r.enqueuer == nil || r.admission == nil {
		return 0, nil
	}
	if limit < 1 || limit > defaultMapLimit {
		limit = defaultMapLimit
	}
	now := time.Now().UTC()
	var rows []pendingMapRow
	if err := r.db.WithContext(ctx).
		Table("task_pending_ops").
		Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
		Where(
			"task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL",
			types.TypeWikiIngest, wikiTaskScope, "ingest",
		).
		Where("(next_attempt_at IS NULL OR next_attempt_at <= ?)", now).
		Where("(map_dispatch_lease_until IS NULL OR map_dispatch_lease_until <= ?)", now).
		Order("COALESCE(next_attempt_at, enqueued_at) ASC, fail_count ASC, id ASC").
		Limit(limit).
		Scan(&rows).Error; err != nil {
		return 0, fmt.Errorf("wiki Map dispatcher: list due rows: %w", err)
	}

	poolByScope := make(map[string]string)
	settings := make(map[string]mapDispatchSetting)
	fullPools := make(map[string]struct{})
	dispatched := 0
	var errs []error
	for _, candidate := range rows {
		if err := ctx.Err(); err != nil {
			errs = append(errs, err)
			break
		}
		scopeKey := fmt.Sprintf("%d\x00%s", candidate.TenantID, candidate.ScopeID)
		poolID := poolByScope[scopeKey]
		if poolID == "" {
			resolved, err := r.resolveMapResourcePool(ctx, candidate)
			if err != nil {
				errs = append(errs, fmt.Errorf(
					"wiki Map dispatcher: resolve row %d route: %w", candidate.ID, err,
				))
				continue
			}
			poolID = resolved
			poolByScope[scopeKey] = poolID
		}
		if _, full := fullPools[poolID]; full {
			continue
		}
		// Publish the resolved route even when this scan cannot reserve a slot.
		// The sibling derivative dispatcher can then see queued Wiki demand and
		// protect the configured weighted share under the common pool lock.
		if err := r.db.WithContext(ctx).Table("task_pending_ops").
			Where("id = ? AND map_ready_at IS NULL", candidate.ID).
			Update("map_resource_pool_id", poolID).Error; err != nil {
			errs = append(errs, fmt.Errorf(
				"wiki Map dispatcher: persist row %d route: %w", candidate.ID, err,
			))
			continue
		}
		candidate.MapResourcePoolID = poolID
		setting, ok := settings[poolID]
		if !ok {
			setting.total, setting.share, setting.lease, setting.err = r.admission.DispatchLimits(
				ctx, poolID, modeladmission.WorkLaneWiki,
			)
			settings[poolID] = setting
		}
		if setting.err != nil {
			errs = append(errs, fmt.Errorf(
				"wiki Map dispatcher: resolve pool %s window: %w", poolID, setting.err,
			))
			fullPools[poolID] = struct{}{}
			continue
		}
		reserved, reserveErr := r.reserveMapDispatch(
			ctx, candidate, poolID, setting.total, setting.share, setting.lease,
		)
		if errors.Is(reserveErr, errMapDispatchWindowFull) {
			fullPools[poolID] = struct{}{}
			continue
		}
		if errors.Is(reserveErr, gorm.ErrRecordNotFound) {
			continue
		}
		if reserveErr != nil {
			errs = append(errs, fmt.Errorf(
				"wiki Map dispatcher: reserve row %d: %w", candidate.ID, reserveErr,
			))
			continue
		}
		if err := r.enqueueReservedMap(reserved); err != nil {
			_ = r.releaseMapDispatchReservation(
				context.WithoutCancel(ctx), reserved.ID, reserved.MapDispatchEpoch,
			)
			errs = append(errs, fmt.Errorf(
				"wiki Map dispatcher: publish row %d: %w", reserved.ID, err,
			))
			continue
		}
		dispatched++
	}
	return dispatched, errors.Join(errs...)
}

func (r *Recovery) resolveMapResourcePool(
	ctx context.Context,
	row pendingMapRow,
) (string, error) {
	var kb struct {
		TenantID          uint64
		DerivativeModelID string
	}
	if err := r.db.WithContext(ctx).
		Table("knowledge_bases").
		Select("tenant_id, derivative_model_id").
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", row.ScopeID, row.TenantID).
		Take(&kb).Error; err != nil {
		return "", err
	}
	modelID := strings.TrimSpace(kb.DerivativeModelID)
	if modelID == "" {
		var config struct{ DefaultModelID string }
		if err := r.db.WithContext(ctx).
			Table("custom_derivative_control_configs").
			Select("default_model_id").Where("id = 1").Take(&config).Error; err != nil {
			return "", err
		}
		modelID = strings.TrimSpace(config.DefaultModelID)
	}
	if modelID == "" {
		return "", errors.New("no derivative model is configured")
	}
	var assignment struct {
		ModelID       string
		ModelTenantID uint64
	}
	if err := r.db.WithContext(ctx).
		Table("custom_derivative_model_assignments").
		Select("model_id, model_tenant_id").Where("model_id = ?", modelID).
		Take(&assignment).Error; err != nil {
		return "", err
	}
	var model types.Model
	if err := r.db.WithContext(ctx).
		Where("id = ? AND tenant_id = ? AND deleted_at IS NULL", assignment.ModelID, assignment.ModelTenantID).
		Take(&model).Error; err != nil {
		return "", err
	}
	policy, err := r.admission.ResolvePolicy(
		ctx, modeladmission.SpecForModel(modeladmission.KindDerivative, &model, ""),
	)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(policy.PoolID) == "" {
		return "", errors.New("resolved derivative resource pool is empty")
	}
	return policy.PoolID, nil
}

func wikiMapDispatchTaskID(rowID int64, epoch uint64) string {
	return fmt.Sprintf("wiki-map:%d:%d", rowID, epoch)
}

func (r *Recovery) reserveMapDispatch(
	ctx context.Context,
	candidate pendingMapRow,
	poolID string,
	totalWindow int,
	laneShare int,
	lease time.Duration,
) (pendingMapRow, error) {
	if totalWindow < 1 {
		return pendingMapRow{}, errMapDispatchWindowFull
	}
	if lease <= 0 {
		lease = 2 * time.Minute
	}
	now := time.Now().UTC()
	var reserved pendingMapRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if tx.Dialector.Name() == "postgres" {
			if err := tx.Exec(
				"SELECT pg_advisory_xact_lock(?)",
				modeladmission.DispatchAdvisoryKey(poolID),
			).Error; err != nil {
				return err
			}
		}
		var wikiActive int64
		if err := tx.Table("task_pending_ops").
			Where(
				"task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL AND map_resource_pool_id = ? AND map_dispatch_lease_until > ?",
				types.TypeWikiIngest, wikiTaskScope, "ingest", poolID, now,
			).
			Count(&wikiActive).Error; err != nil {
			return err
		}
		derivativeActive := int64(0)
		derivativeDemand := int64(0)
		if tx.Migrator().HasTable("custom_derivative_work_items") {
			if err := tx.Table("custom_derivative_work_items").Where(
				`resource_pool_id = ? AND work_kind <> ? AND (
					state IN ? OR (
						state IN ? AND dispatch_lease_until IS NOT NULL AND dispatch_lease_until > ?
					)
				)`,
				poolID, "finalizer",
				[]string{"leased", "admitted", "provider_running", "provider_succeeded", "materializing"},
				[]string{"queued", "retry_wait"}, now,
			).Count(&derivativeActive).Error; err != nil {
				return err
			}
			if err := tx.Table("custom_derivative_work_items").Where(
				"resource_pool_id = ? AND work_kind <> ? AND state IN ? AND next_attempt_at <= ? AND (dispatch_lease_until IS NULL OR dispatch_lease_until <= ?)",
				poolID, "finalizer", []string{"queued", "retry_wait"}, now, now,
			).Count(&derivativeDemand).Error; err != nil {
				return err
			}
		}
		if wikiActive+derivativeActive >= int64(totalWindow) {
			return errMapDispatchWindowFull
		}
		if laneShare > 0 && wikiActive >= int64(laneShare) && derivativeDemand > 0 {
			return errMapDispatchWindowFull
		}
		if err := tx.Table("task_pending_ops").
			Select("id, tenant_id, scope_id, dedup_key, payload, map_resource_pool_id, map_dispatch_epoch, map_dispatch_lease_until").
			Where(
				"id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND op = ? AND map_ready_at IS NULL",
				candidate.ID, candidate.TenantID, types.TypeWikiIngest, wikiTaskScope, "ingest",
			).
			Where("(next_attempt_at IS NULL OR next_attempt_at <= ?)", now).
			Where("(map_dispatch_lease_until IS NULL OR map_dispatch_lease_until <= ?)", now).
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Take(&reserved).Error; err != nil {
			return err
		}
		reserved.MapDispatchEpoch++
		reserved.MapResourcePoolID = poolID
		reserved.MapDispatchLeaseUntil = ptrTime(now.Add(lease))
		taskID := wikiMapDispatchTaskID(reserved.ID, reserved.MapDispatchEpoch)
		result := tx.Table("task_pending_ops").Where(
			"id = ? AND map_dispatch_epoch = ?",
			reserved.ID, reserved.MapDispatchEpoch-1,
		).Updates(map[string]any{
			"map_resource_pool_id":     poolID,
			"map_dispatch_epoch":       reserved.MapDispatchEpoch,
			"map_dispatch_task_id":     taskID,
			"map_dispatch_lease_until": *reserved.MapDispatchLeaseUntil,
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
	return reserved, err
}

func ptrTime(value time.Time) *time.Time { return &value }

func (r *Recovery) enqueueReservedMap(row pendingMapRow) error {
	var document pendingMapDocument
	if err := json.Unmarshal(row.Payload, &document); err != nil {
		document = pendingMapDocument{}
	}
	payload, err := json.Marshal(mapTriggerPayload{
		TenantID:             row.TenantID,
		KnowledgeBaseID:      row.ScopeID,
		TaskMode:             "map",
		MapDedupKey:          row.DedupKey,
		MapDispatchEpoch:     row.MapDispatchEpoch,
		KnowledgeID:          document.KnowledgeID,
		ProcessingGeneration: document.ProcessingGeneration,
	})
	if err != nil {
		return err
	}
	_, err = r.enqueuer.Enqueue(
		asynq.NewTask(types.TypeWikiIngest, payload),
		asynq.Queue(types.QueueWikiMap),
		asynq.TaskID(wikiMapDispatchTaskID(row.ID, row.MapDispatchEpoch)),
		asynq.MaxRetry(0),
		asynq.Timeout(r.config.TaskTimeout),
		asynq.ProcessIn(r.config.ProcessDelay),
	)
	if errors.Is(err, asynq.ErrTaskIDConflict) {
		return nil
	}
	return err
}

func (r *Recovery) releaseMapDispatchReservation(
	ctx context.Context,
	rowID int64,
	epoch uint64,
) error {
	return r.db.WithContext(ctx).Table("task_pending_ops").
		Where("id = ? AND map_dispatch_epoch = ?", rowID, epoch).
		Updates(map[string]any{
			"map_dispatch_task_id":     "",
			"map_dispatch_lease_until": nil,
		}).Error
}

func (r *Recovery) enqueueMapIfActive(
	ctx context.Context,
	row pendingMapRow,
	payload []byte,
) (bool, error) {
	published := false
	err := kbwritefence.WithActive(ctx, r.db, row.TenantID, row.ScopeID, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Table("task_pending_ops").
			Where(
				"id = ? AND tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND map_ready_at IS NULL",
				row.ID,
				row.TenantID,
				types.TypeWikiIngest,
				wikiTaskScope,
				row.ScopeID,
				"ingest",
			).
			Count(&count).Error; err != nil {
			return fmt.Errorf("recheck distributed Map row: %w", err)
		}
		if count == 0 {
			return nil
		}
		_, err := r.enqueuer.Enqueue(
			asynq.NewTask(types.TypeWikiIngest, payload),
			asynq.Queue(types.QueueWikiMap),
			asynq.MaxRetry(r.config.MaxRetry),
			asynq.Timeout(r.config.TaskTimeout),
			asynq.ProcessIn(r.config.ProcessDelay),
			asynq.Unique(30*time.Minute),
		)
		if err != nil && !errors.Is(err, asynq.ErrDuplicateTask) {
			return err
		}
		published = true
		return nil
	})
	if errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) {
		return false, nil
	}
	return published, err
}

// enqueueTriggerIfActive closes the scan-to-publication race with whole-KB
// deletion. A recovery cycle may have loaded a pending scope before deletion
// purged it; taking the same parent lock as KB deletion and rechecking both
// parent liveness and durable work ensures it cannot publish after final
// completion. If publication wins the lock ordering, the delete worker's
// subsequent queue barrier observes and cancels the signal.
func (r *Recovery) enqueueTriggerIfActive(ctx context.Context, scope pendingScope) (bool, error) {
	published := false
	err := kbwritefence.WithActive(ctx, r.db, scope.TenantID, scope.ScopeID, func(tx *gorm.DB) error {
		var count int64
		if err := tx.Table("task_pending_ops").
			Where("tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ?",
				scope.TenantID, types.TypeWikiIngest, wikiTaskScope, scope.ScopeID).
			Count(&count).Error; err != nil {
			return fmt.Errorf("recheck durable Wiki work: %w", err)
		}
		if count == 0 {
			return nil
		}
		if err := r.enqueueTrigger(scope); err != nil {
			return err
		}
		published = true
		return nil
	})
	if errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) {
		// A missing/tombstoned parent is terminal for recovery publication. Its
		// deletion coordinator owns any remaining cleanup rows and purge.
		return false, nil
	}
	return published, err
}

func (r *Recovery) isActive(ctx context.Context, knowledgeBaseID string) (bool, error) {
	if r.activeKeys == nil {
		return false, nil
	}
	count, err := r.activeKeys.Exists(ctx, wikiActiveKeyPrefix+knowledgeBaseID).Result()
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (r *Recovery) enqueueTrigger(scope pendingScope) error {
	payload, err := json.Marshal(triggerPayload{
		TenantID:        scope.TenantID,
		KnowledgeBaseID: scope.ScopeID,
	})
	if err != nil {
		return fmt.Errorf("marshal trigger payload: %w", err)
	}

	task := asynq.NewTask(types.TypeWikiIngest, payload)
	_, err = r.enqueuer.Enqueue(
		task,
		asynq.Queue(types.QueueWikiControl),
		asynq.MaxRetry(r.config.MaxRetry),
		asynq.Timeout(r.config.TaskTimeout),
		asynq.ProcessIn(r.config.ProcessDelay),
		asynq.Unique(r.config.UniqueTTL),
	)
	if err == nil || errors.Is(err, asynq.ErrDuplicateTask) {
		return nil
	}
	return fmt.Errorf("enqueue wiki trigger: %w", err)
}
