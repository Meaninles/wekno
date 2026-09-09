package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/agent/tools"
	"net/http/httptest"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestProcessSourcesGroupDocumentsAndPreserveWebAddresses(t *testing.T) {
	data := map[string]any{"results": []map[string]any{
		{"knowledge_id": "a", "knowledge_title": "制度", "chunk_id": "1", "content": "第一段"},
		{"knowledge_id": "a", "knowledge_title": "制度", "chunk_id": "2", "content": "第二段"},
		{"knowledge_id": "b", "knowledge_title": "制度", "content": "同名不同文档"},
		{"url": "https://example.com/x?q=1#part", "title": "网页", "snippet": "网页片段"},
		{"url": "https://example.com/x?q=1", "title": "网页", "snippet": "网页片段"},
		{"url": "javascript:alert(1)", "title": "bad"},
	}}
	sources := processSources(data)
	require.Len(t, sources, 3)
	require.Len(t, sources[0].Snippets, 2)
	require.Equal(t, "https://example.com/x?q=1#part", sources[2].URL)
	require.Len(t, sources[2].Snippets, 1)
	preview := withProcessSources(data)
	require.Equal(t, 3, preview["process_source_count"])
	require.Len(t, preview["process_sources"], 2)
	require.Nil(t, preview["process_sources"].([]map[string]any)[0]["snippets"])
	require.Len(t, tools.SanitizeToolDataForClient(preview)["process_sources"], 2)
}

func TestProcessSourcesDoNotTreatUnknownAsEmpty(t *testing.T) {
	for _, data := range []map[string]any{{}, {"results": nil}, {"results": "unstructured"}, {"results": []map[string]any{{"unexpected": "value"}}}} {
		require.NotContains(t, withProcessSources(data), "process_source_count")
	}
	require.Equal(t, 0, withProcessSources(map[string]any{"results": []any{}})["process_source_count"])
	sources := processSources(map[string]any{
		"chunk_results": []map[string]any{{"knowledge_id": "doc", "content": "匹配片段"}},
		"results":       []map[string]any{{"url": "https://example.com/page", "raw_content": "网页正文"}, {"url": "https://example.com/failed", "error": "failed"}},
		"pages":         []map[string]any{{"slug": "test", "knowledge_base_id": "kb", "title": "Wiki 页面", "snippet": "Wiki 片段"}},
	})
	require.Len(t, sources, 3)
	require.Equal(t, "网页正文", sources[0].Snippets[0].Content)
	require.Equal(t, "wiki", sources[2].Kind)
}

func TestProcessStepsSeparateRoundsAndKeepCommentaryOutOfAnswers(t *testing.T) {
	records := []RunOutbox{}
	for _, item := range []StreamEvent{
		{Type: "thought_delta", ID: "block", Revision: 1, Content: "第一轮"},
		{Type: "thought_delta", ID: "block", Revision: 1, Done: true},
		{Type: "commentary", ID: "text", Revision: 1, Content: "查找制度", Done: true},
		{Type: "thought_delta", ID: "block", Revision: 2, Content: "第二轮"},
		{Type: "answer_delta", Content: "private"},
	} {
		raw, _ := json.Marshal(item)
		records = append(records, RunOutbox{Body: raw})
	}
	steps, err := projectProcessSteps("run", nil, records)
	require.NoError(t, err)
	require.Len(t, steps, 3)
	require.NotEqual(t, steps[0].EventID, steps[2].EventID)
	require.Equal(t, "success", steps[0].ProcessStatus)
	require.Equal(t, "查找制度", steps[1].Thought)
}

func TestPostgresProcessSourcesAreScopedToTenantAndOwner(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ToolReceipt{})
	run := fixtureRun(t, db)
	require.NoError(t, db.Exec(`CREATE TABLE messages (id text PRIMARY KEY, session_id text, deleted_at timestamptz)`).Error)
	require.NoError(t, db.Exec(`INSERT INTO messages(id,session_id) VALUES ('message-1','session-1')`).Error)
	raw, _ := json.Marshal(ToolCallResponse{Data: map[string]any{"results": []map[string]any{{"knowledge_id": "doc", "knowledge_title": "私有文档", "content": "片段"}}}})
	require.NoError(t, db.Create(&ToolReceipt{RunID: run.ID, CallID: "call", Arguments: json.RawMessage(`{}`), Response: raw}).Error)
	h := NewHandler(&Service{db: db})
	for _, tc := range []struct {
		tenant uint64
		user   string
		want   int
	}{{7, "alice", 200}, {7, "bob", 404}, {8, "alice", 404}} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		ctx := context.WithValue(context.Background(), types.TenantIDContextKey, tc.tenant)
		ctx = context.WithValue(ctx, types.UserIDContextKey, tc.user)
		c.Request = httptest.NewRequest("GET", "/", nil).WithContext(ctx)
		c.Params = gin.Params{{Key: "message_id", Value: run.MessageID}, {Key: "call_id", Value: "call"}}
		h.ProcessSources(c)
		require.Equal(t, tc.want, w.Code)
		if tc.want != 200 {
			require.NotContains(t, w.Body.String(), "私有文档")
		}
	}
}
