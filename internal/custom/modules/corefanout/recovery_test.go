package corefanout

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/kbwritefence"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newRecoveryDB(t *testing.T) *gorm.DB {
	t.Helper()
	// A canceled SQLite query may retire the sole pool connection. A file DB
	// keeps schema/data across that reconnect, matching production Lite mode.
	dsn := filepath.Join(t.TempDir(), "recovery.db") + "?_journal_mode=WAL&_busy_timeout=5000"
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	// One connection also models Lite mode's process-wide serialization and
	// makes the multi-replica test deterministic instead of SQLITE_BUSY-prone.
	sqlDB.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, sqlDB.Close()) })
	for _, statement := range []string{
		`CREATE TABLE knowledge_bases (
            id VARCHAR(64) PRIMARY KEY,
            tenant_id INTEGER NOT NULL,
            deleted_at DATETIME
        )`,
		`CREATE TABLE knowledges (
            id VARCHAR(64) PRIMARY KEY,
            tenant_id INTEGER NOT NULL,
            knowledge_base_id VARCHAR(64) NOT NULL,
            parse_status VARCHAR(32) NOT NULL,
            processing_generation VARCHAR(64) NOT NULL DEFAULT '',
            processing_owner VARCHAR(160) NOT NULL DEFAULT '',
            processing_fanout TEXT,
            processed_at DATETIME,
            deleted_at DATETIME
        )`,
		`CREATE TABLE knowledge_fanout_completions (
            tenant_id INTEGER NOT NULL,
            knowledge_id VARCHAR(64) NOT NULL,
            knowledge_base_id VARCHAR(64) NOT NULL,
            processing_generation VARCHAR(64) NOT NULL,
            item_id VARCHAR(128) NOT NULL,
            completed_at DATETIME NOT NULL,
            PRIMARY KEY (tenant_id, knowledge_id, processing_generation, item_id)
        )`,
	} {
		require.NoError(t, db.Exec(statement).Error)
	}
	return db
}

type memoryCompletionStore struct {
	mu    sync.Mutex
	items map[string]struct{}
}

func completionKey(tenantID uint64, knowledgeID, kbID, generation, item string) string {
	return fmt.Sprintf("%d/%s/%s/%s/%s", tenantID, kbID, knowledgeID, generation, item)
}

func completionPrefix(tenantID uint64, knowledgeID, kbID, generation string) string {
	return fmt.Sprintf("%d/%s/%s/%s/", tenantID, kbID, knowledgeID, generation)
}

func (s *memoryCompletionStore) RecordKnowledgeFanoutCompletion(
	_ context.Context,
	tenantID uint64,
	knowledgeID, kbID, generation, item string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.items == nil {
		s.items = make(map[string]struct{})
	}
	key := completionKey(tenantID, knowledgeID, kbID, generation, item)
	_, existed := s.items[key]
	s.items[key] = struct{}{}
	return !existed, nil
}

func (s *memoryCompletionStore) ListKnowledgeFanoutCompletions(
	_ context.Context,
	tenantID uint64,
	knowledgeID, kbID, generation string,
) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prefix := completionPrefix(tenantID, knowledgeID, kbID, generation)
	var result []string
	for key := range s.items {
		if len(key) >= len(prefix) && key[:len(prefix)] == prefix {
			result = append(result, key[len(prefix):])
		}
	}
	return result, nil
}

func (s *memoryCompletionStore) CountKnowledgeFanoutCompletions(
	ctx context.Context,
	tenantID uint64,
	knowledgeID, kbID, generation string,
) (int64, error) {
	items, err := s.ListKnowledgeFanoutCompletions(ctx, tenantID, knowledgeID, kbID, generation)
	return int64(len(items)), err
}

func (s *memoryCompletionStore) KnowledgeFanoutCompletionExists(
	_ context.Context,
	tenantID uint64,
	knowledgeID, kbID, generation, item string,
) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, exists := s.items[completionKey(tenantID, knowledgeID, kbID, generation, item)]
	return exists, nil
}

type stableRecordingEnqueuer struct {
	mu sync.Mutex

	attempts    []string
	accepted    map[string]*asynq.Task
	failOnce    map[string]error
	failEntered chan struct{}
	failRelease <-chan struct{}
	called      chan struct{}
}

func taskIDFromOptions(options []asynq.Option) string {
	for _, option := range options {
		if option != nil && option.Type() == asynq.TaskIDOpt {
			value, _ := option.Value().(string)
			return value
		}
	}
	return ""
}

