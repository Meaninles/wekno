package types

import "testing"

func TestAllProfilesRetainIdentityWithFixedBudgets(t *testing.T) {
	if err := LoadBuiltinAgentsConfig("../../config"); err != nil {
		t.Fatal(err)
	}
	wantTypes := map[string]string{
		BuiltinKnowledgeQAID: AgentTypeKnowledgeQA, BuiltinSimpleChatID: AgentTypeCustom,
		BuiltinGeneralAgentID: AgentTypeGeneralAgent, BuiltinDataAnalystID: AgentTypeDataAnalysis,
		BuiltinTableAnalystID: AgentTypeTableAnalysis, BuiltinDocumentProcessingID: AgentTypeDocumentProcessingAgent,
		BuiltinWikiResearcherID: AgentTypeWikiQA, BuiltinWikiFixerID: AgentTypeCustom,
	}
	if GetBuiltinAgent("builtin-quick-answer", 1) != nil {
		t.Fatal("retired profile remains callable")
	}
	for id, typ := range wantTypes {
		a := GetBuiltinAgent(id, 1)
		if a == nil || a.Config.AgentType != typ {
			t.Fatalf("profile identity lost: %s", id)
		}
		a.Config.MaxIterations = 999
		a.EnsureDefaults()
		want := 50
		if id == BuiltinKnowledgeQAID {
			want = 15
		}
		if a.Config.MaxIterations != want || a.Config.AgentMode != AgentModeUnified {
			t.Fatalf("wrong budget/runtime for %s", id)
		}
	}
	for _, typ := range []string{AgentTypeKnowledgeBaseManager, AgentTypeHybridRAGWiki, AgentTypeCustom} {
		a := &CustomAgent{Config: CustomAgentConfig{AgentType: typ, MaxIterations: 1}}
		a.EnsureDefaults()
		if a.Config.MaxIterations != 50 || a.Config.AgentType != typ {
			t.Fatalf("custom profile lost: %s", typ)
		}
	}
}
