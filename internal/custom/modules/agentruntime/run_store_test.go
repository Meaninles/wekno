package agentruntime

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestPostgresCitationResolutionNeverRequestsRegeneration(t *testing.T) {
	db := testsupport.Postgres(t, &Artifact{})
	ref := &types.SearchResult{ID: "chunk", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "source fact", EvidenceContent: "source fact"}
	sourcerefs.AssignCitationIDs([]*types.SearchResult{ref})
	row := &RunRecord{ID: "run", TenantID: 7, References: []*types.SearchResult{ref}}
	service := &Service{db: db}
	for _, answer := range []string{"uncited answer", `answer <src id="S999" />`, `answer <src id="S1" />`} {
		result := &ChatResult{RunID: row.ID, Answer: answer}
		violations, err := service.validateResult(context.Background(), db, row, result)
		require.NoError(t, err)
		require.Empty(t, violations)
		require.Equal(t, answer, result.Answer, "validation must not modify the candidate before commit")
		require.Empty(t, result.References)
	}
}

func fixtureRun(t *testing.T, db *gorm.DB) *RunRecord {
	t.Helper()
	row := &RunRecord{ID: "run-1", TenantID: 7, UserID: "alice", SessionID: "session-1", MessageID: "message-1", Status: "running", OwnerEpoch: 2, LeaseUntil: time.Now().Add(time.Minute), Deadline: time.Now().Add(time.Hour), Payload: json.RawMessage(`{}`), Scope: json.RawMessage(`{}`)}
	require.NoError(t, db.Create(row).Error)
	return row
}

func TestPostgresBudgetFailureIsIncompleteAndPublicInHistory(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text, content text, error_code varchar(40), agent_steps jsonb, is_completed boolean, updated_at timestamptz, deleted_at timestamptz)`).Error)
	row := fixtureRun(t, db)
	require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id,role,is_completed) VALUES ('message-1','session-1','assistant',false)`).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return terminateRun(tx, row, "failed", "private runtime diagnostic", "task_limit")
	}))
	var stored RunRecord
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, "incomplete", stored.Status)
	require.Equal(t, "task_limit", stored.ErrorCode)
	var msg struct {
		Content     string
		ErrorCode   string
		IsCompleted bool
	}
	require.NoError(t, db.Table("messages").First(&msg).Error)
	require.True(t, msg.IsCompleted)
	require.Equal(t, "task_limit", msg.ErrorCode)
	require.NotContains(t, msg.Content, "private")
}

