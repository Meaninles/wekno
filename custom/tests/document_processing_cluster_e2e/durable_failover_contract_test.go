package document_processing_cluster_e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hibiken/asynq"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/types"
)

// These tests form the deterministic, independently runnable durability suite.
// Each test stops at a named producer/consumer crash boundary. The ordinary
// parsing E2E deliberately does not own these assertions: a functional parser
// pass must not mask a lost or duplicated workflow transition.

func offlineQueueClient(t *testing.T) *asynq.Client {
	t.Helper()
	client := asynq.NewClient(asynq.RedisClientOpt{
		Addr: "127.0.0.1:1", DialTimeout: 20 * time.Millisecond,
		ReadTimeout: 20 * time.Millisecond, WriteTimeout: 20 * time.Millisecond,
	})
	t.Cleanup(func() { _ = client.Close() })
	return client
}

func insertUnboundKnowledge(t *testing.T, db *gorm.DB, workflow *documentqueue.Workflow) documentqueue.WorkflowBinding {
	t.Helper()
	binding, err := documentqueue.BindingForWorkflow(workflow)
	if err != nil {
		t.Fatalf("derive workflow binding: %v", err)
	}
	if err := db.Exec(`INSERT INTO knowledges
		(id, tenant_id, knowledge_base_id, processing_generation, processing_owner, processing_workflow_id, parse_status)
		VALUES (?, ?, ?, ?, ?, '', 'pending')`, binding.KnowledgeID, binding.TenantID,
		binding.KnowledgeBaseID, binding.ProcessingGeneration, binding.ProcessingOwner).Error; err != nil {
		t.Fatalf("insert unbound knowledge: %v", err)
	}
	return binding
}

func prepareContractWorkflow(
	t *testing.T, db *gorm.DB, coordinator *documentqueue.Coordinator, tenantID uint64, knowledgeID string,
) (*documentqueue.Workflow, documentqueue.WorkflowBinding) {
	t.Helper()
	payload := rootPayload(t, tenantID, "kb-"+knowledgeID, knowledgeID, "generation-"+knowledgeID, nil)
	workflow, created, err := coordinator.PrepareWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	if err != nil {
		t.Fatalf("prepare workflow: %v", err)
	}
	if !created || workflow.State != documentqueue.StatePreparing {
		t.Fatalf("prepared workflow = created %v state %s, want true/preparing", created, workflow.State)
	}
	binding := insertUnboundKnowledge(t, db, workflow)
	return workflow, binding
}

func newDurableContractCoordinator(
	t *testing.T, db *gorm.DB, client *asynq.Client, instanceID, bootID string,
) *documentqueue.Coordinator {
	t.Helper()
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, client, instanceID, bootID, 2, coordinatorConfig(),
	)
	return coordinator
}

func workflowDelivery(t *testing.T, workflow *documentqueue.Workflow) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(workflow.Payload, &fields); err != nil {
		t.Fatalf("decode workflow payload: %v", err)
	}
	fields["document_workflow_id"] = workflow.ID
	fields["document_workflow_epoch"] = workflow.DispatchEpoch
	delivery, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal workflow delivery: %v", err)
	}
	return delivery
}

func loadContractWorkflow(t *testing.T, db *gorm.DB, id string) documentqueue.Workflow {
	t.Helper()
	var workflow documentqueue.Workflow
	if err := db.Where("id = ?", id).Take(&workflow).Error; err != nil {
		t.Fatalf("load workflow %s: %v", id, err)
	}
	return workflow
}

func TestDurableFailoverKillPointPreparingUnboundIsInvisible(t *testing.T) {
	db := openQueueContractDB(t)
	coordinator := newDurableContractCoordinator(t, db, offlineQueueClient(t), "prepare-worker", "prepare-boot")
	workflow, binding := prepareContractWorkflow(t, db, coordinator, 101, "durable-preparing")

	status, err := coordinator.QueueStatus(context.Background(), binding.TenantID, []string{binding.KnowledgeID})
	if err != nil {
		t.Fatalf("queue status: %v", err)
	}
	if status.WaitingTotal != 0 || status.ActiveTotal != 0 || status.Items[binding.KnowledgeID].State != "none" {
		t.Fatalf("unbound preparing workflow leaked into queue status: %+v", status)
	}
	if _, _, err := coordinator.ActivatePreparedWorkflow(context.Background(), binding); !errors.Is(err, documentqueue.ErrWorkflowNotBound) {
		t.Fatalf("unbound activation error = %v, want ErrWorkflowNotBound", err)
	}
	_ = coordinator.RecoverNow(context.Background()) // Redis is deliberately unavailable.
	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StatePreparing || persisted.DispatchTaskID != "" {
		t.Fatalf("unbound prepare was made visible after recovery: %+v", persisted)
	}
}

