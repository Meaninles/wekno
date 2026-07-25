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
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/hibiken/asynq"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
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

// Recovery republishes missing asynq wake-up signals for durable Wiki work.
// It never mutates task_pending_ops. Multiple application replicas may run a
// Recovery concurrently: asynq.Unique coalesces their identical KB-scoped
// signals, and the Wiki worker's active lock remains the processing boundary.
type Recovery struct {
	db         *gorm.DB
	enqueuer   interfaces.TaskEnqueuer
	activeKeys activeKeyChecker
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
	ID       int64  `gorm:"column:id"`
	TenantID uint64 `gorm:"column:tenant_id"`
	ScopeID  string `gorm:"column:scope_id"`
	DedupKey string `gorm:"column:dedup_key"`
	Payload  []byte `gorm:"column:payload"`
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
// The method is read-only with respect to task_pending_ops.
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

// recoverPendingMaps republishes document-generation wake-ups whose durable
// Map output is not ready yet. Multiple replicas may scan the same rows:
// asynq.Unique coalesces identical payloads and the Map handler owns a second,
// renewable per-document lease for correctness after uniqueness expiry.
func (r *Recovery) recoverPendingMaps(ctx context.Context) (int, []error) {
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
		asynq.Queue(types.QueueLow),
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
