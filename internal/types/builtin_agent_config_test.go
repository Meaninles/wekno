package types

import "testing"

func TestBuiltinWikiFixerUsesWriteCapableRuntime(t *testing.T) {
	if err := LoadBuiltinAgentsConfig("../../config"); err != nil {
		t.Fatal(err)
	}
	agent := GetBuiltinAgent(BuiltinWikiFixerID, 10000)
	if agent == nil || !agent.IsAgentMode() {
		t.Fatal("Wiki edits would be routed through ordinary QA")
	}
}

func TestBuiltinSmartReasoningUsesStableFactualRAGDefaults(t *testing.T) {
	if err := LoadBuiltinAgentsConfig("../../config"); err != nil {
		t.Fatalf("load built-in agent config: %v", err)
	}

	agent := GetBuiltinAgent(BuiltinSmartReasoningID, 10000)
	if agent == nil {
		t.Fatal("smart-reasoning built-in agent is missing")
	}
	if agent.Config.AgentType != AgentTypeRAGQA {
		t.Fatalf("agent type = %q, want %q", agent.Config.AgentType, AgentTypeRAGQA)
	}
	if agent.Config.Temperature != 0 {
		t.Fatalf("temperature = %v, want deterministic factual RAG default 0", agent.Config.Temperature)
	}
	if agent.Config.HistoryTurns != 10 {
		t.Fatalf("history turns = %d, want 10", agent.Config.HistoryTurns)
	}
}
