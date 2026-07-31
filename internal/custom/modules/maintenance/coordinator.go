// Package maintenance owns the single-leader background control plane. Every
// maintenance replica stays ready, but only the PostgreSQL advisory-lock
// holder runs recovery, dispatch, and retention loops.
package maintenance

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/dig"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/corefanout"
	"github.com/Tencent/WeKnora/internal/custom/modules/derivativequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentrecovery"
	"github.com/Tencent/WeKnora/internal/custom/modules/kbdeletequeue"
	"github.com/Tencent/WeKnora/internal/custom/modules/knowledgeaux"
	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/custom/modules/processingtrace"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikidelete"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

const advisoryLockName = "weknora:maintenance:v2"

const (
	derivativeWorkInterval = 5 * time.Second
	recoveryScanInterval   = 10 * time.Second
	recoveryScanTimeout    = 8 * time.Second
	retentionInterval      = time.Minute
)

type Params struct {
	dig.In

	DB                   *gorm.DB
	Enqueuer             interfaces.TaskEnqueuer
	DerivativeRepository *derivativequeue.Repository
	ProcessingTrace      *processingtrace.Repository
	CoreFanout           *corefanout.Recovery
	Enrichment           *enrichmentrecovery.Recovery
	WikiQueue            *wikiqueue.Recovery
	KBDelete             *kbdeletequeue.Recovery
	KnowledgeAux         *knowledgeaux.Recovery
	WikiDelete           *wikidelete.Recovery
}

type Coordinator struct {
	params Params

	mu             sync.Mutex
	cancel         context.CancelFunc
	done           chan struct{}
	hooks          []Hook
	recoveryCursor int
}

// Hook is a leader-scoped background loop. Hooks are registered during
// dependency construction and are started/stopped on every advisory-lock
// leadership transition.
type Hook struct {
	Name  string
	Start func(context.Context) error
	Stop  func()
}

func NewCoordinator(params Params) *Coordinator {
	return &Coordinator{params: params}
}

func (c *Coordinator) Register(hook Hook) error {
	if c == nil || hook.Start == nil {
		return errors.New("maintenance leader hook requires a start function")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cancel != nil {
		return errors.New("maintenance leader hooks must be registered before start")
	}
	for _, existing := range c.hooks {
		if existing.Name == hook.Name {
			return fmt.Errorf("maintenance leader hook %q is already registered", hook.Name)
		}
	}
	c.hooks = append(c.hooks, hook)
	return nil
}

func (c *Coordinator) Start(parent context.Context) {
	if c == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	c.cancel = cancel
	c.done = make(chan struct{})
	done := c.done
	c.mu.Unlock()
	go c.run(ctx, done)
}

func (c *Coordinator) Stop() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel, done := c.cancel, c.done
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	c.mu.Lock()
	if c.done == done {
		c.cancel, c.done = nil, nil
	}
	c.mu.Unlock()
}

func (c *Coordinator) run(ctx context.Context, done chan struct{}) {
	defer close(done)
	pipelineobs.SetMaintenanceLeader(false)
	for ctx.Err() == nil {
		conn, leader, err := c.tryAcquire(ctx)
		if err != nil {
			logger.Warnf(ctx, "[maintenance leader] acquire failed: %v", err)
		}
		if !leader {
			if conn != nil {
				_ = conn.Close()
			}
			if !wait(ctx, 10*time.Second) {
				return
			}
			continue
		}
		logger.Infof(ctx, "[maintenance leader] acquired PostgreSQL advisory lock %s", advisoryLockName)
		pipelineobs.SetMaintenanceLeader(true)
		c.runAsLeader(ctx, conn)
		pipelineobs.SetMaintenanceLeader(false)
		if conn != nil {
			_, _ = conn.ExecContext(context.Background(),
				"SELECT pg_advisory_unlock(hashtextextended($1, 0))", advisoryLockName)
			_ = conn.Close()
		}
		if ctx.Err() == nil {
			logger.Warnf(ctx, "[maintenance leader] session lost; entering follower election")
		}
	}
}

