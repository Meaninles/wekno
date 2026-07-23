package wikidelete

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/hibiken/asynq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func markDeleteRecoveryStale(t *testing.T, db *gorm.DB, ids ...string) time.Time {
	t.Helper()
	stale := time.Now().UTC().Add(-deleteRecoveryStaleAfter - time.Minute)
	for _, id := range ids {
		require.NoError(t, db.Exec("UPDATE knowledges SET updated_at = ? WHERE id = ?", stale, id).Error)
	}
	return stale
}

type deleteRecoveryEnqueuer struct {
	mu    sync.Mutex
	tasks []*asynq.Task
	err   error
}

func (e *deleteRecoveryEnqueuer) Enqueue(task *asynq.Task, _ ...asynq.Option) (*asynq.TaskInfo, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.tasks = append(e.tasks, task)
	if e.err != nil {
		return nil, e.err
	}
	return &asynq.TaskInfo{ID: "delete-recovery", Type: task.Type()}, nil
}

func TestDeleteRecoveryPublishesOnlyDurableDeletingRows(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "deleting", 7, "kb-1", types.ParseStatusDeleting)
	insertKnowledge(t, db, "completed", 7, "kb-1", types.ParseStatusCompleted)
	stale := markDeleteRecoveryStale(t, db, "deleting", "completed")
	enqueuer := &deleteRecoveryEnqueuer{}

	require.NoError(t, NewRecovery(db, enqueuer).RecoverNow(context.Background()))
	require.Len(t, enqueuer.tasks, 1)
	assert.Equal(t, types.TypeKnowledgeListDelete, enqueuer.tasks[0].Type())
	var payload types.KnowledgeListDeletePayload
	require.NoError(t, json.Unmarshal(enqueuer.tasks[0].Payload(), &payload))
	assert.Equal(t, uint64(7), payload.TenantID)
	assert.Equal(t, []string{"deleting"}, payload.KnowledgeIDs)
	assert.Equal(t, "kb-1", payload.ExpectedKnowledgeBaseID)
	require.NotNil(t, payload.RecoveryClaimedAt)
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "deleting"),
		"recovery must retain the durable intent")
	var claimedAt time.Time
	require.NoError(t, db.Table("knowledges").Select("updated_at").Where("id = ?", "deleting").Scan(&claimedAt).Error)
	assert.True(t, claimedAt.After(stale), "a published wake-up must advance the durable claim")
}

func TestDeleteRecoveryReturnsEnqueueFailureAndKeepsIntent(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "deleting", 7, "kb-1", types.ParseStatusDeleting)
	stale := markDeleteRecoveryStale(t, db, "deleting")
	queueErr := errors.New("redis unavailable")
	err := NewRecovery(db, &deleteRecoveryEnqueuer{err: queueErr}).RecoverNow(context.Background())
	require.ErrorIs(t, err, queueErr)
	assert.Equal(t, types.ParseStatusDeleting, knowledgeStatus(t, db, "deleting"))
	var updatedAt time.Time
	require.NoError(t, db.Table("knowledges").Select("updated_at").Where("id = ?", "deleting").Scan(&updatedAt).Error)
	assert.WithinDuration(t, stale, updatedAt, time.Microsecond, "a failed enqueue must not consume the stale claim")
}

func TestDeleteRecoveryIgnoresFreshDeletingRows(t *testing.T) {
	db := newCoordinatorDB(t)
	insertKnowledge(t, db, "fresh", 7, "kb-1", types.ParseStatusDeleting)
	require.NoError(t, db.Exec("UPDATE knowledges SET updated_at = ? WHERE id = ?", time.Now().UTC(), "fresh").Error)
	enqueuer := &deleteRecoveryEnqueuer{}

	require.NoError(t, NewRecovery(db, enqueuer).RecoverNow(context.Background()))
	assert.Empty(t, enqueuer.tasks, "a normal in-flight delete owns a fresh deleting row")
}

func TestDeleteRecoveryRotatesPastFirstLimit(t *testing.T) {
	db := newCoordinatorDB(t)
	ids := make([]string, 0, deleteRecoveryLimit+1)
	for i := 0; i < deleteRecoveryLimit+1; i++ {
		id := fmt.Sprintf("deleting-%04d", i)
		ids = append(ids, id)
		insertKnowledge(t, db, id, 7, "kb-1", types.ParseStatusDeleting)
	}
	markDeleteRecoveryStale(t, db, ids...)
	enqueuer := &deleteRecoveryEnqueuer{}
	recovery := NewRecovery(db, enqueuer)

	require.NoError(t, recovery.RecoverNow(context.Background()))
	require.Len(t, enqueuer.tasks, deleteRecoveryLimit)
	require.NoError(t, recovery.RecoverNow(context.Background()))
	assert.Len(t, enqueuer.tasks, deleteRecoveryLimit+1,
		"advanced claims must expose rows beyond the first bounded page")
}
