package agent

import (
	"strings"
	"testing"
)

func TestBuildSystemPromptAppliesCitationContractToCustomKnowledgeAgent(t *testing.T) {
	prompt := BuildSystemPrompt(
		[]*KnowledgeBaseInfo{{ID: "kb-1", Name: "Knowledge"}},
		false,
		"Custom agent instructions.",
	)
	if !strings.Contains(prompt, "[WEKNORA_CITATION_OUTPUT]") ||
		!strings.Contains(prompt, `Copy the matching cite_exactly value verbatim`) {
		t.Fatalf("custom knowledge agent did not receive the shared citation contract: %s", prompt)
	}
	if strings.Count(prompt, "[WEKNORA_CITATION_OUTPUT]") != 1 {
		t.Fatalf("citation contract should be injected exactly once: %s", prompt)
	}
}

func TestBuildSystemPromptKeepsContractReusableForPureAndFutureAgentTypes(t *testing.T) {
	prompt := BuildSystemPrompt(nil, false, "Custom pure-agent instructions.")
	if !strings.Contains(prompt, "[WEKNORA_CITATION_OUTPUT]") {
		t.Fatalf("all native agents must share the same turn and citation contract: %s", prompt)
	}
}
