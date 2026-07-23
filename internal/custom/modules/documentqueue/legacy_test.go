package documentqueue

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/require"

	"github.com/Tencent/WeKnora/internal/types"
)

func legacyForwardRedisOption(t *testing.T) asynq.RedisClientOpt {
	t.Helper()
	if os.Getenv("WEKNORA_DOCUMENT_QUEUE_REDIS_CONTRACT") != "1" {
		t.Skip("set WEKNORA_DOCUMENT_QUEUE_REDIS_CONTRACT=1 to run the Redis forwarding contract")
	}
	addr := strings.TrimSpace(os.Getenv("REDIS_ADDR"))
	if addr == "" {
		t.Fatal("REDIS_ADDR is required for the Redis forwarding contract")
	}
	database := 15
	if raw := strings.TrimSpace(os.Getenv("WEKNORA_DOCUMENT_QUEUE_REDIS_CONTRACT_DB")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		require.NoError(t, err)
		database = parsed
	}
	return asynq.RedisClientOpt{
		Addr: addr, Username: os.Getenv("REDIS_USERNAME"),
		Password: os.Getenv("REDIS_PASSWORD"), DB: database,
	}
}

func TestForwardLegacyRootIsIdempotent(t *testing.T) {
	redisOption := legacyForwardRedisOption(t)
	inspector := asynq.NewInspector(redisOption)
	t.Cleanup(func() { _ = inspector.Close() })
	queues, err := inspector.Queues()
	require.NoError(t, err)
	for _, queue := range queues {
		if queue == types.QueueDocument {
			t.Skipf("Redis DB %d already contains %q; choose an empty contract DB", redisOption.DB, queue)
		}
	}
	t.Cleanup(func() {
		err := inspector.DeleteQueue(types.QueueDocument, true)
		if err != nil && !errors.Is(err, asynq.ErrQueueNotFound) {
			t.Errorf("delete document contract queue: %v", err)
		}
	})

	client := asynq.NewClient(redisOption)
	require.NoError(t, client.Ping())
	t.Cleanup(func() { _ = client.Close() })
	base := newQueueTestCoordinator(t, "legacy-forwarder", "legacy-boot", 2)
	baseSQL, err := base.db.DB()
	require.NoError(t, err)
	t.Cleanup(func() { _ = baseSQL.Close() })
	coordinator := NewCoordinatorWithConfig(
		base.db, client, "legacy-forwarder", "legacy-boot", 2, base.config,
	)
	coordinator.inspector = inspector
	payload := workflowPayload(t, 23, "knowledge-legacy-forward", "generation-legacy")
	task := asynq.NewTask(types.TypeDocumentProcess, payload)

	// Repeated legacy deliveries while queued converge on the same stable Asynq
	// task ID; TaskID conflict is an acknowledgement, not a second publication.
	require.NoError(t, coordinator.ForwardLegacyRoot(context.Background(), task))
	require.NoError(t, coordinator.ForwardLegacyRoot(context.Background(), task))

	var workflow Workflow
	require.NoError(t, coordinator.db.Take(&workflow).Error)
	require.NoError(t, coordinator.db.Model(&Workflow{}).Where("id = ?", workflow.ID).
		Update("state", StateLeased).Error)
	// A late legacy copy must also ACK when QueueDocument already owns the row.
	require.NoError(t, coordinator.ForwardLegacyRoot(context.Background(), task))

	info, err := inspector.GetQueueInfo(types.QueueDocument)
	require.NoError(t, err)
	require.Equal(t, 1, info.Pending)
	require.Equal(t, 1, info.Size)
	pending, err := inspector.ListPendingTasks(types.QueueDocument)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	var delivery identityPayload
	require.NoError(t, json.Unmarshal(pending[0].Payload, &delivery))
	require.Equal(t, workflow.ID, delivery.DocumentWorkflowID)
	require.EqualValues(t, 1, delivery.DocumentWorkflowEpoch)
	var rows int64
	require.NoError(t, coordinator.db.Model(&Workflow{}).Count(&rows).Error)
	require.EqualValues(t, 1, rows)
}