func (e *stableRecordingEnqueuer) Enqueue(
	task *asynq.Task,
	options ...asynq.Option,
) (*asynq.TaskInfo, error) {
	id := taskIDFromOptions(options)
	if id == "" {
		return nil, errors.New("test enqueuer requires a stable task ID")
	}
	e.mu.Lock()
	if e.accepted == nil {
		e.accepted = make(map[string]*asynq.Task)
	}
	e.attempts = append(e.attempts, id)
	if _, exists := e.accepted[id]; exists {
		e.mu.Unlock()
		return nil, asynq.ErrTaskIDConflict
	}
	if err := e.failOnce[id]; err != nil {
		delete(e.failOnce, id)
		entered := e.failEntered
		release := e.failRelease
		e.failEntered = nil
		e.failRelease = nil
		e.mu.Unlock()
		if entered != nil {
			entered <- struct{}{}
		}
		if release != nil {
			<-release
		}
		return nil, err
	}
	e.accepted[id] = task
	called := e.called
	e.mu.Unlock()
	if called != nil {
		select {
		case called <- struct{}{}:
		default:
		}
	}
	return &asynq.TaskInfo{ID: id, Type: task.Type()}, nil
}

func (e *stableRecordingEnqueuer) acceptedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.accepted))
	for id := range e.accepted {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (e *stableRecordingEnqueuer) attemptedIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.attempts)
}

func validPlan(id, kbID, generation string) processownership.FanoutPlan {
	return processownership.FanoutPlan{
		Version:              processownership.FanoutPlanVersion,
		TenantID:             7,
		KnowledgeID:          id,
		KnowledgeBaseID:      kbID,
		ProcessingGeneration: generation,
	}
}

func insertCandidate(
	t *testing.T,
	db *gorm.DB,
	id string,
	planRaw []byte,
) *types.Knowledge {
	t.Helper()
	kbID := "kb-1"
	generation := "generation-" + id
	require.NoError(t, db.Exec(
		"INSERT OR IGNORE INTO knowledge_bases (id, tenant_id) VALUES (?, ?)", kbID, 7,
	).Error)
	now := time.Now().UTC()
	require.NoError(t, db.Exec(
		`INSERT INTO knowledges
         (id, tenant_id, knowledge_base_id, parse_status, processing_generation,
          processing_owner, processing_fanout, processed_at)
         VALUES (?, 7, ?, ?, ?, '', ?, ?)`,
		id, kbID, types.ParseStatusProcessing, generation, string(planRaw), now,
	).Error)
	return &types.Knowledge{
		ID:                   id,
		TenantID:             7,
		KnowledgeBaseID:      kbID,
		ParseStatus:          types.ParseStatusProcessing,
		ProcessingGeneration: generation,
		ProcessingFanout:     append(types.JSON(nil), planRaw...),
		ProcessedAt:          &now,
	}
}

func insertValidCandidate(t *testing.T, db *gorm.DB, id string) (*types.Knowledge, processownership.FanoutPlan) {
	t.Helper()
	plan := validPlan(id, "kb-1", "generation-"+id)
	raw, err := processownership.MarshalFanoutPlan(plan)
	require.NoError(t, err)
	return insertCandidate(t, db, id, raw), plan
}

func testRecovery(
	db *gorm.DB,
	enqueuer *stableRecordingEnqueuer,
	config Config,
) *Recovery {
	return NewRecoveryWithConfig(db, enqueuer, nil, &memoryCompletionStore{}, config)
}

func TestRecoverNowReplaysCommittedCorePlan(t *testing.T) {
	db := newRecoveryDB(t)
	_, plan := insertValidCandidate(t, db, "doc-1")
	enqueuer := &stableRecordingEnqueuer{}
	recovery := testRecovery(db, enqueuer, Config{BatchSize: 2})

	require.NoError(t, recovery.RecoverNow(context.Background()))
	assert.Equal(t,
		[]string{processownership.PostProcessTaskID(plan.KnowledgeID, plan.ProcessingGeneration)},
		enqueuer.acceptedIDs(),
	)

	var stored types.Knowledge
	require.NoError(t, db.Table("knowledges").Where("id = ?", plan.KnowledgeID).Take(&stored).Error)
	assert.Equal(t, types.ParseStatusProcessing, stored.ParseStatus)
	assert.JSONEq(t, string(mustMarshalPlan(t, plan)), string(stored.ProcessingFanout))
}

