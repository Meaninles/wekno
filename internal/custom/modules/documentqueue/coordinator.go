package documentqueue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"runtime/debug"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"go.uber.org/dig"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/Tencent/WeKnora/internal/custom/modules/pipelineobs"
	"github.com/Tencent/WeKnora/internal/custom/modules/wikiqueue"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var (
	ErrStaleDelivery            = errors.New("document queue: stale delivery")
	ErrLeaseLost                = errors.New("document queue: execution lease lost")
	ErrAlreadyLeased            = errors.New("document queue: workflow already leased")
	ErrInstanceCapacity         = errors.New("document queue: durable instance capacity reached")
	ErrFairnessDeferred         = errors.New("document queue: delivery deferred by fair scheduler")
	ErrInstanceFenced           = errors.New("document queue: instance boot was fenced")
	ErrInstanceIdentityConflict = errors.New("document queue: stable instance identity is already held by another boot")
	ErrTerminationNotProven     = errors.New("document queue: exact instance termination is not proven")
	ErrPlanConflict             = errors.New("document queue: workflow plan conflict")
	ErrWorkflowNotBound         = errors.New("document queue: prepared workflow is not bound to the exact knowledge generation")
)

const defaultDocumentConcurrency = 4
const leaseMutationTimeout = 10 * time.Second
const expiredRecoveryScanMultiplier = 10
const maxTerminationProofBytes = 1024
const documentSchedulerAdvisoryLock int64 = 0x574b4e4f524151

func workflowTerminalDiagnostic(
	state WorkflowState,
	errorCode string,
	errorMessage string,
) types.JSON {
	status := ""
	defaultCode := ""
	switch state {
	case StateCompleted:
		status = types.SpanStatusDone
	case StateFailed:
		status = types.SpanStatusFailed
		defaultCode = "DOCUMENT_WORKFLOW_FAILED"
	case StateCancelled, StateSuperseded:
		status = types.SpanStatusCancelled
		defaultCode = "DOCUMENT_WORKFLOW_CANCELLED"
	default:
		return nil
	}
	if strings.TrimSpace(errorCode) == "" {
		errorCode = defaultCode
	}
	payload, _ := json.Marshal(map[string]string{
		"source":        "workflow",
		"status":        status,
		"error_code":    strings.TrimSpace(errorCode),
		"error_message": strings.TrimSpace(errorMessage),
	})
	return types.JSON(payload)
}

func workflowTerminalError(
	state WorkflowState,
	stage string,
	snapshot *knowledgeSnapshot,
) string {
	if state != StateFailed {
		return ""
	}
	if snapshot != nil {
		if message := strings.TrimSpace(snapshot.WikiErrorMessage); message != "" {
			return message
		}
	}
	return "required document derivative finished with status " + strings.TrimSpace(stage)
}

// SQLite state-machine tests and multiple coordinators inside one process need
// the same serialization guarantee that pg_advisory_xact_lock provides across
// production Pods. The process mutex is intentionally acquired outside the
// transaction; PostgreSQL still supplies the cross-process half.
var documentSchedulerProcessMu sync.Mutex

type CoordinatorParams struct {
	dig.In

	DB        *gorm.DB
	Client    *asynq.Client                   `optional:"true"`
	Inspector *asynq.Inspector                `optional:"true"`
	Settings  interfaces.SystemSettingService `optional:"true"`
}

// Coordinator owns the durable workflow outbox, instance heartbeats and
// execution leases. Every application replica runs one; all decisions that
// change ownership are serialized in PostgreSQL and are safe when several
// coordinators reconcile the same rows concurrently.
type Coordinator struct {
	db        *gorm.DB
	client    *asynq.Client
	inspector *asynq.Inspector
	config    Config

	runtimeVerifier        RuntimeTerminationVerifier
	runtimeVerifierInitErr error

	instanceID string
	bootID     string
	capacity   int
	slots      chan struct{}

	mu       sync.Mutex
	cancel   context.CancelFunc
	done     chan struct{}
	draining atomic.Bool
	fenced   atomic.Bool
	ready    atomic.Bool
	redisOK  atomic.Bool
	stuck    atomic.Int64
	// lastHeartbeatSuccess is used by HTTP readiness. The database row is the
	// cross-instance signal; this local timestamp prevents a pod whose database
	// heartbeat has stopped from continuing to receive new HTTP traffic.
	lastHeartbeatSuccess atomic.Int64

	activeMu sync.Mutex
	active   map[string]context.CancelFunc

	// expiredRecoveryMu serializes the local replica's expired-lease scan and
	// protects its keyset cursor. The cursor prevents an old, deliberately
	// fail-closed owner (for example a Pod whose termination cannot be proven)
	// from permanently occupying the first recovery batch after every tick.
	expiredRecoveryMu     sync.Mutex
	expiredRecoveryCursor expiredWorkflowCursor

	// recoverCycleHook is a deterministic test seam used to prove that a slow
	// recovery scan cannot starve the independent heartbeat loop.
	recoverCycleHook func(context.Context) error
	// observeHook is a deterministic test seam for failures between a
	// successful ownership CAS and the first business-handler invocation.
	observeHook func(context.Context, *Lease) error
}

type expiredWorkflowCursor struct {
	LeaseUntil          time.Time
	WorkflowID          string
	PositionValid       bool
	HighWaterLeaseUntil time.Time
	HighWaterWorkflowID string
	Valid               bool
}

func NewCoordinator(params CoordinatorParams) *Coordinator {
	capacity := defaultDocumentConcurrency
	if params.Settings != nil {
		if configured := params.Settings.GetInt(
			context.Background(), "asynq.concurrency", "WEKNORA_ASYNQ_CONCURRENCY", defaultDocumentConcurrency,
		); configured > 0 {
			capacity = int(configured)
		}
	}
	instanceID := strings.TrimSpace(os.Getenv("CUSTOM_DOCUMENT_QUEUE_INSTANCE_ID"))
	if instanceID == "" {
		instanceID = strings.TrimSpace(os.Getenv("WEKNORA_DOCUMENT_INSTANCE_ID"))
	}
	if instanceID == "" {
		instanceID, _ = os.Hostname()
	}
	if strings.TrimSpace(instanceID) == "" {
		instanceID = "instance-" + uuid.NewString()
	}
	coordinator := NewCoordinatorWithConfig(params.DB, params.Client, instanceID, uuid.NewString(), capacity, LoadConfig())
	coordinator.inspector = params.Inspector
	return coordinator
}

func NewCoordinatorWithConfig(
	db *gorm.DB,
	client *asynq.Client,
	instanceID string,
	bootID string,
	capacity int,
	config Config,
) *Coordinator {
	if capacity <= 0 {
		capacity = defaultDocumentConcurrency
	}
	if strings.TrimSpace(instanceID) == "" {
		instanceID = "instance-" + uuid.NewString()
	}
	if strings.TrimSpace(bootID) == "" {
		bootID = uuid.NewString()
	}
	coordinator := &Coordinator{
		db:         db,
		client:     client,
		config:     config.normalized(),
		instanceID: instanceID,
		bootID:     bootID,
		capacity:   capacity,
		slots:      make(chan struct{}, capacity),
		active:     make(map[string]context.CancelFunc),
	}
	coordinator.runtimeVerifier, coordinator.runtimeVerifierInitErr = newKubernetesRuntimeVerifier(coordinator.config)
	coordinator.redisOK.Store(client == nil)
	pipelineobs.SetDocumentWorkerCapacity(capacity)
	return coordinator
}

func (c *Coordinator) InstanceID() string { return c.instanceID }
func (c *Coordinator) BootID() string     { return c.bootID }
func (c *Coordinator) Capacity() int      { return c.capacity }

// AssertCurrentBoot fences non-root workers (for example physical document
// parts) against the same durable instance identity used by root workflows.
// Claims require Ready; already-running work may finish while Draining or
// Degraded, but a superseded boot can never keep writing.
func (c *Coordinator) AssertCurrentBoot(ctx context.Context, allowDraining bool) error {
	if c == nil || c.db == nil {
		return nil
	}
	if c.fenced.Load() || (!allowDraining && (c.draining.Load() || !c.ready.Load())) {
		return ErrInstanceFenced
	}
	states := []string{InstanceReady}
	if allowDraining {
		states = append(states, InstanceDraining, InstanceDegraded)
	}
	var count int64
	if err := c.db.WithContext(ctx).Model(&Instance{}).
		Where("instance_id = ? AND boot_id = ? AND state IN ?", c.instanceID, c.bootID, states).
		Count(&count).Error; err != nil {
		return err
	}
	if count != 1 {
		return fmt.Errorf("%w: instance=%s boot=%s", ErrInstanceFenced, c.instanceID, c.bootID)
	}
	return nil
}

// IsReady reports whether this boot can safely receive new work. A shared
// dependency outage removes readiness but does not deliberately restart every
// replica. IsLive is reserved for process-local fencing or a handler that has
// ignored cancellation beyond its deadline.
func (c *Coordinator) IsReady() bool {
	if c == nil {
		return true
	}
	if !c.ready.Load() || c.draining.Load() || c.fenced.Load() || c.stuck.Load() > 0 {
		return false
	}
	if c.client != nil && !c.redisOK.Load() {
		return false
	}
	last := c.lastHeartbeatSuccess.Load()
	return last > 0 && time.Since(time.Unix(0, last)) <= c.config.InstanceStaleAfter
}

func (c *Coordinator) IsLive() bool {
	return c == nil || (!c.fenced.Load() && c.stuck.Load() == 0)
}

func (c *Coordinator) Migrate(ctx context.Context) error {
	if c == nil || c.db == nil {
		return errors.New("document queue: database is unavailable")
	}
	// Production schema is installed once by versioned migration 000076 before
	// dependency construction. Running concurrent AutoMigrate DDL from every pod
	// makes horizontal cold starts race on PostgreSQL catalog locks. SQLite is
	// retained only for deterministic unit-state-machine fixtures.
	if c.db.Dialector.Name() != "sqlite" {
		migrator := c.db.Migrator()
		if !migrator.HasTable(&Workflow{}) || !migrator.HasTable(&Instance{}) ||
			!migrator.HasTable(&ScheduleGroup{}) {
			return errors.New("document queue schema is missing; run versioned migrations through 000085")
		}
		for _, field := range []string{
			"dispatch_epoch", "max_retry", "delegate_timeout_nanos", "workflow_timeout_nanos",
			"deadline_at", "retention_nanos", "lease_until", "plan_hash", "terminal_diagnostic",
		} {
			if !migrator.HasColumn(&Workflow{}, field) {
				version := "000076"
				if field == "terminal_diagnostic" {
					version = "000091"
				}
				return fmt.Errorf(
					"document queue schema column %s is missing; run versioned migration %s",
					field,
					version,
				)
			}
		}
		if !migrator.HasConstraint(
			&Workflow{},
			"ck_document_workflow_terminal_diagnostic",
		) {
			return errors.New(
				"document queue terminal diagnostic constraint is missing; run versioned migration 000091",
			)
		}
		if !migrator.HasColumn("knowledges", "processing_workflow_id") {
			return errors.New("document queue schema column knowledges.processing_workflow_id is missing; run versioned migration 000076")
		}
		return nil
	}
	db := c.db.Session(&gorm.Session{NewDB: true})
	config := *db.Config
	config.DisableForeignKeyConstraintWhenMigrating = true
	db.Config = &config
	return db.WithContext(ctx).AutoMigrate(&Workflow{}, &Instance{}, &ScheduleGroup{})
}

// Start registers this process incarnation, atomically adopts unfinished work
// from an earlier boot of the same stable instance, and starts heartbeat and
// recovery loops. Adoption increments DispatchEpoch, so a delayed delivery
// from the old process can never pass Claim after the restart.
func (c *Coordinator) Start(parent context.Context) error {
	if c == nil {
		return nil
	}
	if parent == nil {
		parent = context.Background()
	}
	if c.runtimeVerifierInitErr != nil {
		return fmt.Errorf("document queue initialize Kubernetes runtime verifier: %w", c.runtimeVerifierInitErr)
	}
	if err := c.Migrate(parent); err != nil {
		return fmt.Errorf("document queue migrate: %w", err)
	}
	if err := c.registerAndAdopt(parent); err != nil {
		return err
	}
	c.draining.Store(false)
	c.fenced.Store(false)
	c.ready.Store(false)

	c.mu.Lock()
	if c.cancel != nil {
		c.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(parent)
	done := make(chan struct{})
	c.cancel = cancel
	c.done = done
	c.mu.Unlock()

	go c.run(runCtx, done)
	// Unit/Lite coordinators have no Redis consumer to start. Distributed
	// instances are promoted only after both Asynq servers have started.
	if c.client == nil {
		if err := c.MarkReady(parent); err != nil {
			return err
		}
	}
	// The durable rows are authoritative. A failed immediate Redis publish is
	// logged and retried by the loop; it must not prevent the API from starting.
	initialCtx, initialCancel := context.WithTimeout(parent, c.config.RecoveryCycleTimeout)
	err := c.recoverCycle(initialCtx)
	initialCancel()
	if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		logger.Warnf(parent, "[document queue] initial recovery degraded: %v", err)
	}
	logger.Infof(parent,
		"[document queue] instance registered/starting instance_id=%s boot_id=%s capacity=%d",
		c.instanceID, c.bootID, c.capacity,
	)
	return nil
}

func (c *Coordinator) Stop() {
	if c == nil {
		return
	}
	c.MarkDraining()
	c.fenceActiveExecutions()
	deadline := time.Now().Add(c.config.ShutdownDrainTimeout)
	for c.activeExecutionCount() > 0 && time.Now().Before(deadline) {
		time.Sleep(25 * time.Millisecond)
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
	now := time.Now()
	// Marking a boot stopped is an immediate takeover signal. Only publish it
	// after every local business handler actually returned; otherwise an Asynq
	// timeout-ignoring goroutine could overlap its replacement. If the drain
	// budget expires, leave the last state as draining and let stale-heartbeat
	// recovery wait until the process has really exited.
	if c.db != nil && c.activeExecutionCount() == 0 {
		_ = c.db.WithContext(context.Background()).Model(&Instance{}).
			Where("instance_id = ? AND boot_id = ?", c.instanceID, c.bootID).
			Updates(map[string]interface{}{
				"state": InstanceStopped, "stopped_at": now, "updated_at": now,
			}).Error
	}
}

func (c *Coordinator) MarkDraining() {
	if c == nil || c.db == nil {
		return
	}
	c.draining.Store(true)
	now := time.Now()
	_ = c.db.WithContext(context.Background()).Model(&Instance{}).
		Where("instance_id = ? AND boot_id = ?", c.instanceID, c.bootID).
		Updates(map[string]interface{}{
			"state": InstanceDraining, "last_heartbeat_at": now, "updated_at": now,
		}).Error
}

// ConfirmInstanceTermination records an external orchestration proof for one
// exact boot. The API is SystemAdmin-only and intentionally refuses a fresh
// heartbeat; callers must first prove the container/Pod is terminated (or its
// node is fenced) and then attest that fact. Recovery still requires the
// workflow lease to expire and the Redis delivery to be inactive.
func (c *Coordinator) ConfirmInstanceTermination(
	ctx context.Context, instanceID, bootID, proof string,
) error {
	if c == nil || c.db == nil {
		return errors.New("document queue: coordinator is unavailable")
	}
	instanceID = strings.TrimSpace(instanceID)
	bootID = strings.TrimSpace(bootID)
	proof = strings.TrimSpace(proof)
	if instanceID == "" || bootID == "" || proof == "" {
		return fmt.Errorf("%w: instance_id, boot_id and proof are required", ErrTerminationNotProven)
	}
	if len(proof) > maxTerminationProofBytes {
		return fmt.Errorf("%w: proof exceeds %d bytes", ErrTerminationNotProven, maxTerminationProofBytes)
	}
	now := time.Now()
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("instance_id = ?", instanceID).Take(&instance).Error; err != nil {
			return err
		}
		if instance.BootID != bootID {
			return fmt.Errorf("%w: stable instance now belongs to boot %s", ErrStaleDelivery, instance.BootID)
		}
		if instance.State == InstanceStopped {
			return nil
		}
		if !instance.LastHeartbeatAt.Before(now.Add(-c.config.InstanceStaleAfter)) {
			return fmt.Errorf("%w: last heartbeat is still within the %s safety window",
				ErrTerminationNotProven, c.config.InstanceStaleAfter)
		}
		result := tx.Model(&Instance{}).
			Where("instance_id = ? AND boot_id = ?", instanceID, bootID).
			Updates(map[string]interface{}{
				"state": InstanceStopped, "stopped_at": now, "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrStaleDelivery
		}
		return nil
	})
	if err != nil {
		return err
	}
	logger.Warnf(ctx,
		"[document queue] external termination attested instance=%q boot=%q proof=%q",
		instanceID, bootID, proof,
	)
	return nil
}