func TestDurableFailoverKillPointBoundBeforeActivateRecoversOnce(t *testing.T) {
	db := openQueueContractDB(t)
	client := offlineQueueClient(t)
	first := newDurableContractCoordinator(t, db, client, "recovery-a", "boot-a")
	second := newDurableContractCoordinator(t, db, client, "recovery-b", "boot-b")
	workflow, binding := prepareContractWorkflow(t, db, first, 102, "durable-bound")
	if err := first.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("commit exact business binding: %v", err)
	}
	before := loadContractWorkflow(t, db, workflow.ID)

	// This is the producer crash point: the business row is committed, but no
	// producer calls Activate. Competing replicas must converge on one CAS.
	start := make(chan struct{})
	var group sync.WaitGroup
	for _, candidate := range []*documentqueue.Coordinator{first, second} {
		group.Add(1)
		go func(coordinator *documentqueue.Coordinator) {
			defer group.Done()
			<-start
			_ = coordinator.RecoverNow(context.Background())
		}(candidate)
	}
	close(start)
	group.Wait()

	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued {
		t.Fatalf("bound preparing workflow state = %s, want queued", persisted.State)
	}
	if persisted.Version != before.Version+1 {
		t.Fatalf("activation version = %d, want one CAS transition from %d", persisted.Version, before.Version)
	}
	if persisted.DispatchTaskID == "" {
		t.Fatal("recovery did not persist the stable delivery identity before the Redis outage")
	}
	status, err := first.QueueStatus(context.Background(), binding.TenantID, []string{binding.KnowledgeID})
	if err != nil || status.WaitingTotal != 1 || status.Items[binding.KnowledgeID].Position != 1 {
		t.Fatalf("recovered queue status = %+v err=%v", status, err)
	}
}

func TestDurableFailoverKillPointQueuedBeforeRedisRemainsRecoverable(t *testing.T) {
	db := openQueueContractDB(t)
	coordinator := newDurableContractCoordinator(t, db, offlineQueueClient(t), "queued-worker", "queued-boot")
	workflow, binding := prepareContractWorkflow(t, db, coordinator, 103, "durable-queued")
	if err := coordinator.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, activated, err := coordinator.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil || !activated {
		t.Fatalf("activate workflow: activated=%v err=%v", activated, err)
	}

	// This is the outbox crash point: state=queued exists but Redis has not
	// accepted a task. Failed publication must never roll back acceptance.
	for attempt := 0; attempt < 3; attempt++ {
		if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
			Update("last_dispatched_at", nil).Error; err != nil {
			t.Fatalf("advance recovery tick: %v", err)
		}
		if err := coordinator.RecoverNow(context.Background()); err == nil {
			t.Fatal("offline Redis recovery unexpectedly succeeded")
		}
	}
	persisted := loadContractWorkflow(t, db, workflow.ID)
	wantTaskID := fmt.Sprintf("document-workflow:%s:%d", workflow.ID, workflow.DispatchEpoch)
	if persisted.State != documentqueue.StateQueued || persisted.DispatchEpoch != workflow.DispatchEpoch {
		t.Fatalf("Redis outage changed durable ownership: %+v", persisted)
	}
	if persisted.DispatchTaskID != wantTaskID {
		t.Fatalf("stable dispatch task ID = %q, want %q", persisted.DispatchTaskID, wantTaskID)
	}
	if persisted.DispatchAttempts < 1 {
		t.Fatal("Redis outage did not leave observable delivery attempts")
	}
}

func TestDurableFailoverStableInstanceNewBootAdoptsOnlyItsLease(t *testing.T) {
	db := openQueueContractDB(t)
	oldBoot := newDurableContractCoordinator(t, db, nil, "stable-parser", "boot-old")
	if err := oldBoot.Start(context.Background()); err != nil {
		t.Fatalf("start old boot: %v", err)
	}
	workflow, binding := prepareContractWorkflow(t, db, oldBoot, 104, "durable-reboot")
	if err := oldBoot.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, _, err := oldBoot.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil {
		t.Fatalf("activate workflow: %v", err)
	}
	oldDelivery := workflowDelivery(t, workflow)
	if _, err := oldBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); err != nil {
		t.Fatalf("old boot claim: %v", err)
	}
	oldBoot.Stop()

	newBoot := newDurableContractCoordinator(t, db, nil, "stable-parser", "boot-new")
	if err := newBoot.Start(context.Background()); err != nil {
		t.Fatalf("start new boot: %v", err)
	}
	t.Cleanup(newBoot.Stop)
	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued || persisted.DispatchEpoch != workflow.DispatchEpoch+1 {
		t.Fatalf("same-instance reboot did not adopt lease: %+v", persisted)
	}
	if persisted.OwnerInstanceID != "" || persisted.OwnerBootID != "" || persisted.LeaseUntil != nil {
		t.Fatalf("adopted workflow retained old ownership: %+v", persisted)
	}
	if _, err := oldBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); !errors.Is(err, documentqueue.ErrInstanceFenced) && !errors.Is(err, documentqueue.ErrStaleDelivery) {
		t.Fatalf("old boot delivery was not fenced: %v", err)
	}
}

