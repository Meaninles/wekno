package chatpipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestQueryUnderstandResponseSchemaRequiresIndependentEvidenceNeed(t *testing.T) {
	var schema struct {
		Properties map[string]json.RawMessage `json:"properties"`
		Required   []string                   `json:"required"`
	}
	if err := json.Unmarshal(queryUnderstandResponseFormat, &schema); err != nil {
		t.Fatalf("query-understand response schema is invalid: %v", err)
	}
	if _, ok := schema.Properties["evidence_need"]; !ok {
		t.Fatal("query-understand response schema omits evidence_need")
	}
	required := make(map[string]bool, len(schema.Required))
	for _, field := range schema.Required {
		required[field] = true
	}
	for _, field := range []string{"rewrite_query", "evidence_query", "intent", "evidence_need", "image_description"} {
		if !required[field] {
			t.Fatalf("query-understand response schema does not require %q", field)
		}
	}
}

func TestParseStructuredQueryOutputConversationStateIsNonRetrieval(t *testing.T) {
	parsed, ok := parseStructuredQueryOutput(`{
		"rewrite_query":"update only the supplied owner and keep the date pending",
		"evidence_query":"",
		"intent":"conversation_state",
		"evidence_need":"none",
		"image_description":""
	}`)
	if !ok {
		t.Fatal("conversation-state output did not parse")
	}
	if parsed.Intent != types.IntentConversation {
		t.Fatalf("intent = %q, want %q", parsed.Intent, types.IntentConversation)
	}
	if parsed.EvidenceNeed != types.EvidenceNeedNone {
		t.Fatalf("evidence need = %q, want %q", parsed.EvidenceNeed, types.EvidenceNeedNone)
	}
	cm := &types.ChatManage{PipelineState: types.PipelineState{
		Intent: parsed.Intent, EvidenceNeed: parsed.EvidenceNeed,
	}}
	if cm.NeedsRetrieval() {
		t.Fatal("conversation-state intent must not activate retrieval")
	}
}

func TestParseStructuredQueryOutputMixedStateActivatesExistingKB(t *testing.T) {
	parsed, ok := parseStructuredQueryOutput(`{
		"rewrite_query":"summarize the confirmed owner and cite the governing policy",
		"evidence_query":"governing policy for the confirmed owner",
		"intent":"conversation_state",
		"evidence_need":"knowledge_base",
		"image_description":""
	}`)
	if !ok {
		t.Fatal("mixed state/evidence output did not parse")
	}
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{KnowledgeBaseIDs: []string{"kb-cross-domain"}},
		PipelineState: types.PipelineState{
			Intent: parsed.Intent, EvidenceNeed: parsed.EvidenceNeed,
		},
	}
	if !cm.NeedsRetrieval() {
		t.Fatal("mixed state/evidence request must activate an available KB")
	}
	cm.EvidenceQuery = parsed.EvidenceQuery
	if got := cm.RetrievalQuery(); got != "governing policy for the confirmed owner" {
		t.Fatalf("retrieval query = %q, want evidence-only query", got)
	}
}

func TestQueryUnderstandingContractKeepsModalityAndRetrievalBoundary(t *testing.T) {
	prompt := conversationmemory.EnsureQueryUnderstandingContract("base")
	for _, required := range []string{
		`JSON "intent" field MUST be exactly "conversation_state"`,
		`JSON "evidence_need" field MUST be exactly "none", "knowledge_base", or "web"`,
		`JSON "evidence_query" field is the source-facing question`,
		`"conversation_state" has priority over "chitchat"`,
		"Questions, examples, hypotheticals",
		`evidence_need determines whether that same turn also retrieves`,
		"does not claim that every such turn creates durable state",
		`conversation-only state or transformation task must use evidence_need "none"`,
		"external portion of a mixed request still require the appropriate evidence_need",
		"source-facing evidence_query",
		"For a mixed request, preserve the primary semantic intent",
		`Asking only to quote or attribute the user's own messages uses evidence_need "none"`,
		"Counts, outcomes (including zero), absent records, and analytical questions do not establish lifecycle state",
		"semantically equivalent unresolved labels as one state",
		"not externally persisted",
		"do not by themselves establish a concrete object's lifecycle status",
		`Use "chitchat" only for social or casual conversation`,
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("query-understanding contract missing %q: %s", required, prompt)
		}
	}
}

