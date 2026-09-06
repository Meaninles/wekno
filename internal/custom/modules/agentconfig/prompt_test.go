package agentconfig

import (
	"github.com/Tencent/WeKnora/internal/config"
	"github.com/Tencent/WeKnora/internal/types"
	"testing"
)

func TestRepairPromptReferenceResolution(t *testing.T) {
	templates := &config.PromptTemplatesConfig{AgentSystemPrompt: []config.PromptTemplate{{ID: "preset", Content: "shipped body"}}, ContextTemplate: []config.PromptTemplate{{ID: "context", Content: "evidence"}}}
	original := types.CustomAgentConfig{SystemPromptID: "preset", ContextTemplateID: "context"}
	got, err := ResolvePrompts(original, templates)
	if err != nil || got.SystemPrompt != "shipped body" || got.ContextTemplate != "evidence" || original.SystemPrompt != "" {
		t.Fatal(got, err)
	}
	original.SystemPrompt = "user override"
	got, err = ResolvePrompts(original, templates)
	if err != nil || got.SystemPrompt != "user override" {
		t.Fatal(got, err)
	}
	original.SystemPromptID = "missing"
	if _, err = ResolvePrompts(original, templates); err == nil {
		t.Fatal("unknown reference silently accepted")
	}
}

func TestTemplateEditSaveReloadDoesNotFreezeDefaults(t *testing.T) {
	templates := &config.PromptTemplatesConfig{AgentSystemPrompt: []config.PromptTemplate{{ID: "preset", Content: "shipped body"}}}
	for _, id := range []string{"", "preset"} {
		stored, err := NormalizePrompts(types.CustomAgentConfig{SystemPromptID: id, SystemPrompt: "shipped body"}, templates)
		if err != nil || stored.SystemPromptID != "preset" || stored.SystemPrompt != "" {
			t.Fatal(stored, err)
		}
		next := &config.PromptTemplatesConfig{AgentSystemPrompt: []config.PromptTemplate{{ID: "preset", Content: "updated body"}}}
		actual, err := ResolvePrompts(stored, next)
		if err != nil || actual.SystemPrompt != "updated body" {
			t.Fatal(actual, err)
		}
	}
	custom := types.CustomAgentConfig{SystemPromptID: "preset", SystemPrompt: "user override\n"}
	stored, err := NormalizePrompts(custom, templates)
	if err != nil || stored.SystemPrompt != custom.SystemPrompt {
		t.Fatal("explicit override was changed", stored, err)
	}
}