// confirmForeignRuntimeTermination is the automated half of the termination
// proof boundary. It deliberately repeats every prerequisite even though the
// recovery scan already filters leases: tests and future callers cannot invoke
// the verifier on a fresh/local/unexpired owner by accident.
func (c *Coordinator) confirmForeignRuntimeTermination(
	ctx context.Context,
	workflow *Workflow,
	now time.Time,
) (bool, error) {
	if c == nil || workflow == nil || c.runtimeVerifier == nil || !c.IsReady() {
		return false, nil
	}
	if workflow.OwnerInstanceID == "" || workflow.OwnerBootID == "" ||
		workflow.OwnerInstanceID == c.instanceID || workflow.LeaseUntil == nil ||
		!workflow.LeaseUntil.Before(now) {
		return false, nil
	}
	return c.ProveInstanceBootTermination(
		ctx, workflow.OwnerInstanceID, workflow.OwnerBootID,
	)
}

// ProveInstanceBootTermination is shared by every durable worker tier. It
// never treats an expired lease, a stale heartbeat, a missing Pod, or an
// inactive Redis delivery as proof. A takeover is allowed only after the
// durable instance row is Stopped, a trusted stable-identity restart replaced
// the exact boot, or the Kubernetes runtime verifier proves that exact
// container incarnation terminated.
func (c *Coordinator) ProveInstanceBootTermination(
	ctx context.Context,
	instanceID string,
	bootID string,
) (bool, error) {
	if c == nil || c.db == nil {
		return false, nil
	}
	instanceID = strings.TrimSpace(instanceID)
	bootID = strings.TrimSpace(bootID)
	if instanceID == "" || bootID == "" || instanceID == c.instanceID {
		return false, nil
	}
	proven, err := c.instanceTerminationProven(ctx, instanceID, bootID)
	if err != nil || proven {
		return proven, err
	}
	if c.runtimeVerifier == nil || !c.IsReady() {
		return false, nil
	}
	now := time.Now()
	stale, err := c.instanceIsStale(ctx, instanceID, bootID, now)
	if err != nil || !stale {
		return false, err
	}
	evidence, err := c.runtimeVerifier.VerifyTermination(
		ctx, instanceID, bootID,
	)
	if err != nil {
		return false, fmt.Errorf(
			"verify Kubernetes runtime termination for %s/%s: %w",
			instanceID, bootID, err,
		)
	}
	if !evidence.Proven || strings.TrimSpace(evidence.Proof) == "" {
		return false, nil
	}
	if err := c.ConfirmInstanceTermination(
		ctx, instanceID, bootID, evidence.Proof,
	); err != nil {
		if errors.Is(err, ErrStaleDelivery) {
			// A trusted same-Pod restart may have replaced the boot after the API
			// observation. Re-read the durable proof instead of overwriting it.
			return c.instanceTerminationProven(ctx, instanceID, bootID)
		}
		return false, err
	}
	logger.Warnf(ctx,
		"[document queue] Kubernetes runtime proved owner terminated instance=%q boot=%q reason=%q",
		instanceID, bootID, evidence.Reason,
	)
	return true, nil
}

func (c *Coordinator) run(ctx context.Context, done chan struct{}) {
	defer func() {
		close(done)
		c.mu.Lock()
		if c.done == done {
			c.cancel = nil
			c.done = nil
		}
		c.mu.Unlock()
	}()
	var loops sync.WaitGroup
	loops.Add(2)
	go func() {
		defer loops.Done()
		c.heartbeatLoop(ctx)
	}()
	go func() {
		defer loops.Done()
		c.recoveryLoop(ctx)
	}()
	loops.Wait()
}

func (c *Coordinator) heartbeatLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := c.heartbeat(ctx); err != nil {
				logger.Errorf(ctx, "[document queue] instance heartbeat failed: %v", err)
				if errors.Is(err, ErrInstanceFenced) {
					c.fenceActiveExecutions()
					c.cancelRun()
					return
				}
			}
		}
	}
}

func (c *Coordinator) recoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(c.config.RecoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			cycleCtx, cancel := context.WithTimeout(ctx, c.config.RecoveryCycleTimeout)
			err := c.recoverCycle(cycleCtx)
			cancel()
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				logger.Errorf(ctx, "[document queue] recovery cycle failed: %v", err)
			} else if errors.Is(err, context.DeadlineExceeded) {
				logger.Warnf(ctx, "[document queue] recovery cycle reached %s budget; remaining rows stay durable for the next cycle",
					c.config.RecoveryCycleTimeout)
			}
		}
	}
}

func (c *Coordinator) recoverCycle(ctx context.Context) error {
	if c != nil && c.recoverCycleHook != nil {
		return c.recoverCycleHook(ctx)
	}
	return c.RecoverNow(ctx)
}

func (c *Coordinator) cancelRun() {
	if c == nil {
		return
	}
	c.mu.Lock()
	cancel := c.cancel
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Coordinator) registerAndAdopt(ctx context.Context) error {
	now := time.Now()
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// A stable ID is a fencing credential, not merely a label. Never let an
		// arbitrary second process overwrite a live boot: heartbeat age cannot
		// prove that a paused/network-partitioned process has terminated. Helm
		// binds the ID to a Pod UID and Docker binds it to a unique container, so
		// those explicitly trusted runtimes may atomically replace the prior boot.
		var existing Instance
		lookup := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("instance_id = ?", c.instanceID).Take(&existing)
		if lookup.Error != nil && !errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			return fmt.Errorf("document queue lock stable instance identity: %w", lookup.Error)
		}
		if errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
			row := Instance{
				InstanceID: c.instanceID, BootID: c.bootID, State: InstanceStarting,
				Capacity: c.capacity, StartedAt: now, LastHeartbeatAt: now,
			}
			if err := tx.Create(&row).Error; err != nil {
				return fmt.Errorf("document queue register instance: %w", err)
			}
		} else {
			differentLiveBoot := existing.BootID != c.bootID && existing.State != InstanceStopped
			if differentLiveBoot && !c.config.TrustStableInstanceRestart {
				return fmt.Errorf("%w: instance=%s existing_boot=%s requested_boot=%s",
					ErrInstanceIdentityConflict, c.instanceID, existing.BootID, c.bootID)
			}
			if err := tx.Model(&Instance{}).Where("instance_id = ?", c.instanceID).
				Updates(map[string]interface{}{
					"boot_id": c.bootID, "state": InstanceStarting, "capacity": c.capacity,
					"started_at": now, "last_heartbeat_at": now, "stopped_at": nil,
					"updated_at": now,
				}).Error; err != nil {
				return fmt.Errorf("document queue replace stable instance boot: %w", err)
			}
		}
		result := tx.Model(&Workflow{}).
			Where("state = ? AND owner_instance_id = ? AND owner_boot_id <> ?",
				StateLeased, c.instanceID, c.bootID).
			Updates(map[string]interface{}{
				"state": StateQueued, "stage": "queued", "owner_instance_id": "", "owner_boot_id": "",
				"lease_until": nil, "last_heartbeat_at": nil,
				"dispatch_epoch": gorm.Expr("dispatch_epoch + 1"),
				// Delay publication for three recovery ticks. This gives a
				// mistakenly duplicated stable instance enough time to observe
				// its boot fence and cancel before the new boot resumes work.
				"dispatch_task_id": "", "last_dispatched_at": now,
				"last_error": "stable instance restarted; workflow adopted by new boot",
				"version":    gorm.Expr("version + 1"), "updated_at": now,
			})
		if result.Error != nil {
			return fmt.Errorf("document queue adopt previous boot: %w", result.Error)
		}
		if result.RowsAffected > 0 {
			logger.Infof(ctx, "[document queue] adopted %d workflow(s) from previous boot of %s",
				result.RowsAffected, c.instanceID)
		}
		return nil
	})
}

func (c *Coordinator) heartbeat(ctx context.Context) error {
	now := time.Now()
	state := InstanceStarting
	if c.ready.Load() {
		state = InstanceReady
	}
	if c.draining.Load() {
		state = InstanceDraining
	}
	var redisErr error
	if c.client != nil && state != InstanceDraining {
		redisErr = c.client.Ping()
		c.redisOK.Store(redisErr == nil)
		if redisErr != nil {
			state = InstanceDegraded
		}
	}
	if c.stuck.Load() > 0 && state != InstanceDraining {
		state = InstanceDegraded
	}
	result := c.db.WithContext(ctx).Model(&Instance{}).
		Where("instance_id = ? AND boot_id = ?", c.instanceID, c.bootID).
		Updates(map[string]interface{}{
			"state": state, "capacity": c.capacity,
			"last_heartbeat_at": now, "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("%w: instance=%s boot=%s", ErrInstanceFenced, c.instanceID, c.bootID)
	}
	c.lastHeartbeatSuccess.Store(now.UnixNano())
	if redisErr != nil {
		return fmt.Errorf("document queue Redis health probe: %w", redisErr)
	}
	return nil
}

func (c *Coordinator) MarkReady(ctx context.Context) error {
	if c == nil || c.db == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.client != nil {
		if err := c.client.Ping(); err != nil {
			c.redisOK.Store(false)
			return fmt.Errorf("document queue Redis readiness probe: %w", err)
		}
		c.redisOK.Store(true)
	}
	now := time.Now()
	result := c.db.WithContext(ctx).Model(&Instance{}).
		Where("instance_id = ? AND boot_id = ? AND state IN ?", c.instanceID, c.bootID,
			[]string{InstanceStarting, InstanceReady}).
		Updates(map[string]interface{}{
			"state": InstanceReady, "capacity": c.capacity,
			"last_heartbeat_at": now, "updated_at": now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		c.fenceActiveExecutions()
		return ErrInstanceFenced
	}
	c.ready.Store(true)
	c.lastHeartbeatSuccess.Store(now.UnixNano())
	return nil
}

func executionKey(workflowID string, epoch int64) string {
	return fmt.Sprintf("%s:%d", workflowID, epoch)
}

func (c *Coordinator) reserveExecution(cancel context.CancelFunc) (string, error) {
	if c == nil || cancel == nil {
		return "", ErrInstanceFenced
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	if c.draining.Load() || c.fenced.Load() {
		return "", ErrInstanceFenced
	}
	key := "admission:" + uuid.NewString()
	c.active[key] = cancel
	return key, nil
}

// RegisterAuxiliaryExecution adds non-root work which can still mutate a
// document (for example a physically split part) to the same process-lifetime
// fence as root workflows. The returned release function is idempotent.
//
// Stop must not publish InstanceStopped while any registered handler is still
// running: other replicas use that durable state as an immediate takeover
// proof. Registration therefore tracks lifecycle only and deliberately does
// not consume an additional document-capacity slot.
func (c *Coordinator) RegisterAuxiliaryExecution(cancel context.CancelFunc) (func(), error) {
	key, err := c.reserveExecution(cancel)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			c.removeExecution(key)
		})
	}, nil
}

func (c *Coordinator) bindExecution(admissionKey string, lease *Lease, cancel context.CancelFunc) string {
	if c == nil || lease == nil || admissionKey == "" {
		return admissionKey
	}
	key := executionKey(lease.WorkflowID, lease.Epoch)
	c.activeMu.Lock()
	delete(c.active, admissionKey)
	c.active[key] = cancel
	c.activeMu.Unlock()
	return key
}

func (c *Coordinator) removeExecution(key string) {
	if c == nil || key == "" {
		return
	}
	c.activeMu.Lock()
	delete(c.active, key)
	c.activeMu.Unlock()
}

func (c *Coordinator) activeExecutionCount() int {
	if c == nil {
		return 0
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	return len(c.active)
}

func (c *Coordinator) hasActiveExecution(workflowID string, epoch int64) bool {
	if c == nil {
		return false
	}
	c.activeMu.Lock()
	defer c.activeMu.Unlock()
	_, ok := c.active[executionKey(workflowID, epoch)]
	return ok
}

func (c *Coordinator) fenceActiveExecutions() {
	if c == nil {
		return
	}
	c.fenced.Store(true)
	c.activeMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.active))
	for _, cancel := range c.active {
		cancels = append(cancels, cancel)
	}
	c.activeMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
}

type identityPayload struct {
	TenantID              uint64 `json:"tenant_id"`
	KnowledgeID           string `json:"knowledge_id"`
	KnowledgeBaseID       string `json:"knowledge_base_id"`
	ProcessingGeneration  string `json:"processing_generation"`
	ProcessingOwner       string `json:"processing_owner,omitempty"`
	DocumentWorkflowID    string `json:"document_workflow_id,omitempty"`
	DocumentWorkflowEpoch int64  `json:"document_workflow_epoch,omitempty"`
}

func decodeIdentity(payload []byte) (identityPayload, error) {
	var identity identityPayload
	if err := json.Unmarshal(payload, &identity); err != nil {
		return identity, err
	}
	identity.KnowledgeID = strings.TrimSpace(identity.KnowledgeID)
	identity.KnowledgeBaseID = strings.TrimSpace(identity.KnowledgeBaseID)
	identity.ProcessingGeneration = strings.TrimSpace(identity.ProcessingGeneration)
	if identity.TenantID == 0 || identity.KnowledgeID == "" || identity.KnowledgeBaseID == "" || identity.ProcessingGeneration == "" {
		return identity, errors.New("tenant, knowledge base, knowledge and processing generation are required")
	}
	return identity, nil
}

type workflowPlan struct {
	MaxRetry             int        `json:"max_retry"`
	DelegateTimeoutNanos int64      `json:"delegate_timeout_nanos"`
	WorkflowTimeoutNanos int64      `json:"workflow_timeout_nanos"`
	DeadlineAt           *time.Time `json:"deadline_at,omitempty"`
	RetentionNanos       int64      `json:"retention_nanos"`
	ProducerTaskID       string     `json:"producer_task_id,omitempty"`
}

// resolveWorkflowPlan converts the caller's options into the exact effective
// plan persisted by this queue. Options which this queue cannot faithfully
// reproduce are rejected instead of silently disappearing from the durable
// submission plan.
func (c *Coordinator) resolveWorkflowPlan(opts []asynq.Option) (workflowPlan, error) {
	plan := workflowPlan{
		MaxRetry:             c.config.MaxRetry,
		DelegateTimeoutNanos: int64(c.config.DelegateTimeout),
		WorkflowTimeoutNanos: int64(c.config.WorkflowTimeout),
	}
	for _, opt := range opts {
		if opt == nil {
			continue
		}
		switch opt.Type() {
		case asynq.MaxRetryOpt:
			value, ok := opt.Value().(int)
			if !ok {
				return plan, fmt.Errorf("invalid max retry option %T", opt.Value())
			}
			plan.MaxRetry = value
		case asynq.QueueOpt:
			// Root document workflows always use QueueDocument. A producer's
			// queue hint therefore has no effect on the durable execution plan.
		case asynq.TimeoutOpt:
			value, ok := opt.Value().(time.Duration)
			if !ok {
				return plan, fmt.Errorf("invalid timeout option %T", opt.Value())
			}
			plan.DelegateTimeoutNanos = int64(value)
		case asynq.DeadlineOpt:
			value, ok := opt.Value().(time.Time)
			if !ok {
				return plan, fmt.Errorf("invalid deadline option %T", opt.Value())
			}
			value = value.UTC()
			plan.DeadlineAt = &value
		case asynq.RetentionOpt:
			value, ok := opt.Value().(time.Duration)
			if !ok {
				return plan, fmt.Errorf("invalid retention option %T", opt.Value())
			}
			plan.RetentionNanos = int64(value)
		case asynq.TaskIDOpt:
			value, ok := opt.Value().(string)
			if !ok {
				return plan, fmt.Errorf("invalid task id option %T", opt.Value())
			}
			plan.ProducerTaskID = value
		default:
			return plan, fmt.Errorf("unsupported durable document workflow option %s", opt.String())
		}
	}
	return plan, nil
}

// workflowPayloadForPlanHash removes observability-only fields before hashing
// an immutable business plan. A producer retry may run under a new request or
// Langfuse span after the first Prepare committed; those values must not turn
// the same document generation into a conflicting plan. The original payload
// is still persisted unchanged so the first accepted trace remains intact.
func workflowPayloadForPlanHash(payload []byte) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	for _, key := range []string{
		"request_id",
		"lf_trace_id",
		"lf_parent_obs_id",
		"lf_user_id",
		"lf_session_id",
		"document_workflow_id",
		"document_workflow_epoch",
	} {
		delete(fields, key)
	}
	return json.Marshal(fields)
}