func mustMarshalPlan(t *testing.T, plan processownership.FanoutPlan) []byte {
	t.Helper()
	raw, err := processownership.MarshalFanoutPlan(plan)
	require.NoError(t, err)
	return raw
}

func TestRecoverNowPartialEnqueueReplayIsIdempotentAcrossReplicas(t *testing.T) {
	db := newRecoveryDB(t)
	plan := validPlan("doc-partial", "kb-1", "generation-doc-partial")
	plan.DataTable = &processownership.DataTableFanout{SummaryModel: "summary", EmbeddingModel: "embedding"}
	plan.Images = []processownership.ImageFanout{
		{ChunkID: "chunk-0", ImageURL: "local://image-0", Index: 0},
		{ChunkID: "chunk-1", ImageURL: "local://image-1", Index: 1},
	}
	insertCandidate(t, db, plan.KnowledgeID, mustMarshalPlan(t, plan))
	failingID := processownership.ImageTaskID(plan.KnowledgeID, plan.ProcessingGeneration, 0)
	enqueueErr := errors.New("redis temporarily unavailable")
	enqueuer := &stableRecordingEnqueuer{failOnce: map[string]error{failingID: enqueueErr}}
	store := &memoryCompletionStore{}
	first := NewRecoveryWithConfig(db, enqueuer, nil, store, Config{BatchSize: 1})
	second := NewRecoveryWithConfig(db, enqueuer, nil, store, Config{BatchSize: 1})

	err := first.RecoverNow(context.Background())
	require.ErrorIs(t, err, enqueueErr)
	require.NoError(t, second.RecoverNow(context.Background()))
	require.NoError(t, first.RecoverNow(context.Background()))

	want := []string{
		processownership.DataTableSummaryTaskID(plan.KnowledgeID, plan.ProcessingGeneration),
		processownership.ImageTaskID(plan.KnowledgeID, plan.ProcessingGeneration, 0),
		processownership.ImageTaskID(plan.KnowledgeID, plan.ProcessingGeneration, 1),
	}
	slices.Sort(want)
	assert.Equal(t, want, enqueuer.acceptedIDs())
}