func TestDurableFailoverCrossInstanceRequiresTerminationProofHeartbeatAndLeaseExpiry(t *testing.T) {
	db := openQueueContractDB(t)
	config := coordinatorConfig()
	owner := newDurableContractCoordinator(t, db, nil, "owner-parser", "owner-boot")
	if err := owner.Start(context.Background()); err != nil {
		t.Fatalf("start owner: %v", err)
	}
	t.Cleanup(owner.Stop)
	workflow, binding := prepareContractWorkflow(t, db, owner, 105, "durable-takeover")
	if err := owner.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, _, err := owner.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil {
		t.Fatalf("activate workflow: %v", err)
	}
	if _, err := owner.Claim(context.Background(), workflow.TaskType, workflowDelivery(t, workflow)); err != nil {
		t.Fatalf("owner claim: %v", err)
	}
	survivor := newDurableContractCoordinator(t, db, offlineQueueClient(t), "survivor-parser", "survivor-boot")

	now := time.Now()
	expired := now.Add(-time.Minute)
	future := now.Add(time.Hour)
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Update("lease_until", expired).Error; err != nil {
		t.Fatalf("expire only lease: %v", err)
	}
	_ = survivor.RecoverNow(context.Background())
	if state := loadContractWorkflow(t, db, workflow.ID).State; state != documentqueue.StateLeased {
		t.Fatalf("fresh heartbeat allowed takeover with state %s", state)
	}

	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Update("lease_until", future).Error; err != nil {
		t.Fatalf("restore future lease: %v", err)
	}
	if err := db.Model(&documentqueue.Instance{}).Where("instance_id = ?", owner.InstanceID()).
		Update("last_heartbeat_at", now.Add(-config.InstanceStaleAfter-time.Minute)).Error; err != nil {
		t.Fatalf("stale owner heartbeat: %v", err)
	}
	_ = survivor.RecoverNow(context.Background())
	if state := loadContractWorkflow(t, db, workflow.ID).State; state != documentqueue.StateLeased {
		t.Fatalf("future lease allowed takeover with state %s", state)
	}

	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Update("lease_until", expired).Error; err != nil {
		t.Fatalf("expire final lease: %v", err)
	}
	if err := survivor.RecoverNow(context.Background()); err != nil {
		t.Fatalf("recover stale owner: %v", err)
	}
	if state := loadContractWorkflow(t, db, workflow.ID).State; state != documentqueue.StateLeased {
		t.Fatalf("stale heartbeat without termination proof allowed takeover with state %s", state)
	}
	if err := survivor.ConfirmInstanceTermination(
		context.Background(), owner.InstanceID(), owner.BootID(), "contract-runtime-terminated",
	); err != nil {
		t.Fatalf("attest exact owner termination: %v", err)
	}
	if err := survivor.RecoverNow(context.Background()); err != nil {
		t.Fatalf("recover terminated owner: %v", err)
	}
	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued || persisted.DispatchEpoch != workflow.DispatchEpoch+1 {
		t.Fatalf("fully stale workflow was not reclaimed: %+v", persisted)
	}
}

func durableRedisOption(t *testing.T) asynq.RedisClientOpt {
	t.Helper()
	if os.Getenv("WEKNORA_DURABLE_FAILOVER_REDIS_CONTRACT") != "1" {
		t.Skip("set WEKNORA_DURABLE_FAILOVER_REDIS_CONTRACT=1 for Redis durability contracts")
	}
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		t.Fatal("REDIS_ADDR is required for Redis durability contracts")
	}
	database := 14
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_DURABLE_FAILOVER_REDIS_DB")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil {
			t.Fatalf("parse WEKNORA_DURABLE_FAILOVER_REDIS_DB: %v", err)
		}
		database = parsed
	}
	return asynq.RedisClientOpt{
		Addr: addr, Username: os.Getenv("REDIS_USERNAME"), Password: os.Getenv("REDIS_PASSWORD"), DB: database,
		DialTimeout: time.Second, ReadTimeout: time.Second, WriteTimeout: time.Second,
	}
}

func cleanDurableRedisQueue(t *testing.T, option asynq.RedisClientOpt) *asynq.Inspector {
	t.Helper()
	inspector := asynq.NewInspector(option)
	queues, err := inspector.Queues()
	if err != nil {
		_ = inspector.Close()
		t.Fatalf("connect Redis contract DB %d: %v", option.DB, err)
	}
	for _, queue := range queues {
		if queue != types.QueueDocument {
			continue
		}
		info, infoErr := inspector.GetQueueInfo(types.QueueDocument)
		if infoErr != nil {
			_ = inspector.Close()
			t.Fatalf("inspect Redis contract queue: %v", infoErr)
		}
		if info.Size != 0 {
			_ = inspector.Close()
			t.Skipf("Redis contract DB %d document queue is not empty", option.DB)
		}
	}
	t.Cleanup(func() {
		err := inspector.DeleteQueue(types.QueueDocument, true)
		if err != nil && !redisQueueOrTaskMissing(err) {
			t.Errorf("delete Redis durability queue: %v", err)
		}
		_ = inspector.Close()
	})
	return inspector
}

func redisQueueOrTaskMissing(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return errors.Is(err, asynq.ErrQueueNotFound) || errors.Is(err, asynq.ErrTaskNotFound) ||
		strings.Contains(message, "does not exist") || strings.Contains(message, "not_found") ||
		strings.Contains(message, "not found")
}

func TestDurableFailoverRedisRecoveryPublishesOneStableDelivery(t *testing.T) {
	option := durableRedisOption(t)
	inspector := cleanDurableRedisQueue(t, option)
	client := asynq.NewClient(option)
	if err := client.Ping(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	db := openQueueContractDB(t)
	offline := newDurableContractCoordinator(t, db, offlineQueueClient(t), "redis-outage", "outage-boot")
	workflow, binding := prepareContractWorkflow(t, db, offline, 106, "durable-redis-publish")
	if err := offline.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, _, err := offline.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil {
		t.Fatalf("activate workflow: %v", err)
	}
	if err := offline.RecoverNow(context.Background()); err == nil {
		t.Fatal("pre-recovery Redis outage unexpectedly accepted the delivery")
	}
	duringOutage := loadContractWorkflow(t, db, workflow.ID)
	if duringOutage.State != documentqueue.StateQueued || duringOutage.DispatchTaskID == "" {
		t.Fatalf("Redis outage did not preserve the durable queued outbox: %+v", duringOutage)
	}
	// Move the logical clock to the next recovery tick without sleeping. A
	// real Redis/container restart is covered by the opt-in Python scenario.
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Update("last_dispatched_at", nil).Error; err != nil {
		t.Fatalf("advance Redis recovery tick: %v", err)
	}

	const replicas = 8
	start := make(chan struct{})
	errs := make(chan error, replicas)
	var group sync.WaitGroup
	for index := 0; index < replicas; index++ {
		candidate := documentqueue.NewCoordinator(documentqueue.CoordinatorParams{
			DB: db, Client: client, Inspector: inspector,
		})
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			errs <- candidate.RecoverNow(context.Background())
		}()
	}
	close(start)
	group.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Redis recovery: %v", err)
		}
	}
	info, err := inspector.GetQueueInfo(types.QueueDocument)
	if err != nil {
		t.Fatalf("get recovered queue info: %v", err)
	}
	if info.Pending != 1 || info.Size != 1 {
		t.Fatalf("recovered Redis delivery count = pending %d size %d, want 1/1", info.Pending, info.Size)
	}
	pending, err := inspector.ListPendingTasks(types.QueueDocument)
	if err != nil || len(pending) != 1 {
		t.Fatalf("list recovered delivery: count=%d err=%v", len(pending), err)
	}
	wantTaskID := fmt.Sprintf("document-workflow:%s:%d", workflow.ID, workflow.DispatchEpoch)
	if pending[0].ID != wantTaskID {
		t.Fatalf("recovered task ID = %s, want %s", pending[0].ID, wantTaskID)
	}
}

