package corefanout

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/redis/go-redis/v9"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	defaultScanInterval = time.Minute
	defaultScanTimeout  = 30 * time.Second
	defaultBatchSize    = 200
	maximumBatchSize    = 1000
)

// Config bounds one recovery cycle. BatchSize is a database page size, not a
// whole-cycle cap: a cycle walks to the high-water mark it observed at start.
// If its timeout expires, the in-memory keyset cursor resumes there next time
// so a permanently malformed early row cannot starve later documents.
type Config struct {
	ScanInterval time.Duration
	ScanTimeout  time.Duration
	BatchSize    int
}

func DefaultConfig() Config {
	return Config{
		ScanInterval: defaultScanInterval,
		ScanTimeout:  defaultScanTimeout,
		BatchSize:    defaultBatchSize,
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
	if c.BatchSize <= 0 {
		c.BatchSize = defaults.BatchSize
	}
	if c.BatchSize > maximumBatchSize {
		c.BatchSize = maximumBatchSize
	}
	return c
}

// Recovery republishes a committed document's downstream fan-out when the
// core worker crashed after its database commit but before Redis accepted all
// tasks. It never changes document state or the persisted plan: malformed or
// mismatched rows remain visible for an operator to diagnose and repair.
type Recovery struct {
	db              *gorm.DB
	enqueuer        interfaces.TaskEnqueuer
	redisClient     *redis.Client
	completionStore processownership.DurableFanoutCompletionStore
	config          Config

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	// scanMu serializes explicit health-check scans with the periodic runner.
	// scanCursor/highWater survive a bounded timeout and advance after every
	// attempted row, including corrupt and temporarily failing rows.
	scanMu        sync.Mutex
	scanCursor    string
	scanHighWater string
}

// NewRecovery is the production DI constructor. A durable completion store is
// mandatory: Redis is only a reconstructable mirror and must never decide
// whether fan-in has completed.
func NewRecovery(
	db *gorm.DB,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	repository interfaces.KnowledgeRepository,
) (*Recovery, error) {
	completionStore, ok := repository.(processownership.DurableFanoutCompletionStore)
	if !ok || completionStore == nil {
		return nil, errors.New("core fanout recovery: knowledge repository lacks durable completion storage")
	}
	return NewRecoveryWithConfig(db, enqueuer, redisClient, completionStore, DefaultConfig()), nil
}

// NewRecoveryWithConfig is exposed for deterministic tests and deployments
// that need a different scan cadence without changing replay semantics.
func NewRecoveryWithConfig(
	db *gorm.DB,
	enqueuer interfaces.TaskEnqueuer,
	redisClient *redis.Client,
	completionStore processownership.DurableFanoutCompletionStore,
	config Config,
) *Recovery {
	return &Recovery{
		db:              db,
		enqueuer:        enqueuer,
		redisClient:     redisClient,
		completionStore: completionStore,
		config:          config.normalized(),
	}
}

// Start runs one bounded scan immediately and then periodically. Repeated or
// concurrent calls while the runner is active are no-ops.
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
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	r.mu.Unlock()

	go r.run(ctx, done)
}

// Stop cancels the runner and waits for the current bounded scan. It is safe
// to call repeatedly or concurrently.
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

func (r *Recovery) run(ctx context.Context, done chan struct{}) {
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
}

func (r *Recovery) runCycle(parent context.Context) {
	ctx, cancel := context.WithTimeout(parent, r.config.ScanTimeout)
	defer cancel()
	if err := r.recoverSafely(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[core fanout recovery] scan incomplete; durable plans remain for retry: %v", err)
	}
}

func (r *Recovery) recoverSafely(ctx context.Context) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("core fanout recovery panic: %v", recovered)
		}
	}()
	return r.RecoverNow(ctx)
}

func committedPlanCandidates(query *gorm.DB) *gorm.DB {
	return query.
		Where("parse_status = ?", types.ParseStatusProcessing).
		Where("COALESCE(processing_owner, '') = ''").
		Where("processed_at IS NOT NULL").
		Where("processing_fanout IS NOT NULL").
		Where("COALESCE(CAST(processing_fanout AS TEXT), '') <> ''").
		Where("deleted_at IS NULL")
}

func (r *Recovery) startHighWater(ctx context.Context) error {
	if r.scanHighWater != "" {
		return nil
	}
	var result struct {
		MaxID string `gorm:"column:max_id"`
	}
	if err := committedPlanCandidates(r.db.WithContext(ctx).Table("knowledges")).
		Select("COALESCE(MAX(id), '') AS max_id").
		Scan(&result).Error; err != nil {
		return fmt.Errorf("core fanout recovery: read scan high-water mark: %w", err)
	}
	r.scanHighWater = result.MaxID
	if r.scanHighWater == "" {
		r.scanCursor = ""
	}
	return nil
}

// candidateRow intentionally keeps processing_fanout as an opaque string.
// types.JSON.Scan validates JSON while scanning, which would make one corrupt
// row fail the entire page before recovery could log it and advance the
// starvation-proof cursor.
type candidateRow struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ParseStatus          string
	ProcessingGeneration string
	ProcessingOwner      string
	ProcessingFanout     string
	ProcessedAt          *time.Time
}

func (r candidateRow) knowledge() *types.Knowledge {
	return &types.Knowledge{
		ID:                   r.ID,
		TenantID:             r.TenantID,
		KnowledgeBaseID:      r.KnowledgeBaseID,
		ParseStatus:          r.ParseStatus,
		ProcessingGeneration: r.ProcessingGeneration,
		ProcessingOwner:      r.ProcessingOwner,
		ProcessingFanout:     types.JSON([]byte(r.ProcessingFanout)),
		ProcessedAt:          r.ProcessedAt,
	}
}

