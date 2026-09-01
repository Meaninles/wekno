package types

import "testing"

func TestQueryIntentRetrievalBoundary(t *testing.T) {
	tests := []struct {
		intent QueryIntent
		want   bool
	}{
		{intent: IntentKBSearch, want: true},
		{intent: IntentClarification, want: true},
		{intent: "", want: true},
		{intent: IntentConversation, want: false},
		{intent: IntentSummarize, want: false},
		{intent: IntentFollowUp, want: false},
		{intent: IntentGreeting, want: false},
	}
	for _, test := range tests {
		if got := test.intent.NeedsKBRetrieval(); got != test.want {
			t.Fatalf("intent %q NeedsKBRetrieval() = %v, want %v", test.intent, got, test.want)
		}
	}
}

func TestChatManageNeedsRetrievalRequiresConcreteSource(t *testing.T) {
	tests := []struct {
		name string
		cm   *ChatManage
		want bool
	}{
		{
			name: "kb intent without source skips empty retrieval",
			cm:   &ChatManage{PipelineState: PipelineState{Intent: IntentKBSearch}},
			want: false,
		},
		{
			name: "kb intent with selected source retrieves",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{KnowledgeBaseIDs: []string{"kb-any-domain"}},
				PipelineState:   PipelineState{Intent: IntentKBSearch},
			},
			want: true,
		},
		{
			name: "blank and nil sources do not create retrieval",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{
					KnowledgeBaseIDs: []string{"  "},
					KnowledgeIDs:     []string{""},
					SearchTargets:    SearchTargets{nil, &SearchTarget{}},
				},
				PipelineState: PipelineState{Intent: IntentKBSearch},
			},
			want: false,
		},
		{
			name: "conversation state never retrieves despite source",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{KnowledgeBaseIDs: []string{"kb-any-domain"}},
				PipelineState:   PipelineState{Intent: IntentConversation},
			},
			want: false,
		},
		{
			name: "web intent follows web availability",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{WebSearchEnabled: true},
				PipelineState:   PipelineState{Intent: IntentWebSearch},
			},
			want: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.cm.NeedsRetrieval(); got != test.want {
				t.Fatalf("NeedsRetrieval() = %v, want %v", got, test.want)
			}
		})
	}
}
