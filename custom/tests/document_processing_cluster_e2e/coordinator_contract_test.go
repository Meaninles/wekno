package document_processing_cluster_e2e_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/Tencent/WeKnora/internal/types"
)

func openQueueContractDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:document-queue-contract-%s?mode=memory&cache=shared&_busy_timeout=5000", uuid.NewString())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get sql db: %v", err)
	}
	// One connection keeps SQLite's in-memory schema deterministic while the
	// goroutine race still exercises the coordinator's transactional CAS.
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = sqlDB.Close() })

	if err := db.Exec(`CREATE TABLE knowledges (
		id text NOT NULL,
		tenant_id integer NOT NULL,
		knowledge_base_id text NOT NULL,
		processing_generation text NOT NULL,
		processing_owner text NOT NULL DEFAULT '',
		processing_workflow_id text NOT NULL DEFAULT '',
		processing_fanout text NULL,
		parse_status text NOT NULL DEFAULT 'pending',
		pending_subtasks_count integer NOT NULL DEFAULT 0,
		summary_status text NOT NULL DEFAULT 'none',
		enrichment_status text NOT NULL DEFAULT 'none',
		wiki_status text NOT NULL DEFAULT 'none',
		wiki_error_message text NOT NULL DEFAULT '',
		error_message text NOT NULL DEFAULT '',
		processed_at datetime NULL,
		updated_at datetime NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at datetime NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create knowledge contract table: %v", err)
	}
	if err := db.AutoMigrate(
		&documentqueue.Workflow{}, &documentqueue.Instance{}, &documentqueue.ScheduleGroup{},
	); err != nil {
		t.Fatalf("migrate document queue: %v", err)
	}
	return db
}

func openPostgresQueueContractDB(t *testing.T) *gorm.DB {
	t.Helper()
	if os.Getenv("WEKNORA_DOCUMENT_QUEUE_POSTGRES_CONTRACT") != "1" {
		t.Skip("set WEKNORA_DOCUMENT_QUEUE_POSTGRES_CONTRACT=1 to run the PostgreSQL race contract")
	}
	required := map[string]string{
		"DB_HOST": os.Getenv("DB_HOST"), "DB_PORT": os.Getenv("DB_PORT"),
		"DB_USER": os.Getenv("DB_USER"), "DB_PASSWORD": os.Getenv("DB_PASSWORD"),
		"DB_NAME": os.Getenv("DB_NAME"),
	}
	for name, value := range required {
		if strings.TrimSpace(value) == "" {
			t.Fatalf("%s is required for the PostgreSQL race contract", name)
		}
	}

	baseURL := &url.URL{
		Scheme: "postgresql",
		User:   url.UserPassword(required["DB_USER"], required["DB_PASSWORD"]),
		Host:   net.JoinHostPort(required["DB_HOST"], required["DB_PORT"]),
		Path:   "/" + required["DB_NAME"],
	}
	query := baseURL.Query()
	query.Set("sslmode", "disable")
	baseURL.RawQuery = query.Encode()
	base, err := gorm.Open(postgres.Open(baseURL.String()), &gorm.Config{})
	if err != nil {
		t.Fatalf("open PostgreSQL contract database: %v", err)
	}
	baseSQL, err := base.DB()
	if err != nil {
		t.Fatalf("get PostgreSQL contract pool: %v", err)
	}

	schema := "document_queue_contract_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := base.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		_ = baseSQL.Close()
		t.Fatalf("create PostgreSQL contract schema: %v", err)
	}
	scopedURL := *baseURL
	scopedQuery := scopedURL.Query()
	scopedQuery.Set("search_path", schema)
	scopedURL.RawQuery = scopedQuery.Encode()
	scoped, err := gorm.Open(postgres.Open(scopedURL.String()), &gorm.Config{})
	if err != nil {
		_ = base.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = baseSQL.Close()
		t.Fatalf("open schema-scoped PostgreSQL contract database: %v", err)
	}
	scopedSQL, err := scoped.DB()
	if err != nil {
		_ = base.Exec("DROP SCHEMA " + schema + " CASCADE").Error
		_ = baseSQL.Close()
		t.Fatalf("get schema-scoped PostgreSQL pool: %v", err)
	}
	scopedSQL.SetMaxOpenConns(32)
	t.Cleanup(func() {
		_ = scopedSQL.Close()
		if err := base.Exec("DROP SCHEMA " + schema + " CASCADE").Error; err != nil {
			t.Errorf("drop PostgreSQL contract schema: %v", err)
		}
		_ = baseSQL.Close()
	})
	if err := scoped.AutoMigrate(
		&documentqueue.Workflow{}, &documentqueue.Instance{}, &documentqueue.ScheduleGroup{},
	); err != nil {
		t.Fatalf("migrate PostgreSQL document queue contract: %v", err)
	}
	if err := scoped.Exec(`CREATE TABLE knowledges (
		id text NOT NULL,
		tenant_id bigint NOT NULL,
		knowledge_base_id text NOT NULL,
		processing_generation text NOT NULL,
		processing_owner text NOT NULL DEFAULT '',
		processing_workflow_id text NOT NULL DEFAULT '',
		processing_fanout jsonb NULL,
		parse_status text NOT NULL DEFAULT 'pending',
		pending_subtasks_count integer NOT NULL DEFAULT 0,
		summary_status text NOT NULL DEFAULT 'none',
		enrichment_status text NOT NULL DEFAULT 'none',
		wiki_status text NOT NULL DEFAULT 'none',
		wiki_error_message text NOT NULL DEFAULT '',
		error_message text NOT NULL DEFAULT '',
		processed_at timestamptz NULL,
		updated_at timestamptz NOT NULL DEFAULT CURRENT_TIMESTAMP,
		deleted_at timestamptz NULL,
		PRIMARY KEY (tenant_id, id)
	)`).Error; err != nil {
		t.Fatalf("create PostgreSQL knowledge contract table: %v", err)
	}
	return scoped
}

func rootPayload(t *testing.T, tenantID uint64, kbID, knowledgeID, generation string, extra map[string]any) []byte {
	t.Helper()
	value := map[string]any{
		"tenant_id": tenantID, "knowledge_base_id": kbID,
		"knowledge_id": knowledgeID, "processing_generation": generation,
		"processing_owner": uuid.NewString(),
	}
	for key, item := range extra {
		value[key] = item
	}
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal root payload: %v", err)
	}
	return payload
}

func bindContractWorkflow(t *testing.T, db *gorm.DB, workflow *documentqueue.Workflow) []byte {
	t.Helper()
	var fields map[string]any
	if err := json.Unmarshal(workflow.Payload, &fields); err != nil {
		t.Fatalf("decode workflow payload: %v", err)
	}
	owner, _ := fields["processing_owner"].(string)
	if err := db.Exec(`INSERT INTO knowledges
		(id, tenant_id, knowledge_base_id, processing_generation, processing_owner, processing_workflow_id, parse_status)
		VALUES (?, ?, ?, ?, ?, ?, 'pending')
		ON CONFLICT (tenant_id, id) DO UPDATE SET
		knowledge_base_id = excluded.knowledge_base_id,
		processing_generation = excluded.processing_generation,
		processing_owner = excluded.processing_owner,
		processing_workflow_id = excluded.processing_workflow_id,
		parse_status = 'pending'`, workflow.KnowledgeID, workflow.TenantID,
		workflow.KnowledgeBaseID, workflow.ProcessingGeneration, owner, workflow.ID).Error; err != nil {
		t.Fatalf("bind contract workflow: %v", err)
	}
	fields["document_workflow_id"] = workflow.ID
	fields["document_workflow_epoch"] = workflow.DispatchEpoch
	delivery, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("marshal workflow delivery: %v", err)
	}
	return delivery
}

func coordinatorConfig() documentqueue.Config {
	return documentqueue.Config{
		HeartbeatInterval:    time.Hour,
		LeaseDuration:        3 * time.Hour,
		RecoveryInterval:     time.Hour,
		InstanceStaleAfter:   2 * time.Hour,
		WorkflowPollInterval: time.Hour,
		WorkflowTimeout:      48 * time.Hour,
		MaxRetry:             100,
	}
}

func TestQueueStatusRanksGloballyBeforeTenantFiltering(t *testing.T) {
	db := openQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "worker-a", "boot-a", 2, coordinatorConfig(),
	)
	now := time.Now().Add(-time.Minute)
	rows := []struct {
		tenantID uint64
		kbID     string
		id       string
		gen      string
	}{
		{tenantID: 1, kbID: "kb-1", id: "knowledge-1", gen: "generation-1"},
		{tenantID: 2, kbID: "kb-2", id: "knowledge-2", gen: "generation-2"},
	}
	for index, row := range rows {
		if err := db.Exec(
			`INSERT INTO knowledges (id, tenant_id, knowledge_base_id, processing_generation)
			 VALUES (?, ?, ?, ?)`, row.id, row.tenantID, row.kbID, row.gen,
		).Error; err != nil {
			t.Fatalf("insert knowledge %d: %v", index, err)
		}
		workflow := documentqueue.Workflow{
			ID: uuid.NewString(), TenantID: row.tenantID, KnowledgeBaseID: row.kbID,
			KnowledgeID: row.id, ProcessingGeneration: row.gen,
			TaskType: types.TypeDocumentProcess,
			Payload:  rootPayload(t, row.tenantID, row.kbID, row.id, row.gen, nil),
			State:    documentqueue.StateQueued, Stage: "queued", DispatchEpoch: 1,
			EnqueuedAt: now.Add(time.Duration(index) * time.Second), Version: 1,
		}
		if err := db.Create(&workflow).Error; err != nil {
			t.Fatalf("insert workflow %d: %v", index, err)
		}
	}

	status, err := coordinator.QueueStatus(context.Background(), 2, []string{"knowledge-2"})
	if err != nil {
		t.Fatalf("queue status: %v", err)
	}
	if status.WaitingTotal != 2 {
		t.Fatalf("waiting total = %d, want 2", status.WaitingTotal)
	}
	item := status.Items["knowledge-2"]
	if item.State != "waiting" || item.Position != 2 {
		t.Fatalf("tenant-filtered item = %+v, want global position 2", item)
	}
}

func TestConcurrentClaimHasExactlyOneWinner(t *testing.T) {
	db := openQueueContractDB(t)
	first := documentqueue.NewCoordinatorWithConfig(db, nil, "worker-a", "boot-a", 1, coordinatorConfig())
	second := documentqueue.NewCoordinatorWithConfig(db, nil, "worker-b", "boot-b", 1, coordinatorConfig())
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start first coordinator: %v", err)
	}
	t.Cleanup(first.Stop)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start second coordinator: %v", err)
	}
	t.Cleanup(second.Stop)
	payload := rootPayload(t, 1, "kb-1", "knowledge-1", "generation-1", nil)
	workflow, _, err := first.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	if err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	delivery := bindContractWorkflow(t, db, workflow)

	start := make(chan struct{})
	results := make(chan error, 2)
	var group sync.WaitGroup
	for _, candidate := range []*documentqueue.Coordinator{first, second} {
		group.Add(1)
		go func(coordinator *documentqueue.Coordinator) {
			defer group.Done()
			<-start
			_, claimErr := coordinator.Claim(context.Background(), types.TypeDocumentProcess, delivery)
			results <- claimErr
		}(candidate)
	}
	close(start)
	group.Wait()
	close(results)

	winners, alreadyLeased := 0, 0
	for claimErr := range results {
		switch {
		case claimErr == nil:
			winners++
		case errors.Is(claimErr, documentqueue.ErrAlreadyLeased):
			alreadyLeased++
		default:
			t.Fatalf("unexpected claim error: %v", claimErr)
		}
	}
	if winners != 1 || alreadyLeased != 1 {
		t.Fatalf("claim results: winners=%d already_leased=%d, want 1/1", winners, alreadyLeased)
	}
}

func TestConcurrentFirstRegistrationConvergesOnOneWorkflow(t *testing.T) {
	db := openQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "worker-register", "boot-register", 4, coordinatorConfig(),
	)
	payload := rootPayload(t, 9, "kb-register", "knowledge-register", "generation-register", nil)

	type registration struct {
		id      string
		created bool
		err     error
	}
	const producers = 32
	start := make(chan struct{})
	results := make(chan registration, producers)
	var group sync.WaitGroup
	for index := 0; index < producers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			workflow, created, err := coordinator.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess, payload,
			)
			result := registration{created: created, err: err}
			if workflow != nil {
				result.id = workflow.ID
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)

	workflowID := ""
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("concurrent registration failed: %v", result.err)
		}
		if result.id == "" {
			t.Fatal("concurrent registration returned an empty workflow ID")
		}
		if workflowID == "" {
			workflowID = result.id
		}
		if result.id != workflowID {
			t.Fatalf("registrations diverged: got %s, want %s", result.id, workflowID)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created=true count = %d, want exactly 1", createdCount)
	}
	var rows int64
	if err := db.Model(&documentqueue.Workflow{}).Count(&rows).Error; err != nil {
		t.Fatalf("count workflows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("workflow row count = %d, want 1", rows)
	}
}

func TestPostgresConcurrentFirstRegistrationConvergesOnOneWorkflow(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "postgres-worker-register", "postgres-boot-register", 8, coordinatorConfig(),
	)
	payload := rootPayload(t, 19, "kb-postgres-register", "knowledge-postgres-register", "generation-register", nil)

	type registration struct {
		id      string
		created bool
		err     error
	}
	const producers = 64
	start := make(chan struct{})
	results := make(chan registration, producers)
	var group sync.WaitGroup
	for index := 0; index < producers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			workflow, created, err := coordinator.RegisterWorkflow(
				context.Background(), types.TypeDocumentProcess, payload,
			)
			result := registration{created: created, err: err}
			if workflow != nil {
				result.id = workflow.ID
			}
			results <- result
		}()
	}
	close(start)
	group.Wait()
	close(results)

	workflowID := ""
	createdCount := 0
	for result := range results {
		if result.err != nil {
			t.Fatalf("PostgreSQL concurrent registration failed: %v", result.err)
		}
		if workflowID == "" {
			workflowID = result.id
		}
		if result.id == "" || result.id != workflowID {
			t.Fatalf("PostgreSQL registrations diverged: got %q, want %q", result.id, workflowID)
		}
		if result.created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("PostgreSQL created=true count = %d, want exactly 1", createdCount)
	}
	var rows int64
	if err := db.Model(&documentqueue.Workflow{}).Count(&rows).Error; err != nil {
		t.Fatalf("count PostgreSQL workflows: %v", err)
	}
	if rows != 1 {
		t.Fatalf("PostgreSQL workflow row count = %d, want 1", rows)
	}
}

func TestPostgresFairSchedulerDefersOldBacklogForLateKnowledgeBase(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	first := documentqueue.NewCoordinatorWithConfig(
		db, nil, "postgres-fair-a", "boot-a", 4, coordinatorConfig(),
	)
	second := documentqueue.NewCoordinatorWithConfig(
		db, nil, "postgres-fair-b", "boot-b", 4, coordinatorConfig(),
	)
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start first fair coordinator: %v", err)
	}
	t.Cleanup(first.Stop)
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start second fair coordinator: %v", err)
	}
	t.Cleanup(second.Stop)

	create := func(tenantID uint64, kbID, knowledgeID, generation string) (*documentqueue.Workflow, []byte) {
		t.Helper()
		workflow, _, err := first.RegisterWorkflow(
			context.Background(), types.TypeDocumentProcess,
			rootPayload(t, tenantID, kbID, knowledgeID, generation, nil),
		)
		if err != nil {
			t.Fatalf("register %s: %v", knowledgeID, err)
		}
		return workflow, bindContractWorkflow(t, db, workflow)
	}
	firstA, deliveryA1 := create(31, "kb-a", "a-1", "generation-a-1")
	secondA, deliveryA2 := create(31, "kb-a", "a-2", "generation-a-2")
	lateB, deliveryB := create(32, "kb-b", "b-1", "generation-b-1")
	if _, err := first.Claim(context.Background(), types.TypeDocumentProcess, deliveryA1); err != nil {
		t.Fatalf("claim first A workflow: %v", err)
	}

	if _, err := second.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryA2,
	); !errors.Is(err, documentqueue.ErrFairnessDeferred) {
		t.Fatalf("second A claim = %v, want ErrFairnessDeferred", err)
	}
	if _, err := second.Claim(
		context.Background(), types.TypeDocumentProcess, deliveryB,
	); err != nil {
		t.Fatalf("late B claim: %v", err)
	}

	var currentA documentqueue.Workflow
	if err := db.Where("id = ?", secondA.ID).Take(&currentA).Error; err != nil {
		t.Fatalf("load deferred A workflow: %v", err)
	}
	if currentA.State != documentqueue.StateQueued ||
		currentA.DispatchEpoch != secondA.DispatchEpoch+1 {
		t.Fatalf("deferred A workflow = state %s epoch %d, want queued epoch %d",
			currentA.State, currentA.DispatchEpoch, secondA.DispatchEpoch+1)
	}
	var currentFirstA, currentB documentqueue.Workflow
	if err := db.Where("id = ?", firstA.ID).Take(&currentFirstA).Error; err != nil {
		t.Fatalf("load active A workflow: %v", err)
	}
	if err := db.Where("id = ?", lateB.ID).Take(&currentB).Error; err != nil {
		t.Fatalf("load active B workflow: %v", err)
	}
	if currentFirstA.State != documentqueue.StateLeased ||
		currentB.State != documentqueue.StateLeased {
		t.Fatalf("fair active states = A:%s B:%s, want leased/leased",
			currentFirstA.State, currentB.State)
	}
}

func TestPostgresClaimUsesCrossProcessSchedulerAdvisoryLock(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "postgres-advisory", "boot-1", 1, coordinatorConfig(),
	)
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatalf("start advisory coordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	workflow, _, err := coordinator.RegisterWorkflow(
		context.Background(), types.TypeDocumentProcess,
		rootPayload(t, 33, "kb-advisory", "knowledge-advisory", "generation-1", nil),
	)
	if err != nil {
		t.Fatalf("register advisory workflow: %v", err)
	}
	delivery := bindContractWorkflow(t, db, workflow)

	lockTx := db.Begin()
	if lockTx.Error != nil {
		t.Fatalf("begin scheduler-lock transaction: %v", lockTx.Error)
	}
	const schedulerLockKey int64 = 0x574b4e4f524151
	if err := lockTx.Exec("SELECT pg_advisory_xact_lock(?)", schedulerLockKey).Error; err != nil {
		_ = lockTx.Rollback().Error
		t.Fatalf("hold scheduler advisory lock: %v", err)
	}

	claimDone := make(chan error, 1)
	go func() {
		_, claimErr := coordinator.Claim(
			context.Background(), types.TypeDocumentProcess, delivery,
		)
		claimDone <- claimErr
	}()
	select {
	case claimErr := <-claimDone:
		_ = lockTx.Rollback().Error
		t.Fatalf("claim bypassed PostgreSQL scheduler lock: %v", claimErr)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lockTx.Commit().Error; err != nil {
		t.Fatalf("release scheduler advisory lock: %v", err)
	}
	if claimErr := <-claimDone; claimErr != nil {
		t.Fatalf("claim after scheduler lock release: %v", claimErr)
	}
}

func TestPostgresPreparedMoveBindingFencesConcurrentAbort(t *testing.T) {
	db := openPostgresQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "postgres-move-binding", "boot-1", 1, coordinatorConfig(),
	)
	payload := rootPayload(t, 7, "kb-target", "knowledge-move-binding", "generation-1", nil)
	workflow, _, err := coordinator.PrepareWorkflowWithOptions(
		context.Background(), types.TypeDocumentProcess, payload, nil,
	)
	if err != nil {
		t.Fatalf("prepare workflow: %v", err)
	}
	binding, err := documentqueue.BindingForWorkflow(workflow)
	if err != nil {
		t.Fatalf("build workflow binding: %v", err)
	}
	if err := db.Exec(`INSERT INTO knowledges
		(id, tenant_id, knowledge_base_id, processing_generation, processing_owner, parse_status)
		VALUES (?, ?, ?, ?, ?, 'pending')`,
		binding.KnowledgeID, binding.TenantID, binding.KnowledgeBaseID,
		binding.ProcessingGeneration, binding.ProcessingOwner,
	).Error; err != nil {
		t.Fatalf("insert pending knowledge: %v", err)
	}

	transitionStarted := make(chan struct{})
	releaseTransition := make(chan struct{})
	bindResult := make(chan error, 1)
	go func() {
		bindResult <- db.Transaction(func(tx *gorm.DB) error {
			return coordinator.BindPreparedWorkflowTransitionTx(tx, binding, func(tx *gorm.DB) error {
				close(transitionStarted)
				<-releaseTransition
				return tx.Table("knowledges").Where(
					"id = ? AND tenant_id = ?", binding.KnowledgeID, binding.TenantID,
				).Update("processing_workflow_id", binding.WorkflowID).Error
			})
		})
	}()
	<-transitionStarted
	abortResult := make(chan error, 1)
	go func() {
		abortResult <- coordinator.AbortPreparedWorkflow(
			context.Background(), binding, "concurrent producer abort",
		)
	}()
	select {
	case abortErr := <-abortResult:
		t.Fatalf("abort bypassed the prepared workflow row lock: %v", abortErr)
	case <-time.After(100 * time.Millisecond):
	}
	close(releaseTransition)
	if err := <-bindResult; err != nil {
		t.Fatalf("commit prepared move binding: %v", err)
	}
	if err := <-abortResult; err == nil || !strings.Contains(err.Error(), "bound prepared workflow cannot be aborted") {
		t.Fatalf("abort after committed binding = %v, want bound-workflow rejection", err)
	}

	var state documentqueue.WorkflowState
	if err := db.Model(&documentqueue.Workflow{}).Select("state").
		Where("id = ?", binding.WorkflowID).Scan(&state).Error; err != nil {
		t.Fatalf("load workflow state: %v", err)
	}
	if state != documentqueue.StatePreparing {
		t.Fatalf("workflow state = %s, want preparing", state)
	}
}

func TestMarkDrainingSurvivesSubsequentHeartbeats(t *testing.T) {
	db := openQueueContractDB(t)
	config := coordinatorConfig()
	config.HeartbeatInterval = 10 * time.Millisecond
	coordinator := documentqueue.NewCoordinatorWithConfig(
		db, nil, "worker-draining", "boot-draining", 2, config,
	)
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)

	coordinator.MarkDraining()
	var marked documentqueue.Instance
	if err := db.Where("instance_id = ?", "worker-draining").Take(&marked).Error; err != nil {
		t.Fatalf("load marked instance: %v", err)
	}
	if marked.State != documentqueue.InstanceDraining {
		t.Fatalf("state immediately after MarkDraining = %s, want draining", marked.State)
	}

	// Wait for a heartbeat newer than the MarkDraining write. This proves the
	// assertion is checking the heartbeat path, not only the synchronous update.
	deadline := time.Now().Add(time.Second)
	for {
		var afterHeartbeat documentqueue.Instance
		if err := db.Where("instance_id = ?", "worker-draining").Take(&afterHeartbeat).Error; err != nil {
			t.Fatalf("load instance after heartbeat: %v", err)
		}
		if afterHeartbeat.LastHeartbeatAt.After(marked.LastHeartbeatAt) {
			if afterHeartbeat.State != documentqueue.InstanceDraining {
				t.Fatalf("heartbeat reverted draining state to %s", afterHeartbeat.State)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("coordinator did not emit a heartbeat after MarkDraining")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStaleDispatchEpochCannotClaim(t *testing.T) {
	db := openQueueContractDB(t)
	coordinator := documentqueue.NewCoordinatorWithConfig(db, nil, "worker-a", "boot-a", 1, coordinatorConfig())
	if err := coordinator.Start(context.Background()); err != nil {
		t.Fatalf("start coordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	payload := rootPayload(t, 1, "kb-1", "knowledge-1", "generation-1", nil)
	workflow, _, err := coordinator.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	if err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	if err := db.Model(&documentqueue.Workflow{}).Where("id = ?", workflow.ID).
		Update("dispatch_epoch", 2).Error; err != nil {
		t.Fatalf("advance epoch: %v", err)
	}
	stale := rootPayload(t, 1, "kb-1", "knowledge-1", "generation-1", map[string]any{
		"document_workflow_id": workflow.ID, "document_workflow_epoch": 1,
	})
	if _, err := coordinator.Claim(context.Background(), types.TypeDocumentProcess, stale); !errors.Is(err, documentqueue.ErrStaleDelivery) {
		t.Fatalf("stale claim error = %v, want ErrStaleDelivery", err)
	}
}

func TestNewBootAdoptsAndFencesStableInstanceLease(t *testing.T) {
	db := openQueueContractDB(t)
	first := documentqueue.NewCoordinatorWithConfig(db, nil, "stable-worker", "boot-old", 1, coordinatorConfig())
	if err := first.Start(context.Background()); err != nil {
		t.Fatalf("start first boot: %v", err)
	}
	payload := rootPayload(t, 1, "kb-1", "knowledge-1", "generation-1", nil)
	workflow, _, err := first.RegisterWorkflow(context.Background(), types.TypeDocumentProcess, payload)
	if err != nil {
		t.Fatalf("register workflow: %v", err)
	}
	delivery := bindContractWorkflow(t, db, workflow)
	if _, err := first.Claim(context.Background(), types.TypeDocumentProcess, delivery); err != nil {
		t.Fatalf("claim old boot: %v", err)
	}
	first.Stop()

	second := documentqueue.NewCoordinatorWithConfig(db, nil, "stable-worker", "boot-new", 1, coordinatorConfig())
	if err := second.Start(context.Background()); err != nil {
		t.Fatalf("start new boot: %v", err)
	}
	t.Cleanup(second.Stop)

	var adopted documentqueue.Workflow
	if err := db.Where("id = ?", workflow.ID).Take(&adopted).Error; err != nil {
		t.Fatalf("load adopted workflow: %v", err)
	}
	if adopted.State != documentqueue.StateQueued || adopted.DispatchEpoch != workflow.DispatchEpoch+1 {
		t.Fatalf("adopted workflow = state %s epoch %d, want queued epoch %d",
			adopted.State, adopted.DispatchEpoch, workflow.DispatchEpoch+1)
	}
	if adopted.OwnerInstanceID != "" || adopted.OwnerBootID != "" || adopted.LeaseUntil != nil {
		t.Fatalf("adopted workflow retained stale lease: %+v", adopted)
	}
}
