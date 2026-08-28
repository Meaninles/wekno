package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestQueryUnderstandStateOnlyTurnSkipsModelRewrite(t *testing.T) {
	plugin := &PluginQueryUnderstand{}
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "预算调整为390万元，只更新台账，不要重新检索。",
			EnableRewrite: true,
		},
	}
	nextCalled := false
	err := plugin.OnEvent(context.Background(), types.QUERY_UNDERSTAND, chatManage, func() *PluginError {
		nextCalled = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !nextCalled {
		t.Fatal("next() was not called")
	}
	if chatManage.Intent != types.IntentFollowUp {
		t.Fatalf("intent = %q, want follow_up", chatManage.Intent)
	}
	if chatManage.RewriteQuery != chatManage.Query {
		t.Fatalf("state-only query was rewritten: %q", chatManage.RewriteQuery)
	}
}

func TestEnforceFreshEvidenceIntentOverridesFollowUp(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query: "仅基于刚才明确的项目事实和制度比较三种方式，每个制度判断就近引用。",
		},
		PipelineState: types.PipelineState{
			RewriteQuery:         "比较三种采购方式",
			Intent:               types.IntentFollowUp,
			SystemPromptOverride: "follow-up prompt",
		},
	}

	if !enforceFreshEvidenceIntent(cm) {
		t.Fatal("expected explicit citation request to override follow-up intent")
	}
	if cm.Intent != types.IntentKBSearch {
		t.Fatalf("intent = %q, want kb_search", cm.Intent)
	}
	if cm.RewriteQuery != "比较三种采购方式" {
		t.Fatalf("useful rewrite was discarded: %q", cm.RewriteQuery)
	}
	if cm.SystemPromptOverride != "" {
		t.Fatalf("stale non-retrieval prompt override remained: %q", cm.SystemPromptOverride)
	}
}

func TestEnforceFreshEvidenceIntentLeavesOrdinaryFollowUpAlone(t *testing.T) {
	cm := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{Query: "那预算呢？"},
		PipelineState: types.PipelineState{
			RewriteQuery: "当前项目预算",
			Intent:       types.IntentFollowUp,
		},
	}

	if enforceFreshEvidenceIntent(cm) {
		t.Fatal("ordinary follow-up should not be forced through retrieval")
	}
	if cm.Intent != types.IntentFollowUp || cm.RewriteQuery != "当前项目预算" {
		t.Fatalf("ordinary follow-up changed: %#v", cm.PipelineState)
	}
}

func TestQueryUnderstandRoutesFreshEvidenceWhenRewriteDisabled(t *testing.T) {
	plugin := &PluginQueryUnderstand{}
	chatManage := &types.ChatManage{
		PipelineRequest: types.PipelineRequest{
			Query:         "依据已选制度比较三个方案，每个制度判断就近引用。",
			EnableRewrite: false,
		},
		PipelineState: types.PipelineState{Intent: types.IntentFollowUp},
	}
	called := false
	err := plugin.OnEvent(context.Background(), types.QUERY_UNDERSTAND, chatManage, func() *PluginError {
		called = true
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected plugin error: %v", err)
	}
	if !called {
		t.Fatal("query-understand chain did not continue")
	}
	if chatManage.Intent != types.IntentKBSearch {
		t.Fatalf("intent = %q, want %q", chatManage.Intent, types.IntentKBSearch)
	}
	if chatManage.RewriteQuery != chatManage.Query {
		t.Fatalf("rewrite_query = %q, want original query", chatManage.RewriteQuery)
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
