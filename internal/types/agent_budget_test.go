package types

import "testing"

func TestAllProfilesRetainIdentityWithFixedBudgets(t *testing.T) {
	if err := LoadBuiltinAgentsConfig("../../config"); err != nil {
		t.Fatal(err)
	}
	wantTypes := map[string]string{
		BuiltinKnowledgeQAID:  AgentTypeKnowledgeQA,
		BuiltinGeneralAgentID: AgentTypeGeneralAgent, BuiltinDataAnalystID: AgentTypeDataAnalysis,
		BuiltinTableAnalystID: AgentTypeTableAnalysis, BuiltinDocumentProcessingID: AgentTypeDocumentProcessingAgent,
		BuiltinWikiFixerID: AgentTypeCustom,
	}
	if GetBuiltinAgent("builtin-quick-answer", 1) != nil {
		t.Fatal("retired profile remains callable")
	}
	if GetBuiltinAgent("builtin-simple-chat", 1) != nil {
		t.Fatal("retired simple chat remains callable")
	}
	if GetBuiltinAgent("builtin-wiki-researcher", 1) != nil || IsBuiltinAgentID("builtin-wiki-researcher") {
		t.Fatal("removed Wiki Questioner remains registered")
	}
	for id, typ := range wantTypes {
		a := GetBuiltinAgent(id, 1)
		if a == nil || a.Config.AgentType != typ {
			t.Fatalf("profile identity lost: %s", id)
		}
		a.Config.MaxIterations = 999
		a.EnsureDefaults()
		want := 50
		if a.Config.EnableArtifacts {
			want = 100
		}
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

func TestArtifactBudgetFollowsCapabilityAcrossEveryProfile(t *testing.T) {
	for _, kind := range []string{AgentTypeGeneralAgent, AgentTypeWikiQA, AgentTypeCustom, AgentTypeDataAnalysis, AgentTypeTableAnalysis, AgentTypeDocumentProcessingAgent, AgentTypeKnowledgeBaseManager} {
		if AgentIterationBudget(kind, true) != 100 || AgentIterationBudget(kind, false) != 50 {
			t.Fatalf("wrong capability budget for %s", kind)
		}
	}
	if AgentIterationBudget(AgentTypeKnowledgeQA, true) != 15 {
		t.Fatal("knowledge QA budget changed")
	}
}
