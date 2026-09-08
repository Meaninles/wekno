package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/Tencent/WeKnora/internal/middleware"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPostgresFinalizationSurvivesDeadlineAndReclaimsWithoutModelConfiguration(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	row.Payload = json.RawMessage(`{"enable_artifacts":true}`)
	row.OutputBaseline = json.RawMessage(`{"/workspace/outputs/old.txt":"old-hash"}`)
	row.Deadline = time.Now().Add(-time.Minute)
	require.NoError(t, db.Save(row).Error)
	service := &Service{db: db} // No model service, media or tool registry.
	payload, err := service.claimRun(context.Background(), "delivery-worker")
	require.NoError(t, err)
	require.NotNil(t, payload.Finalization)
	require.Equal(t, "task_limit", payload.Finalization.ErrorCode)
	require.Nil(t, payload.LLM)
	require.JSONEq(t, string(row.OutputBaseline), string(payload.OutputBaseline))
	require.Greater(t, payload.DeadlineUnix, float64(time.Now().Unix()))
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	require.Equal(t, "finalizing", row.Status)
	require.ErrorIs(t, owned(row, row.OwnerEpoch), errRunFenced, "No model/tool execution in delivery phase")
	require.NoError(t, ownedDelivery(row, row.OwnerEpoch))
	require.ErrorIs(t, ownedDelivery(row, row.OwnerEpoch-1), errRunFenced)
	// Two workers may race for a lost delivery lease; exactly one gets it.
	require.NoError(t, db.Model(row).Update("lease_until", time.Now().Add(-time.Minute)).Error)
	var wg sync.WaitGroup
	results := make(chan *ChatPayload, 2)
	for _, name := range []string{"recovery-a", "recovery-b"} {
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			p, e := service.claimRun(context.Background(), name)
			if e == nil {
				results <- p
			}
		}(name)
	}
	wg.Wait()
	close(results)
	count := 0
	for p := range results {
		count++
		require.NotNil(t, p.Finalization)
		require.Nil(t, p.LLM)
	}
	require.Equal(t, 1, count)
}

func TestPostgresBaselineAndFinalizationUseGlobalAuthAndImmutableRunOwnership(t *testing.T) {
	t.Setenv("AGENT_RUNTIME_API_KEY", "internal-test-key")
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	router := gin.New()
	router.Use(middleware.Auth(nil, &accountJWTRejector{}, nil, nil))
	router.POST("/api/v1/custom/agent-runtime/internal/runs/:operation", NewHandler(&Service{db: db}).RunControl)
	call := func(op, body, key string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/custom/agent-runtime/internal/runs/"+op, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	baseline := `{"run_id":"run-1","owner_epoch":2,"output_baseline":{"old":"hash"}}`
	require.Equal(t, 401, call("baseline", baseline, "wrong").Code)
	require.Equal(t, 200, call("baseline", baseline, "internal-test-key").Code)
	r := call("baseline", strings.Replace(baseline, "hash", "different", 1), "internal-test-key")
	require.Equal(t, 200, r.Code)
	require.Contains(t, r.Body.String(), "hash")
	require.NotContains(t, r.Body.String(), "different")
	require.NoError(t, db.Model(row).Update("deadline", time.Now().Add(-time.Second)).Error)
	body := `{"run_id":"run-1","owner_epoch":2,"result":{"run_id":"run-1","answer":"done"}}`
	require.Equal(t, 401, call("finalize", body, "wrong").Code)
	r = call("finalize", body, "internal-test-key")
	require.Equal(t, 200, r.Code, r.Body.String())
	require.Equal(t, 200, call("finalize", body, "internal-test-key").Code)
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	require.Equal(t, "finalizing", row.Status)
	require.JSONEq(t, `{"old":"hash"}`, string(row.OutputBaseline))
	require.Equal(t, 409, call("budget", `{"run_id":"run-1","owner_epoch":2}`, "internal-test-key").Code)
	require.Equal(t, 409, call("finalize", strings.Replace(body, `"owner_epoch":2`, `"owner_epoch":1`, 1), "internal-test-key").Code)
	require.Equal(t, 200, call("heartbeat", `{"run_id":"run-1","owner_epoch":2}`, "internal-test-key").Code)
}

func TestMessageArtifactsOnlyExposeCommittedRunInItsOwnSession(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{})
	row := fixtureRun(t, db)
	result := ChatResult{Answer: "完成", Artifacts: []SidecarArtifact{{ArtifactID: "ready", FileName: "result.html"}}}
	body, err := json.Marshal(result)
	require.NoError(t, err)
	require.NoError(t, db.Model(row).Updates(map[string]any{"status": "finalizing", "result": body}).Error)
	service := &Service{db: db}
	current := &types.Message{ID: row.MessageID, SessionID: row.SessionID, Role: "assistant"}
	wrong := &types.Message{ID: row.MessageID, SessionID: "another-session", Role: "assistant"}
	require.NoError(t, service.attachMessageArtifacts(context.Background(), []*types.Message{current, wrong}))
	require.Empty(t, current.Artifacts)
	require.NoError(t, db.Model(row).Update("status", "completed").Error)
	require.NoError(t, service.attachMessageArtifacts(context.Background(), []*types.Message{current, wrong}))
	require.Equal(t, result.Artifacts, current.Artifacts)
	require.Empty(t, wrong.Artifacts)
	history, _ := buildGeneralAgentHistory([]*types.Message{{ID: "user", SessionID: row.SessionID, Role: "user", Content: "生成文件", RequestID: "r"}, {ID: current.ID, SessionID: row.SessionID, Role: "assistant", Content: "完成", RequestID: "r", IsCompleted: true, Artifacts: current.Artifacts}}, 2)
	require.Len(t, history, 2)
	require.Contains(t, string(history[1].ContextMetadata), "delivered_artifacts")
}
