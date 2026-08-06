package im

import (
	"reflect"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestIMAnswerAdmissionPrefersCurrentAgentOverStaleSessionState(t *testing.T) {
	req := &qaRequest{
		agent: &types.CustomAgent{Config: types.CustomAgentConfig{
			ModelID:        "current-model",
			KnowledgeBases: []string{"current-kb"},
		}},
		session: &types.Session{LastRequestState: &types.SessionLastRequestState{
			ModelID:          "stale-model",
			KnowledgeBaseIDs: []string{"stale-kb"},
		}},
	}
	agentModel, summaryModel, knowledgeBases := imAnswerAdmissionHints(req)
	if agentModel != "current-model" || summaryModel != "" {
		t.Fatalf("agent=%q summary=%q", agentModel, summaryModel)
	}
	if !reflect.DeepEqual(knowledgeBases, []string{"current-kb"}) {
		t.Fatalf("knowledge bases=%v", knowledgeBases)
	}
}

func TestIMAnswerAdmissionFallsBackWhenCurrentAgentHasNoRoutingHints(t *testing.T) {
	req := &qaRequest{
		agent: &types.CustomAgent{},
		session: &types.Session{LastRequestState: &types.SessionLastRequestState{
			ModelID:          "session-model",
			KnowledgeBaseIDs: []string{"session-kb"},
		}},
	}
	agentModel, summaryModel, knowledgeBases := imAnswerAdmissionHints(req)
	if agentModel != "" || summaryModel != "session-model" {
		t.Fatalf("agent=%q summary=%q", agentModel, summaryModel)
	}
	if !reflect.DeepEqual(knowledgeBases, []string{"session-kb"}) {
		t.Fatalf("knowledge bases=%v", knowledgeBases)
	}
}
