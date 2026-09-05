package chatpipeline

import (
	"context"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestPrepareMessagesWithHistoryInjectsSharedCitationContractForEveryTurn(t *testing.T) {
	withEvidence := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query: "question",
			SummaryConfig: types.SummaryConfig{
				Prompt: "Custom agent instructions.",
			},
		},
		PipelineState: types.PipelineState{
			RenderedContexts: "[AVAILABLE_CITATIONS]\n- evidence_id=S1 | cite_exactly=<src id=\"S1\" />\n[/AVAILABLE_CITATIONS]",
			UserContent:      "question with evidence",
		},
	}
	messages := prepareMessagesWithHistory(withEvidence)
	if len(messages) < 1 || !strings.Contains(messages[0].Content, "[WEKNORA_CITATION_OUTPUT]") {
		t.Fatalf("shared citation contract missing from custom system prompt: %#v", messages)
	}
	if strings.Count(messages[0].Content, "[WEKNORA_CITATION_OUTPUT]") != 1 {
		t.Fatalf("shared citation contract should be injected once: %s", messages[0].Content)
	}
	if !strings.Contains(messages[0].Content, "A prior turn's output format, ending, or citation constraint is inactive") {
		t.Fatalf("evidence-backed multi-turn answers must not inherit stale turn constraints: %s", messages[0].Content)
	}
	if strings.Count(messages[len(messages)-1].Content, "question with evidence") != 1 {
		t.Fatalf("current request must occur once: %#v", messages)
	}

	withoutEvidence := *withEvidence
	withoutEvidence.RenderedContexts = ""
	messages = prepareMessagesWithHistory(&withoutEvidence)
	if !strings.Contains(messages[0].Content, "[WEKNORA_CITATION_OUTPUT]") {
		t.Fatalf("turn precedence and citation contract must also cover no-evidence turns: %s", messages[0].Content)
	}
}

func TestEffectiveThinkingOptionOnlyDisablesDialogueStateTurns(t *testing.T) {
	enabled := true
	conversation := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SummaryConfig: types.SummaryConfig{Thinking: &enabled}},
		PipelineState:   types.PipelineState{Intent: types.IntentConversation},
	}
	if got := effectiveThinkingOption(conversation); got == nil || *got {
		t.Fatalf("conversation-state thinking = %v, want explicit false", got)
	}
	mixed := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			KnowledgeBaseIDs: []string{"kb-other-domain"},
			SummaryConfig:    types.SummaryConfig{Thinking: &enabled},
		},
		PipelineState: types.PipelineState{
			Intent: types.IntentConversation, EvidenceNeed: types.EvidenceNeedKnowledgeBase,
		},
	}
	if got := effectiveThinkingOption(mixed); got == nil || !*got {
		t.Fatalf("mixed evidence-seeking thinking = %v, want configured true", got)
	}

	knowledge := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SummaryConfig: types.SummaryConfig{Thinking: &enabled}},
		PipelineState:   types.PipelineState{Intent: types.IntentKBSearch},
	}
	if got := effectiveThinkingOption(knowledge); got != &enabled && (got == nil || !*got) {
		t.Fatalf("knowledge thinking = %v, want configured true", got)
	}
	if got := effectiveThinkingOption(nil); got != nil {
		t.Fatalf("nil chat manage thinking = %v, want nil", got)
	}
}

func TestEffectiveTemperatureOnlyCapsDialogueStateTurns(t *testing.T) {
	conversation := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SummaryConfig: types.SummaryConfig{Temperature: 0.8}},
		PipelineState:   types.PipelineState{Intent: types.IntentConversation},
	}
	if got := effectiveTemperature(conversation); got != 0.2 {
		t.Fatalf("conversation-state temperature = %v, want 0.2", got)
	}
	conversation.SummaryConfig.Temperature = 0.1
	if got := effectiveTemperature(conversation); got != 0.1 {
		t.Fatalf("low configured temperature = %v, want 0.1", got)
	}
	mixed := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			KnowledgeBaseIDs: []string{"kb-other-domain"},
			SummaryConfig:    types.SummaryConfig{Temperature: 0.8},
		},
		PipelineState: types.PipelineState{
			Intent: types.IntentConversation, EvidenceNeed: types.EvidenceNeedKnowledgeBase,
		},
	}
	if got := effectiveTemperature(mixed); got != 0.8 {
		t.Fatalf("mixed evidence-seeking temperature = %v, want configured 0.8", got)
	}
	knowledge := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{SummaryConfig: types.SummaryConfig{Temperature: 0.8}},
		PipelineState:   types.PipelineState{Intent: types.IntentKBSearch},
	}
	if got := effectiveTemperature(knowledge); got != 0.8 {
		t.Fatalf("knowledge temperature = %v, want configured 0.8", got)
	}
}