func workflowPlanHash(taskType string, payload []byte, plan workflowPlan) (string, error) {
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return "", err
	}
	semanticPayload, err := workflowPayloadForPlanHash(payload)
	if err != nil {
		return "", fmt.Errorf("canonicalize document workflow payload: %w", err)
	}
	h := sha256.New()
	for _, part := range [][]byte{[]byte(taskType), semanticPayload, planJSON} {
		_, _ = fmt.Fprintf(h, "%d:", len(part))
		_, _ = h.Write(part)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Coordinator) buildWorkflow(
	taskType string, payload []byte, opts []asynq.Option, state WorkflowState,
) (Workflow, error) {
	if taskType != types.TypeDocumentProcess && taskType != types.TypeManualProcess {
		return Workflow{}, fmt.Errorf("unsupported document workflow task type %q", taskType)
	}
	identity, err := decodeIdentity(payload)
	if err != nil {
		return Workflow{}, fmt.Errorf("decode document workflow identity: %w", err)
	}
	if strings.TrimSpace(identity.ProcessingOwner) == "" {
		return Workflow{}, errors.New("document workflow processing owner is required")
	}
	plan, err := c.resolveWorkflowPlan(opts)
	if err != nil {
		return Workflow{}, err
	}
	planHash, err := workflowPlanHash(taskType, payload, plan)
	if err != nil {
		return Workflow{}, fmt.Errorf("hash document workflow plan: %w", err)
	}
	now := time.Now()
	stage := "queued"
	if state == StatePreparing {
		stage = StagePreparing
	}
	return Workflow{
		ID: uuid.NewString(), TenantID: identity.TenantID,
		KnowledgeBaseID: identity.KnowledgeBaseID, KnowledgeID: identity.KnowledgeID,
		ProcessingGeneration: identity.ProcessingGeneration, TaskType: taskType,
		Payload: append([]byte(nil), payload...), PlanHash: planHash, State: state, Stage: stage,
		DispatchEpoch: 1, EnqueuedAt: now, LastProgressAt: &now, Version: 1,
		MaxRetry: plan.MaxRetry, DelegateTimeoutNanos: plan.DelegateTimeoutNanos,
		WorkflowTimeoutNanos: plan.WorkflowTimeoutNanos, DeadlineAt: plan.DeadlineAt,
		RetentionNanos: plan.RetentionNanos,
	}, nil
}

func (c *Coordinator) insertWorkflow(ctx context.Context, candidate Workflow) (*Workflow, bool, error) {
	result := c.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "tenant_id"}, {Name: "knowledge_id"}, {Name: "processing_generation"},
		},
		DoNothing: true,
	}).Create(&candidate)
	if result.Error != nil {
		return nil, false, fmt.Errorf("persist document workflow: %w", result.Error)
	}
	created := result.RowsAffected == 1
	var workflow Workflow
	if err := c.db.WithContext(ctx).
		Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
			candidate.TenantID, candidate.KnowledgeID, candidate.ProcessingGeneration).
		Take(&workflow).Error; err != nil {
		return nil, false, fmt.Errorf("reload document workflow: %w", err)
	}
	if workflow.PlanHash != candidate.PlanHash {
		return nil, false, fmt.Errorf("%w: generation %s persisted=%s requested=%s",
			ErrPlanConflict, candidate.ProcessingGeneration, workflow.PlanHash, candidate.PlanHash)
	}
	return &workflow, created, nil
}

// PrepareWorkflowWithOptions durably records the complete immutable execution
// plan before the knowledge generation is committed. Preparing rows are not
// queue work and can neither be dispatched nor claimed.
func (c *Coordinator) PrepareWorkflowWithOptions(
	ctx context.Context, taskType string, payload []byte, opts []asynq.Option,
) (*Workflow, bool, error) {
	if c == nil || c.db == nil {
		return nil, false, errors.New("document queue is unavailable")
	}
	candidate, err := c.buildWorkflow(taskType, payload, opts, StatePreparing)
	if err != nil {
		return nil, false, err
	}
	return c.insertWorkflow(ctx, candidate)
}

func (c *Coordinator) PrepareWorkflow(
	ctx context.Context, taskType string, payload []byte,
) (*Workflow, bool, error) {
	return c.PrepareWorkflowWithOptions(ctx, taskType, payload, nil)
}

// RegisterWorkflow is retained for legacy Redis deliveries during rollout.
// New producers must use PrepareWorkflowWithOptions, bind the returned ID on
// the knowledge generation, and only then activate it.
func (c *Coordinator) RegisterWorkflow(ctx context.Context, taskType string, payload []byte) (*Workflow, bool, error) {
	return c.RegisterWorkflowWithOptions(ctx, taskType, payload, nil)
}

func (c *Coordinator) RegisterWorkflowWithOptions(
	ctx context.Context, taskType string, payload []byte, opts []asynq.Option,
) (*Workflow, bool, error) {
	if c == nil || c.db == nil {
		return nil, false, errors.New("document queue is unavailable")
	}
	candidate, err := c.buildWorkflow(taskType, payload, opts, StateQueued)
	if err != nil {
		return nil, false, err
	}
	return c.insertWorkflow(ctx, candidate)
}

type WorkflowBinding struct {
	WorkflowID           string
	TenantID             uint64
	KnowledgeBaseID      string
	KnowledgeID          string
	ProcessingGeneration string
	ProcessingOwner      string
}

func BindingForWorkflow(workflow *Workflow) (WorkflowBinding, error) {
	if workflow == nil {
		return WorkflowBinding{}, errors.New("document workflow is nil")
	}
	identity, err := decodeIdentity(workflow.Payload)
	if err != nil {
		return WorkflowBinding{}, fmt.Errorf("decode workflow binding identity: %w", err)
	}
	if identity.TenantID != workflow.TenantID || identity.KnowledgeBaseID != workflow.KnowledgeBaseID ||
		identity.KnowledgeID != workflow.KnowledgeID ||
		identity.ProcessingGeneration != workflow.ProcessingGeneration {
		return WorkflowBinding{}, errors.New("document workflow payload identity differs from persisted workflow")
	}
	binding := WorkflowBinding{
		WorkflowID: workflow.ID, TenantID: workflow.TenantID,
		KnowledgeBaseID: workflow.KnowledgeBaseID, KnowledgeID: workflow.KnowledgeID,
		ProcessingGeneration: workflow.ProcessingGeneration,
		ProcessingOwner:      strings.TrimSpace(identity.ProcessingOwner),
	}
	if binding.WorkflowID == "" || binding.ProcessingOwner == "" {
		return WorkflowBinding{}, errors.New("document workflow binding is incomplete")
	}
	return binding, nil
}

func validateWorkflowBinding(workflow *Workflow, binding WorkflowBinding) error {
	if workflow == nil || workflow.ID != strings.TrimSpace(binding.WorkflowID) ||
		workflow.TenantID != binding.TenantID || workflow.KnowledgeBaseID != strings.TrimSpace(binding.KnowledgeBaseID) ||
		workflow.KnowledgeID != strings.TrimSpace(binding.KnowledgeID) ||
		workflow.ProcessingGeneration != strings.TrimSpace(binding.ProcessingGeneration) {
		return ErrWorkflowNotBound
	}
	identity, err := decodeIdentity(workflow.Payload)
	if err != nil {
		return fmt.Errorf("decode persisted workflow identity: %w", err)
	}
	if identity.TenantID != workflow.TenantID || identity.KnowledgeBaseID != workflow.KnowledgeBaseID ||
		identity.KnowledgeID != workflow.KnowledgeID ||
		identity.ProcessingGeneration != workflow.ProcessingGeneration {
		return ErrWorkflowNotBound
	}
	if strings.TrimSpace(identity.ProcessingOwner) == "" ||
		strings.TrimSpace(identity.ProcessingOwner) != strings.TrimSpace(binding.ProcessingOwner) {
		return ErrWorkflowNotBound
	}
	return nil
}

func knowledgeBindingIdentityQuery(tx *gorm.DB, binding WorkflowBinding) *gorm.DB {
	return tx.Table("knowledges").Where(
		"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processing_owner = ?",
		binding.KnowledgeID, binding.TenantID, binding.KnowledgeBaseID,
		binding.ProcessingGeneration, binding.ProcessingOwner,
	)
}

func exactKnowledgeBindingQuery(tx *gorm.DB, binding WorkflowBinding) *gorm.DB {
	return knowledgeBindingIdentityQuery(tx, binding).
		Where("parse_status = ? AND deleted_at IS NULL", types.ParseStatusPending)
}

// workflowKnowledgeBindingQuery proves the immutable generation-to-workflow
// relationship without requiring the transient processing owner. Core commit
// deliberately consumes that owner, while the workflow ID remains the durable
// identity used by idempotent Resume calls in leased and terminal states.
func workflowKnowledgeBindingQuery(tx *gorm.DB, binding WorkflowBinding) *gorm.DB {
	return tx.Table("knowledges").Where(
		"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processing_workflow_id = ? AND deleted_at IS NULL",
		binding.KnowledgeID, binding.TenantID, binding.KnowledgeBaseID,
		binding.ProcessingGeneration, binding.WorkflowID,
	)
}

func validateCancellationBinding(workflow *Workflow, binding CancellationBinding) error {
	if workflow == nil || workflow.ID != strings.TrimSpace(binding.WorkflowID) ||
		workflow.TenantID != binding.TenantID ||
		workflow.KnowledgeBaseID != strings.TrimSpace(binding.KnowledgeBaseID) ||
		workflow.KnowledgeID != strings.TrimSpace(binding.KnowledgeID) ||
		workflow.ProcessingGeneration != strings.TrimSpace(binding.ProcessingGeneration) {
		return ErrWorkflowNotBound
	}
	identity, err := decodeIdentity(workflow.Payload)
	if err != nil {
		return fmt.Errorf("decode persisted workflow cancellation identity: %w", err)
	}
	if identity.TenantID != workflow.TenantID ||
		identity.KnowledgeBaseID != workflow.KnowledgeBaseID ||
		identity.KnowledgeID != workflow.KnowledgeID ||
		identity.ProcessingGeneration != workflow.ProcessingGeneration {
		return ErrWorkflowNotBound
	}
	return nil
}

// claimableKnowledgeBindingQuery accepts every durable whole-document resume
// boundary, not only the first Pending delivery. A terminated worker may have
// already claimed core processing, or committed the immutable fanout plan and
// entered enrichment before its workflow lease is safely reassigned.
func claimableKnowledgeBindingQuery(tx *gorm.DB, binding WorkflowBinding) *gorm.DB {
	return workflowKnowledgeBindingQuery(tx, binding).Where(
		`(parse_status = ? AND processing_owner = ?)
		 OR (parse_status = ? AND (processing_owner = ? OR (processing_owner = '' AND processed_at IS NOT NULL)))
		 OR (parse_status = ? AND processing_owner = '' AND processed_at IS NOT NULL)
		 OR (parse_status = ? AND processing_owner = '' AND processed_at IS NOT NULL)`,
		types.ParseStatusPending, binding.ProcessingOwner,
		types.ParseStatusProcessing, binding.ProcessingOwner,
		types.ParseStatusFinalizing,
		types.ParseStatusCompleted,
	)
}

// BindPreparedWorkflowTransitionTx is the single transactional primitive for
// business flows which make a prepared generation ready. It locks and
// validates the immutable workflow before transition is allowed to lock or
// update the knowledge row, then proves that transition committed the exact
// Pending binding before releasing the workflow lock. Abort and activation use
// the same workflow -> knowledge order, so they cannot observe an unbound row
// and cancel a workflow while its business transaction is committing.
//
// Callers may lock parent/scoping rows before entering this method, but must not
// lock the knowledge row first.
func (c *Coordinator) BindPreparedWorkflowTransitionTx(
	tx *gorm.DB,
	binding WorkflowBinding,
	transition func(*gorm.DB) error,
) error {
	if c == nil || tx == nil {
		return errors.New("document queue: binding transaction is unavailable")
	}
	if transition == nil {
		return errors.New("document queue: binding transition is unavailable")
	}
	var workflow Workflow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", strings.TrimSpace(binding.WorkflowID)).Take(&workflow).Error; err != nil {
		return err
	}
	if err := validateWorkflowBinding(&workflow, binding); err != nil {
		return err
	}
	if workflow.State != StatePreparing && workflow.State != StateQueued {
		return ErrStaleDelivery
	}
	if err := transition(tx); err != nil {
		return err
	}
	var bound int64
	if err := exactKnowledgeBindingQuery(tx, binding).
		Where("processing_workflow_id = ?", binding.WorkflowID).
		Count(&bound).Error; err != nil {
		return fmt.Errorf("validate prepared workflow business binding: %w", err)
	}
	if bound != 1 {
		return ErrWorkflowNotBound
	}
	return nil
}

// BindPreparedWorkflowTx is intended to run inside the same transaction which
// makes an already-Pending knowledge generation publishable. It never
// broad-updates by document ID: all tenant, KB, generation, owner and workflow
// identities must agree.
func (c *Coordinator) BindPreparedWorkflowTx(tx *gorm.DB, binding WorkflowBinding) error {
	return c.BindPreparedWorkflowTransitionTx(tx, binding, func(tx *gorm.DB) error {
		result := exactKnowledgeBindingQuery(tx, binding).
			Where("processing_workflow_id = '' OR processing_workflow_id = ?", binding.WorkflowID).
			Update("processing_workflow_id", binding.WorkflowID)
		if result.Error != nil {
			return fmt.Errorf("bind prepared workflow to knowledge generation: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrWorkflowNotBound
		}
		return nil
	})
}

func (c *Coordinator) BindPreparedWorkflow(ctx context.Context, binding WorkflowBinding) error {
	if c == nil || c.db == nil {
		return errors.New("document queue is unavailable")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return c.BindPreparedWorkflowTx(tx, binding)
	})
}

// CommitPreparedReparse atomically publishes a reparse generation as Pending
// and binds it to the exact immutable workflow plan. The workflow row is
// locked first, matching AbortPreparedWorkflow/activation lock ordering, so a
// concurrent abort can never leave a committed knowledge row pointing at a
// cancelled workflow.
//
// A successful transaction intentionally leaves the workflow in Preparing.
// The producer activates it after commit; recoverPreparing closes the crash
// window if the process exits between those two operations.
func (c *Coordinator) CommitPreparedReparse(
	ctx context.Context,
	binding WorkflowBinding,
	transition ReparsePendingTransition,
) error {
	if c == nil || c.db == nil {
		return errors.New("document queue: reparse transaction is unavailable")
	}
	if transition.UpdatedAt.IsZero() {
		return errors.New("document queue: reparse transition timestamp is required")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var workflow Workflow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", strings.TrimSpace(binding.WorkflowID)).Take(&workflow).Error; err != nil {
			return err
		}
		if err := validateWorkflowBinding(&workflow, binding); err != nil {
			return err
		}

		// Idempotency for an ambiguous commit response or a producer retry: the
		// exact Pending generation is already accepted, regardless of whether
		// recovery has since activated the workflow.
		var alreadyBound int64
		if err := exactKnowledgeBindingQuery(tx, binding).
			Where("processing_workflow_id = ?", binding.WorkflowID).
			Count(&alreadyBound).Error; err != nil {
			return fmt.Errorf("validate committed reparse workflow binding: %w", err)
		}
		if alreadyBound == 1 {
			switch workflow.State {
			case StatePreparing, StateQueued, StateLeased, StateWaitingExternal:
				return nil
			default:
				return ErrStaleDelivery
			}
		}
		if workflow.State != StatePreparing {
			return ErrStaleDelivery
		}

		result := knowledgeBindingIdentityQuery(tx, binding).
			Where("parse_status = ? AND deleted_at IS NULL", types.ParseStatusProcessing).
			Where("processing_workflow_id = '' OR processing_workflow_id = ?", binding.WorkflowID).
			Updates(map[string]interface{}{
				"parse_status":           types.ParseStatusPending,
				"enable_status":          "disabled",
				"description":            "",
				"processed_at":           nil,
				"embedding_model_id":     transition.EmbeddingModelID,
				"pending_subtasks_count": 0,
				"enrichment_status":      types.EnrichmentStatusNone,
				"wiki_status":            types.WikiStatusNone,
				"wiki_error_message":     "",
				"error_message":          transition.ErrorMessage,
				"processing_workflow_id": binding.WorkflowID,
				"updated_at":             transition.UpdatedAt,
			})
		if result.Error != nil {
			return fmt.Errorf("commit prepared reparse workflow binding: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrWorkflowNotBound
		}
		return nil
	})
}