func TestDurableFailoverRedisActiveDeliveryBlocksCrossInstanceTakeover(t *testing.T) {
	option := durableRedisOption(t)
	inspector := cleanDurableRedisQueue(t, option)
	client := asynq.NewClient(option)
	if err := client.Ping(); err != nil {
		t.Fatalf("ping Redis: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })
	db := openQueueContractDB(t)
	producer := newDurableContractCoordinator(t, db, client, "redis-owner", "owner-boot")
	workflow, binding := prepareContractWorkflow(t, db, producer, 107, "durable-redis-active")
	if err := producer.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, _, err := producer.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil {
		t.Fatalf("activate workflow: %v", err)
	}
	if _, err := producer.Dispatch(context.Background(), workflow); err != nil {
		t.Fatalf("dispatch workflow: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	var enteredOnce sync.Once
	server := asynq.NewServer(option, asynq.Config{
		Concurrency: 1, Queues: map[string]int{types.QueueDocument: 1},
		ShutdownTimeout: time.Second,
	})
	if err := server.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error {
		enteredOnce.Do(func() { close(entered) })
		<-release
		return nil
	})); err != nil {
		t.Fatalf("start Asynq Redis contract server: %v", err)
	}
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
		server.Shutdown()
	})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("Redis contract delivery did not become active")
	}

	now := time.Now()
	staleHeartbeat := now.Add(-24 * time.Hour)
	expiredLease := now.Add(-time.Hour)
	if err := db.Create(&documentqueue.Instance{
		InstanceID: "dead-owner", BootID: "dead-boot", State: documentqueue.InstanceReady,
		Capacity: 1, StartedAt: staleHeartbeat, LastHeartbeatAt: staleHeartbeat,
	}).Error; err != nil {
		t.Fatalf("insert dead owner instance: %v", err)
	}
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).Updates(map[string]any{
		"state": documentqueue.StateLeased, "owner_instance_id": "dead-owner", "owner_boot_id": "dead-boot",
		"lease_until": expiredLease,
	}).Error; err != nil {
		t.Fatalf("mark workflow stale while Redis active: %v", err)
	}
	recovery := documentqueue.NewCoordinator(documentqueue.CoordinatorParams{
		DB: db, Client: client, Inspector: inspector,
	})
	if err := recovery.RecoverNow(context.Background()); err != nil {
		t.Fatalf("recover while Redis active: %v", err)
	}
	if state := loadContractWorkflow(t, db, workflow.ID).State; state != documentqueue.StateLeased {
		t.Fatalf("Redis-active workflow was reclaimed with state %s", state)
	}

	close(release)
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, err := inspector.GetTaskInfo(types.QueueDocument, fmt.Sprintf("document-workflow:%s:%d", workflow.ID, workflow.DispatchEpoch))
		if redisQueueOrTaskMissing(err) {
			break
		}
		if err != nil {
			t.Fatalf("wait for inactive Redis delivery: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatal("Redis delivery remained active after handler exit")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := recovery.RecoverNow(context.Background()); err != nil {
		t.Fatalf("recover after Redis inactive: %v", err)
	}
	if state := loadContractWorkflow(t, db, workflow.ID).State; state != documentqueue.StateLeased {
		t.Fatalf("Redis-inactive workflow without termination proof was reclaimed with state %s", state)
	}
	if err := recovery.ConfirmInstanceTermination(
		context.Background(), "dead-owner", "dead-boot", "contract-runtime-terminated",
	); err != nil {
		t.Fatalf("attest dead Redis owner termination: %v", err)
	}
	if err := recovery.RecoverNow(context.Background()); err != nil {
		t.Fatalf("recover after Redis inactive and termination proof: %v", err)
	}
	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued || persisted.DispatchEpoch != workflow.DispatchEpoch+1 {
		t.Fatalf("Redis-inactive stale workflow was not reclaimed: %+v", persisted)
	}
}