func TestPrepareMessagesWithHistoryAddsLightweightSkillsToSystemPrompt(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:                   "question",
			LightweightSkillContext: "Lightweight skill execution contract:\n[本轮有效轻量 Skills]\n制度助手",
			SummaryConfig: types.SummaryConfig{
				Prompt: "Generic assistant baseline.",
			},
		},
		PipelineState: types.PipelineState{UserContent: "question"},
	}
	messages := prepareMessagesWithHistory(chatManage)
	if len(messages) == 0 || !strings.Contains(messages[0].Content, "制度助手") {
		t.Fatalf("lightweight Skill context missing from system prompt: %#v", messages)
	}
	if strings.Contains(messages[len(messages)-1].Content, "制度助手") {
		t.Fatalf("lightweight Skill context must not be injected as user text: %#v", messages)
	}
}

func TestPrepareMessagesWithHistoryDoesNotPhraseFilterNormalHistory(t *testing.T) {
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "请做完整状态审计，不要重新检索制度。",
			SummaryConfig: types.SummaryConfig{Prompt: "Audit the conversation state."},
		},
		PipelineState: types.PipelineState{
			UserContent: "请做完整状态审计，不要重新检索制度。",
			History: []*types.History{
				{Query: "预算改为220万元。", Answer: "预算仍是360万元。"},
				{Query: "项目负责人改为林梅。", Answer: "负责人是王强。"},
			},
		},
	}

	messages := prepareMessagesWithHistory(chatManage)
	if len(messages) != 6 {
		t.Fatalf("messages = %d, want system + two complete prior turns + current user: %#v", len(messages), messages)
	}
	roles := make([]string, 0, len(messages))
	for _, message := range messages {
		roles = append(roles, message.Role)
	}
	if strings.Join(roles, ",") != "system,user,assistant,user,assistant,user" {
		t.Fatalf("state-shaped words changed normal history replay: %#v", roles)
	}
	joined := messages[1].Content + messages[3].Content
	if !strings.Contains(joined, "220万元") || !strings.Contains(joined, "林梅") {
		t.Fatalf("normal history dropped user facts: %#v", messages)
	}
	if !strings.Contains(messages[2].Content, "360万元") || !strings.Contains(messages[4].Content, "王强") {
		t.Fatalf("phrase classifier removed assistant dialogue context: %#v", messages)
	}
}

// --- IntoChatMessage tests ---