// CommitWorkflowCancellation is the cancellation publication boundary for a
// bound document generation. It locks workflow before knowledge, matching the
// reparse/activation lock order, then cancels both rows atomically. A restart
// adoption racing this transaction can therefore occur only before the fence
// (and is overwritten) or after it (and no longer matches state=leased).
//
// Incrementing dispatch_epoch also makes every already-published Redis copy
// stale. The caller still performs best-effort Redis cancellation afterwards,
// but no stale copy can reacquire this workflow or run the business delegate.
func (c *Coordinator) CommitWorkflowCancellation(
	ctx context.Context,
	binding CancellationBinding,
	updatedAt time.Time,
) error {
	if c == nil || c.db == nil {
		return errors.New("document queue: cancellation transaction is unavailable")
	}
	if updatedAt.IsZero() {
		return errors.New("document queue: cancellation timestamp is required")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var workflow Workflow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", strings.TrimSpace(binding.WorkflowID)).
			Take(&workflow).Error; err != nil {
			return err
		}
		if err := validateCancellationBinding(&workflow, binding); err != nil {
			return err
		}

		var knowledgeState struct {
			ParseStatus string
		}
		knowledgeQuery := tx.Table("knowledges").
			Clauses(clause.Locking{Strength: "UPDATE"}).
			Select("parse_status").
			Where(
				"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processing_workflow_id = ? AND deleted_at IS NULL",
				binding.KnowledgeID,
				binding.TenantID,
				binding.KnowledgeBaseID,
				binding.ProcessingGeneration,
				binding.WorkflowID,
			)
		if err := knowledgeQuery.Take(&knowledgeState).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrWorkflowNotBound
			}
			return err
		}
		if knowledgeState.ParseStatus != types.ParseStatusCancelling &&
			knowledgeState.ParseStatus != types.ParseStatusCancelled {
			return ErrWorkflowNotBound
		}

		if knowledgeState.ParseStatus == types.ParseStatusCancelling {
			result := tx.Table("knowledges").
				Where(
					"id = ? AND tenant_id = ? AND knowledge_base_id = ? AND processing_generation = ? AND processing_workflow_id = ? AND parse_status = ? AND deleted_at IS NULL",
					binding.KnowledgeID,
					binding.TenantID,
					binding.KnowledgeBaseID,
					binding.ProcessingGeneration,
					binding.WorkflowID,
					types.ParseStatusCancelling,
				).
				Updates(map[string]interface{}{
					"parse_status":           types.ParseStatusCancelled,
					"error_message":          "用户已取消解析",
					"pending_subtasks_count": 0,
					"summary_status":         types.SummaryStatusNone,
					"enrichment_status":      types.EnrichmentStatusNone,
					"wiki_status":            types.WikiStatusNone,
					"wiki_error_message":     "",
					"processing_owner":       "",
					"processing_fanout":      nil,
					"updated_at":             updatedAt,
				})
			if result.Error != nil {
				return fmt.Errorf("commit cancelled knowledge generation: %w", result.Error)
			}
			if result.RowsAffected != 1 {
				return ErrWorkflowNotBound
			}
		}

		if workflow.State == StateCancelled {
			return nil
		}
		result := tx.Model(&Workflow{}).
			Where("id = ? AND version = ?", workflow.ID, workflow.Version).
			Updates(map[string]interface{}{
				"state":              StateCancelled,
				"stage":              "cancelled",
				"dispatch_epoch":     gorm.Expr("dispatch_epoch + 1"),
				"dispatch_task_id":   "",
				"owner_instance_id":  "",
				"owner_boot_id":      "",
				"lease_until":        nil,
				"last_dispatched_at": nil,
				"last_heartbeat_at":  nil,
				"completed_at":       updatedAt,
				"last_error":         "cancelled by user",
				"terminal_diagnostic": workflowTerminalDiagnostic(
					StateCancelled, "USER_CANCELLED", "cancelled by user",
				),
				"version":    gorm.Expr("version + 1"),
				"updated_at": updatedAt,
			})
		if result.Error != nil {
			return fmt.Errorf("commit cancelled document workflow: %w", result.Error)
		}
		if result.RowsAffected != 1 {
			return ErrStaleDelivery
		}
		return nil
	})
}

func (c *Coordinator) activatePreparedWorkflowTx(tx *gorm.DB, binding WorkflowBinding) (*Workflow, bool, error) {
	var workflow Workflow
	if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id = ?", strings.TrimSpace(binding.WorkflowID)).Take(&workflow).Error; err != nil {
		return nil, false, err
	}
	if err := validateWorkflowBinding(&workflow, binding); err != nil {
		return nil, false, err
	}
	var bound int64
	if workflow.State != StatePreparing {
		var bindingQuery *gorm.DB
		switch workflow.State {
		case StateQueued:
			// A queued workflow may be a recovered lease whose delegate already
			// crossed the core/fanout boundary. Keep Resume and Claim on the same
			// durable recovery predicate.
			bindingQuery = claimableKnowledgeBindingQuery(tx, binding)
		case StateLeased, StateWaitingExternal, StateCompleted, StateFailed, StateCancelled, StateSuperseded:
			// Idempotent Resume must still prove the exact immutable generation
			// and workflow relationship. The processing owner is intentionally
			// absent after core commit and in terminal document states.
			bindingQuery = workflowKnowledgeBindingQuery(tx, binding)
		default:
			return nil, false, ErrStaleDelivery
		}
		if err := bindingQuery.Count(&bound).Error; err != nil {
			return nil, false, fmt.Errorf("validate workflow knowledge binding: %w", err)
		}
		if bound != 1 {
			return nil, false, ErrWorkflowNotBound
		}
		return &workflow, false, nil
	}
	if err := exactKnowledgeBindingQuery(tx, binding).
		Where("processing_workflow_id = ?", binding.WorkflowID).Count(&bound).Error; err != nil {
		return nil, false, fmt.Errorf("validate prepared workflow knowledge binding: %w", err)
	}
	if bound != 1 {
		return nil, false, ErrWorkflowNotBound
	}
	now := time.Now()
	result := tx.Model(&Workflow{}).
		Where("id = ? AND state = ? AND version = ?", workflow.ID, StatePreparing, workflow.Version).
		Updates(map[string]interface{}{
			"state": StateQueued, "stage": "queued", "enqueued_at": now,
			"last_progress_at": now, "last_error": "", "version": gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		return nil, false, fmt.Errorf("activate prepared document workflow: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return nil, false, ErrStaleDelivery
	}
	workflow.State = StateQueued
	workflow.Stage = "queued"
	workflow.EnqueuedAt = now
	workflow.LastProgressAt = &now
	workflow.Version++
	return &workflow, true, nil
}

// ActivatePreparedWorkflow is an idempotent CAS. It only makes work visible
// after re-reading the exact durable knowledge binding.
func (c *Coordinator) ActivatePreparedWorkflow(
	ctx context.Context, binding WorkflowBinding,
) (*Workflow, bool, error) {
	if c == nil || c.db == nil {
		return nil, false, errors.New("document queue is unavailable")
	}
	var workflow *Workflow
	var activated bool
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var err error
		workflow, activated, err = c.activatePreparedWorkflowTx(tx, binding)
		return err
	})
	return workflow, activated, err
}

func (c *Coordinator) LoadWorkflow(ctx context.Context, binding WorkflowBinding) (*Workflow, error) {
	if c == nil || c.db == nil {
		return nil, errors.New("document queue is unavailable")
	}
	var workflow Workflow
	if err := c.db.WithContext(ctx).Where("id = ?", strings.TrimSpace(binding.WorkflowID)).Take(&workflow).Error; err != nil {
		return nil, err
	}
	if err := validateWorkflowBinding(&workflow, binding); err != nil {
		return nil, err
	}
	return &workflow, nil
}

// AbortPreparedWorkflow cancels only an unbound preparation. Once the exact
// knowledge generation has committed the workflow ID, recovery owns activation
// and a producer is not allowed to discard the accepted work.
func (c *Coordinator) AbortPreparedWorkflow(
	ctx context.Context, binding WorkflowBinding, reason string,
) error {
	if c == nil || c.db == nil {
		return errors.New("document queue is unavailable")
	}
	return c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var workflow Workflow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", strings.TrimSpace(binding.WorkflowID)).Take(&workflow).Error; err != nil {
			return err
		}
		if err := validateWorkflowBinding(&workflow, binding); err != nil {
			return err
		}
		if workflow.State == StateCancelled {
			return nil
		}
		if workflow.State != StatePreparing {
			return ErrStaleDelivery
		}
		var bound int64
		if err := knowledgeBindingIdentityQuery(tx, binding).
			Where("processing_workflow_id = ?", binding.WorkflowID).Count(&bound).Error; err != nil {
			return err
		}
		if bound != 0 {
			return errors.New("document queue: bound prepared workflow cannot be aborted")
		}
		now := time.Now()
		result := tx.Model(&Workflow{}).
			Where("id = ? AND state = ? AND version = ?", workflow.ID, StatePreparing, workflow.Version).
			Updates(map[string]interface{}{
				"state": StateCancelled, "stage": "cancelled", "completed_at": now,
				"last_error": strings.TrimSpace(reason), "version": gorm.Expr("version + 1"), "updated_at": now,
				"terminal_diagnostic": workflowTerminalDiagnostic(
					StateCancelled, "DOCUMENT_WORKFLOW_CANCELLED", reason,
				),
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrStaleDelivery
		}
		return nil
	})
}

func workflowTaskID(workflowID string, epoch int64) string {
	return fmt.Sprintf("document-workflow:%s:%d", workflowID, epoch)
}

func addDeliveryIdentity(payload []byte, workflowID string, epoch int64) ([]byte, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return nil, err
	}
	id, _ := json.Marshal(workflowID)
	epochJSON, _ := json.Marshal(epoch)
	fields["document_workflow_id"] = id
	fields["document_workflow_epoch"] = epochJSON
	return json.Marshal(fields)
}

func (c *Coordinator) Dispatch(ctx context.Context, workflow *Workflow) (*asynq.TaskInfo, error) {
	if workflow == nil {
		return nil, errors.New("document queue: workflow is nil")
	}
	if workflow.State != StateQueued {
		return nil, ErrStaleDelivery
	}
	if c == nil || c.client == nil {
		return nil, errors.New("document queue: Redis task client is unavailable")
	}
	payload, err := addDeliveryIdentity(workflow.Payload, workflow.ID, workflow.DispatchEpoch)
	if err != nil {
		return nil, fmt.Errorf("prepare document workflow delivery: %w", err)
	}
	taskID := workflowTaskID(workflow.ID, workflow.DispatchEpoch)
	task := asynq.NewTask(workflow.TaskType, payload)
	// Persist the intended delivery before Redis can expose it to a worker. If
	// Claim wins immediately after Enqueue, the producer must still observe a
	// successful accepted outbox rather than a false state=queued CAS failure.
	// A Redis failure leaves this row queued and the recovery loop retries it.
	if err := c.recordDispatch(ctx, workflow, taskID); err != nil {
		return nil, err
	}
	workflowTimeout := time.Duration(workflow.WorkflowTimeoutNanos)
	if workflowTimeout <= 0 {
		workflowTimeout = c.config.WorkflowTimeout
	}
	opts := []asynq.Option{
		asynq.Queue(types.QueueDocument),
		asynq.MaxRetry(workflow.MaxRetry),
		asynq.Timeout(workflowTimeout),
		asynq.TaskID(taskID),
	}
	if workflow.RetentionNanos > 0 {
		opts = append(opts, asynq.Retention(time.Duration(workflow.RetentionNanos)))
	}
	info, enqueueErr := c.client.Enqueue(task, opts...)
	if errors.Is(enqueueErr, asynq.ErrTaskIDConflict) {
		return c.resolveDispatchConflict(ctx, workflow, taskID)
	}
	if enqueueErr != nil {
		return nil, enqueueErr
	}
	return info, nil
}

