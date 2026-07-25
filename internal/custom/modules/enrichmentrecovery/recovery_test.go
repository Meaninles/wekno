package enrichmentrecovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"

	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/types"
)

type recoveryEnqueuer struct {
	mu      sync.Mutex
	taskIDs map[string]struct{}
	tasks   []*asynq.Task
	err     error
}

func (e *recoveryEnqueuer) Enqueue(
	task *asynq.Task,
	opts ...asynq.Option,
) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.err != nil {
		return nil, e.err
	}
	var taskID string
	for _, option := range opts {
		if option.Type() == asynq.TaskIDOpt {
			taskID, _ = option.Value().(string)
		}
	}
	if taskID == "" {
		return nil, errors.New("test recovery enqueue requires stable task ID")
	}
	if e.taskIDs == nil {
		e.taskIDs = make(map[string]struct{})
	}
	if _, exists := e.taskIDs[taskID]; exists {
		return nil, asynq.ErrTaskIDConflict
	}
	e.taskIDs[taskID] = struct{}{}
	e.tasks = append(e.tasks, task)
	return &asynq.TaskInfo{ID: taskID}, nil
}

func newRecoveryDB(t *testing.T) *gorm.DB {
	t.Helper()
	path := filepath.ToSlash(filepath.Join(t.TempDir(), "recovery.db"))
	db, err := gorm.Open(sqlite.Open(
		"file:"+path+"?_busy_timeout=5000&_journal_mode=WAL",
	), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&types.Knowledge{}))
	return db
}

func enrichmentPlan(
	t *testing.T,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
) types.JSON {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{
		"stage":                 "enrichment",
		"version":               3,
		"tenant_id":             tenantID,
		"knowledge_id":          knowledgeID,
		"knowledge_base_id":     knowledgeBaseID,
		"processing_generation": generation,
		"language":              "zh",
		"attempt":               4,
		"tracing": map[string]any{
			"lf_trace_id": "trace-1",
		},
		"text_chunk_count": 1,
		"spawn_summary":    true,
	})
	require.NoError(t, err)
	return encoded
}

func insertCandidate(
	t *testing.T,
	db *gorm.DB,
	id string,
	updatedAt time.Time,
	plan types.JSON,
) {
	t.Helper()
	require.NoError(t, db.Create(&types.Knowledge{
		ID:                   id,
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		Type:                 "file",
		Title:                id,
		ParseStatus:          types.ParseStatusFinalizing,
		EnableStatus:         "enabled",
		ProcessingGeneration: "generation-" + id,
		ProcessingFanout:     plan,
		PendingSubtasksCount: 1,
		UpdatedAt:            updatedAt,
	}).Error)
}