func TestRecoverNowUsesDurableLedgerAndReplaysPostProcessAfterFanIn(t *testing.T) {
	db := newRecoveryDB(t)
	plan := validPlan("doc-complete", "kb-1", "generation-doc-complete")
	plan.Images = []processownership.ImageFanout{
		{ChunkID: "chunk-0", ImageURL: "local://image-0", Index: 0},
		{ChunkID: "chunk-1", ImageURL: "local://image-1", Index: 1},
	}
	insertCandidate(t, db, plan.KnowledgeID, mustMarshalPlan(t, plan))
	for _, item := range []string{
		processownership.ImageFanoutItem(0),
		processownership.ImageFanoutItem(1),
	} {
		require.NoError(t, db.Exec(
			`INSERT INTO knowledge_fanout_completions
			 (tenant_id, knowledge_id, knowledge_base_id, processing_generation, item_id, completed_at)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			plan.TenantID, plan.KnowledgeID, plan.KnowledgeBaseID,
			plan.ProcessingGeneration, item, time.Now().UTC(),
		).Error)
	}
	enqueuer := &stableRecordingEnqueuer{}

	require.NoError(t, testRecovery(db, enqueuer, Config{BatchSize: 1}).RecoverNow(context.Background()))
	assert.Equal(t,
		[]string{processownership.PostProcessTaskID(plan.KnowledgeID, plan.ProcessingGeneration)},
		enqueuer.acceptedIDs(),
	)
}

func TestRecoverNowFrontPageFailuresDoNotStarveLaterPages(t *testing.T) {
	db := newRecoveryDB(t)
	for _, id := range []string{"doc-001", "doc-002", "doc-003", "doc-004", "doc-005"} {
		insertCandidate(t, db, id, []byte("{"))
	}
	_, valid := insertValidCandidate(t, db, "doc-999")
	enqueuer := &stableRecordingEnqueuer{}
	recovery := testRecovery(db, enqueuer, Config{BatchSize: 2})

	err := recovery.RecoverNow(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "doc-001")
	assert.Contains(t, err.Error(), "doc-005")
	assert.Equal(t,
		[]string{processownership.PostProcessTaskID(valid.KnowledgeID, valid.ProcessingGeneration)},
		enqueuer.acceptedIDs(),
	)
	cursor, highWater := recovery.scanState()
	assert.Empty(t, cursor)
	assert.Empty(t, highWater)
}

func TestRecoverNowCanceledCycleResumesAfterAttemptedFailure(t *testing.T) {
	db := newRecoveryDB(t)
	_, firstPlan := insertValidCandidate(t, db, "doc-001")
	_, secondPlan := insertValidCandidate(t, db, "doc-002")
	firstID := processownership.PostProcessTaskID(firstPlan.KnowledgeID, firstPlan.ProcessingGeneration)
	secondID := processownership.PostProcessTaskID(secondPlan.KnowledgeID, secondPlan.ProcessingGeneration)
	enqueueErr := errors.New("controlled redis failure")
	entered := make(chan struct{})
	release := make(chan struct{})
	firstEnqueuer := &stableRecordingEnqueuer{
		failOnce:    map[string]error{firstID: enqueueErr},
		failEntered: entered,
		failRelease: release,
	}
	recovery := testRecovery(db, firstEnqueuer, Config{BatchSize: 1})
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- recovery.RecoverNow(ctx)
	}()

	// The first row is now inside Enqueue. Cancel only after that observable
	// barrier, then release the controlled failure so RecoverNow advances the
	// cursor for the attempted row before observing cancellation.
	<-entered
	cancel()
	close(release)
	err := <-result
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.ErrorIs(t, err, enqueueErr)
	cursor, highWater := recovery.scanState()
	assert.Equal(t, firstPlan.KnowledgeID, cursor)
	assert.Equal(t, secondPlan.KnowledgeID, highWater)
	assert.Equal(t, []string{firstID}, firstEnqueuer.attemptedIDs())

	secondEnqueuer := &stableRecordingEnqueuer{}
	recovery.enqueuer = secondEnqueuer
	require.NoError(t, recovery.RecoverNow(context.Background()))
	assert.Equal(t, []string{secondID}, secondEnqueuer.attemptedIDs())
	assert.Equal(t, []string{secondID}, secondEnqueuer.acceptedIDs())
	assert.Equal(t, []string{firstID}, firstEnqueuer.attemptedIDs())
}

func TestRecoverNowMalformedAndMismatchedPlansRemainUntouched(t *testing.T) {
	tests := []struct {
		name string
		raw  func(*testing.T) []byte
	}{
		{name: "malformed", raw: func(*testing.T) []byte { return []byte("{") }},
		{name: "identity mismatch", raw: func(t *testing.T) []byte {
			return mustMarshalPlan(t, validPlan("different-document", "kb-1", "generation-doc"))
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRecoveryDB(t)
			raw := tt.raw(t)
			insertCandidate(t, db, "doc", raw)
			enqueuer := &stableRecordingEnqueuer{}
			err := testRecovery(db, enqueuer, Config{BatchSize: 1}).RecoverNow(context.Background())
			require.Error(t, err)
			assert.Empty(t, enqueuer.acceptedIDs())

			var row struct {
				ParseStatus      string
				ProcessingFanout string
			}
			require.NoError(t, db.Table("knowledges").Where("id = 'doc'").Take(&row).Error)
			assert.Equal(t, types.ParseStatusProcessing, row.ParseStatus)
			assert.Equal(t, string(raw), row.ProcessingFanout)
		})
	}
}

func TestRecoverOneStaleGenerationCancelAndDeletePublishNothing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*testing.T, *gorm.DB)
	}{
		{
			name: "new generation",
			mutate: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"UPDATE knowledges SET processing_generation = 'new-generation' WHERE id = 'doc'",
				).Error)
			},
		},
		{
			name: "cancelled",
			mutate: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"UPDATE knowledges SET parse_status = ? WHERE id = 'doc'", types.ParseStatusCancelled,
				).Error)
			},
		},
		{
			name: "soft deleted",
			mutate: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"UPDATE knowledges SET deleted_at = ? WHERE id = 'doc'", time.Now().UTC(),
				).Error)
			},
		},
		{
			name: "KB tombstone",
			mutate: func(t *testing.T, db *gorm.DB) {
				require.NoError(t, db.Exec(
					"UPDATE knowledge_bases SET deleted_at = ? WHERE id = 'kb-1'", time.Now().UTC(),
				).Error)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			db := newRecoveryDB(t)
			candidate, _ := insertValidCandidate(t, db, "doc")
			tt.mutate(t, db)
			enqueuer := &stableRecordingEnqueuer{}
			recovery := testRecovery(db, enqueuer, Config{BatchSize: 1})

			published, err := recovery.recoverOne(context.Background(), candidate)
			require.NoError(t, err)
			assert.False(t, published)
			assert.Empty(t, enqueuer.acceptedIDs())
		})
	}
}

func TestRecoverOneLosesConcurrentKBDeleteRaceAndPublishesNothing(t *testing.T) {
	db := newRecoveryDB(t)
	candidate, _ := insertValidCandidate(t, db, "doc-delete-race")
	enqueuer := &stableRecordingEnqueuer{}
	recovery := testRecovery(db, enqueuer, Config{BatchSize: 1})

	deleteLocked := make(chan struct{})
	releaseDelete := make(chan struct{})
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- kbwritefence.WithDeleteTransaction(context.Background(), db, func(tx *gorm.DB) error {
			if _, err := kbwritefence.LockExisting(tx, candidate.TenantID, candidate.KnowledgeBaseID); err != nil {
				return err
			}
			if err := tx.Table("knowledge_bases").Where(
				"tenant_id = ? AND id = ?", candidate.TenantID, candidate.KnowledgeBaseID,
			).Update("deleted_at", time.Now().UTC()).Error; err != nil {
				return err
			}
			close(deleteLocked)
			<-releaseDelete
			return nil
		})
	}()
	<-deleteLocked

	recoveryStarted := make(chan struct{})
	recoveryDone := make(chan struct {
		published bool
		err       error
	}, 1)
	go func() {
		close(recoveryStarted)
		published, err := recovery.recoverOne(context.Background(), candidate)
		recoveryDone <- struct {
			published bool
			err       error
		}{published: published, err: err}
	}()
	<-recoveryStarted
	close(releaseDelete)
	require.NoError(t, <-deleteDone)
	result := <-recoveryDone
	require.NoError(t, result.err)
	assert.False(t, result.published)
	assert.Empty(t, enqueuer.acceptedIDs())
}

func TestRecoverNowConcurrentReplicasPublishOneStableTask(t *testing.T) {
	db := newRecoveryDB(t)
	_, plan := insertValidCandidate(t, db, "doc")
	enqueuer := &stableRecordingEnqueuer{}
	store := &memoryCompletionStore{}
	recoveries := []*Recovery{
		NewRecoveryWithConfig(db, enqueuer, nil, store, Config{BatchSize: 1}),
		NewRecoveryWithConfig(db, enqueuer, nil, store, Config{BatchSize: 1}),
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(recoveries))
	for _, recovery := range recoveries {
		wg.Add(1)
		go func(recovery *Recovery) {
			defer wg.Done()
			errs <- recovery.RecoverNow(context.Background())
		}(recovery)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	assert.Equal(t,
		[]string{processownership.PostProcessTaskID(plan.KnowledgeID, plan.ProcessingGeneration)},
		enqueuer.acceptedIDs(),
	)
}

func TestRecoveryRunsImmediatelyAndPeriodically(t *testing.T) {
	t.Run("startup", func(t *testing.T) {
		db := newRecoveryDB(t)
		insertValidCandidate(t, db, "startup-doc")
		called := make(chan struct{}, 1)
		enqueuer := &stableRecordingEnqueuer{called: called}
		recovery := testRecovery(db, enqueuer, Config{
			ScanInterval: time.Hour,
			ScanTimeout:  time.Second,
			BatchSize:    1,
		})
		recovery.Start(context.Background())
		t.Cleanup(recovery.Stop)
		select {
		case <-called:
		case <-time.After(time.Second):
			t.Fatal("startup recovery did not replay the durable plan")
		}
	})

	t.Run("periodic", func(t *testing.T) {
		db := newRecoveryDB(t)
		called := make(chan struct{}, 1)
		enqueuer := &stableRecordingEnqueuer{called: called}
		recovery := testRecovery(db, enqueuer, Config{
			ScanInterval: 10 * time.Millisecond,
			ScanTimeout:  time.Second,
			BatchSize:    1,
		})
		recovery.Start(context.Background())
		t.Cleanup(recovery.Stop)
		// Let the immediate empty scan complete, then add durable work for a
		// later ticker cycle.
		time.Sleep(25 * time.Millisecond)
		insertValidCandidate(t, db, "periodic-doc")
		select {
		case <-called:
		case <-time.After(time.Second):
			t.Fatal("periodic recovery did not replay the durable plan")
		}
	})
}
