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

	withoutEvidence := *withEvidence
	withoutEvidence.RenderedContexts = ""
	messages = prepareMessagesWithHistory(&withoutEvidence)
	if !strings.Contains(messages[0].Content, "[WEKNORA_CITATION_OUTPUT]") {
		t.Fatalf("turn precedence and citation contract must also cover no-evidence turns: %s", messages[0].Content)
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
			Query: "test query",
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