func TestDurableFailoverConcurrentClaimNeverDoubleLeases(t *testing.T) {
	db := openQueueContractDB(t)
	first := newDurableContractCoordinator(t, db, nil, "claim-racer-a", "boot-a")
	second := newDurableContractCoordinator(t, db, nil, "claim-racer-b", "boot-b")
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start first claimant: %v", err)
	}
	t.Cleanup(first.Stop)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start second claimant: %v", err)
	}
	t.Cleanup(second.Stop)
	workflow, binding := prepareContractWorkflow(t, db, first, 108, "durable-claim-race")
	if err := first.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind workflow: %v", err)
	}
	workflow, _, err := first.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil {
		t.Fatalf("activate workflow: %v", err)
	}
	delivery := workflowDelivery(t, workflow)

	const contenders = 32
	start := make(chan struct{})
	var winners atomic.Int32
	var rejected atomic.Int32
	var group sync.WaitGroup
	for index := 0; index < contenders; index++ {
		candidate := first
		if index%2 == 1 {
			candidate = second
		}
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, claimErr := candidate.Claim(context.Background(), workflow.TaskType, delivery)
			switch {
			case claimErr == nil:
				winners.Add(1)
			case errors.Is(claimErr, documentqueue.ErrAlreadyLeased):
				rejected.Add(1)
			default:
				t.Errorf("unexpected claim error: %v", claimErr)
			}
		}()
	}
	close(start)
	group.Wait()
	if winners.Load() != 1 || rejected.Load() != contenders-1 {
		t.Fatalf("claim race = winners %d rejected %d, want 1/%d", winners.Load(), rejected.Load(), contenders-1)
	}
}

func TestDurableFailoverPostgresConcurrentSubmissionAndClaim(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	payload := rootPayload(t, 109, "kb-durable-postgres", "durable-postgres", "generation-postgres", nil)
	const producers = 64
	start := make(chan struct{})
	type preparation struct {
		workflow *documentqueue.Workflow
		created  bool
		err      error
	}
	prepared := make(chan preparation, producers)
	var group sync.WaitGroup
	for index := 0; index < producers; index++ {
		coordinator := newDurableContractCoordinator(
			t, db, nil, fmt.Sprintf("pg-producer-%d", index), uuid.NewString(),
		)
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			workflow, created, err := coordinator.PrepareWorkflow(
				context.Background(), types.TypeDocumentProcess, payload,
			)
			prepared <- preparation{workflow: workflow, created: created, err: err}
		}()
	}
	close(start)
	group.Wait()
	close(prepared)

	var workflow *documentqueue.Workflow
	createdCount := 0
	for result := range prepared {
		if result.err != nil {
			t.Fatalf("PostgreSQL concurrent prepare: %v", result.err)
		}
		if workflow == nil {
			workflow = result.workflow
		}
		if result.workflow == nil || result.workflow.ID != workflow.ID {
			t.Fatalf("PostgreSQL prepares diverged: got %+v want ID %s", result.workflow, workflow.ID)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("PostgreSQL created count = %d, want 1", createdCount)
	}
	binding := insertUnboundKnowledge(t, db, workflow)
	if err := documentqueue.NewCoordinatorWithConfig(
		db, nil, "pg-binder", "pg-binder-boot", 4, coordinatorConfig(),
	).BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("PostgreSQL bind prepared workflow: %v", err)
	}

	const replicas = 16
	activators := make([]*documentqueue.Coordinator, 0, replicas)
	for index := 0; index < replicas; index++ {
		candidate := newDurableContractCoordinator(
			t, db, nil, fmt.Sprintf("pg-worker-%d", index), uuid.NewString(),
		)
		if err := candidate.Start(context.Background()); err != nil {
			t.Fatalf("start PostgreSQL worker %d: %v", index, err)
		}
		t.Cleanup(candidate.Stop)
		activators = append(activators, candidate)
	}
	start = make(chan struct{})
	var activationWinners atomic.Int32
	group = sync.WaitGroup{}
	for _, candidate := range activators {
		candidate := candidate
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, activated, err := candidate.ActivatePreparedWorkflow(context.Background(), binding)
			if err != nil {
				t.Errorf("PostgreSQL activation: %v", err)
				return
			}
			if activated {
				activationWinners.Add(1)
			}
		}()
	}
	close(start)
	group.Wait()
	if activationWinners.Load() != 1 {
		t.Fatalf("PostgreSQL activation winners = %d, want 1", activationWinners.Load())
	}
	workflow = func() *documentqueue.Workflow {
		loaded := loadContractWorkflow(t, db, workflow.ID)
		return &loaded
	}()
	delivery := workflowDelivery(t, workflow)
	start = make(chan struct{})
	var claimWinners atomic.Int32
	var claimRejected atomic.Int32
	group = sync.WaitGroup{}
	for index := 0; index < producers; index++ {
		candidate := activators[index%len(activators)]
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			_, err := candidate.Claim(context.Background(), workflow.TaskType, delivery)
			switch {
			case err == nil:
				claimWinners.Add(1)
			case errors.Is(err, documentqueue.ErrAlreadyLeased):
				claimRejected.Add(1)
			default:
				t.Errorf("PostgreSQL claim error: %v", err)
			}
		}()
	}
	close(start)
	group.Wait()
	if claimWinners.Load() != 1 || claimRejected.Load() != producers-1 {
		t.Fatalf("PostgreSQL claims = winners %d rejected %d, want 1/%d",
			claimWinners.Load(), claimRejected.Load(), producers-1)
	}
}