func (r *Recovery) readPage(ctx context.Context) ([]candidateRow, error) {
	var rows []candidateRow
	query := committedPlanCandidates(r.db.WithContext(ctx).Table("knowledges")).
		Select("id, tenant_id, knowledge_base_id, parse_status, processing_generation, processing_owner, processing_fanout, processed_at, deleted_at").
		Where("id > ? AND id <= ?", r.scanCursor, r.scanHighWater).
		Order("id ASC").
		Limit(r.config.BatchSize)
	if err := query.Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("core fanout recovery: read candidate page after %q: %w", r.scanCursor, err)
	}
	return rows, nil
}

// RecoverNow walks one scan snapshot synchronously. It attempts every row,
// joins errors, and advances the keyset cursor even for malformed plans or
// partial enqueue failures. The cursor resets only after reaching that scan's
// high-water mark; new candidates are picked up by the following cycle.
func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil || r.db == nil || r.enqueuer == nil || r.completionStore == nil {
		return errors.New("core fanout recovery: dependencies are unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.scanMu.Lock()
	defer r.scanMu.Unlock()

	if err := r.startHighWater(ctx); err != nil {
		return err
	}
	if r.scanHighWater == "" {
		return nil
	}

	var errs []error
	attempted := 0
	published := 0
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(errs...), fmt.Errorf("core fanout recovery: scan canceled: %w", err))
		}
		rows, err := r.readPage(ctx)
		if err != nil {
			return errors.Join(errors.Join(errs...), err)
		}
		if len(rows) == 0 {
			r.scanCursor, r.scanHighWater = "", ""
			break
		}

		for i := range rows {
			if err := ctx.Err(); err != nil {
				return errors.Join(errors.Join(errs...), fmt.Errorf("core fanout recovery: scan canceled: %w", err))
			}
			row := &rows[i]
			knowledge := row.knowledge()
			attempted++
			didPublish, recoverErr := r.recoverOne(ctx, knowledge)
			// Advance after this exact row regardless of its result. A malformed
			// plan remains durable and will be retried after the cursor wraps.
			r.scanCursor = row.ID
			if recoverErr != nil {
				errs = append(errs, fmt.Errorf(
					"tenant=%d knowledge_base=%q knowledge=%q generation=%q: %w",
					knowledge.TenantID, knowledge.KnowledgeBaseID, knowledge.ID,
					knowledge.ProcessingGeneration, recoverErr,
				))
			} else if didPublish {
				published++
			}
		}

		if r.scanCursor == r.scanHighWater {
			r.scanCursor, r.scanHighWater = "", ""
			break
		}
	}

	if attempted > 0 {
		logger.Infof(ctx,
			"[core fanout recovery] scanned=%d replayed=%d failed=%d",
			attempted, published, len(errs))
	}
	return errors.Join(errs...)
}

// recoverOne first validates the scan snapshot for useful diagnostics, then
// repeats every identity predicate while holding the active KB and knowledge
// row locks. The external enqueue occurs before those locks are released, so
// delete/cancel/reparse either wins first (zero publication) or observes the
// stable tasks and fences them by generation afterward.
func (r *Recovery) recoverOne(ctx context.Context, candidate *types.Knowledge) (bool, error) {
	if _, err := ParseExact(candidate); err != nil {
		return false, err
	}
	published := false
	err := kbwritefence.WithActive(
		ctx,
		r.db,
		candidate.TenantID,
		candidate.KnowledgeBaseID,
		func(tx *gorm.DB) error {
			query := committedPlanCandidates(tx.Table("knowledges")).
				Select("id, tenant_id, knowledge_base_id, parse_status, processing_generation, processing_owner, processing_fanout, processed_at").
				Where(
					"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ?",
					candidate.TenantID,
					candidate.ID,
					candidate.KnowledgeBaseID,
					candidate.ProcessingGeneration,
				)
			if tx.Dialector.Name() != "sqlite" {
				query = query.Clauses(clause.Locking{Strength: "UPDATE"})
			}
			var persisted candidateRow
			if err := query.Take(&persisted).Error; err != nil {
				if errors.Is(err, gorm.ErrRecordNotFound) {
					return nil
				}
				return fmt.Errorf("recheck committed fanout row: %w", err)
			}
			current := persisted.knowledge()
			if _, err := ParseExact(current); err != nil {
				return fmt.Errorf("recheck committed fanout plan: %w", err)
			}
			completionStore := r.completionStore
			if tx.Dialector.Name() == "sqlite" {
				// Lite deliberately has a one-connection pool, so a base-repository
				// ledger read from inside this transaction would self-deadlock.
				completionStore = &transactionCompletionStore{tx: tx}
			}
			if err := Replay(ctx, r.enqueuer, r.redisClient, completionStore, current); err != nil {
				return fmt.Errorf("dispatch durable fanout: %w", err)
			}
			published = true
			return nil
		},
	)
	if errors.Is(err, kbwritefence.ErrKnowledgeBaseUnavailable) {
		// Missing/tombstoned parents are terminal for publication. KB deletion
		// owns cleanup; preserving the child row until then is intentional.
		return false, nil
	}
	return published, err
}

// scanState is intentionally test-only visibility kept private to the module.
func (r *Recovery) scanState() (cursor, highWater string) {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	return strings.TrimSpace(r.scanCursor), strings.TrimSpace(r.scanHighWater)
}