func TestIntoChatMessage_NoKBRetrieval(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query: "hello world",
		},
		PipelineState: types.PipelineState{
			Intent:       types.IntentChitchat,
			RewriteQuery: "hello",
		},
	}
	plugin := &PluginIntoChatMessage{messageService: nil}
	nextCalled := false
	err := plugin.OnEvent(context.Background(), types.INTO_CHAT_MESSAGE, cm, func() *PluginError {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nextCalled {
		t.Fatal("next() was not called")
	}
	if cm.UserContent != "hello world" {
		t.Errorf("UserContent: got %q, want %q", cm.UserContent, "hello world")
	}
}

func TestIntoChatMessage_WithMergeResults(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:            "test query",
			KnowledgeBaseIDs: []string{"kb-test"},
			SummaryConfig: types.SummaryConfig{
				ContextTemplate: "Question: {{query}}\n\nReferences:\n{{contexts}}",
			},
		},
		PipelineState: types.PipelineState{
			MergeResult: []*types.SearchResult{
				{ID: "chunk-a", KnowledgeID: "doc-a", KnowledgeBaseID: "kb-a", KnowledgeTitle: "A", Content: "chunk A content", MatchedContent: "generated retrieval question", ChunkType: string(types.ChunkTypeText)},
				{ID: "chunk-b", KnowledgeID: "doc-b", KnowledgeBaseID: "kb-b", KnowledgeTitle: "B", Content: "chunk B content", ChunkType: string(types.ChunkTypeText)},
			},
		},
	}
	plugin := &PluginIntoChatMessage{messageService: nil}
	nextCalled := false
	err := plugin.OnEvent(context.Background(), types.INTO_CHAT_MESSAGE, cm, func() *PluginError {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nextCalled {
		t.Fatal("next() was not called")
	}
	if cm.UserContent == "" {
		t.Fatal("expected UserContent to be populated")
	}
	if !contains(cm.UserContent, "test query") {
		t.Errorf("UserContent should contain query, got: %s", cm.UserContent)
	}
	if !contains(cm.UserContent, "chunk A content") {
		t.Errorf("UserContent should contain chunk A, got: %s", cm.UserContent)
	}
	if contains(cm.UserContent, "generated retrieval question") || len(cm.CitationResult) != 2 || cm.CitationResult[0].EvidenceContent != "chunk A content" {
		t.Errorf("retrieval aids must not become model or presentation evidence: content=%s refs=%#v", cm.UserContent, cm.CitationResult)
	}
	for _, expected := range []string{
		`[AVAILABLE_CITATIONS]`,
		`cite_exactly=<src id="S1" />`,
		`[EVIDENCE id=S1 type=document_fragment`,
		`citation_handle_for_this_evidence: <src id="S1" />`,
		`[CITATION_USE]`,
		`The handle must appear in the final user-visible answer`,
	} {
		if !contains(cm.UserContent, expected) {
			t.Errorf("UserContent should contain %q, got: %s", expected, cm.UserContent)
		}
	}
	if !strings.HasSuffix(cm.UserContent, sourcerefs.TerminalCitationInstruction()) {
		t.Errorf("terminal citation instruction should be the final model-visible block, got: %s", cm.UserContent)
	}
	for _, forbidden := range []string{`<source `, `<context `, `<document`, `source_id=`, `chunk_id=`} {
		if contains(cm.UserContent, forbidden) {
			t.Errorf("UserContent should not prime alternate citation syntax %q, got: %s", forbidden, cm.UserContent)
		}
	}
}

func TestIntoChatMessage_ImageDescriptionAppended(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:                   "what is this?",
			ChatModelSupportsVision: false,
		},
		PipelineState: types.PipelineState{
			Intent:           types.IntentChitchat,
			ImageDescription: "a cat sitting on a mat",
		},
	}
	plugin := &PluginIntoChatMessage{messageService: nil}
	_ = plugin.OnEvent(context.Background(), types.INTO_CHAT_MESSAGE, cm, func() *PluginError {
		return nil
	})
	if !contains(cm.UserContent, "a cat sitting on a mat") {
		t.Errorf("UserContent should contain image description, got: %s", cm.UserContent)
	}
}

// --- PipelineBuilder tests ---

func TestPipelineBuilder_Basic(t *testing.T) {
	pipeline := types.NewPipelineBuilder().
		Add(types.LOAD_HISTORY).
		Add(types.CHAT_COMPLETION_STREAM).
		Build()

	if len(pipeline) != 2 {
		t.Fatalf("expected 2 stages, got %d", len(pipeline))
	}
	if pipeline[0] != types.LOAD_HISTORY {
		t.Errorf("stage 0: got %v, want %v", pipeline[0], types.LOAD_HISTORY)
	}
}

func TestPipelineBuilder_AddIf(t *testing.T) {
	pipeline := types.NewPipelineBuilder().
		Add(types.LOAD_HISTORY).
		AddIf(false, types.QUERY_UNDERSTAND).
		AddIf(true, types.CHAT_COMPLETION_STREAM).
		Build()

	if len(pipeline) != 2 {
		t.Fatalf("expected 2 stages (QUERY_UNDERSTAND skipped), got %d", len(pipeline))
	}
	if pipeline[1] != types.CHAT_COMPLETION_STREAM {
		t.Errorf("stage 1: got %v, want %v", pipeline[1], types.CHAT_COMPLETION_STREAM)
	}
}

func TestPipelineBuilder_Empty(t *testing.T) {
	pipeline := types.NewPipelineBuilder().Build()
	if len(pipeline) != 0 {
		t.Fatalf("expected 0 stages, got %d", len(pipeline))
	}
}

// --- helpers ---

func contains(s, substr string) bool {
	return len(s) >= len(substr) && (s == substr || len(s) > 0 && containsString(s, substr))
}

func containsString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