func TestDurableFailoverPostgresConcurrentExpiredRecoverySingleEpoch(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	producer := newDurableContractCoordinator(t, db, nil, "pg-recovery-producer", "pg-recovery-producer-boot")
	payload := rootPayload(
		t,
		110,
		"kb-durable-postgres-recovery",
		"durable-postgres-recovery",
		"generation-postgres-recovery",
		nil,
	)
	workflow, created, err := producer.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess, payload,
	)
	if err != nil || !created {
		t.Fatalf("register PostgreSQL recovery workflow: created=%v err=%v", created, err)
	}
	_ = bindContractWorkflow(t, db, workflow)

	now := time.Now()
	staleHeartbeat := now.Add(-24 * time.Hour)
	expiredLease := now.Add(-time.Hour)
	owner := documentqueue.Instance{
		InstanceID:      "pg-expired-recovery-owner",
		BootID:          "pg-expired-recovery-boot",
		State:           documentqueue.InstanceStopped,
		Capacity:        1,
		StartedAt:       staleHeartbeat,
		LastHeartbeatAt: staleHeartbeat,
		StoppedAt:       &staleHeartbeat,
	}
	if err := db.Create(&owner).Error; err != nil {
		t.Fatalf("insert PostgreSQL terminated recovery owner: %v", err)
	}
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Updates(map[string]any{
			"state":             documentqueue.StateLeased,
			"stage":             "core",
			"owner_instance_id": owner.InstanceID,
			"owner_boot_id":     owner.BootID,
			"lease_until":       expiredLease,
		}).Error; err != nil {
		t.Fatalf("expire PostgreSQL recovery workflow: %v", err)
	}
	before := loadContractWorkflow(t, db, workflow.ID)

	const replicas = 16
	client := offlineQueueClient(t)
	coordinators := make([]*documentqueue.Coordinator, 0, replicas)
	for index := 0; index < replicas; index++ {
		coordinators = append(coordinators, newDurableContractCoordinator(
			t,
			db,
			client,
			fmt.Sprintf("pg-recovery-survivor-%02d", index),
			fmt.Sprintf("pg-recovery-survivor-boot-%02d", index),
		))
	}

	start := make(chan struct{})
	var ready sync.WaitGroup
	var group sync.WaitGroup
	ready.Add(replicas)
	group.Add(replicas)
	errs := make(chan error, replicas)
	for _, candidate := range coordinators {
		candidate := candidate
		go func() {
			defer group.Done()
			ready.Done()
			<-start
			errs <- candidate.RecoverNow(context.Background())
		}()
	}
	ready.Wait()
	close(start)
	group.Wait()
	close(errs)
	for recoveryErr := range errs {
		if recoveryErr != nil {
			t.Fatalf("concurrent PostgreSQL expired recovery: %v", recoveryErr)
		}
	}

	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued {
		t.Fatalf("PostgreSQL recovered workflow state = %s, want queued", persisted.State)
	}
	if persisted.DispatchEpoch != before.DispatchEpoch+1 {
		t.Fatalf("PostgreSQL recovered epoch = %d, want exactly %d",
			persisted.DispatchEpoch, before.DispatchEpoch+1)
	}
	if persisted.Version != before.Version+1 {
		t.Fatalf("PostgreSQL recovered version = %d, want one CAS transition from %d",
			persisted.Version, before.Version)
	}
	if persisted.OwnerInstanceID != "" || persisted.OwnerBootID != "" || persisted.LeaseUntil != nil {
		t.Fatalf("PostgreSQL recovered workflow retained lease ownership: %+v", persisted)
	}
	var generationRows int64
	if err := db.Model(&documentqueue.Workflow{}).
		Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
			workflow.TenantID, workflow.KnowledgeID, workflow.ProcessingGeneration).
		Count(&generationRows).Error; err != nil {
		t.Fatalf("count PostgreSQL recovery generation rows: %v", err)
	}
	if generationRows != 1 {
		t.Fatalf("PostgreSQL recovery generation has %d workflow rows, want one", generationRows)
	}
}

