package documentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/custom/modules/enrichmentoutcome"
	"github.com/Tencent/WeKnora/internal/custom/modules/processownership"
	"github.com/Tencent/WeKnora/internal/custom/modules/taskdefer"
	"github.com/Tencent/WeKnora/internal/types"
)

func stableTaskRedisContractOption(t *testing.T) asynq.RedisClientOpt {
	t.Helper()
	if os.Getenv("WEKNORA_STABLE_TASK_REDIS_CONTRACT") != "1" {
		t.Skip("set WEKNORA_STABLE_TASK_REDIS_CONTRACT=1 to run stable-task Redis contracts")
	}
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		t.Fatal("REDIS_ADDR is required for the stable-task Redis contract")
	}
	database := 14
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_STABLE_TASK_REDIS_CONTRACT_DB")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		require.NoError(t, err)
		database = parsed
	}
	return asynq.RedisClientOpt{
		Addr:     addr,
		Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"),
		DB:       database,
	}
}

func newStableTaskContractEnqueuer(
	t *testing.T,
	redisOption asynq.RedisClientOpt,
) (*Enqueuer, *asynq.Inspector, *asynq.Client) {
	t.Helper()
	base := newQueueTestCoordinator(
		t,
		"stable-repair-"+fmt.Sprint(time.Now().UnixNano()),
		"boot-1",
		2,
	)
	require.NoError(t, base.db.AutoMigrate(
		&enrichmentoutcome.Outcome{},
		&types.KnowledgeFanoutCompletion{},
	))
	inspector := asynq.NewInspector(redisOption)
	client := asynq.NewClient(redisOption)
	require.NoError(t, client.Ping())
	base.inspector = inspector
	base.client = client
	return &Enqueuer{client: client, coordinator: base}, inspector, client
}

func seedTerminalStableTask(
	t *testing.T,
	redisOption asynq.RedisClientOpt,
	inspector *asynq.Inspector,
	client *asynq.Client,
	queue string,
	taskID string,
	task *asynq.Task,
	handlerErr error,
) *asynq.TaskInfo {
	t.Helper()
	_, err := client.Enqueue(
		task,
		asynq.Queue(queue),
		asynq.TaskID(taskID),
		asynq.MaxRetry(0),
		asynq.Retention(time.Hour),
	)
	require.NoError(t, err)

	server := asynq.NewServer(redisOption, asynq.Config{
		Concurrency:     1,
		Queues:          map[string]int{queue: 1},
		ShutdownTimeout: 2 * time.Second,
	})
	require.NoError(t, server.Start(asynq.HandlerFunc(func(context.Context, *asynq.Task) error {
		return handlerErr
	})))
	t.Cleanup(server.Shutdown)

	var terminal *asynq.TaskInfo
	require.Eventually(t, func() bool {
		info, inspectErr := inspector.GetTaskInfo(queue, taskID)
		if inspectErr != nil {
			return false
		}
		if handlerErr == nil {
			if info.State != asynq.TaskStateCompleted {
				return false
			}
		} else if info.State != asynq.TaskStateArchived {
			return false
		}
		terminal = info
		return true
	}, 10*time.Second, 25*time.Millisecond)
	server.Shutdown()
	return terminal
}

func seedStableSummaryKnowledge(
	t *testing.T,
	enqueuer *Enqueuer,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
) {
	t.Helper()
	require.NoError(t, enqueuer.coordinator.db.Create(&queueTestKnowledge{
		ID:                   knowledgeID,
		TenantID:             tenantID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: generation,
		ParseStatus:          types.ParseStatusFinalizing,
		EnableStatus:         "enabled",
		UpdatedAt:            time.Now(),
	}).Error)
}

func stableSummaryTask(
	t *testing.T,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(types.SummaryGenerationPayload{
		TenantID:             tenantID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: generation,
	})
	require.NoError(t, err)
	return asynq.NewTask(types.TypeSummaryGeneration, payload)
}

func stablePostProcessTask(
	t *testing.T,
	tenantID uint64,
	knowledgeID string,
	knowledgeBaseID string,
	generation string,
) *asynq.Task {
	t.Helper()
	payload, err := json.Marshal(types.KnowledgePostProcessPayload{
		TenantID:             tenantID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: generation,
	})
	require.NoError(t, err)
	return asynq.NewTask(types.TypeKnowledgePostProcess, payload)
}