func TestApplyIntentPromptOverride_AgentOverrideWins(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			IntentPromptOverrides: map[string]string{"chitchat": "agent prompt"},
		},
		PipelineState: types.PipelineState{Intent: types.IntentChitchat},
	}
	global := map[string]string{"chitchat": "global prompt"}

	if !applyIntentPromptOverride(cm, global) {
		t.Fatal("expected applied=true")
	}
	if cm.SystemPromptOverride != "agent prompt" {
		t.Errorf("override: got %q, want %q", cm.SystemPromptOverride, "agent prompt")
	}
}

func TestApplyIntentPromptOverride_PreservesAgentWhitespace(t *testing.T) {
	// Agent-supplied prompts with surrounding whitespace must reach the model
	// verbatim; trim is only used for emptiness detection.
	raw := "  agent prompt with trailing newline\n"
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			IntentPromptOverrides: map[string]string{"chitchat": raw},
		},
		PipelineState: types.PipelineState{Intent: types.IntentChitchat},
	}

	if !applyIntentPromptOverride(cm, nil) {
		t.Fatal("expected applied=true")
	}
	if cm.SystemPromptOverride != raw {
		t.Errorf("override: got %q, want %q", cm.SystemPromptOverride, raw)
	}
}

func TestApplyIntentPromptOverride_BlankAgentFallsBackToGlobal(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			IntentPromptOverrides: map[string]string{"chitchat": "   \n\t  "},
		},
		PipelineState: types.PipelineState{Intent: types.IntentChitchat},
	}
	global := map[string]string{"chitchat": "global prompt"}

	if !applyIntentPromptOverride(cm, global) {
		t.Fatal("expected applied=true")
	}
	if cm.SystemPromptOverride != "global prompt" {
		t.Errorf("override: got %q, want %q", cm.SystemPromptOverride, "global prompt")
	}
}

func TestApplyIntentPromptOverride_NoOverrideAndNoGlobal(t *testing.T) {
	cm := &types.ChatManage{
		PipelineState: types.PipelineState{Intent: types.IntentChitchat},
	}

	if applyIntentPromptOverride(cm, nil) {
		t.Fatal("expected applied=false")
	}
	if cm.SystemPromptOverride != "" {
		t.Errorf("override should remain empty, got %q", cm.SystemPromptOverride)
	}
}

func TestApplyIntentPromptOverride_GlobalOnly(t *testing.T) {
	cm := &types.ChatManage{
		PipelineState: types.PipelineState{Intent: types.IntentGreeting},
	}
	global := map[string]string{"greeting": "hi there"}

	if !applyIntentPromptOverride(cm, global) {
		t.Fatal("expected applied=true")
	}
	if cm.SystemPromptOverride != "hi there" {
		t.Errorf("override: got %q, want %q", cm.SystemPromptOverride, "hi there")
	}
}

func TestApplyIntentPromptOverrideMixedRequestWithoutKBUsesSourceUnavailablePrompt(t *testing.T) {
	cm := &types.ChatManage{
		PipelineState: types.PipelineState{
			Intent:       types.IntentConversation,
			EvidenceNeed: types.EvidenceNeedKnowledgeBase,
		},
	}
	global := map[string]string{
		"conversation_state": "dialogue-only prompt",
		"kb_search":          "source unavailable prompt",
	}

	if !applyIntentPromptOverride(cm, global) {
		t.Fatal("expected source-unavailable prompt to be applied")
	}
	if cm.SystemPromptOverride != "source unavailable prompt" {
		t.Fatalf("override = %q, want source-unavailable prompt", cm.SystemPromptOverride)
	}
}