func TestDurableFailoverPostgresRestartTerminationRecoveryInterleaving(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	const (
		stableInstanceID = "pg-interleaved-stable-instance"
		oldBootID        = "pg-interleaved-old-boot"
		newBootID        = "pg-interleaved-new-boot"
	)

	oldBoot := newDurableContractCoordinator(t, db, nil, stableInstanceID, oldBootID)
	if err := oldBoot.Start(context.Background()); err != nil {
		t.Fatalf("start PostgreSQL old boot: %v", err)
	}
	t.Cleanup(oldBoot.Stop)
	workflow, binding := prepareContractWorkflow(t, db, oldBoot, 111, "durable-postgres-interleaving")
	if err := oldBoot.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind PostgreSQL interleaving workflow: %v", err)
	}
	workflow, activated, err := oldBoot.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil || !activated {
		t.Fatalf("activate PostgreSQL interleaving workflow: activated=%v err=%v", activated, err)
	}
	oldDelivery := workflowDelivery(t, workflow)
	if _, err := oldBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); err != nil {
		t.Fatalf("old PostgreSQL boot claim: %v", err)
	}

	now := time.Now()
	staleHeartbeat := now.Add(-24 * time.Hour)
	expiredLease := now.Add(-time.Hour)
	if err := db.Model(&documentqueue.Instance{}).
		Where("instance_id = ? AND boot_id = ?", stableInstanceID, oldBootID).
		Updates(map[string]any{
			"state":             documentqueue.InstanceReady,
			"last_heartbeat_at": staleHeartbeat,
			"updated_at":        staleHeartbeat,
		}).Error; err != nil {
		t.Fatalf("make PostgreSQL old boot stale: %v", err)
	}
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Updates(map[string]any{
			"lease_until":       expiredLease,
			"last_heartbeat_at": staleHeartbeat,
			"updated_at":        staleHeartbeat,
		}).Error; err != nil {
		t.Fatalf("expire PostgreSQL interleaving workflow: %v", err)
	}
	before := loadContractWorkflow(t, db, workflow.ID)
	if before.State != documentqueue.StateLeased || before.OwnerInstanceID != stableInstanceID ||
		before.OwnerBootID != oldBootID || before.LeaseUntil == nil {
		t.Fatalf("PostgreSQL interleaving precondition is not an old-boot lease: %+v", before)
	}

	// TrustStableInstanceRestart is enabled only for this new-boot contender:
	// the stable ID represents an orchestrator-fenced identity. That lets the
	// replacement transaction race the survivor and the exact-boot termination
	// attestation instead of requiring the test to serialize them first.
	newBootConfig := coordinatorConfig()
	newBootConfig.TrustStableInstanceRestart = true
	newBoot := documentqueue.NewCoordinatorWithConfig(
		db, nil, stableInstanceID, newBootID, 2, newBootConfig,
	)
	t.Cleanup(newBoot.Stop)
	survivor := newDurableContractCoordinator(
		t, db, offlineQueueClient(t), "pg-interleaved-survivor", "pg-interleaved-survivor-boot",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type interleaveResult struct {
		operation string
		err       error
	}
	const contenders = 3
	start := make(chan struct{})
	results := make(chan interleaveResult, contenders)
	var ready sync.WaitGroup
	var group sync.WaitGroup
	ready.Add(contenders)
	group.Add(contenders)
	run := func(operation string, action func(context.Context) error) {
		defer group.Done()
		ready.Done()
		select {
		case <-start:
			results <- interleaveResult{operation: operation, err: action(ctx)}
		case <-ctx.Done():
			results <- interleaveResult{operation: operation, err: ctx.Err()}
		}
	}
	go run("survivor recovery", survivor.RecoverNow)
	go run("old boot termination confirmation", func(ctx context.Context) error {
		return survivor.ConfirmInstanceTermination(
			ctx, stableInstanceID, oldBootID, "postgres-interleaving-exact-boot-terminated",
		)
	})
	go run("same-instance new boot adoption", newBoot.Start)
	allReady := make(chan struct{})
	go func() {
		ready.Wait()
		close(allReady)
	}()
	select {
	case <-allReady:
	case <-ctx.Done():
		t.Fatalf("PostgreSQL interleaving barrier did not become ready: %v", ctx.Err())
	}
	close(start)
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("PostgreSQL restart/termination/recovery interleaving deadlocked: %v", ctx.Err())
	}
	close(results)
	newBootStarted := false
	for result := range results {
		switch result.operation {
		case "old boot termination confirmation":
			// The replacement may fence the old boot before its attestation
			// obtains the instance-row lock. That stale exact-boot result is the
			// expected safe outcome, not a failed termination proof.
			if result.err != nil && !errors.Is(result.err, documentqueue.ErrStaleDelivery) {
				t.Fatalf("%s: %v", result.operation, result.err)
			}
		case "same-instance new boot adoption":
			if result.err != nil {
				t.Fatalf("%s: %v", result.operation, result.err)
			}
			newBootStarted = true
		default:
			if result.err != nil {
				t.Fatalf("%s: %v", result.operation, result.err)
			}
		}
	}
	if !newBootStarted {
		t.Fatal("same-instance new boot did not complete its adoption attempt")
	}

	var instance documentqueue.Instance
	if err := db.Where("instance_id = ?", stableInstanceID).Take(&instance).Error; err != nil {
		t.Fatalf("load PostgreSQL stable instance after interleaving: %v", err)
	}
	if instance.BootID != newBootID || instance.State != documentqueue.InstanceReady {
		t.Fatalf("PostgreSQL stable instance = boot %s state %s, want %s/ready",
			instance.BootID, instance.State, newBootID)
	}
	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateQueued {
		t.Fatalf("PostgreSQL interleaved workflow state = %s, want queued", persisted.State)
	}
	if persisted.DispatchEpoch != before.DispatchEpoch+1 {
		t.Fatalf("PostgreSQL interleaved epoch = %d, want exactly %d",
			persisted.DispatchEpoch, before.DispatchEpoch+1)
	}
	if persisted.Version != before.Version+1 {
		t.Fatalf("PostgreSQL interleaved version = %d, want one CAS transition from %d",
			persisted.Version, before.Version)
	}
	if persisted.OwnerInstanceID != "" || persisted.OwnerBootID != "" || persisted.LeaseUntil != nil {
		t.Fatalf("PostgreSQL interleaved workflow retained old ownership: %+v", persisted)
	}

	// The old process identity and its old delivery are both fenced after the
	// single epoch rotation, regardless of which contender performed it.
	if _, err := oldBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); !errors.Is(err, documentqueue.ErrInstanceFenced) {
		t.Fatalf("old PostgreSQL boot was not fenced: %v", err)
	}
	if _, err := newBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); !errors.Is(err, documentqueue.ErrStaleDelivery) {
		t.Fatalf("old PostgreSQL delivery epoch was not fenced: %v", err)
	}
}

