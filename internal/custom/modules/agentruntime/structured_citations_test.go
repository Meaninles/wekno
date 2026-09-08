package agentruntime

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestPostgresStructuredCitationCommitAndLostAcknowledgement(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_API_KEY", "citation-test-key")
	db := testsupport.Postgres(t, &RunRecord{}, &RunOutbox{}, &ToolReceipt{}, &Artifact{}, &ModelRequest{})
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, role text,
 content text, error_code varchar(40), knowledge_references jsonb, agent_steps jsonb, is_completed boolean,
 agent_duration_ms bigint, retrieval_stats jsonb, agent_tool_count integer, updated_at timestamptz, deleted_at timestamptz)`).Error)
	row := fixtureRun(t, db)
	row.References = []*types.SearchResult{
		{ID: "a", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "source A", EvidenceContent: "source A"},
		{ID: "b", KnowledgeBaseID: "kb", KnowledgeID: "doc", ChunkType: "text", Content: "source B", EvidenceContent: "source B"},
	}
	sourcerefs.AssignCitationIDs(row.References)
	require.NoError(t, db.Save(row).Error)
	require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id,role,is_completed) VALUES ('message-1','session-1','assistant',false)`).Error)
	router := gin.New()
	router.POST("/runs/:operation", NewHandler(&Service{db: db}).RunControl)
	call := func(op, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/runs/"+op, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer citation-test-key")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	body := `{"run_id":"run-1","owner_epoch":2,"result":{"run_id":"run-1","answer":"正文。正文。","citations":[{"text":"正文。","source_ids":["S2","S999"]}]}}`
	validated := call("validate", body)
	require.Equal(t, 200, validated.Code, validated.Body.String())
	require.NotContains(t, validated.Body.String(), "<src")
	budget := call("budget", `{"run_id":"run-1","owner_epoch":2}`)
	require.Equal(t, 200, budget.Code, budget.Body.String())
	var b BudgetState
	require.NoError(t, json.Unmarshal(budget.Body.Bytes(), &b))
	require.Equal(t, "run-1", b.RunID)
	require.Len(t, b.CurrentRunSources, 2)
	committed := call("commit", body)
	require.Equal(t, 200, committed.Code, committed.Body.String())
	// The exact original request can be retried after a lost response.
	replayed := call("commit", body)
	require.Equal(t, 200, replayed.Code, replayed.Body.String())
	require.JSONEq(t, committed.Body.String(), replayed.Body.String())
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	var result ChatResult
	require.NoError(t, json.Unmarshal(row.Result, &result))
	require.Equal(t, `正文。<src id="S2" />正文。`, result.Answer)
	require.Len(t, result.References, 1)
	require.Len(t, row.References, 2, "complete registry must survive filtering")
	require.JSONEq(t, `[{"text":"正文。","source_ids":["S2"]}]`, string(result.Citations))
	var count int64
	require.NoError(t, db.Model(&RunOutbox{}).Count(&count).Error)
	require.EqualValues(t, 1, count)
	require.Equal(t, 409, call("commit", strings.Replace(body, "正文。正文。", "different", 1)).Code)
}