func (c *Coordinator) recordDispatch(ctx context.Context, workflow *Workflow, taskID string) error {
	now := time.Now()
	query := c.db.WithContext(ctx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ?", workflow.ID, StateQueued, workflow.DispatchEpoch)
	if workflow.LastDispatchedAt == nil {
		query = query.Where("last_dispatched_at IS NULL")
	} else {
		query = query.Where("last_dispatched_at = ?", *workflow.LastDispatchedAt)
	}
	result := query.
		Updates(map[string]interface{}{
			"dispatch_task_id": taskID,
			// Recovery is intentionally run by every replica. Re-publishing the
			// same durable TaskID is a liveness probe, not a new delivery
			// attempt; count only a newly recorded outbox identity. The
			// last_dispatched_at compare-and-swap also makes one replica the
			// publisher for a stale snapshot instead of letting the whole fleet
			// hammer Redis with the same TaskID concurrently.
			"dispatch_attempts": gorm.Expr(
				"dispatch_attempts + CASE WHEN dispatch_task_id = ? THEN 0 ELSE 1 END",
				taskID,
			),
			"last_dispatched_at": now, "updated_at": now,
		})
	if result.Error != nil {
		return fmt.Errorf("record document workflow dispatch: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrStaleDelivery
	}
	if workflow.DispatchTaskID != taskID {
		workflow.DispatchAttempts++
	}
	workflow.DispatchTaskID = taskID
	workflow.LastDispatchedAt = &now
	return nil
}

func synthesizedTaskInfo(workflow *Workflow) *asynq.TaskInfo {
	if workflow == nil {
		return nil
	}
	return &asynq.TaskInfo{
		ID:    workflowTaskID(workflow.ID, workflow.DispatchEpoch),
		Queue: types.QueueDocument, Type: workflow.TaskType,
	}
}

func (c *Coordinator) reconcileQueuedTerminal(
	ctx context.Context, workflow *Workflow,
) (bool, *asynq.TaskInfo, error) {
	lease := &Lease{
		WorkflowID: workflow.ID, Epoch: workflow.DispatchEpoch,
		TenantID: workflow.TenantID, KnowledgeID: workflow.KnowledgeID,
		KnowledgeBaseID: workflow.KnowledgeBaseID, Generation: workflow.ProcessingGeneration,
	}
	snapshot, state, stage, terminal, err := c.observe(ctx, lease)
	if err != nil || !terminal {
		return false, nil, err
	}
	now := time.Now()
	lastError := workflowTerminalError(state, stage, snapshot)
	result := c.db.WithContext(ctx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ?", workflow.ID, StateQueued, workflow.DispatchEpoch).
		Updates(map[string]interface{}{
			"state": state, "stage": stage, "completed_at": now,
			"last_error": lastError, "version": gorm.Expr("version + 1"), "updated_at": now,
			"terminal_diagnostic": workflowTerminalDiagnostic(state, "", lastError),
		})
	if result.Error != nil {
		return false, nil, result.Error
	}
	if result.RowsAffected == 1 {
		workflow.State = state
		workflow.Stage = stage
		if spanErr := c.reconcileTerminalAttemptSpans(
			ctx, workflow.KnowledgeID, state, stage,
		); spanErr != nil {
			logger.Warnf(ctx,
				"[document queue] queued terminal span reconciliation failed workflow=%s: %v",
				workflow.ID, spanErr)
		}
		return true, synthesizedTaskInfo(workflow), nil
	}
	var current Workflow
	if err := c.db.WithContext(ctx).Where("id = ?", workflow.ID).Take(&current).Error; err != nil {
		return false, nil, err
	}
	if current.State != StateQueued {
		return true, synthesizedTaskInfo(&current), nil
	}
	return false, nil, nil
}

func (c *Coordinator) rotateDispatchEpoch(ctx context.Context, workflow *Workflow, reason string) (*Workflow, error) {
	now := time.Now()
	result := c.db.WithContext(ctx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ?", workflow.ID, StateQueued, workflow.DispatchEpoch).
		Updates(map[string]interface{}{
			"dispatch_epoch": gorm.Expr("dispatch_epoch + 1"), "dispatch_task_id": "",
			"last_dispatched_at": nil, "last_error": reason,
			"version": gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		return nil, result.Error
	}
	var current Workflow
	if err := c.db.WithContext(ctx).Where("id = ?", workflow.ID).Take(&current).Error; err != nil {
		return nil, err
	}
	return &current, nil
}

func (c *Coordinator) failArchivedWorkflow(
	ctx context.Context, workflow *Workflow, archived *asynq.TaskInfo,
) (*asynq.TaskInfo, error) {
	reason := "document workflow delivery exhausted retries and was archived"
	if archived != nil && strings.TrimSpace(archived.LastErr) != "" {
		reason += ": " + strings.TrimSpace(archived.LastErr)
	}
	now := time.Now()
	result := c.db.WithContext(ctx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ?", workflow.ID, StateQueued, workflow.DispatchEpoch).
		Updates(map[string]interface{}{
			"state": StateFailed, "stage": "failed", "completed_at": now,
			"last_error": reason, "version": gorm.Expr("version + 1"), "updated_at": now,
			"terminal_diagnostic": workflowTerminalDiagnostic(
				StateFailed, "DOCUMENT_WORKFLOW_FAILED", reason,
			),
		})
	if result.Error != nil {
		return nil, result.Error
	}
	var current Workflow
	if err := c.db.WithContext(ctx).Where("id = ?", workflow.ID).Take(&current).Error; err != nil {
		return nil, err
	}
	return synthesizedTaskInfo(&current), nil
}

func (c *Coordinator) resolveDispatchConflict(
	ctx context.Context, workflow *Workflow, taskID string,
) (*asynq.TaskInfo, error) {
	if c.inspector == nil {
		return nil, fmt.Errorf("inspect conflicting document delivery %s: inspector unavailable", taskID)
	}
	info, err := c.inspector.GetTaskInfo(types.QueueDocument, taskID)
	if errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound) {
		info = nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect conflicting document delivery %s: %w", taskID, err)
	}
	if info != nil {
		switch info.State {
		case asynq.TaskStatePending, asynq.TaskStateActive, asynq.TaskStateScheduled, asynq.TaskStateRetry:
			return info, nil
		case asynq.TaskStateCompleted:
			if terminal, terminalInfo, terminalErr := c.reconcileQueuedTerminal(ctx, workflow); terminalErr != nil {
				return nil, terminalErr
			} else if terminal {
				return terminalInfo, nil
			}
		case asynq.TaskStateArchived:
			if terminal, terminalInfo, terminalErr := c.reconcileQueuedTerminal(ctx, workflow); terminalErr != nil {
				return nil, terminalErr
			} else if terminal {
				return terminalInfo, nil
			}
			return c.failArchivedWorkflow(ctx, workflow, info)
		default:
			return nil, fmt.Errorf("conflicting document delivery %s has unsafe state %s", taskID, info.State)
		}
	}

	rotated, err := c.rotateDispatchEpoch(ctx, workflow,
		fmt.Sprintf("stale Redis TaskID conflict for epoch %d", workflow.DispatchEpoch))
	if err != nil {
		return nil, err
	}
	if rotated.State != StateQueued {
		return synthesizedTaskInfo(rotated), nil
	}
	if rotated.DispatchEpoch == workflow.DispatchEpoch {
		return nil, ErrStaleDelivery
	}
	return c.Dispatch(ctx, rotated)
}

// RecoverNow performs both halves of recovery: it republishes missing outbox
// deliveries and transfers leases only after BOTH the worker heartbeat and the
// workflow lease prove stale. Multiple replicas may run this concurrently.
func (c *Coordinator) RecoverNow(ctx context.Context) error {
	if c == nil || c.db == nil || c.client == nil {
		return nil
	}
	var errs []error
	if err := c.recoverPreparing(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.recoverWaitingExternal(ctx); err != nil {
		errs = append(errs, err)
	}
	if err := c.reconcileTerminalSpanOrphans(ctx); err != nil {
		errs = append(errs, err)
	}
	now := time.Now()
	if _, err := c.dispatchNextQueued(ctx); err != nil &&
		!errors.Is(err, asynq.ErrTaskIDConflict) &&
		!errors.Is(err, ErrStaleDelivery) {
		errs = append(errs, err)
	}

	if err := c.recoverExpiredLeases(ctx, now); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// dispatchNextQueued publishes at most one fair queue head. A successful
// Claim immediately calls this again, forming a short admission chain that
// fills every replica without preloading Redis with the entire document
// backlog. Keeping only the current head deliverable also prevents hundreds of
// out-of-order tasks from repeatedly rotating epochs and hammering PostgreSQL.
//
// Terminal/superseded heads are reconciled in-place and skipped, bounded by the
// recovery batch size, so an old terminal prefix cannot block live work.
func (c *Coordinator) dispatchNextQueued(ctx context.Context) (*asynq.TaskInfo, error) {
	if c == nil || c.db == nil || c.client == nil {
		return nil, nil
	}
	staleDispatch := time.Now().Add(-3 * c.config.RecoveryInterval)
	for scanned := 0; scanned < c.config.RecoveryBatchSize; scanned++ {
		queued, err := c.listQueuedForDispatch(ctx, staleDispatch)
		if err != nil {
			return nil, fmt.Errorf("list queued document workflows: %w", err)
		}
		if len(queued) == 0 {
			return nil, nil
		}
		workflow := &queued[0]
		terminal, _, terminalErr := c.reconcileQueuedTerminal(ctx, workflow)
		if terminalErr != nil {
			return nil, fmt.Errorf("reconcile queued workflow %s: %w", workflow.ID, terminalErr)
		}
		if terminal {
			continue
		}
		info, dispatchErr := c.Dispatch(ctx, workflow)
		if dispatchErr != nil {
			return nil, fmt.Errorf("dispatch workflow %s: %w", workflow.ID, dispatchErr)
		}
		return info, nil
	}
	return nil, nil
}

func (c *Coordinator) listQueuedForDispatch(ctx context.Context, staleDispatch time.Time) ([]Workflow, error) {
	if c == nil || c.db == nil {
		return nil, nil
	}
	var queued []Workflow
	raw := `
		WITH active_groups AS (
			SELECT w.tenant_id, w.knowledge_base_id, COUNT(*) AS active_count
			FROM custom_document_queue_workflows w
			JOIN knowledges k ON k.id = w.knowledge_id
			 AND k.tenant_id = w.tenant_id
			 AND k.processing_generation = w.processing_generation
			 AND k.deleted_at IS NULL
			WHERE w.state = ?
			GROUP BY w.tenant_id, w.knowledge_base_id
		), ranked AS (
			SELECT w.*,
			       ROW_NUMBER() OVER (
			           PARTITION BY w.tenant_id, w.knowledge_base_id
			           ORDER BY w.enqueued_at ASC, w.id ASC
			       ) AS group_position,
			       COALESCE(a.active_count, 0) AS active_count,
			       sg.last_admitted_at AS schedule_last_admitted_at
			FROM custom_document_queue_workflows w
			LEFT JOIN active_groups a
			  ON a.tenant_id = w.tenant_id
			 AND a.knowledge_base_id = w.knowledge_base_id
			LEFT JOIN custom_document_queue_schedule_groups sg
			  ON sg.tenant_id = w.tenant_id
			 AND sg.knowledge_base_id = w.knowledge_base_id
			WHERE w.state = ?
		), fair_head AS (
			SELECT *
			FROM ranked
			ORDER BY group_position ASC,
			         active_count ASC,
			         CASE WHEN schedule_last_admitted_at IS NULL THEN 0 ELSE 1 END ASC,
			         schedule_last_admitted_at ASC,
			         enqueued_at ASC,
			         id ASC
			LIMIT 1
		)
		SELECT *
		FROM fair_head
		WHERE last_dispatched_at IS NULL OR last_dispatched_at < ?`
	err := c.db.WithContext(ctx).Raw(
		raw, StateLeased, StateQueued, staleDispatch,
	).Scan(&queued).Error
	return queued, err
}

func (c *Coordinator) recoverWaitingExternal(ctx context.Context) error {
	if c == nil || c.db == nil {
		return nil
	}
	var waiting []Workflow
	if err := c.db.WithContext(ctx).
		Where("state = ?", StateWaitingExternal).
		Order("updated_at ASC, id ASC").
		Limit(c.config.RecoveryBatchSize).
		Find(&waiting).Error; err != nil {
		return fmt.Errorf("list externally waiting document workflows: %w", err)
	}
	var errs []error
	for i := range waiting {
		workflow := &waiting[i]
		lease := &Lease{
			WorkflowID: workflow.ID, Epoch: workflow.DispatchEpoch,
			TenantID: workflow.TenantID, KnowledgeID: workflow.KnowledgeID,
			KnowledgeBaseID: workflow.KnowledgeBaseID, Generation: workflow.ProcessingGeneration,
		}
		snapshot, state, stage, terminal, err := c.observe(ctx, lease)
		if err != nil {
			errs = append(errs, fmt.Errorf("observe externally waiting workflow %s: %w", workflow.ID, err))
			// Rotate an unobservable row to the back of the bounded scan so one
			// transiently broken knowledge read cannot starve every later
			// derivative workflow. The workflow stays waiting and the error is
			// retained for operators; no business/task state is discarded.
			now := time.Now()
			result := c.db.WithContext(ctx).Model(&Workflow{}).
				Where("id = ? AND state = ? AND version = ?",
					workflow.ID, StateWaitingExternal, workflow.Version).
				Updates(map[string]interface{}{
					"last_error": err.Error(), "version": gorm.Expr("version + 1"),
					"updated_at": now,
				})
			if result.Error != nil {
				errs = append(errs, fmt.Errorf(
					"rotate unobservable externally waiting workflow %s: %w",
					workflow.ID, result.Error,
				))
			}
			continue
		}
		now := time.Now()
		if terminal {
			lastError := workflowTerminalError(state, stage, snapshot)
			result := c.db.WithContext(ctx).Model(&Workflow{}).
				Where("id = ? AND state = ? AND version = ?",
					workflow.ID, StateWaitingExternal, workflow.Version).
				Updates(map[string]interface{}{
					"state": state, "stage": stage, "completed_at": now,
					"last_error": lastError, "version": gorm.Expr("version + 1"), "updated_at": now,
					"terminal_diagnostic": workflowTerminalDiagnostic(state, "", lastError),
				})
			if result.Error != nil {
				errs = append(errs, fmt.Errorf("finalize externally waiting workflow %s: %w", workflow.ID, result.Error))
			} else if result.RowsAffected == 1 {
				if spanErr := c.reconcileTerminalAttemptSpans(
					ctx, workflow.KnowledgeID, state, stage,
				); spanErr != nil {
					errs = append(errs, fmt.Errorf(
						"reconcile terminal spans for externally waiting workflow %s: %w",
						workflow.ID, spanErr,
					))
				}
			}
			continue
		}
		if !coreCommittedForExternalWait(snapshot) {
			// A waiting row without the committed-core fence is unsafe: its
			// root publisher may have crashed before all stable child tasks
			// were emitted. Put only this exceptional row back through the
			// immutable root plan; ordinary derivative waits never reacquire a
			// document slot.
			result := c.db.WithContext(ctx).Model(&Workflow{}).
				Where("id = ? AND state = ? AND version = ?",
					workflow.ID, StateWaitingExternal, workflow.Version).
				Updates(map[string]interface{}{
					"state": StateQueued, "stage": "queued",
					"dispatch_epoch":   gorm.Expr("dispatch_epoch + 1"),
					"dispatch_task_id": "", "last_dispatched_at": nil,
					"last_error": "external wait lost its committed-core fence; resuming durable root plan",
					"version":    gorm.Expr("version + 1"), "updated_at": now,
				})
			if result.Error != nil {
				errs = append(errs, fmt.Errorf(
					"resume unsafe externally waiting workflow %s: %w",
					workflow.ID, result.Error,
				))
			}
			continue
		}

		// Keep the exact workflow non-terminal while derivatives run. Updating
		// updated_at rotates this row behind the rest of the bounded batch; the
		// business timestamp is copied separately into last_progress_at.
		updates := map[string]interface{}{
			"stage": stage, "last_error": "",
			"version": gorm.Expr("version + 1"), "updated_at": now,
		}
		if snapshot != nil && !snapshot.UpdatedAt.IsZero() {
			updates["last_progress_at"] = snapshot.UpdatedAt
		}
		result := c.db.WithContext(ctx).Model(&Workflow{}).
			Where("id = ? AND state = ? AND version = ?",
				workflow.ID, StateWaitingExternal, workflow.Version).
			Updates(updates)
		if result.Error != nil {
			errs = append(errs, fmt.Errorf(
				"refresh externally waiting workflow %s: %w",
				workflow.ID, result.Error,
			))
		}
	}
	return errors.Join(errs...)
}

type expiredOwnerRecovery struct {
	stale              bool
	staleChecked       bool
	terminationProven  bool
	terminationChecked bool
	err                error
	errReported        bool
}

func expiredOwnerRecoveryKey(instanceID, bootID string) string {
	return strings.TrimSpace(instanceID) + "\x00" + strings.TrimSpace(bootID)
}

// recoverExpiredLeases scans with a bounded keyset sweep rather than repeatedly
// applying LIMIT to the oldest leases. Each sweep freezes its upper edge before
// reading the first page. Rows appended beyond that edge wait for the next
// sweep, so a continuously growing expired tail cannot prevent the cursor from
// wrapping around to retry older, deliberately fail-closed rows.
//
// Recovery work remains bounded: one cycle may reclaim at most
// RecoveryBatchSize rows and inspect at most a small multiple of that number.
// A cycle that stops at either budget retains its current position and the
// frozen upper edge for the next tick.
//
// Runtime termination is an owner/boot fact, not a per-document fact. The
// per-cycle cache ensures one unavailable/negative Kubernetes lookup is shared
// by every expired workflow from that exact process incarnation.
func (c *Coordinator) recoverExpiredLeases(ctx context.Context, now time.Time) error {
	if c == nil || c.db == nil {
		return nil
	}
	c.expiredRecoveryMu.Lock()
	defer c.expiredRecoveryMu.Unlock()

	pageSize := c.config.RecoveryBatchSize
	if pageSize <= 0 {
		pageSize = DefaultConfig().RecoveryBatchSize
	}
	maxScanned := pageSize * expiredRecoveryScanMultiplier
	if maxScanned < pageSize { // integer-overflow guard for non-normalized tests
		maxScanned = pageSize
	}

	cursor := c.expiredRecoveryCursor
	if !cursor.Valid {
		var highWater []Workflow
		if err := c.db.WithContext(ctx).
			Where("state = ? AND lease_until IS NOT NULL AND lease_until < ?", StateLeased, now).
			Order("lease_until DESC, id DESC").Limit(1).Find(&highWater).Error; err != nil {
			return fmt.Errorf("find expired document lease recovery high-water mark: %w", err)
		}
		if len(highWater) == 0 {
			c.expiredRecoveryCursor = expiredWorkflowCursor{}
			return nil
		}
		cursor = expiredWorkflowCursor{
			HighWaterLeaseUntil: *highWater[0].LeaseUntil,
			HighWaterWorkflowID: highWater[0].ID,
			Valid:               true,
		}
		c.expiredRecoveryCursor = cursor
	}

	ownerCache := make(map[string]*expiredOwnerRecovery)
	var errs []error
	scanned := 0
	reclaimedCount := 0
	sweepComplete := false

	for scanned < maxScanned && reclaimedCount < pageSize {
		limit := pageSize
		if remaining := maxScanned - scanned; limit > remaining {
			limit = remaining
		}
		query := c.db.WithContext(ctx).
			Where("state = ? AND lease_until IS NOT NULL AND lease_until < ?", StateLeased, now).
			Where(
				"lease_until < ? OR (lease_until = ? AND id <= ?)",
				cursor.HighWaterLeaseUntil, cursor.HighWaterLeaseUntil, cursor.HighWaterWorkflowID,
			)
		if cursor.PositionValid {
			query = query.Where(
				"lease_until > ? OR (lease_until = ? AND id > ?)",
				cursor.LeaseUntil, cursor.LeaseUntil, cursor.WorkflowID,
			)
		}
		var expired []Workflow
		if err := query.Order("lease_until ASC, id ASC").Limit(limit).Find(&expired).Error; err != nil {
			return errors.Join(errors.Join(errs...), fmt.Errorf("list expired document leases: %w", err))
		}
		if len(expired) == 0 {
			// The frozen edge may have been concurrently requeued or otherwise
			// removed. No rows remain inside this sweep, so wrap on the next tick.
			c.expiredRecoveryCursor = expiredWorkflowCursor{}
			break
		}

		for i := range expired {
			workflow := &expired[i]
			scanned++
			cursor.LeaseUntil = *workflow.LeaseUntil
			cursor.WorkflowID = workflow.ID
			cursor.PositionValid = true
			if expiredRecoveryHighWaterReached(cursor) {
				// Reset before processing the boundary row. Even if its inspection
				// fails, this completed sweep must not chase work appended later.
				c.expiredRecoveryCursor = expiredWorkflowCursor{}
				sweepComplete = true
			} else {
				c.expiredRecoveryCursor = cursor
			}

			reclaimed, err := c.recoverOneExpiredLease(ctx, workflow, now, ownerCache)
			if err != nil {
				errs = append(errs, err)
			}
			if reclaimed {
				reclaimedCount++
			}
			if sweepComplete || reclaimedCount >= pageSize || scanned >= maxScanned {
				break
			}
		}
		if sweepComplete {
			break
		}
	}
	return errors.Join(errs...)
}

func expiredRecoveryHighWaterReached(cursor expiredWorkflowCursor) bool {
	if !cursor.Valid || !cursor.PositionValid {
		return false
	}
	return cursor.LeaseUntil.Equal(cursor.HighWaterLeaseUntil) &&
		cursor.WorkflowID == cursor.HighWaterWorkflowID
}

func (c *Coordinator) recoverOneExpiredLease(
	ctx context.Context,
	workflow *Workflow,
	now time.Time,
	ownerCache map[string]*expiredOwnerRecovery,
) (bool, error) {
	ownerKey := expiredOwnerRecoveryKey(workflow.OwnerInstanceID, workflow.OwnerBootID)
	owner := ownerCache[ownerKey]
	if owner == nil {
		owner = &expiredOwnerRecovery{}
		ownerCache[ownerKey] = owner
	}
	if !owner.staleChecked {
		owner.stale, owner.err = c.instanceIsStale(
			ctx, workflow.OwnerInstanceID, workflow.OwnerBootID, now,
		)
		owner.staleChecked = true
	}
	if owner.err != nil {
		if owner.errReported {
			return false, nil
		}
		owner.errReported = true
		return false, owner.err
	}

	// Redis becoming inactive is not proof that the business handler returned:
	// Asynq can move a timed-out task while its perform goroutine is still
	// running. A healthy owner may reclaim only its own row after the
	// process-local registry proves handler exit.
	localExecutionGone := workflow.OwnerInstanceID == c.instanceID &&
		workflow.OwnerBootID == c.bootID &&
		!c.hasActiveExecution(workflow.ID, workflow.DispatchEpoch)
	if !owner.stale && !localExecutionGone {
		return false, nil
	}
	if owner.stale && !localExecutionGone {
		if !owner.terminationChecked {
			owner.terminationProven, owner.err = c.instanceTerminationProven(
				ctx, workflow.OwnerInstanceID, workflow.OwnerBootID,
			)
			if owner.err == nil && !owner.terminationProven {
				owner.terminationProven, owner.err = c.confirmForeignRuntimeTermination(
					ctx, workflow, now,
				)
			}
			owner.terminationChecked = true
		}
		if owner.err != nil {
			if owner.errReported {
				return false, nil
			}
			owner.errReported = true
			return false, owner.err
		}
		if !owner.terminationProven {
			// Heartbeat + workflow lease + Redis inactivity still cannot
			// distinguish a dead process from a paused partitioned process.
			// Keep the row leased until a hard execution-boundary proof exists.
			return false, nil
		}
	}

	safe, err := c.deliveryIsInactive(ctx, workflow)
	if err != nil {
		return false, err
	}
	if !safe {
		return false, nil
	}
	reason := "owner heartbeat and workflow lease expired"
	if localExecutionGone && !owner.stale {
		reason = "local handler exited without persisting lease release"
	}
	reclaimed, err := c.requeueExpired(ctx, workflow, now, reason)
	if err != nil {
		return false, err
	}
	return reclaimed != nil, nil
}

// recoverPreparing closes the crash window after the knowledge transaction
// committed its workflow binding but before the producer could activate it.
// Unbound preparations are intentionally left invisible and may be retried by
// the producer; they can never enter queue counts or worker delivery.
func (c *Coordinator) recoverPreparing(ctx context.Context) error {
	var preparing []Workflow
	ownerExpression := "json_extract(w.payload, '$.processing_owner')"
	if c.db.Dialector.Name() == "postgres" {
		ownerExpression = "w.payload ->> 'processing_owner'"
	}
	join := "JOIN knowledges k ON k.id = w.knowledge_id AND k.tenant_id = w.tenant_id " +
		"AND k.knowledge_base_id = w.knowledge_base_id AND k.processing_generation = w.processing_generation " +
		"AND k.processing_owner = " + ownerExpression + " AND k.processing_workflow_id = w.id " +
		"AND k.parse_status = ? AND k.deleted_at IS NULL"
	if err := c.db.WithContext(ctx).Table(Workflow{}.TableName()+" w").Select("w.*").
		Joins(join, types.ParseStatusPending).Where("w.state = ?", StatePreparing).
		Order("w.created_at ASC, w.id ASC").Limit(c.config.RecoveryBatchSize).Find(&preparing).Error; err != nil {
		return fmt.Errorf("list preparing document workflows: %w", err)
	}
	var errs []error
	for i := range preparing {
		binding, err := BindingForWorkflow(&preparing[i])
		if err != nil {
			errs = append(errs, fmt.Errorf("decode preparing workflow %s: %w", preparing[i].ID, err))
			continue
		}
		_, _, err = c.ActivatePreparedWorkflow(ctx, binding)
		if errors.Is(err, ErrWorkflowNotBound) || errors.Is(err, ErrStaleDelivery) {
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("activate preparing workflow %s: %w", preparing[i].ID, err))
		}
	}
	return errors.Join(errs...)
}

// deliveryIsInactive adds Asynq's own liveness state as the final takeover
// gate. A stale database heartbeat alone must never duplicate a task that
// Redis still considers active. Once inactive, publish a best-effort cancel
// signal for a paused old process; requeueExpired delays the new epoch for
// three recovery ticks so that signal can be observed before replacement work
// starts.
func (c *Coordinator) deliveryIsInactive(ctx context.Context, workflow *Workflow) (bool, error) {
	if workflow == nil || workflow.DispatchTaskID == "" {
		return true, nil
	}
	if c.inspector == nil {
		if c.client == nil {
			return true, nil
		}
		return false, errors.New("document queue: Asynq inspector is required before cross-instance takeover")
	}
	info, err := c.inspector.GetTaskInfo(types.QueueDocument, workflow.DispatchTaskID)
	if errors.Is(err, asynq.ErrTaskNotFound) || errors.Is(err, asynq.ErrQueueNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect Asynq delivery %s before takeover: %w", workflow.DispatchTaskID, err)
	}
	if info.State == asynq.TaskStateActive {
		return false, nil
	}
	if err := c.inspector.CancelProcessing(workflow.DispatchTaskID); err != nil {
		logger.Warnf(ctx, "[document queue] best-effort cancel before takeover failed task=%s: %v",
			workflow.DispatchTaskID, err)
	}
	return true, nil
}

func (c *Coordinator) instanceIsStale(
	ctx context.Context, instanceID, bootID string, now time.Time,
) (bool, error) {
	if instanceID == "" || bootID == "" {
		return true, nil
	}
	var instance Instance
	err := c.db.WithContext(ctx).Where("instance_id = ?", instanceID).Take(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect owner instance %s: %w", instanceID, err)
	}
	if instance.BootID != bootID {
		return true, nil
	}
	if instance.State == InstanceStopped {
		return true, nil
	}
	return instance.LastHeartbeatAt.Before(now.Add(-c.config.InstanceStaleAfter)), nil
}

// instanceTerminationProven is deliberately stricter than heartbeat staleness.
// A stopped state is published only after every local handler returned, or by
// the SystemAdmin termination-attestation endpoint after an external
// orchestrator has proved the exact boot is gone. A different boot can exist
// only after registerAndAdopt accepted a trusted stable identity replacement.
func (c *Coordinator) instanceTerminationProven(
	ctx context.Context, instanceID, bootID string,
) (bool, error) {
	if strings.TrimSpace(instanceID) == "" || strings.TrimSpace(bootID) == "" {
		return false, nil
	}
	var instance Instance
	err := c.db.WithContext(ctx).Where("instance_id = ?", instanceID).Take(&instance).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("inspect owner termination proof %s: %w", instanceID, err)
	}
	if instance.BootID != bootID {
		return true, nil
	}
	return instance.State == InstanceStopped, nil
}

func (c *Coordinator) requeueExpired(
	ctx context.Context, workflow *Workflow, now time.Time, reasons ...string,
) (*Workflow, error) {
	if workflow == nil {
		return nil, nil
	}
	reason := "owner heartbeat and workflow lease expired"
	if len(reasons) > 0 && strings.TrimSpace(reasons[0]) != "" {
		reason = strings.TrimSpace(reasons[0])
	}
	localExecutionGone := workflow.OwnerInstanceID == c.instanceID &&
		workflow.OwnerBootID == c.bootID &&
		!c.hasActiveExecution(workflow.ID, workflow.DispatchEpoch)
	var updated *Workflow
	err := c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if !localExecutionGone {
			// Lock the proof row before the workflow. registerAndAdopt uses the
			// same instance→workflow order, closing the gap in which a new boot
			// could replace a stopped proof while a survivor reclaimed the lease.
			var instance Instance
			lookup := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
				Where("instance_id = ?", workflow.OwnerInstanceID).Take(&instance)
			if errors.Is(lookup.Error, gorm.ErrRecordNotFound) {
				return nil // a missing heartbeat row is not termination proof
			}
			if lookup.Error != nil {
				return lookup.Error
			}
			if instance.BootID == workflow.OwnerBootID && instance.State != InstanceStopped {
				return nil
			}
		}
		result := tx.Model(&Workflow{}).
			Where("id = ? AND state = ? AND dispatch_epoch = ? AND owner_instance_id = ? AND owner_boot_id = ? AND lease_until < ?",
				workflow.ID, StateLeased, workflow.DispatchEpoch,
				workflow.OwnerInstanceID, workflow.OwnerBootID, now).
			Updates(map[string]interface{}{
				"state": StateQueued, "stage": "queued", "owner_instance_id": "", "owner_boot_id": "",
				"lease_until": nil, "last_heartbeat_at": nil,
				"dispatch_epoch": gorm.Expr("dispatch_epoch + 1"), "dispatch_task_id": "",
				"last_dispatched_at": now,
				"last_error": fmt.Sprintf("%s; owner=%s/%s; workflow reclaimed",
					reason, workflow.OwnerInstanceID, workflow.OwnerBootID),
				"version": gorm.Expr("version + 1"), "updated_at": now,
			})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return nil
		}
		var row Workflow
		if err := tx.Where("id = ?", workflow.ID).Take(&row).Error; err != nil {
			return err
		}
		updated = &row
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("requeue expired workflow %s: %w", workflow.ID, err)
	}
	if updated == nil {
		return nil, nil
	}
	logger.Warnf(ctx,
		"[document queue] reclaimed workflow=%s knowledge=%s old_instance=%s old_boot=%s epoch=%d",
		updated.ID, updated.KnowledgeID, workflow.OwnerInstanceID, workflow.OwnerBootID, updated.DispatchEpoch,
	)
	return updated, nil
}

type Lease struct {
	WorkflowID       string
	Epoch            int64
	TenantID         uint64
	KnowledgeID      string
	KnowledgeBaseID  string
	Generation       string
	DelegateTimeout  time.Duration
	DelegateDeadline *time.Time
}

func (c *Coordinator) acquireSlot(ctx context.Context) (func(), error) {
	select {
	case c.slots <- struct{}{}:
		return func() { <-c.slots }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func lockFairSchedulerTx(tx *gorm.DB) error {
	if tx == nil || tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SELECT pg_advisory_xact_lock(?)", documentSchedulerAdvisoryLock).Error
}

func fairNextWorkflowID(tx *gorm.DB) (string, error) {
	if tx == nil {
		return "", errors.New("document queue: fair scheduler transaction is unavailable")
	}
	type fairHead struct {
		ID string
	}
	var head fairHead
	raw := `
		WITH current_work AS (
			SELECT w.id, w.tenant_id, w.knowledge_base_id, w.state, w.enqueued_at
			FROM custom_document_queue_workflows w
			JOIN knowledges k ON k.id = w.knowledge_id
			 AND k.tenant_id = w.tenant_id
			 AND k.processing_generation = w.processing_generation
			 AND k.deleted_at IS NULL
			WHERE w.state IN (?, ?)
		), group_stats AS (
			SELECT tenant_id, knowledge_base_id,
			       SUM(CASE WHEN state = ? THEN 1 ELSE 0 END) AS active_count
			FROM current_work
			GROUP BY tenant_id, knowledge_base_id
		), queued_heads AS (
			SELECT id, tenant_id, knowledge_base_id, enqueued_at,
			       ROW_NUMBER() OVER (
			           PARTITION BY tenant_id, knowledge_base_id
			           ORDER BY enqueued_at ASC, id ASC
			       ) AS group_position
			FROM current_work
			WHERE state = ?
		)
		SELECT q.id
		FROM queued_heads q
		JOIN group_stats g
		  ON g.tenant_id = q.tenant_id
		 AND g.knowledge_base_id = q.knowledge_base_id
		LEFT JOIN custom_document_queue_schedule_groups sg
		  ON sg.tenant_id = q.tenant_id
		 AND sg.knowledge_base_id = q.knowledge_base_id
		WHERE q.group_position = 1
		ORDER BY g.active_count ASC,
		         CASE WHEN sg.last_admitted_at IS NULL THEN 0 ELSE 1 END ASC,
		         sg.last_admitted_at ASC,
		         q.enqueued_at ASC,
		         q.id ASC
		LIMIT 1`
	result := tx.Raw(raw, StateQueued, StateLeased, StateLeased, StateQueued).Scan(&head)
	if result.Error != nil {
		return "", result.Error
	}
	return strings.TrimSpace(head.ID), nil
}

func deferFairDeliveryTx(tx *gorm.DB, workflow *Workflow, now time.Time) error {
	if tx == nil || workflow == nil {
		return errors.New("document queue: fair deferral is unavailable")
	}
	result := tx.Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ?",
			workflow.ID, StateQueued, workflow.DispatchEpoch).
		Updates(map[string]interface{}{
			"dispatch_epoch":     gorm.Expr("dispatch_epoch + 1"),
			"dispatch_task_id":   "",
			"last_dispatched_at": nil,
			"last_error":         "delivery deferred for fair knowledge-base scheduling",
			"version":            gorm.Expr("version + 1"),
			"updated_at":         now,
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrStaleDelivery
	}
	return nil
}

func recordScheduleAdmissionTx(tx *gorm.DB, workflow *Workflow, now time.Time) error {
	if tx == nil || workflow == nil {
		return errors.New("document queue: schedule admission is unavailable")
	}
	group := ScheduleGroup{
		TenantID: workflow.TenantID, KnowledgeBaseID: workflow.KnowledgeBaseID,
		LastAdmittedAt: now, CreatedAt: now, UpdatedAt: now,
	}
	return tx.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "tenant_id"}, {Name: "knowledge_base_id"}},
		DoUpdates: clause.Assignments(map[string]interface{}{
			"last_admitted_at": now,
			"updated_at":       now,
		}),
	}).Create(&group).Error
}

// Claim is the single transition from queued to leased. A task that is
// delivered twice while the first copy is active gets ErrAlreadyLeased even
// when both copies run in the same process.
func (c *Coordinator) Claim(ctx context.Context, taskType string, payload []byte) (*Lease, error) {
	if c == nil || c.db == nil {
		return nil, errors.New("document queue: coordinator is unavailable")
	}
	if c.fenced.Load() || c.draining.Load() {
		return nil, ErrInstanceFenced
	}
	identity, err := decodeIdentity(payload)
	if err != nil {
		return nil, err
	}
	if identity.DocumentWorkflowID == "" {
		workflow, _, registerErr := c.RegisterWorkflow(ctx, taskType, payload)
		if registerErr != nil {
			return nil, registerErr
		}
		identity.DocumentWorkflowID = workflow.ID
		identity.DocumentWorkflowEpoch = workflow.DispatchEpoch
	}
	var lease Lease
	var fairnessDeferred bool
	now := time.Now()
	documentSchedulerProcessMu.Lock()
	defer documentSchedulerProcessMu.Unlock()
	err = c.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var instance Instance
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("instance_id = ?", c.instanceID).Take(&instance).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInstanceFenced
			}
			return err
		}
		if instance.BootID != c.bootID || instance.State != InstanceReady {
			return ErrInstanceFenced
		}
		if err := lockFairSchedulerTx(tx); err != nil {
			return fmt.Errorf("lock document fair scheduler: %w", err)
		}
		var workflow Workflow
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).
			Where("id = ?", identity.DocumentWorkflowID).Take(&workflow).Error; err != nil {
			return err
		}
		if workflow.TenantID != identity.TenantID || workflow.KnowledgeID != identity.KnowledgeID ||
			workflow.KnowledgeBaseID != identity.KnowledgeBaseID ||
			workflow.ProcessingGeneration != identity.ProcessingGeneration || workflow.TaskType != taskType {
			return ErrStaleDelivery
		}
		if workflow.DispatchEpoch != identity.DocumentWorkflowEpoch {
			return ErrStaleDelivery
		}
		binding := WorkflowBinding{
			WorkflowID: workflow.ID, TenantID: identity.TenantID,
			KnowledgeBaseID: identity.KnowledgeBaseID, KnowledgeID: identity.KnowledgeID,
			ProcessingGeneration: identity.ProcessingGeneration,
			ProcessingOwner:      strings.TrimSpace(identity.ProcessingOwner),
		}
		if err := validateWorkflowBinding(&workflow, binding); err != nil {
			return ErrStaleDelivery
		}
		if workflow.State != StateQueued {
			if workflow.State == StateLeased {
				return ErrAlreadyLeased
			}
			return ErrStaleDelivery
		}
		// The process-local semaphore is the fast-path admission gate, but it
		// cannot be the authority after a database outage. A handler may return
		// and release its local slot while its best-effort lease release fails,
		// leaving a durable leased row owned by this exact boot. Without this
		// transaction-level check the recovered process can claim another
		// capacity worth of documents and temporarily own 2x (or more) its
		// configured limit. The locked instance row serializes this count with
		// every concurrent Claim from the same stable boot.
		var durableLeases int64
		if err := tx.Model(&Workflow{}).
			Where("state = ? AND owner_instance_id = ? AND owner_boot_id = ?",
				StateLeased, c.instanceID, c.bootID).
			Count(&durableLeases).Error; err != nil {
			return fmt.Errorf("count durable instance leases: %w", err)
		}
		if durableLeases >= int64(c.capacity) {
			return ErrInstanceCapacity
		}
		var bound int64
		if err := claimableKnowledgeBindingQuery(tx, binding).Count(&bound).Error; err != nil {
			return err
		}
		if bound != 1 {
			return ErrStaleDelivery
		}
		nextWorkflowID, err := fairNextWorkflowID(tx)
		if err != nil {
			return fmt.Errorf("select fair document workflow: %w", err)
		}
		if nextWorkflowID == "" {
			return ErrStaleDelivery
		}
		if nextWorkflowID != workflow.ID {
			if err := deferFairDeliveryTx(tx, &workflow, now); err != nil {
				return err
			}
			fairnessDeferred = true
			return nil
		}
		leaseUntil := now.Add(c.config.LeaseDuration)
		updates := map[string]interface{}{
			"state": StateLeased, "stage": "core", "owner_instance_id": c.instanceID,
			"owner_boot_id": c.bootID, "lease_until": leaseUntil,
			"last_heartbeat_at": now, "last_error": "", "updated_at": now,
			"version": gorm.Expr("version + 1"),
		}
		if workflow.StartedAt == nil {
			updates["started_at"] = now
		}
		result := tx.Model(&Workflow{}).
			Where("id = ? AND state = ? AND dispatch_epoch = ?", workflow.ID, StateQueued, workflow.DispatchEpoch).
			Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrAlreadyLeased
		}
		if err := recordScheduleAdmissionTx(tx, &workflow, now); err != nil {
			return fmt.Errorf("record document schedule admission: %w", err)
		}
		lease = Lease{
			WorkflowID: workflow.ID, Epoch: workflow.DispatchEpoch,
			TenantID: workflow.TenantID, KnowledgeID: workflow.KnowledgeID,
			KnowledgeBaseID: workflow.KnowledgeBaseID, Generation: workflow.ProcessingGeneration,
			DelegateTimeout:  time.Duration(workflow.DelegateTimeoutNanos),
			DelegateDeadline: workflow.DeadlineAt,
		}
		return nil
	})
	if err != nil {
		result := "error"
		switch {
		case errors.Is(err, ErrStaleDelivery):
			result = "stale"
		case errors.Is(err, ErrAlreadyLeased):
			result = "already_leased"
		case errors.Is(err, ErrInstanceCapacity):
			result = "capacity"
		case errors.Is(err, ErrInstanceFenced):
			result = "fenced"
		}
		pipelineobs.ObserveDocumentWorkflow("claim", result)
		return nil, err
	}
	if fairnessDeferred {
		pipelineobs.ObserveDocumentWorkflow("claim", "fairness_deferred")
		return nil, ErrFairnessDeferred
	}
	pipelineobs.ObserveDocumentWorkflow("claim", "success")
	return &lease, nil
}