func TestDurableFailoverPostgresCancellationRestartInterleaving(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	const (
		stableInstanceID = "pg-cancel-restart-stable-instance"
		oldBootID        = "pg-cancel-restart-old-boot"
		newBootID        = "pg-cancel-restart-new-boot"
	)

	oldBoot := newDurableContractCoordinator(t, db, nil, stableInstanceID, oldBootID)
	if err := oldBoot.Start(context.Background()); err != nil {
		t.Fatalf("start PostgreSQL cancellation old boot: %v", err)
	}
	t.Cleanup(oldBoot.Stop)
	workflow, binding := prepareContractWorkflow(t, db, oldBoot, 112, "durable-postgres-cancel-restart")
	if err := oldBoot.BindPreparedWorkflow(context.Background(), binding); err != nil {
		t.Fatalf("bind PostgreSQL cancellation workflow: %v", err)
	}
	workflow, activated, err := oldBoot.ActivatePreparedWorkflow(context.Background(), binding)
	if err != nil || !activated {
		t.Fatalf("activate PostgreSQL cancellation workflow: activated=%v err=%v", activated, err)
	}
	oldDelivery := workflowDelivery(t, workflow)
	if _, err := oldBoot.Claim(context.Background(), workflow.TaskType, oldDelivery); err != nil {
		t.Fatalf("claim PostgreSQL cancellation workflow: %v", err)
	}
	if err := db.Table("knowledges").
		Where("tenant_id = ? AND id = ?", workflow.TenantID, workflow.KnowledgeID).
		Updates(map[string]any{
			"parse_status":           types.ParseStatusCancelling,
			"pending_subtasks_count": 9,
			"summary_status":         types.SummaryStatusProcessing,
			"enrichment_status":      types.EnrichmentStatusPending,
			"wiki_status":            types.WikiStatusPending,
			"wiki_error_message":     "stale wiki error",
		}).Error; err != nil {
		t.Fatalf("publish PostgreSQL cancellation intent: %v", err)
	}
	before := loadContractWorkflow(t, db, workflow.ID)

	newBootConfig := coordinatorConfig()
	newBootConfig.TrustStableInstanceRestart = true
	newBoot := documentqueue.NewCoordinatorWithConfig(
		db, nil, stableInstanceID, newBootID, 2, newBootConfig,
	)
	t.Cleanup(newBoot.Stop)
	cancellation := documentqueue.CancellationBinding{
		WorkflowID:           workflow.ID,
		TenantID:             workflow.TenantID,
		KnowledgeBaseID:      workflow.KnowledgeBaseID,
		KnowledgeID:          workflow.KnowledgeID,
		ProcessingGeneration: workflow.ProcessingGeneration,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	type cancellationRaceResult struct {
		operation string
		err       error
	}
	start := make(chan struct{})
	results := make(chan cancellationRaceResult, 2)
	var ready sync.WaitGroup
	var group sync.WaitGroup
	ready.Add(2)
	group.Add(2)
	run := func(operation string, action func(context.Context) error) {
		defer group.Done()
		ready.Done()
		select {
		case <-start:
			results <- cancellationRaceResult{operation: operation, err: action(ctx)}
		case <-ctx.Done():
			results <- cancellationRaceResult{operation: operation, err: ctx.Err()}
		}
	}
	go run("same-instance restart adoption", newBoot.Start)
	go run("exact-generation cancellation", func(ctx context.Context) error {
		return oldBoot.CommitWorkflowCancellation(ctx, cancellation, time.Now())
	})
	ready.Wait()
	close(start)
	done := make(chan struct{})
	go func() {
		group.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatalf("PostgreSQL cancellation/restart interleaving deadlocked: %v", ctx.Err())
	}
	close(results)
	for result := range results {
		if result.err != nil {
			t.Fatalf("%s: %v", result.operation, result.err)
		}
	}

	persisted := loadContractWorkflow(t, db, workflow.ID)
	if persisted.State != documentqueue.StateCancelled || persisted.Stage != "cancelled" {
		t.Fatalf("PostgreSQL cancelled workflow was revived: %+v", persisted)
	}
	if persisted.OwnerInstanceID != "" || persisted.OwnerBootID != "" ||
		persisted.LeaseUntil != nil || persisted.DispatchTaskID != "" {
		t.Fatalf("PostgreSQL cancelled workflow retained execution ownership: %+v", persisted)
	}
	if persisted.DispatchEpoch < before.DispatchEpoch+1 ||
		persisted.DispatchEpoch > before.DispatchEpoch+2 {
		t.Fatalf("PostgreSQL cancellation/restart epoch = %d, want %d or %d",
			persisted.DispatchEpoch, before.DispatchEpoch+1, before.DispatchEpoch+2)
	}

	var knowledge struct {
		ParseStatus          string
		ProcessingOwner      string
		PendingSubtasksCount int
		SummaryStatus        string
		EnrichmentStatus     string
		WikiStatus           string
		WikiErrorMessage     string
	}
	if err := db.Table("knowledges").
		Where("tenant_id = ? AND id = ?", workflow.TenantID, workflow.KnowledgeID).
		Take(&knowledge).Error; err != nil {
		t.Fatalf("load PostgreSQL cancelled knowledge: %v", err)
	}
	if knowledge.ParseStatus != types.ParseStatusCancelled ||
		knowledge.ProcessingOwner != "" ||
		knowledge.PendingSubtasksCount != 0 ||
		knowledge.SummaryStatus != types.SummaryStatusNone ||
		knowledge.EnrichmentStatus != types.EnrichmentStatusNone ||
		knowledge.WikiStatus != types.WikiStatusNone ||
		knowledge.WikiErrorMessage != "" {
		t.Fatalf("PostgreSQL cancellation business state is not terminal: %+v", knowledge)
	}

	var generationRows int64
	if err := db.Model(&documentqueue.Workflow{}).
		Where("tenant_id = ? AND knowledge_id = ? AND processing_generation = ?",
			workflow.TenantID, workflow.KnowledgeID, workflow.ProcessingGeneration).
		Count(&generationRows).Error; err != nil {
		t.Fatalf("count PostgreSQL cancellation workflow rows: %v", err)
	}
	if generationRows != 1 {
		t.Fatalf("PostgreSQL cancellation generation has %d workflow rows, want one", generationRows)
	}
	if _, err := oldBoot.Claim(
		context.Background(), workflow.TaskType, oldDelivery,
	); !errors.Is(err, documentqueue.ErrInstanceFenced) {
		t.Fatalf("old PostgreSQL cancellation boot was not fenced: %v", err)
	}
}