func cleanupStableTaskQueue(t *testing.T, inspector *asynq.Inspector, queue string) {
	t.Helper()
	t.Cleanup(func() {
		err := inspector.DeleteQueue(queue, true)
		if err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			t.Errorf("delete stable-task contract queue: %v", err)
		}
	})
}

func TestStableTaskLeaseExpiryConcurrentRepairKeepsOneLiveCopy(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_repair_" + suffix
	taskID := "summary:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(42)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stableSummaryTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task,
		errors.New("asynq: task lease expired"),
	)
	require.Equal(t, asynq.TaskStateArchived, terminal.State)
	require.True(t, isLeaseExpiryArchive(terminal.LastErr))

	const contenders = 32
	results := make(chan error, contenders)
	var start sync.WaitGroup
	start.Add(1)
	var workers sync.WaitGroup
	for i := 0; i < contenders; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			start.Wait()
			_, enqueueErr := processownership.EnqueueStableTask(
				context.Background(),
				enqueuer,
				task,
				queue,
				taskID,
				asynq.MaxRetry(3),
				asynq.Retention(time.Hour),
			)
			results <- enqueueErr
		}()
	}
	start.Done()
	workers.Wait()
	close(results)

	accepted := 0
	for result := range results {
		if result == nil || errors.Is(result, asynq.ErrTaskIDConflict) {
			accepted++
			continue
		}
		t.Fatalf("concurrent stable task repair returned unexpected error: %v", result)
	}
	require.Equal(t, contenders, accepted)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStatePending, info.State)
	queueInfo, err := inspector.GetQueueInfo(queue)
	require.NoError(t, err)
	require.Equal(t, 1, queueInfo.Pending)
	require.Equal(t, 1, queueInfo.Size)
}

func TestStableTaskCompletedWithoutDurableProofIsReplayed(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_completed_" + suffix
	taskID := "summary:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(43)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stableSummaryTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task, nil,
	)
	require.Equal(t, asynq.TaskStateCompleted, terminal.State)

	_, err := processownership.EnqueueStableTask(
		context.Background(),
		enqueuer,
		task,
		queue,
		taskID,
		asynq.MaxRetry(3),
		asynq.Retention(time.Hour),
	)
	require.NoError(t, err)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStatePending, info.State)
}

func TestStablePostProcessCompletedWhileFinalizingIsReplayed(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_postprocess_" + suffix
	taskID := "postprocess:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(45)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stablePostProcessTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task, nil,
	)
	require.Equal(t, asynq.TaskStateCompleted, terminal.State)

	_, err := processownership.EnqueueStableTask(
		context.Background(),
		enqueuer,
		task,
		queue,
		taskID,
		asynq.MaxRetry(3),
		asynq.Retention(time.Hour),
	)
	require.NoError(t, err)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStatePending, info.State)
}

func TestStablePostProcessDurablePublicationReceiptPreventsReplay(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_postprocess_receipt_" + suffix
	taskID := "postprocess:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(46)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	require.NoError(t, enqueuer.coordinator.db.Create(&types.KnowledgeFanoutCompletion{
		TenantID: tenantID, KnowledgeID: knowledgeID, KnowledgeBaseID: knowledgeBaseID,
		ProcessingGeneration: generation,
		ItemID:               processownership.PostProcessCompletionItem,
		CompletedAt:          time.Now(),
	}).Error)
	task := stablePostProcessTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task, nil,
	)
	require.Equal(t, asynq.TaskStateCompleted, terminal.State)

	_, err := processownership.EnqueueStableTask(
		context.Background(),
		enqueuer,
		task,
		queue,
		taskID,
		asynq.MaxRetry(3),
		asynq.Retention(time.Hour),
	)
	require.ErrorIs(t, err, asynq.ErrTaskIDConflict)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStateCompleted, info.State)
}