func (c *Coordinator) tryAcquire(ctx context.Context) (*sql.Conn, bool, error) {
	if c.params.DB == nil {
		return nil, false, errors.New("database is unavailable")
	}
	if c.params.DB.Dialector.Name() != "postgres" {
		return nil, true, nil
	}
	sqlDB, err := c.params.DB.DB()
	if err != nil {
		return nil, false, err
	}
	conn, err := sqlDB.Conn(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRowContext(
		ctx,
		"SELECT pg_try_advisory_lock(hashtextextended($1, 0))",
		advisoryLockName,
	).Scan(&acquired); err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	return conn, acquired, nil
}

func (c *Coordinator) runAsLeader(ctx context.Context, conn *sql.Conn) {
	leaderCtx, cancelLeadership := context.WithCancel(ctx)
	var sessionDone chan struct{}
	if conn != nil {
		sessionDone = make(chan struct{})
		go c.monitorLeadershipSession(leaderCtx, conn, cancelLeadership, sessionDone)
	}
	defer func() {
		cancelLeadership()
		if sessionDone != nil {
			<-sessionDone
		}
	}()

	workTicker := time.NewTicker(derivativeWorkInterval)
	recoveryTicker := time.NewTicker(recoveryScanInterval)
	retentionTicker := time.NewTicker(retentionInterval)
	defer workTicker.Stop()
	defer recoveryTicker.Stop()
	defer retentionTicker.Stop()
	// The maintenance role has a deliberate two-connection budget. The
	// advisory-lock session owns one connection for the lifetime of the
	// leadership term, so all database recovery scans must share the remaining
	// connection serially. Starting each Recovery's independent loop here
	// creates a startup stampede and can permanently starve derivative lease
	// recovery and dispatch.
	c.runWork(leaderCtx)
	if leaderCtx.Err() != nil {
		return
	}
	startedHooks := c.startHooks(leaderCtx)
	defer c.stopHooks(startedHooks)
	c.runNextRecovery(leaderCtx)
	for {
		select {
		case <-leaderCtx.Done():
			return
		case <-workTicker.C:
			c.runWork(leaderCtx)
		case <-recoveryTicker.C:
			// Durable derivative work is the latency-sensitive control-plane
			// path. Give it first use of the sole work connection, then run one
			// bounded recovery scan. Six scans therefore complete one fair
			// round per minute without concurrent pool contention.
			c.runWork(leaderCtx)
			c.runNextRecovery(leaderCtx)
		case <-retentionTicker.C:
			c.runWork(leaderCtx)
			c.runRetention(leaderCtx)
		}
	}
}

// monitorLeadershipSession owns the advisory-lock session liveness probe.
// It runs independently from maintenance work so a slow recovery scan cannot
// delay leadership loss detection. Cancelling leaderCtx also stops every hook
// and in-flight scan immediately.
func (c *Coordinator) monitorLeadershipSession(
	ctx context.Context,
	conn *sql.Conn,
	cancelLeadership context.CancelFunc,
	done chan<- struct{},
) {
	defer close(done)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			err := conn.PingContext(pingCtx)
			cancel()
			if err != nil {
				cancelLeadership()
				return
			}
		}
	}
}

func (c *Coordinator) startHooks(ctx context.Context) []Hook {
	c.mu.Lock()
	hooks := append([]Hook(nil), c.hooks...)
	c.mu.Unlock()
	started := make([]Hook, 0, len(hooks))
	for _, hook := range hooks {
		if err := hook.Start(ctx); err != nil {
			logger.Warnf(ctx, "[maintenance leader] hook %s failed to start: %v", hook.Name, err)
			continue
		}
		started = append(started, hook)
		logger.Infof(ctx, "[maintenance leader] hook %s started", hook.Name)
	}
	return started
}

func (c *Coordinator) stopHooks(hooks []Hook) {
	for index := len(hooks) - 1; index >= 0; index-- {
		if hooks[index].Stop != nil {
			hooks[index].Stop()
		}
	}
}

func (c *Coordinator) runWork(ctx context.Context) {
	scanCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, err := c.params.DerivativeRepository.RecoverExpiredLeases(scanCtx, 500)
	if err != nil {
		logger.Warnf(ctx, "[maintenance leader] derivative lease recovery failed: %v", err)
		return
	}
	if _, err := c.params.DerivativeRepository.DispatchDue(
		scanCtx, c.params.Enqueuer, 500,
	); err != nil {
		logger.Warnf(ctx, "[maintenance leader] derivative dispatch failed: %v", err)
	}
	if err := c.params.DerivativeRepository.RefreshMetrics(scanCtx); err != nil {
		logger.Warnf(ctx, "[maintenance leader] derivative metrics refresh failed: %v", err)
	}
	if err := c.params.ProcessingTrace.RefreshMetrics(scanCtx); err != nil {
		logger.Warnf(ctx, "[maintenance leader] processing trace metrics refresh failed: %v", err)
	}
	pipelineobs.RefreshDBPoolMetrics()
}

func (c *Coordinator) runRetention(ctx context.Context) {
	retentionCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	now := time.Now().UTC()
	if _, err := c.params.ProcessingTrace.DeleteExpired(
		retentionCtx, now.Add(-7*24*time.Hour), 5000,
	); err != nil {
		logger.Warnf(ctx, "[maintenance leader] processing trace retention skipped: %v", err)
	}
	if _, _, err := c.params.DerivativeRepository.DeleteExpired(
		retentionCtx, now, now.Add(-30*24*time.Hour), 5000,
	); err != nil {
		logger.Warnf(ctx, "[maintenance leader] derivative retention skipped: %v", err)
	}
}

func (c *Coordinator) runNextRecovery(ctx context.Context) {
	scans := []struct {
		name string
		run  func(context.Context) error
	}{
		{name: "core-fanout", run: c.params.CoreFanout.RecoverNow},
		{name: "enrichment", run: c.params.Enrichment.RecoverNow},
		{name: "wiki-queue", run: c.params.WikiQueue.RecoverNow},
		{name: "kb-delete", run: c.params.KBDelete.RecoverNow},
		{name: "knowledge-aux", run: c.params.KnowledgeAux.RecoverNow},
		{name: "wiki-delete", run: c.params.WikiDelete.RecoverNow},
	}
	index := c.recoveryCursor % len(scans)
	c.recoveryCursor = (index + 1) % len(scans)

	scan := scans[index]
	scanCtx, cancel := context.WithTimeout(ctx, recoveryScanTimeout)
	defer cancel()
	if err := scan.run(scanCtx); err != nil && ctx.Err() == nil {
		logger.Warnf(ctx, "[maintenance leader] %s recovery scan incomplete: %v", scan.name, err)
	}
}

func wait(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func Start(coordinator *Coordinator, cleaner interfaces.ResourceCleaner) error {
	if coordinator == nil {
		return fmt.Errorf("maintenance coordinator is unavailable")
	}
	coordinator.Start(context.Background())
	if cleaner != nil {
		cleaner.RegisterWithName("MaintenanceLeaderV2", func() error {
			coordinator.Stop()
			return nil
		})
	}
	return nil
}
