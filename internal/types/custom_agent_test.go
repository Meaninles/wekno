package types

import "testing"

func TestEnsureDefaults_ThinkingPreservesProviderDefault(t *testing.T) {
	agent := &CustomAgent{Config: CustomAgentConfig{}}
	agent.EnsureDefaults()
	if agent.Config.Thinking != nil {
		t.Fatal("EnsureDefaults must leave an unset model capability unset")
	}
}

func TestEnsureDefaults_ThinkingPreservesTrue(t *testing.T) {
	enabled := true
	agent := &CustomAgent{Config: CustomAgentConfig{Thinking: &enabled}}
	agent.EnsureDefaults()
	if agent.Config.Thinking == nil || !*agent.Config.Thinking {
		t.Fatal("EnsureDefaults must not overwrite an explicit Thinking=true")
	}
}
