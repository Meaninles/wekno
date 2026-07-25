package taskdefer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/types"
)

func taskDeferRedisOption(t *testing.T) asynq.RedisClientOpt {
	t.Helper()
	if os.Getenv("WEKNORA_TASK_DEFER_REDIS_CONTRACT") != "1" {
		t.Skip("set WEKNORA_TASK_DEFER_REDIS_CONTRACT=1 to run Redis contract")
	}
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		t.Fatal("REDIS_ADDR is required")
	}
	database := 13
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_TASK_DEFER_REDIS_DB")); raw != "" {
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

func TestRedisContractRetryLimitPersistsResumeBeforeAck(t *testing.T) {
	option := taskDeferRedisOption(t)
	client := asynq.NewClient(option)
	inspector := asynq.NewInspector(option)
	t.Cleanup(func() {
		_ = client.Close()
		_ = inspector.Close()
	})
	require.NoError(t, client.Ping())

	suffix := fmt.Sprint(time.Now().UnixNano())
	queue := "task_defer_contract_" + suffix
	for _, name := range []string{queue, types.QueueCritical} {
		err := inspector.DeleteQueue(name, true)
		require.True(t, err == nil || errors.Is(err, asynq.ErrQueueNotFound))
		t.Cleanup(func() {
			_ = inspector.DeleteQueue(name, true)
		})
	}

	mux := asynq.NewServeMux()
	mux.Use(Middleware(client))
	mux.HandleFunc(types.TypeSummaryGeneration, func(context.Context, *asynq.Task) error {
		return &modeladmission.ProviderUnavailableError{
			Kind:       modeladmission.KindChat,
			RetryAfter: time.Millisecond,
			Cause:      errors.New("connection reset by peer"),
		}
	})
	server := asynq.NewServer(option, asynq.Config{
		Concurrency:     1,
		Queues:          map[string]int{queue: 1},
		ShutdownTimeout: 2 * time.Second,
	})
	require.NoError(t, server.Start(mux))
	t.Cleanup(server.Shutdown)

	taskID := "summary:knowledge-1:generation-1"
	_, err := client.Enqueue(
		summaryTask(t),
		asynq.Queue(queue),
		asynq.TaskID(taskID),
		asynq.MaxRetry(0),
		asynq.Retention(time.Hour),
	)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, inspectErr := inspector.GetTaskInfo(queue, taskID)
		return inspectErr == nil && info.State == asynq.TaskStateCompleted
	}, 10*time.Second, 25*time.Millisecond)

	var wake *asynq.TaskInfo
	require.Eventually(t, func() bool {
		tasks, inspectErr := inspector.ListScheduledTasks(types.QueueCritical)
		if inspectErr != nil || len(tasks) != 1 {
			return false
		}
		wake = tasks[0]
		return wake.Type == TypeResume
	}, 10*time.Second, 25*time.Millisecond)
	var payload resumePayload
	require.NoError(t, json.Unmarshal(wake.Payload, &payload))
	require.Equal(t, taskID, payload.TaskID)
	require.Equal(t, queue, payload.Queue)
	require.Equal(t, 0, payload.MaxRetry)

	archived, err := inspector.ListArchivedTasks(queue)
	require.NoError(t, err)
	require.Empty(t, archived)
	server.Shutdown()
}
