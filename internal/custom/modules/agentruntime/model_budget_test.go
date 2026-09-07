package agentruntime

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPostgresModelBudgetRecoversLostUsageAndRejectsAdditionalRequests(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	s := &Service{db: db}
	t.Setenv("AGENT_RUNTIME_MAX_MODEL_REQUESTS", "2")
	call, err := s.reserveModelRequest(context.Background(), row.ID, 2, "", 100, 20)
	require.NoError(t, err)
	require.NoError(t, s.finishModelRequest(context.Background(), call, 40, 8))
	require.NoError(t, s.finishModelRequest(context.Background(), call, 999, 999))
	second, err := s.reserveModelRequest(context.Background(), row.ID, 2, "vision", 60, 10)
	require.NoError(t, err)
	_, err = s.reserveModelRequest(context.Background(), row.ID, 2, "", 1, 1)
	require.ErrorContains(t, err, "budget exhausted")
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		current, err := lockRun(tx, row.ID)
		if err != nil {
			return err
		}
		current.OwnerEpoch = 3
		if err = reconcileModelRequests(tx, current); err != nil {
			return err
		}
		return tx.Save(current).Error
	}))
	require.NoError(t, s.finishModelRequest(context.Background(), second, 500, 500))
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	require.EqualValues(t, 0, row.ReservedTokens)
	require.EqualValues(t, 100, row.InputTokens)
	require.EqualValues(t, 18, row.OutputTokens)
	require.EqualValues(t, 1, row.UnknownUsageRequests)
	require.EqualValues(t, 2, row.ModelRequests)
}

func TestPostgresCheckpointRejectsDelayedStateWithinSameOwner(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{})
	row := fixtureRun(t, db)
	s := &Service{db: db}
	first := json.RawMessage(`{"run_id":"run-1","sdk_version":"2.0.7.post1","checkpoint_seq":1,"agent":{}}`)
	second := json.RawMessage(`{"run_id":"run-1","sdk_version":"2.0.7.post1","checkpoint_seq":2,"agent":{"state":"latest"}}`)
	require.NoError(t, s.saveCheckpoint(context.Background(), row.ID, 2, first))
	require.NoError(t, s.saveCheckpoint(context.Background(), row.ID, 2, second))
	require.NoError(t, s.saveCheckpoint(context.Background(), row.ID, 2, second))
	require.ErrorContains(t, s.saveCheckpoint(context.Background(), row.ID, 2, first), "sequence must advance")
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	require.JSONEq(t, string(second), string(row.Checkpoint))
}

func TestPostgresLocalToolReceiptRejectsReusedIdentity(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	row := fixtureRun(t, db)
	apply := func(body string) error {
		return db.Transaction(func(tx *gorm.DB) error {
			return recordLocalTool(tx, row, StreamEvent{Type: "local_tool_start", Data: json.RawMessage(body)})
		})
	}
	require.NoError(t, apply(`{"tool_call_id":"sdk-1","tool_name":"Bash","arguments":{"command":"echo yes"}}`))
	require.ErrorContains(t, apply(`{"tool_call_id":"sdk-1","tool_name":"Bash","arguments":{"command":"echo changed"}}`), "different arguments")
}
