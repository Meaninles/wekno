package agentruntime

import (
	"context"
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/custom/testsupport"
	"github.com/stretchr/testify/require"
	"testing"
)

func TestPostgresSharedBudgetReservesFinalCallAndSeparatesAgentPolicies(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	s := &Service{db: db}
	t.Setenv("AGENT_RUNTIME_MAX_TOTAL_TOKENS", "3000000")
	t.Setenv("AGENT_RUNTIME_KNOWLEDGE_QA_MAX_TOTAL_TOKENS", "1000000")
	for _, kind := range []string{"general-agent", "document-processing-agent", "table-analysis", "custom-agent", "knowledge-qa"} {
		row.Payload = json.RawMessage(`{"runtime_config":{"agent_type":"` + kind + `"}}`)
		b, err := modelBudget(db, row, "")
		require.NoError(t, err)
		want := int64(3000000)
		if kind == "knowledge-qa" {
			want = 1000000
		}
		require.Equal(t, want, b.MaxTokens)
	}
	row.Payload = json.RawMessage(`{"runtime_config":{"agent_type":"general-agent"}}`)
	row.InputTokens = 2990000
	require.NoError(t, db.Save(row).Error)
	_, err := s.reserveModelRequest(context.Background(), row.ID, 2, "vision", 1000, 100, true)
	require.ErrorIs(t, err, errFinalizationRequired, "Auxiliary calls cannot use the final reserve")
	_, err = s.reserveModelRequest(context.Background(), row.ID, 2, "", 1000, 100)
	require.ErrorIs(t, err, errFinalizationRequired)
	final, err := s.reserveModelRequest(context.Background(), row.ID, 2, "", 1000, 100, true)
	require.NoError(t, err)
	require.NoError(t, s.finishModelRequest(context.Background(), final, 800, 50))
	require.NoError(t, db.First(row, "id = ?", row.ID).Error)
	require.EqualValues(t, 1, row.ModelRequests)
	require.Zero(t, row.ReservedTokens)
	_, err = s.reserveModelRequest(context.Background(), row.ID, 2, "", 50000, 100, true)
	require.ErrorIs(t, err, errModelBudget)
}

func TestPostgresInputCalibrationUsesActualUsageWithinModelRole(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	s := &Service{db: db}
	call, err := s.reserveModelRequest(context.Background(), row.ID, 2, "", 10000, 1000)
	require.NoError(t, err)
	require.EqualValues(t, 12500, call.ReservedInput)
	require.NoError(t, s.finishModelRequest(context.Background(), call, 6000, 100))
	b, err := modelBudget(db, row, "")
	require.NoError(t, err)
	require.InDelta(t, 0.69, b.InputFactor, 0.001)
	vision, err := modelBudget(db, row, "vision")
	require.NoError(t, err)
	require.Equal(t, 1.25, vision.InputFactor)
}

func TestArtifactTokenAndRequestBudgetsFollowCapability(t *testing.T) {
	db := testsupport.Postgres(t, &RunRecord{}, &ModelRequest{})
	row := fixtureRun(t, db)
	t.Setenv("AGENT_RUNTIME_ARTIFACT_MAX_TOTAL_TOKENS", "6000000")
	t.Setenv("AGENT_RUNTIME_ARTIFACT_MAX_MODEL_REQUESTS", "200")
	for _, kind := range []string{"general-agent", "document-processing-agent", "table-analysis", "data-analysis", "custom", "wiki-qa"} {
		row.Payload = json.RawMessage(`{"enable_artifacts":true,"runtime_config":{"agent_type":"` + kind + `"}}`)
		b, err := modelBudget(db, row, "")
		require.NoError(t, err)
		require.EqualValues(t, 6000000, b.MaxTokens)
		require.EqualValues(t, 200, b.MaxRequests)
	}
}
