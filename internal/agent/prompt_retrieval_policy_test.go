package agent

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func loadAgentPromptTemplateForPolicyTest(t *testing.T) string {
	t.Helper()
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve prompt policy test location")
	}
	path := filepath.Join(filepath.Dir(filename), "..", "..", "config", "prompt_templates", "agent_system_prompt.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read agent prompt templates: %v", err)
	}
	return string(data)
}

func promptTemplateSection(t *testing.T, content, start, end string) string {
	t.Helper()
	startAt := strings.Index(content, start)
	if startAt < 0 {
		t.Fatalf("prompt section start %q not found", start)
	}
	endAt := strings.Index(content[startAt+len(start):], end)
	if endAt < 0 {
		t.Fatalf("prompt section end %q not found", end)
	}
	return content[startAt : startAt+len(start)+endAt]
}

func TestProgressiveRAGPromptRoutesByEvidenceNeed(t *testing.T) {
	content := loadAgentPromptTemplateForPolicyTest(t)
	section := promptTemplateSection(t, content, `  - id: "progressive_rag_agent"`, `  - id: "table_analyst"`)

	for _, required := range []string{
		"silently evaluate the user's semantic request",
		"If the request is conversation-only",
		"answer directly without retrieval or tools",
		"If the request needs external or domain evidence",
		"A knowledge question does not become conversation-only merely because it quotes a negative action phrase",
		"Never expose intent classification, chain-of-thought, self-talk, tool planning, or process narration",
	} {
		if !strings.Contains(section, required) {
			t.Errorf("progressive RAG prompt is missing %q", required)
		}
	}
	if strings.Contains(section, "Otherwise, proceed to retrieval") {
		t.Error("progressive RAG prompt still contains the unconditional retrieval branch")
	}
}

func TestGeneralAgentPromptDoesNotInventFileIntent(t *testing.T) {
	content := loadAgentPromptTemplateForPolicyTest(t)
	section := promptTemplateSection(t, content, `  - id: "general_claude_agent"`, `  - id: "knowledge_base_manager_agent"`)

	if !strings.Contains(section, "only when the current user explicitly requests a file/downloadable deliverable") {
		t.Error("general agent prompt does not require current-turn file intent")
	}
	if strings.Contains(section, "when a file is the best deliverable") {
		t.Error("general agent prompt still lets the model invent artifact intent")
	}
}
