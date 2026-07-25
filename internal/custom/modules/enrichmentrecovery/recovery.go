// Package enrichmentrecovery rebuilds finalizing document fan-out from the
// exact plan persisted on the knowledge row. Redis is a wake-up transport;
// PostgreSQL remains the source of truth if Redis is restarted, flushed, or a
// worker dies between committing the plan and publishing all leaf tasks.
package enrichmentrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

const (
	defaultScanInterval = time.Minute
	defaultScanTimeout  = 30 * time.Second
	defaultStaleAfter   = 2 * time.Minute
	defaultBatchSize    = 200
	maximumBatchSize    = 1000
)

type Config struct {
	ScanInterval time.Duration
	ScanTimeout  time.Duration
	StaleAfter   time.Duration
	BatchSize    int
}

func DefaultConfig() Config {
	return Config{
		ScanInterval: defaultScanInterval,
		ScanTimeout:  defaultScanTimeout,
		StaleAfter:   defaultStaleAfter,
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
	if c.StaleAfter <= 0 {
		c.StaleAfter = defaults.StaleAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = defaults.BatchSize
	}
	if c.BatchSize > maximumBatchSize {
		c.BatchSize = maximumBatchSize
	}
	return c
}

type Recovery struct {
	db       *gorm.DB
	enqueuer interfaces.TaskEnqueuer
	config   Config

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}

	scanMu        sync.Mutex
	scanCursor    string
	scanHighWater string
}

func NewRecovery(db *gorm.DB, enqueuer interfaces.TaskEnqueuer) *Recovery {
	return NewRecoveryWithConfig(db, enqueuer, DefaultConfig())
}

func NewRecoveryWithConfig(
	db *gorm.DB,
	enqueuer interfaces.TaskEnqueuer,
	config Config,
) *Recovery {
	return &Recovery{db: db, enqueuer: enqueuer, config: config.normalized()}
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
	done := make(chan struct{})
	r.cancel, r.done = cancel, done
	r.mu.Unlock()
	go r.run(ctx, done)
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
	if err := r.RecoverNow(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[enrichment recovery] scan incomplete: %v", err)
	}
}

type candidateRow struct {
	ID                   string
	TenantID             uint64
	KnowledgeBaseID      string
	ProcessingGeneration string
	ProcessingFanout     string
	PendingSubtasksCount int
	UpdatedAt            time.Time
}

type planIdentity struct {
	Stage                string               `json:"stage"`
	Version              int                  `json:"version"`
	TenantID             uint64               `json:"tenant_id"`
	KnowledgeID          string               `json:"knowledge_id"`
	KnowledgeBaseID      string               `json:"knowledge_base_id"`
	ProcessingGeneration string               `json:"processing_generation"`
	Language             string               `json:"language,omitempty"`
	Attempt              int                  `json:"attempt,omitempty"`
	Tracing              types.TracingContext `json:"tracing,omitempty"`
}

func candidateQuery(db *gorm.DB, cutoff time.Time) *gorm.DB {
	return db.Table("knowledges").
		Where("parse_status = ?", types.ParseStatusFinalizing).
		Where("processing_generation <> ''").
		Where("processing_fanout IS NOT NULL").
		Where("COALESCE(CAST(processing_fanout AS TEXT), '') <> ''").
		Where("updated_at <= ?", cutoff).
		Where("deleted_at IS NULL")
}

func (r *Recovery) startHighWater(ctx context.Context, cutoff time.Time) error {
	if r.scanHighWater != "" {
		return nil
	}
	var result struct {
		MaxID string `gorm:"column:max_id"`
	}
	if err := candidateQuery(r.db.WithContext(ctx), cutoff).
		Select("COALESCE(MAX(id), '') AS max_id").
		Scan(&result).Error; err != nil {
		return fmt.Errorf("read enrichment recovery high-water mark: %w", err)
	}
	r.scanHighWater = result.MaxID
	if r.scanHighWater == "" {
		r.scanCursor = ""
	}
	return nil
}

func (r *Recovery) readPage(
	ctx context.Context,
	cutoff time.Time,
) ([]candidateRow, error) {
	var rows []candidateRow
	err := candidateQuery(r.db.WithContext(ctx), cutoff).
		Select(
			"id, tenant_id, knowledge_base_id, processing_generation, "+
				"CAST(processing_fanout AS TEXT) AS processing_fanout, "+
				"pending_subtasks_count, updated_at",
		).
		Where("id > ? AND id <= ?", r.scanCursor, r.scanHighWater).
		Order("id ASC").
		Limit(r.config.BatchSize).
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("read enrichment recovery page after %q: %w", r.scanCursor, err)
	}
	return rows, nil
}