func TestStablePostProcessReceiptPreventsReplayWhileCountedSlotsDrain(t *testing.T) {
	enqueuer := &Enqueuer{coordinator: newQueueTestCoordinator(
		t, "stable-postprocess-pending", "boot-1", 1,
	)}
	require.NoError(t, enqueuer.coordinator.db.AutoMigrate(
		&types.KnowledgeFanoutCompletion{},
	))
	tenantID := uint64(47)
	knowledgeID := "knowledge-pending"
	knowledgeBaseID := "kb-pending"
	generation := "generation-pending"
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	require.NoError(t, enqueuer.coordinator.db.Model(&queueTestKnowledge{}).
		Where("id = ?", knowledgeID).
		Update("pending_subtasks_count", 1).Error)
	require.NoError(t, enqueuer.coordinator.db.Create(&types.KnowledgeFanoutCompletion{
		TenantID: tenantID, KnowledgeID: knowledgeID, KnowledgeBaseID: knowledgeBaseID,
		ProcessingGeneration: generation,
		ItemID:               processownership.PostProcessCompletionItem,
		CompletedAt:          time.Now(),
	}).Error)
	task := stablePostProcessTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)

	required, complete, err := stableTaskCompletionProof(
		context.Background(),
		enqueuer.coordinator.db,
		task.Type(),
		task.Payload(),
	)
	require.NoError(t, err)
	require.True(t, required)
	require.True(t, complete)
}

func TestStableTaskDurableProofPreventsDuplicateReplay(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_proof_" + suffix
	taskID := "summary:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(44)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stableSummaryTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task,
		errors.New("asynq: task lease expired"),
	)
	require.Equal(t, asynq.TaskStateArchived, terminal.State)
	require.NoError(t, enqueuer.coordinator.db.Create(&enrichmentoutcome.Outcome{
		TenantID:             tenantID,
		KnowledgeID:          knowledgeID,
		KnowledgeBaseID:      knowledgeBaseID,
		ProcessingGeneration: generation,
		ItemID:               "summary",
		Status:               enrichmentoutcome.StatusCompleted,
		CompletedAt:          time.Now(),
	}).Error)

	_, err := processownership.EnqueueStableTask(
		context.Background(),
		enqueuer,
		task,
		queue,
		taskID,
		asynq.MaxRetry(3),
		asynq.Retention(time.Hour),
	)
	require.ErrorIs(t, err, asynq.ErrTaskIDConflict)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStateArchived, info.State)
}

func TestStableTaskArchivedWithoutDurableProofIsReplayed(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_incomplete_archive_" + suffix
	taskID := "summary:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(48)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stableSummaryTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task,
		errors.New("model provider temporarily unavailable for chat"),
	)
	require.Equal(t, asynq.TaskStateArchived, terminal.State)

	_, err := processownership.EnqueueStableTask(
		context.Background(),
		enqueuer,
		task,
		queue,
		taskID,
		asynq.MaxRetry(3),
		asynq.Retention(time.Hour),
	)
	require.NoError(t, err)
	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStatePending, info.State)
	require.Equal(t, 0, info.Retried)
}

func TestDeferredResumeHandlerRevivesCompletedStableTask(t *testing.T) {
	redisOption := stableTaskRedisContractOption(t)
	enqueuer, inspector, client := newStableTaskContractEnqueuer(t, redisOption)
	t.Cleanup(func() {
		_ = inspector.Close()
		_ = client.Close()
	})

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "stable_deferred_resume_" + suffix
	taskID := "summary:knowledge-" + suffix + ":generation-" + suffix
	cleanupStableTaskQueue(t, inspector, queue)
	tenantID := uint64(49)
	knowledgeID := "knowledge-" + suffix
	knowledgeBaseID := "kb-" + suffix
	generation := "generation-" + suffix
	seedStableSummaryKnowledge(
		t, enqueuer, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	task := stableSummaryTask(
		t, tenantID, knowledgeID, knowledgeBaseID, generation,
	)
	terminal := seedTerminalStableTask(
		t, redisOption, inspector, client, queue, taskID, task, nil,
	)
	require.Equal(t, asynq.TaskStateCompleted, terminal.State)

	wakePayload, err := json.Marshal(map[string]any{
		"version":      1,
		"task_type":    task.Type(),
		"task_payload": task.Payload(),
		"queue":        queue,
		"task_id":      taskID,
		"max_retry":    3,
	})
	require.NoError(t, err)
	require.NoError(t, taskdefer.NewHandler(enqueuer).Handle(
		context.Background(),
		asynq.NewTask(taskdefer.TypeResume, wakePayload),
	))

	info, err := inspector.GetTaskInfo(queue, taskID)
	require.NoError(t, err)
	require.Equal(t, asynq.TaskStatePending, info.State)
	require.Equal(t, 0, info.Retried)
}
