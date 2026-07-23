package kbdeletequeue

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

const (
	recoveryInterval = time.Minute
	recoveryTimeout  = 15 * time.Second
)

type Recovery struct {
	db       *gorm.DB
	enqueuer interfaces.TaskEnqueuer
	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
}

func NewRecovery(db *gorm.DB, enqueuer interfaces.TaskEnqueuer) *Recovery {
	return &Recovery{db: db, enqueuer: enqueuer}
}

func (r *Recovery) Start(parent context.Context) {
	if r == nil {
		return
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
				r.cancel = nil
				r.done = nil
			}
			r.mu.Unlock()
		}()
		r.runCycle(ctx)
		ticker := time.NewTicker(recoveryInterval)
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
	ctx, cancel := context.WithTimeout(parent, recoveryTimeout)
	defer cancel()
	if err := r.RecoverNow(ctx); err != nil && parent.Err() == nil {
		logger.Errorf(ctx, "[KB delete outbox] recovery failed: %v", err)
	}
}

func (r *Recovery) RecoverNow(ctx context.Context) error {
	if r == nil || r.db == nil || r.enqueuer == nil {
		return errors.New("KB delete outbox: recovery dependencies are unavailable")
	}
	var rows []*types.TaskPendingOp
	if err := r.db.WithContext(ctx).
		Where("task_type = ? AND scope = ? AND op = ?",
			types.TypeKBDelete, types.TaskScopeKnowledgeBase, operationDelete).
		Order("id ASC").Limit(1000).Find(&rows).Error; err != nil {
		return fmt.Errorf("KB delete outbox: scan intents: %w", err)
	}
	var errs []error
	for _, row := range rows {
		if row == nil || row.ScopeID == "" || len(row.Payload) == 0 {
			errs = append(errs, errors.New("KB delete outbox: invalid persisted intent"))
			continue
		}
		if err := EnqueueTrigger(r.enqueuer, row.Payload, time.Second); err != nil {
			errs = append(errs, fmt.Errorf("KB %s: %w", row.ScopeID, err))
		}
	}
	return errors.Join(errs...)
}