type knowledgeSnapshot struct {
	ParseStatus          string
	ProcessingGeneration string
	ProcessingOwner      string
	ProcessedAt          *time.Time
	PendingSubtasksCount int
	EnrichmentStatus     string
	WikiStatus           string
	WikiErrorMessage     string
	UpdatedAt            time.Time
}

func (c *Coordinator) loadKnowledge(ctx context.Context, lease *Lease) (*knowledgeSnapshot, error) {
	var snapshot knowledgeSnapshot
	err := c.db.WithContext(ctx).Table("knowledges").
		Select("parse_status, processing_generation, processing_owner, processed_at, pending_subtasks_count, enrichment_status, wiki_status, wiki_error_message, updated_at").
		Where("tenant_id = ? AND id = ? AND knowledge_base_id = ? AND deleted_at IS NULL",
			lease.TenantID, lease.KnowledgeID, lease.KnowledgeBaseID).
		Take(&snapshot).Error
	if err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func stageForKnowledge(snapshot *knowledgeSnapshot) string {
	if snapshot == nil {
		return "unknown"
	}
	switch snapshot.ParseStatus {
	case types.ParseStatusPending:
		return "queued"
	case types.ParseStatusProcessing:
		if snapshot.ProcessingOwner == "" {
			return "fanout"
		}
		return "core"
	case types.ParseStatusFinalizing:
		if snapshot.PendingSubtasksCount == 0 && snapshot.WikiStatus == types.WikiStatusPending {
			return "wiki"
		}
		return "derivatives"
	case types.ParseStatusCancelling:
		return "cancelling"
	default:
		return snapshot.ParseStatus
	}
}

func (c *Coordinator) wikiPending(ctx context.Context, lease *Lease) (bool, error) {
	dedupKey, err := wikiqueue.IngestDedupKey(lease.KnowledgeID, lease.Generation)
	if err != nil {
		return false, err
	}
	var count int64
	err = c.db.WithContext(ctx).Table("task_pending_ops").
		Where("tenant_id = ? AND task_type = ? AND scope = ? AND scope_id = ? AND op = ? AND dedup_key = ?",
			lease.TenantID, types.TypeWikiIngest, types.TaskScopeKnowledgeBase,
			lease.KnowledgeBaseID, "ingest", dedupKey).
		Count(&count).Error
	return count > 0, err
}

func (c *Coordinator) terminalState(ctx context.Context, lease *Lease, snapshot *knowledgeSnapshot) (WorkflowState, string, bool, error) {
	if snapshot == nil {
		return StateCancelled, "missing", true, nil
	}
	if snapshot.ProcessingGeneration != lease.Generation {
		return StateSuperseded, "superseded", true, nil
	}
	switch snapshot.ParseStatus {
	case types.ParseStatusCompleted:
		switch snapshot.EnrichmentStatus {
		case types.EnrichmentStatusPending:
			return "", "derivatives", false, nil
		case types.EnrichmentStatusFailed:
			return StateFailed, "enrichment_failed", true, nil
		case types.EnrichmentStatusDegraded:
			return StateFailed, "enrichment_degraded", true, nil
		}
		pending, err := c.wikiPending(ctx, lease)
		if err != nil {
			return "", "wiki", false, err
		}
		if pending {
			return "", "wiki", false, nil
		}
		switch snapshot.WikiStatus {
		case types.WikiStatusPending:
			// A Wiki worker persists its terminal generation status before
			// acknowledging the durable pending row. If the row is temporarily
			// absent while status is still pending, fail closed and let Wiki
			// recovery repair the hand-off instead of reporting false success.
			return "", "wiki", false, nil
		case types.WikiStatusFailed:
			return StateFailed, "wiki_failed", true, nil
		case types.WikiStatusDegraded:
			return StateFailed, "wiki_degraded", true, nil
		}
		return StateCompleted, "completed", true, nil
	case types.ParseStatusFailed:
		return StateFailed, "failed", true, nil
	case types.ParseStatusCancelled, types.ParseStatusDeleting:
		return StateCancelled, snapshot.ParseStatus, true, nil
	default:
		return "", stageForKnowledge(snapshot), false, nil
	}
}

func shouldAwaitCommittedDerivatives(snapshot *knowledgeSnapshot, stage string) bool {
	if stage != "wiki" || !coreCommittedForExternalWait(snapshot) {
		return false
	}
	if snapshot.ParseStatus == types.ParseStatusCompleted {
		return true // backward-compatible recovery of pre-gate generations
	}
	return snapshot.ParseStatus == types.ParseStatusFinalizing &&
		snapshot.PendingSubtasksCount == 0
}

// coreCommittedForExternalWait is the durable hand-off fence between the
// document queue and every descendant queue. processed_at proves chunks/indexes
// committed, while the consumed processing_owner proves a retry may no longer
// rebuild core artifacts. Process uses this only after the root delegate has
// returned successfully, which additionally proves that the immutable fan-out
// plan was published (or replayed) before the document slot is released.
func coreCommittedForExternalWait(snapshot *knowledgeSnapshot) bool {
	if snapshot == nil || snapshot.ProcessedAt == nil ||
		strings.TrimSpace(snapshot.ProcessingOwner) != "" {
		return false
	}
	switch snapshot.ParseStatus {
	case types.ParseStatusProcessing, types.ParseStatusFinalizing, types.ParseStatusCompleted:
		return true
	default:
		return false
	}
}

func (c *Coordinator) renew(ctx context.Context, lease *Lease, stage string, progressAt time.Time) error {
	now := time.Now()
	updates := map[string]interface{}{
		"lease_until": now.Add(c.config.LeaseDuration), "last_heartbeat_at": now,
		"stage": stage, "updated_at": now,
	}
	if !progressAt.IsZero() {
		updates["last_progress_at"] = progressAt
	}
	result := c.db.WithContext(ctx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			lease.WorkflowID, StateLeased, lease.Epoch, c.instanceID, c.bootID).
		Where(`EXISTS (
			SELECT 1 FROM custom_document_queue_instances i
			WHERE i.instance_id = ? AND i.boot_id = ? AND i.state IN ?
		)`, c.instanceID, c.bootID, []string{InstanceReady, InstanceDraining, InstanceDegraded}).
		Updates(updates)
	if result.Error != nil {
		pipelineobs.ObserveDocumentWorkflow("renew", "error")
		return result.Error
	}
	if result.RowsAffected != 1 {
		pipelineobs.ObserveDocumentWorkflow("renew", "lease_lost")
		return ErrLeaseLost
	}
	pipelineobs.ObserveDocumentWorkflow("renew", "success")
	return nil
}

func (c *Coordinator) release(ctx context.Context, lease *Lease, cause error) error {
	now := time.Now()
	message := ""
	if cause != nil {
		message = cause.Error()
	}
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseMutationTimeout)
	defer cancel()
	result := c.db.WithContext(mutationCtx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			lease.WorkflowID, StateLeased, lease.Epoch, c.instanceID, c.bootID).
		Updates(map[string]interface{}{
			"state": StateQueued, "stage": "queued", "owner_instance_id": "", "owner_boot_id": "",
			"lease_until": nil, "last_heartbeat_at": nil, "last_error": message,
			"version": gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		pipelineobs.ObserveDocumentWorkflow("release", "error")
		return result.Error
	}
	if result.RowsAffected != 1 {
		pipelineobs.ObserveDocumentWorkflow("release", "lease_lost")
		return ErrLeaseLost
	}
	pipelineobs.ObserveDocumentWorkflow("release", "success")
	return nil
}

func (c *Coordinator) parkWaitingExternal(
	ctx context.Context,
	lease *Lease,
	stage string,
	progressAt time.Time,
) error {
	now := time.Now()
	updates := map[string]interface{}{
		"state": StateWaitingExternal, "stage": stage,
		"owner_instance_id": "", "owner_boot_id": "",
		"lease_until": nil, "last_heartbeat_at": nil, "last_error": "",
		"version": gorm.Expr("version + 1"), "updated_at": now,
	}
	if !progressAt.IsZero() {
		updates["last_progress_at"] = progressAt
	}
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseMutationTimeout)
	defer cancel()
	result := c.db.WithContext(mutationCtx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			lease.WorkflowID, StateLeased, lease.Epoch, c.instanceID, c.bootID).
		Updates(updates)
	if result.Error != nil {
		pipelineobs.ObserveDocumentWorkflow("wait_external", "error")
		return result.Error
	}
	if result.RowsAffected != 1 {
		pipelineobs.ObserveDocumentWorkflow("wait_external", "lease_lost")
		return ErrLeaseLost
	}
	pipelineobs.ObserveDocumentWorkflow("wait_external", "success")
	return nil
}