func TestPostgresRuntimeRejectsStaleAndCancelledCheckpoints(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text, content text, error_code varchar(40), agent_steps jsonb, is_completed boolean, updated_at timestamptz, deleted_at timestamptz)`).Error)
	row := fixtureRun(t, db)
	service := &Service{db: db}
	state := json.RawMessage(`{"run_id":"run-1","sdk_version":"2.0.7.post1","event_seq":0,"agent":{}}`)
	require.ErrorIs(t, service.saveCheckpoint(context.Background(), row.ID, 1, state), errRunFenced)
	require.NoError(t, service.saveCheckpoint(context.Background(), row.ID, 2, state))
	require.NoError(t, service.cancelRun(context.Background(), row.ID))
	require.ErrorIs(t, service.saveCheckpoint(context.Background(), row.ID, 2, state), errRunFenced)
	var stored RunRecord
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, "cancelled", stored.Status)
	require.JSONEq(t, string(state), string(stored.Checkpoint))
}

func TestPostgresIdleClaimCommitsExpiredRunAndCompletesMessage(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text, content text, error_code varchar(40), agent_steps jsonb, is_completed boolean, updated_at timestamptz, deleted_at timestamptz)`).Error)
	row := fixtureRun(t, db)
	require.NoError(t, db.Model(row).Update("deadline", time.Now().Add(-time.Minute)).Error)
	require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id,role,is_completed) VALUES ('message-1','session-1','assistant',false)`).Error)
	_, err := (&Service{db: db}).claimRun(context.Background(), "idle-worker")
	require.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var stored RunRecord
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, "failed", stored.Status)
	var msg struct {
		Content     string
		IsCompleted bool
	}
	require.NoError(t, db.Table("messages").First(&msg).Error)
	require.True(t, msg.IsCompleted)
	require.Equal(t, "这次处理时间较长，未能完成，请稍后重试。", msg.Content)
}

func TestPostgresRuntimeCommitRollsBackWhenMessageIsMissing(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text,
 content text, error_code varchar(40), knowledge_references jsonb, agent_steps jsonb, is_completed boolean,
 agent_duration_ms bigint, retrieval_stats jsonb, agent_tool_count integer, updated_at timestamptz, deleted_at timestamptz)`).Error)
	row := fixtureRun(t, db)
	result := &ChatResult{RunID: row.ID, Answer: "committed answer"}
	require.Error(t, db.Transaction(func(tx *gorm.DB) error { return commitResult(tx, row, result) }))
	var stored RunRecord
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, "running", stored.Status)
	var count int64
	require.NoError(t, db.Model(&RunOutbox{}).Count(&count).Error)
	require.Zero(t, count)
	require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id,role,is_completed) VALUES ('message-1','session-1','assistant',false)`).Error)
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error { return commitResult(tx, &stored, result) }))
	var msg struct {
		Content     string
		IsCompleted bool
	}
	require.NoError(t, db.Table("messages").First(&msg).Error)
	require.Equal(t, result.Answer, msg.Content)
	require.True(t, msg.IsCompleted)
	require.NoError(t, db.First(&stored, "id = ?", row.ID).Error)
	require.Equal(t, "completed", stored.Status)
	require.NoError(t, db.Model(&RunOutbox{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
}

func TestPostgresRuntimeReplaysReceiptWithoutCallingToolAgain(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{})
	row := fixtureRun(t, db)
	require.NoError(t, db.Create(&ToolReceipt{RunID: row.ID, CallID: "mutation-1", OwnerEpoch: 1, Name: "write", Arguments: json.RawMessage(`{"value":42}`), Status: "completed", Response: json.RawMessage(`{"success":true,"output":"saved"}`)}).Error)
	service := &Service{db: db} // No registry/model service: replay must not initialize either.
	response, err := service.callTool(context.Background(), ToolCallRequest{RunID: row.ID, OwnerEpoch: 2, ToolCallID: "mutation-1", ToolName: "write", Arguments: json.RawMessage(`{ "value": 42 }`)})
	require.NoError(t, err)
	require.True(t, response.Success)
	require.Equal(t, "saved", response.Output)
	_, err = service.callTool(context.Background(), ToolCallRequest{RunID: row.ID, OwnerEpoch: 2, ToolCallID: "mutation-1", ToolName: "write", Arguments: json.RawMessage(`{"value":43}`)})
	require.ErrorContains(t, err, "different arguments")
	require.NoError(t, db.Model(&ToolReceipt{}).Where("run_id = ?", row.ID).Update("status", "executing").Error)
	_, err = service.callTool(context.Background(), ToolCallRequest{RunID: row.ID, OwnerEpoch: 2, ToolCallID: "mutation-1", ToolName: "write", Arguments: json.RawMessage(`{"value":42}`)})
	require.ErrorContains(t, err, "outcome is uncertain")
}

func TestRunIdentityDoesNotGrantAccountRightsToEmbedPrincipal(t *testing.T) {
	row := &RunRecord{TenantID: 7, AccountID: "embed-channel", UserID: "embed_session:7:ch:s", Principal: types.EmbedSessionPrincipal(7, "ch", "s")}
	ctx := runContext(context.Background(), row)
	_, account := types.AccountUserIDFromContext(ctx)
	require.False(t, account)
	require.Equal(t, row.UserID, types.SessionOwnerIDFromContext(ctx))
}