func decodePayload(row candidateRow) (types.KnowledgePostProcessPayload, error) {
	var plan planIdentity
	if err := json.Unmarshal([]byte(row.ProcessingFanout), &plan); err != nil {
		return types.KnowledgePostProcessPayload{}, fmt.Errorf("decode durable enrichment plan: %w", err)
	}
	if plan.Stage != "enrichment" ||
		(plan.Version != 1 && plan.Version != 2 && plan.Version != 3) ||
		plan.TenantID == 0 ||
		plan.TenantID != row.TenantID ||
		plan.KnowledgeID != row.ID ||
		plan.KnowledgeBaseID != row.KnowledgeBaseID ||
		plan.ProcessingGeneration != row.ProcessingGeneration {
		return types.KnowledgePostProcessPayload{}, errors.New("durable enrichment plan identity mismatch")
	}
	return types.KnowledgePostProcessPayload{
		TracingContext:       plan.Tracing,
		TenantID:             plan.TenantID,
		KnowledgeID:          plan.KnowledgeID,
		KnowledgeBaseID:      plan.KnowledgeBaseID,
		ProcessingGeneration: plan.ProcessingGeneration,
		Language:             plan.Language,
		Attempt:              plan.Attempt,
	}, nil
}

func (r *Recovery) recoverOne(
	ctx context.Context,
	row candidateRow,
	now time.Time,
) (bool, error) {
	payload, err := decodePayload(row)
	if err != nil {
		return false, err
	}
	if err := processownership.EnqueuePostProcessContext(ctx, r.enqueuer, payload); err != nil {
		return false, fmt.Errorf("republish stable postprocess: %w", err)
	}
	// A successful enqueue or live stable-ID conflict is a recovery heartbeat.
	// The exact predicates prevent a stale scan from refreshing a new
	// generation or a row concurrently completed/cancelled by another worker.
	result := r.db.WithContext(ctx).Table("knowledges").Where(
		"tenant_id = ? AND id = ? AND knowledge_base_id = ? AND processing_generation = ? AND parse_status = ?",
		row.TenantID,
		row.ID,
		row.KnowledgeBaseID,
		row.ProcessingGeneration,
		types.ParseStatusFinalizing,
	).Update("updated_at", now)
	if result.Error != nil {
		return false, fmt.Errorf("record enrichment recovery heartbeat: %w", result.Error)
	}
	return result.RowsAffected == 1, nil
}

// RecoverNow walks one fixed high-water snapshot. It advances past malformed
// rows and joins their errors so one corrupt document cannot starve later IDs.
func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil || r.db == nil || r.enqueuer == nil {
		return errors.New("enrichment recovery dependencies are unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.scanMu.Lock()
	defer r.scanMu.Unlock()

	cutoff := time.Now().UTC().Add(-r.config.StaleAfter)
	if err := r.startHighWater(ctx, cutoff); err != nil {
		return err
	}
	if r.scanHighWater == "" {
		return nil
	}

	now := time.Now().UTC()
	attempted := 0
	replayed := 0
	var errs []error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(errors.Join(errs...), err)
		}
		rows, err := r.readPage(ctx, cutoff)
		if err != nil {
			return errors.Join(errors.Join(errs...), err)
		}
		if len(rows) == 0 {
			r.scanCursor, r.scanHighWater = "", ""
			break
		}
		for i := range rows {
			row := rows[i]
			attempted++
			ok, recoverErr := r.recoverOne(ctx, row, now)
			r.scanCursor = row.ID
			if recoverErr != nil {
				errs = append(errs, fmt.Errorf(
					"tenant=%d knowledge_base=%q knowledge=%q generation=%q: %w",
					row.TenantID,
					row.KnowledgeBaseID,
					row.ID,
					row.ProcessingGeneration,
					recoverErr,
				))
			} else if ok {
				replayed++
			}
		}
		if r.scanCursor == r.scanHighWater {
			r.scanCursor, r.scanHighWater = "", ""
			break
		}
	}
	if attempted > 0 {
		logger.Infof(ctx,
			"[enrichment recovery] scanned=%d replayed=%d failed=%d",
			attempted, replayed, len(errs),
		)
	}
	return errors.Join(errs...)
}