// reconcileTerminalAttemptSpans enforces the observability side of the
// document-level state machine: once the durable workflow for the current
// generation is terminal, its latest processing attempt cannot legitimately
// retain pending/running nodes. Abrupt pod termination can strand those rows
// after the business writes have already committed; closing them here keeps
// the trace API aligned with PostgreSQL's authoritative workflow outcome.
//
// Historical terminal rows are preserved verbatim. In particular, a failed
// Wiki retry remains visible as history even when a later retry succeeds.
func (c *Coordinator) reconcileTerminalAttemptSpans(
	ctx context.Context,
	knowledgeID string,
	state WorkflowState,
	stage string,
) error {
	if c == nil || c.db == nil || strings.TrimSpace(knowledgeID) == "" {
		return nil
	}
	if !c.db.Migrator().HasTable(&types.KnowledgeProcessingSpan{}) {
		// The lightweight state-machine tests intentionally migrate only the
		// queue tables unless a test is exercising span reconciliation.
		return nil
	}

	var attempt int
	if err := c.db.WithContext(ctx).
		Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ?", knowledgeID).
		Select("COALESCE(MAX(attempt), 0)").
		Row().
		Scan(&attempt); err != nil {
		return fmt.Errorf("read latest processing attempt for %s: %w", knowledgeID, err)
	}
	if attempt <= 0 {
		return nil
	}

	now := time.Now()
	reason := fmt.Sprintf(
		"document workflow reached terminal state %s at stage %s; stale open span reconciled",
		state,
		stage,
	)
	descendants := c.db.WithContext(ctx).
		Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ? AND attempt = ? AND kind <> ? AND status IN ?",
			knowledgeID, attempt, types.SpanKindRoot,
			[]string{types.SpanStatusPending, types.SpanStatusRunning}).
		Updates(map[string]interface{}{
			"status":        types.SpanStatusCancelled,
			"error_code":    "DOCUMENT_WORKFLOW_TERMINAL",
			"error_message": reason,
			"finished_at":   now,
			"updated_at":    now,
		})
	if descendants.Error != nil {
		return fmt.Errorf("reconcile open processing spans for %s attempt %d: %w",
			knowledgeID, attempt, descendants.Error)
	}

	rootStatus := types.SpanStatusCancelled
	rootCode := "DOCUMENT_WORKFLOW_TERMINAL"
	rootMessage := reason
	switch state {
	case StateCompleted:
		rootStatus = types.SpanStatusDone
		rootCode = ""
		rootMessage = ""
	case StateFailed:
		rootStatus = types.SpanStatusFailed
		rootCode = "DOCUMENT_WORKFLOW_FAILED"
	}
	root := c.db.WithContext(ctx).
		Model(&types.KnowledgeProcessingSpan{}).
		Where("knowledge_id = ? AND attempt = ? AND kind = ? AND status IN ?",
			knowledgeID, attempt, types.SpanKindRoot,
			[]string{types.SpanStatusPending, types.SpanStatusRunning}).
		Updates(map[string]interface{}{
			"status":        rootStatus,
			"error_code":    rootCode,
			"error_message": rootMessage,
			"finished_at":   now,
			"updated_at":    now,
		})
	if root.Error != nil {
		return fmt.Errorf("reconcile processing root for %s attempt %d: %w",
			knowledgeID, attempt, root.Error)
	}
	if descendants.RowsAffected+root.RowsAffected > 0 {
		logger.Infof(ctx,
			"[document queue] reconciled %d stale open span(s) for terminal workflow knowledge=%s attempt=%d state=%s",
			descendants.RowsAffected+root.RowsAffected, knowledgeID, attempt, state)
	}
	return nil
}

// reconcileTerminalSpanOrphans is the restart/failover repair path for
// workflows that became terminal before this invariant was introduced or
// whose process died in the narrow interval between workflow commit and span
// cleanup. The generation join is essential: an old terminal workflow must
// never close spans belonging to a newer active reparse of the same document.
func (c *Coordinator) reconcileTerminalSpanOrphans(ctx context.Context) error {
	if c == nil || c.db == nil || !c.db.Migrator().HasTable(&types.KnowledgeProcessingSpan{}) {
		return nil
	}
	type candidate struct {
		KnowledgeID string
		State       WorkflowState
		Stage       string
		UpdatedAt   time.Time
	}
	var candidates []candidate
	err := c.db.WithContext(ctx).
		Table("custom_document_queue_workflows AS w").
		Select("DISTINCT w.knowledge_id, w.state, w.stage, w.updated_at").
		Joins(`JOIN knowledges AS k
			ON k.id = w.knowledge_id
			AND k.tenant_id = w.tenant_id
			AND k.processing_generation = w.processing_generation
			AND k.deleted_at IS NULL`).
		Where("w.state IN ?", []WorkflowState{
			StateCompleted, StateFailed, StateCancelled, StateSuperseded,
		}).
		Where(`EXISTS (
			SELECT 1
			FROM knowledge_processing_spans AS s
			WHERE s.knowledge_id = w.knowledge_id
			  AND s.attempt = (
				SELECT MAX(latest.attempt)
				FROM knowledge_processing_spans AS latest
				WHERE latest.knowledge_id = w.knowledge_id
			  )
			  AND s.status IN ?
		)`, []string{types.SpanStatusPending, types.SpanStatusRunning}).
		Order("w.updated_at ASC").
		Limit(c.config.RecoveryBatchSize).
		Scan(&candidates).Error
	if err != nil {
		return fmt.Errorf("list terminal workflows with open spans: %w", err)
	}
	var errs []error
	for _, item := range candidates {
		if err := c.reconcileTerminalAttemptSpans(
			ctx, item.KnowledgeID, item.State, item.Stage,
		); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (c *Coordinator) finish(ctx context.Context, lease *Lease, state WorkflowState, stage string) error {
	now := time.Now()
	lastError := workflowTerminalError(state, stage, nil)
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseMutationTimeout)
	defer cancel()
	result := c.db.WithContext(mutationCtx).Model(&Workflow{}).
		Where("id = ? AND state = ? AND dispatch_epoch = ? AND owner_instance_id = ? AND owner_boot_id = ?",
			lease.WorkflowID, StateLeased, lease.Epoch, c.instanceID, c.bootID).
		Updates(map[string]interface{}{
			"state": state, "stage": stage, "owner_instance_id": "", "owner_boot_id": "",
			"lease_until": nil, "last_heartbeat_at": nil, "completed_at": now,
			"last_error": lastError, "terminal_diagnostic": workflowTerminalDiagnostic(state, "", lastError),
			"version": gorm.Expr("version + 1"), "updated_at": now,
		})
	if result.Error != nil {
		pipelineobs.ObserveDocumentWorkflow("finish", "error")
		return result.Error
	}
	if result.RowsAffected != 1 {
		pipelineobs.ObserveDocumentWorkflow("finish", "lease_lost")
		return ErrLeaseLost
	}
	if err := c.reconcileTerminalAttemptSpans(
		mutationCtx, lease.KnowledgeID, state, stage,
	); err != nil {
		// Span rows are diagnostic and must never roll back an already durable
		// business completion. Recovery retries this repair on every replica.
		logger.Warnf(mutationCtx,
			"[document queue] terminal span reconciliation failed workflow=%s knowledge=%s: %v",
			lease.WorkflowID, lease.KnowledgeID, err)
	}
	pipelineobs.ObserveDocumentWorkflow("finish", string(state))
	return nil
}

func (c *Coordinator) observe(ctx context.Context, lease *Lease) (*knowledgeSnapshot, WorkflowState, string, bool, error) {
	if c.observeHook != nil {
		if err := c.observeHook(ctx, lease); err != nil {
			return nil, "", "", false, err
		}
	}
	snapshot, err := c.loadKnowledge(ctx, lease)
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, StateCancelled, "missing", true, nil
	}
	if err != nil {
		return nil, "", "", false, err
	}
	state, stage, terminal, err := c.terminalState(ctx, lease, snapshot)
	return snapshot, state, stage, terminal, err
}