func TestRecoverNowReplaysExactFinalizingPlanAndHeartbeats(t *testing.T) {
	db := newRecoveryDB(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	insertCandidate(t, db, "knowledge-a", old, enrichmentPlan(
		t, 42, "knowledge-a", "kb-1", "generation-knowledge-a",
	))
	enqueuer := &recoveryEnqueuer{}
	recovery := NewRecoveryWithConfig(db, enqueuer, Config{
		ScanInterval: time.Hour,
		ScanTimeout:  time.Minute,
		StaleAfter:   time.Hour,
		BatchSize:    1,
	})
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Len(t, enqueuer.tasks, 1)
	require.Equal(t, types.TypeKnowledgePostProcess, enqueuer.tasks[0].Type())

	var payload types.KnowledgePostProcessPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.Equal(t, uint64(42), payload.TenantID)
	require.Equal(t, "knowledge-a", payload.KnowledgeID)
	require.Equal(t, "kb-1", payload.KnowledgeBaseID)
	require.Equal(t, "generation-knowledge-a", payload.ProcessingGeneration)
	require.Equal(t, "zh", payload.Language)
	require.Equal(t, 4, payload.Attempt)
	require.Equal(t, "trace-1", payload.LangfuseTraceID)
	require.Contains(t, enqueuer.taskIDs,
		processownership.PostProcessTaskID("knowledge-a", "generation-knowledge-a"))

	var updated types.Knowledge
	require.NoError(t, db.First(&updated, "id = ?", "knowledge-a").Error)
	require.True(t, updated.UpdatedAt.After(old))
	// The recovery heartbeat suppresses immediate repeated plan walks.
	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Len(t, enqueuer.tasks, 1)
}

func TestRecoverNowAdvancesPastCorruptPlan(t *testing.T) {
	db := newRecoveryDB(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	insertCandidate(t, db, "knowledge-a", old, types.JSON(`{"stage":"enrichment"}`))
	insertCandidate(t, db, "knowledge-b", old, enrichmentPlan(
		t, 42, "knowledge-b", "kb-1", "generation-knowledge-b",
	))
	enqueuer := &recoveryEnqueuer{}
	recovery := NewRecoveryWithConfig(db, enqueuer, Config{
		ScanInterval: time.Hour,
		ScanTimeout:  time.Minute,
		StaleAfter:   time.Hour,
		BatchSize:    1,
	})
	err := recovery.RecoverNow(context.Background())
	require.Error(t, err)
	require.Contains(t, err.Error(), `knowledge="knowledge-a"`)
	require.Len(t, enqueuer.tasks, 1)
	var payload types.KnowledgePostProcessPayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	require.Equal(t, "knowledge-b", payload.KnowledgeID)
}

func TestConcurrentRecoveriesPublishOneStablePostprocess(t *testing.T) {
	db := newRecoveryDB(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	insertCandidate(t, db, "knowledge-a", old, enrichmentPlan(
		t, 42, "knowledge-a", "kb-1", "generation-knowledge-a",
	))
	enqueuer := &recoveryEnqueuer{}
	config := Config{
		ScanInterval: time.Hour,
		ScanTimeout:  time.Minute,
		StaleAfter:   time.Hour,
		BatchSize:    10,
	}
	const replicas = 16
	errs := make(chan error, replicas)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	for i := 0; i < replicas; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			start.Wait()
			errs <- NewRecoveryWithConfig(db, enqueuer, config).
				RecoverNow(context.Background())
		}()
	}
	start.Done()
	workers.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	require.Len(t, enqueuer.tasks, 1)
	require.Len(t, enqueuer.taskIDs, 1)
}

func TestRecoveryKeepsIntentWhenRedisEnqueueFails(t *testing.T) {
	db := newRecoveryDB(t)
	old := time.Now().UTC().Add(-2 * time.Hour)
	insertCandidate(t, db, "knowledge-a", old, enrichmentPlan(
		t, 42, "knowledge-a", "kb-1", "generation-knowledge-a",
	))
	queueErr := fmt.Errorf("redis unavailable")
	recovery := NewRecoveryWithConfig(db, &recoveryEnqueuer{err: queueErr}, Config{
		ScanInterval: time.Hour,
		ScanTimeout:  time.Minute,
		StaleAfter:   time.Hour,
		BatchSize:    10,
	})
	require.ErrorIs(t, recovery.RecoverNow(context.Background()), queueErr)
	var row types.Knowledge
	require.NoError(t, db.First(&row, "id = ?", "knowledge-a").Error)
	require.Equal(t, types.ParseStatusFinalizing, row.ParseStatus)
	require.Equal(t, 1, row.PendingSubtasksCount)
	require.WithinDuration(t, old, row.UpdatedAt, time.Second)
}

func TestRecoverySkipsFreshAndTerminalRows(t *testing.T) {
	db := newRecoveryDB(t)
	insertCandidate(t, db, "fresh", time.Now().UTC(), enrichmentPlan(
		t, 42, "fresh", "kb-1", "generation-fresh",
	))
	insertCandidate(t, db, "completed", time.Now().UTC().Add(-2*time.Hour), enrichmentPlan(
		t, 42, "completed", "kb-1", "generation-completed",
	))
	require.NoError(t, db.Model(&types.Knowledge{}).
		Where("id = ?", "completed").
		Update("parse_status", types.ParseStatusCompleted).Error)
	enqueuer := &recoveryEnqueuer{}
	require.NoError(t, NewRecoveryWithConfig(db, enqueuer, Config{
		ScanInterval: time.Hour,
		ScanTimeout:  time.Minute,
		StaleAfter:   time.Hour,
		BatchSize:    10,
	}).RecoverNow(context.Background()))
	require.Empty(t, enqueuer.tasks)
}