// Process wraps a root document/manual handler. It owns one per-instance slot
// only through the usable core commit and successful publication of the
// immutable fan-out plan. Required derivatives keep the durable workflow and
// user-visible lifecycle non-terminal, but run in their own queues without
// preventing later documents from entering core parsing.
func (c *Coordinator) Process(ctx context.Context, task *asynq.Task, delegate asynq.HandlerFunc) (retErr error) {
	if delegate == nil {
		return errors.New("document queue: root handler is unavailable")
	}
	if c == nil || c.db == nil {
		return delegate(ctx, task)
	}
	releaseSlot, err := c.acquireSlot(ctx)
	if err != nil {
		return err
	}
	defer releaseSlot()

	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	activeKey, err := c.reserveExecution(cancel)
	if err != nil {
		return err
	}
	defer func() { c.removeExecution(activeKey) }()

	lease, err := c.Claim(execCtx, task.Type(), task.Payload())
	if errors.Is(err, ErrStaleDelivery) {
		return nil
	}
	if errors.Is(err, ErrAlreadyLeased) || errors.Is(err, ErrInstanceFenced) {
		// The router ACKs this obsolete Redis copy. PostgreSQL remains the
		// durable authority and republishes if/when its current owner releases.
		return err
	}
	if err != nil {
		return err
	}
	// Admit the next durable fair head only after this row owns a lease. This
	// rapidly fills newly added replicas while keeping the Redis document lane
	// bounded to approximately one pending wake-up instead of the whole
	// PostgreSQL backlog. Failure is harmless: the recovery loop retries the
	// outbox without failing work that already owns a lease.
	if _, dispatchErr := c.dispatchNextQueued(execCtx); dispatchErr != nil &&
		!errors.Is(dispatchErr, asynq.ErrTaskIDConflict) &&
		!errors.Is(dispatchErr, ErrStaleDelivery) {
		logger.Warnf(execCtx,
			"[document queue] next fair delivery deferred after claim workflow=%s: %v",
			lease.WorkflowID, dispatchErr,
		)
	}
	pipelineobs.DocumentWorkerStarted()
	defer pipelineobs.DocumentWorkerStopped()

	activeKey = c.bindExecution(activeKey, lease, cancel)
	defer func() {
		if recovered := recover(); recovered != nil {
			panicErr := fmt.Errorf("document workflow handler panic: %v\n%s", recovered, debug.Stack())
			if releaseErr := c.release(context.Background(), lease, panicErr); releaseErr != nil {
				logger.Errorf(context.Background(),
					"[document queue] panic lease release failed workflow=%s epoch=%d: %v",
					lease.WorkflowID, lease.Epoch, releaseErr,
				)
				retErr = errors.Join(panicErr, releaseErr)
				return
			}
			retErr = panicErr
		}
	}()

	// Asynq cancels ctx before a timed-out/aborted perform goroutine has
	// necessarily returned. Keep the PostgreSQL lease alive until this wrapper
	// actually exits, otherwise another pod could run the same generation while
	// the old delegate is still writing.
	leaseCtx, leaseCancel := context.WithCancel(context.WithoutCancel(ctx))
	defer leaseCancel()
	renewStop := make(chan struct{})
	renewDone := make(chan struct{})
	leaseLost := make(chan error, 1)
	go c.renewLoop(leaseCtx, lease, renewStop, renewDone, leaseLost, cancel)
	defer func() {
		close(renewStop)
		<-renewDone
	}()

	snapshot, state, stage, terminal, observeErr := c.observe(execCtx, lease)
	if observeErr != nil {
		if releaseErr := c.release(execCtx, lease, observeErr); releaseErr != nil {
			return errors.Join(observeErr, releaseErr)
		}
		return observeErr
	}
	if terminal {
		return c.finish(execCtx, lease, state, stage)
	}

	// A recovered workflow whose core and enrichment facts are already
	// committed must not invoke the root parser again. It simply reacquires a
	// document slot and waits for the durable Wiki intent to settle.
	if !shouldAwaitCommittedDerivatives(snapshot, stage) {
		delegateCtx := execCtx
		delegateCancel := func() {}
		if lease.DelegateDeadline != nil {
			delegateCtx, delegateCancel = context.WithDeadline(execCtx, *lease.DelegateDeadline)
		}
		if lease.DelegateTimeout > 0 {
			if deadline, ok := delegateCtx.Deadline(); !ok || time.Now().Add(lease.DelegateTimeout).Before(deadline) {
				delegateCancel()
				delegateCtx, delegateCancel = context.WithTimeout(execCtx, lease.DelegateTimeout)
			}
		}
		delegateReturned := make(chan struct{})
		watchdogDone := c.watchDelegate(delegateCtx, delegateReturned, lease)
		var delegateErr error
		func() {
			defer close(delegateReturned)
			delegateErr = delegate(delegateCtx, task)
		}()
		<-watchdogDone
		delegateCancel()
		if delegateErr != nil {
			select {
			case lostErr := <-leaseLost:
				return errors.Join(delegateErr, lostErr)
			default:
			}
			if releaseErr := c.release(execCtx, lease, delegateErr); releaseErr != nil {
				return errors.Join(delegateErr, releaseErr)
			}
			return delegateErr
		}
	}

	ticker := time.NewTicker(c.config.WorkflowPollInterval)
	defer ticker.Stop()
	lastPersistedStage := ""
	lastPersistedProgress := time.Time{}
	for {
		snapshot, state, stage, terminal, observeErr := c.observe(execCtx, lease)
		if observeErr != nil {
			if releaseErr := c.release(execCtx, lease, observeErr); releaseErr != nil {
				return errors.Join(observeErr, releaseErr)
			}
			return observeErr
		}
		if terminal {
			return c.finish(execCtx, lease, state, stage)
		}
		progressAt := time.Time{}
		if snapshot != nil {
			progressAt = snapshot.UpdatedAt
		}
		if coreCommittedForExternalWait(snapshot) {
			return c.parkWaitingExternal(execCtx, lease, stage, progressAt)
		}
		// Persist an actual stage/business-progress transition immediately so
		// queue status and takeover diagnostics stay current. An unchanged fast
		// observation remains read-only; renewLoop is the periodic lease writer.
		// This avoids every document writing PostgreSQL twice per poll while
		// retaining the state-machine transition that callers and tests rely on.
		if stage != lastPersistedStage || !progressAt.Equal(lastPersistedProgress) {
			if err := c.renew(execCtx, lease, stage, progressAt); err != nil {
				cancel()
				return err
			}
			lastPersistedStage = stage
			lastPersistedProgress = progressAt
		}
		select {
		case <-execCtx.Done():
			cause := execCtx.Err()
			select {
			case lostErr := <-leaseLost:
				cause = lostErr
			default:
			}
			if releaseErr := c.release(execCtx, lease, cause); releaseErr != nil && !errors.Is(releaseErr, ErrLeaseLost) {
				return errors.Join(cause, releaseErr)
			}
			return cause
		case lostErr := <-leaseLost:
			return lostErr
		case <-ticker.C:
		}
	}
}

// watchDelegate detects a process-local failure mode that Redis and database
// heartbeats cannot see: a parser ignored context cancellation and kept
// writing after its deadline. The workflow lease remains renewed (preventing
// duplicate takeover), while liveness becomes unhealthy so the orchestrator
// can terminate the whole process and provide a hard execution boundary.
func (c *Coordinator) watchDelegate(
	delegateCtx context.Context,
	returned <-chan struct{},
	lease *Lease,
) <-chan struct{} {
	done := make(chan struct{})
	deadline, hasDeadline := delegateCtx.Deadline()
	if c == nil || !hasDeadline {
		close(done)
		return done
	}
	go func() {
		defer close(done)
		wait := time.Until(deadline) + c.config.StuckHandlerGrace
		if wait < 0 {
			wait = 0
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-returned:
			return
		case <-timer.C:
			c.stuck.Add(1)
			workflowID := ""
			if lease != nil {
				workflowID = lease.WorkflowID
			}
			logger.Errorf(context.Background(),
				"[document queue] delegate ignored cancellation beyond deadline; marking instance unhealthy instance=%s workflow=%s",
				c.instanceID, workflowID,
			)
			<-returned
			c.stuck.Add(-1)
		}
	}()
	return done
}

func (c *Coordinator) renewLoop(
	ctx context.Context,
	lease *Lease,
	stop <-chan struct{},
	done chan<- struct{},
	lost chan<- error,
	cancel context.CancelFunc,
) {
	defer close(done)
	ticker := time.NewTicker(c.config.HeartbeatInterval)
	defer ticker.Stop()
	consecutiveFailures := 0
	cancelSignaled := false
	for {
		select {
		case <-ctx.Done():
			return
		case <-stop:
			return
		case <-ticker.C:
			snapshot, _, stage, _, err := c.observe(ctx, lease)
			if err == nil {
				progressAt := time.Time{}
				if snapshot != nil {
					progressAt = snapshot.UpdatedAt
				}
				err = c.renew(ctx, lease, stage, progressAt)
			}
			if err == nil {
				consecutiveFailures = 0
				continue
			}
			consecutiveFailures++
			if errors.Is(err, ErrLeaseLost) {
				wrapped := fmt.Errorf("%w: %v", ErrLeaseLost, err)
				select {
				case lost <- wrapped:
				default:
				}
				cancel()
				return
			}
			if consecutiveFailures >= 3 && !cancelSignaled {
				// Ask cooperative business code to stop, but keep renewing. A DB
				// outage can outlive the current lease and Asynq may already have
				// moved the delivery to retry; stopping this goroutine would make a
				// still-running, cancellation-ignoring handler indistinguishable
				// from a dead one and permit unsafe concurrent takeover.
				cancelSignaled = true
				cancel()
			}
		}
	}
}

const maxQueueStatusKnowledgeIDs = 2000

func normalizeKnowledgeIDs(ids []string) []string {
	result := make([]string, 0, len(ids))
	seen := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		result = append(result, id)
		if len(result) >= maxQueueStatusKnowledgeIDs {
			break
		}
	}
	return result
}

type positionRow struct {
	TenantID     uint64
	KnowledgeID  string
	Position     int64
	State        WorkflowState
	Stage        string
	WaitingTotal int64
}

func (c *Coordinator) QueueStatus(ctx context.Context, tenantID uint64, knowledgeIDs []string) (QueueStatus, error) {
	status := QueueStatus{Items: make(map[string]QueueItem)}
	ids := normalizeKnowledgeIDs(knowledgeIDs)
	for _, id := range ids {
		status.Items[id] = QueueItem{State: "none"}
	}
	if c == nil || c.db == nil {
		return status, nil
	}
	currentJoin := "JOIN knowledges k ON k.id = w.knowledge_id AND k.tenant_id = w.tenant_id AND k.processing_generation = w.processing_generation AND k.deleted_at IS NULL"
	if err := c.db.WithContext(ctx).Table(Workflow{}.TableName()+" w").Joins(currentJoin).
		Where("w.state = ?", StateLeased).Count(&status.ActiveTotal).Error; err != nil {
		return status, err
	}
	cutoff := time.Now().Add(-c.config.InstanceStaleAfter)
	if err := c.db.WithContext(ctx).Model(&Instance{}).
		Where("state = ? AND last_heartbeat_at >= ?", InstanceReady, cutoff).
		Select("COALESCE(SUM(capacity), 0)").Scan(&status.CapacityTotal).Error; err != nil {
		return status, err
	}
	if tenantID == 0 || len(ids) == 0 {
		if err := c.db.WithContext(ctx).Table(Workflow{}.TableName()+" w").Joins(currentJoin).
			Where("w.state = ?", StateQueued).Count(&status.WaitingTotal).Error; err != nil {
			return status, err
		}
		return status, nil
	}
	var rows []positionRow
	raw := `
		WITH active_groups AS (
			SELECT w.tenant_id, w.knowledge_base_id, COUNT(*) AS active_count
			FROM custom_document_queue_workflows w
			JOIN knowledges k ON k.id = w.knowledge_id
			 AND k.tenant_id = w.tenant_id
			 AND k.processing_generation = w.processing_generation
			 AND k.deleted_at IS NULL
			WHERE w.state = ?
			GROUP BY w.tenant_id, w.knowledge_base_id
		), ranked_by_group AS (
			SELECT w.tenant_id, w.knowledge_id, w.state, w.stage,
			       w.enqueued_at, w.id, w.knowledge_base_id,
			       ROW_NUMBER() OVER (
			           PARTITION BY w.tenant_id, w.knowledge_base_id
			           ORDER BY w.enqueued_at ASC, w.id ASC
			       ) AS group_position,
			       COALESCE(a.active_count, 0) AS active_count,
			       sg.last_admitted_at AS schedule_last_admitted_at
			FROM custom_document_queue_workflows w
			JOIN knowledges k ON k.id = w.knowledge_id
			 AND k.tenant_id = w.tenant_id
			 AND k.processing_generation = w.processing_generation
			 AND k.deleted_at IS NULL
			LEFT JOIN active_groups a
			  ON a.tenant_id = w.tenant_id
			 AND a.knowledge_base_id = w.knowledge_base_id
			LEFT JOIN custom_document_queue_schedule_groups sg
			  ON sg.tenant_id = w.tenant_id
			 AND sg.knowledge_base_id = w.knowledge_base_id
			WHERE w.state = ?
		), ranked AS (
			SELECT tenant_id, knowledge_id, state, stage,
			       ROW_NUMBER() OVER (
			           ORDER BY group_position ASC,
			                    active_count ASC,
			                    CASE WHEN schedule_last_admitted_at IS NULL THEN 0 ELSE 1 END ASC,
			                    schedule_last_admitted_at ASC,
			                    enqueued_at ASC,
			                    id ASC
			       ) AS position
			FROM ranked_by_group
		), selected AS (
			SELECT tenant_id, knowledge_id, position, state, stage
			FROM ranked
			WHERE tenant_id = ? AND knowledge_id IN ?
		), totals AS (
			SELECT COUNT(*) AS waiting_total FROM ranked
		)
		SELECT COALESCE(selected.tenant_id, 0) AS tenant_id,
		       COALESCE(selected.knowledge_id, '') AS knowledge_id,
		       COALESCE(selected.position, 0) AS position,
		       COALESCE(selected.state, '') AS state,
		       COALESCE(selected.stage, '') AS stage,
		       totals.waiting_total
		FROM totals
		LEFT JOIN selected ON 1 = 1`
	if err := c.db.WithContext(ctx).Raw(
		raw, StateLeased, StateQueued, tenantID, ids,
	).Scan(&rows).Error; err != nil {
		return status, err
	}
	if len(rows) > 0 {
		status.WaitingTotal = rows[0].WaitingTotal
	}
	for _, row := range rows {
		if row.KnowledgeID == "" {
			continue
		}
		status.Items[row.KnowledgeID] = QueueItem{Position: row.Position, State: "waiting", Stage: row.Stage}
	}
	var active []Workflow
	if err := c.db.WithContext(ctx).Table(Workflow{}.TableName()+" w").
		Select("w.*").Joins(currentJoin).
		Where("w.tenant_id = ? AND w.knowledge_id IN ? AND w.state IN ?",
			tenantID, ids, []WorkflowState{StateLeased, StateWaitingExternal}).
		Order("w.created_at DESC").Find(&active).Error; err != nil {
		return status, err
	}
	for _, workflow := range active {
		if status.Items[workflow.KnowledgeID].State == "waiting" {
			continue
		}
		status.Items[workflow.KnowledgeID] = QueueItem{
			State: "active", Stage: workflow.Stage,
			OwnerInstanceID: workflow.OwnerInstanceID,
			OwnerBootID:     workflow.OwnerBootID,
			ExecutionEpoch:  workflow.DispatchEpoch,
			LeaseUntil:      workflow.LeaseUntil,
			LastProgressAt:  workflow.LastProgressAt,
		}
	}
	return status, nil
}

func (c *Coordinator) ListInstances(ctx context.Context) ([]InstanceStatus, error) {
	if c == nil || c.db == nil {
		return nil, nil
	}
	var instances []Instance
	if err := c.db.WithContext(ctx).Order("instance_id ASC").Find(&instances).Error; err != nil {
		return nil, err
	}
	var active []Workflow
	if err := c.db.WithContext(ctx).Where("state = ?", StateLeased).
		Order("enqueued_at ASC").Find(&active).Error; err != nil {
		return nil, err
	}
	byOwner := make(map[string][]string)
	for _, workflow := range active {
		key := workflow.OwnerInstanceID + "\x00" + workflow.OwnerBootID
		byOwner[key] = append(byOwner[key], workflow.KnowledgeID)
	}
	cutoff := time.Now().Add(-c.config.InstanceStaleAfter)
	result := make([]InstanceStatus, 0, len(instances))
	for _, instance := range instances {
		documents := byOwner[instance.InstanceID+"\x00"+instance.BootID]
		result = append(result, InstanceStatus{
			Instance: instance, ActiveCount: int64(len(documents)),
			ActiveDocuments: documents,
			Healthy:         instance.State == InstanceReady && !instance.LastHeartbeatAt.Before(cutoff),
		})
	}
	return result, nil
}
