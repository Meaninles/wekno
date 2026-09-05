package types

import (
	"reflect"
	"testing"
)

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
			name: "dialogue-only conversation state skips retrieval despite source",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{KnowledgeBaseIDs: []string{"kb-any-domain"}},
				PipelineState: PipelineState{
					Intent:       IntentConversation,
					EvidenceNeed: EvidenceNeedNone,
				},
			},
			want: false,
		},
		{
			name: "mixed conversation state retrieves requested KB evidence",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{KnowledgeBaseIDs: []string{"kb-unseen-domain"}},
				PipelineState: PipelineState{
					Intent:       IntentConversation,
					EvidenceNeed: EvidenceNeedKnowledgeBase,
				},
			},
			want: true,
		},
		{
			name: "mixed conversation state does not manufacture a KB source",
			cm: &ChatManage{PipelineState: PipelineState{
				Intent:       IntentConversation,
				EvidenceNeed: EvidenceNeedKnowledgeBase,
			}},
			want: false,
		},
		{
			name: "legacy explicit KB intent remains authoritative over none signal",
			cm: &ChatManage{
				PipelineRequest: PipelineRequest{KnowledgeBaseIDs: []string{"kb-any-domain"}},
				PipelineState: PipelineState{
					Intent:       IntentKBSearch,
					EvidenceNeed: EvidenceNeedNone,
				},
			},
			want: true,
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

func TestRetrievalQuerySeparatesEvidenceFromMixedTurn(t *testing.T) {
	tests := []struct {
		name string
		cm   *ChatManage
		want string
	}{
		{
			name: "mixed turn uses evidence-only query",
			cm: &ChatManage{PipelineState: PipelineState{
				RewriteQuery:  "summarize the current owner and cite the retention policy",
				EvidenceQuery: "retention policy for the current owner",
			}},
			want: "retention policy for the current owner",
		},
		{
			name: "legacy output falls back to rewrite query",
			cm: &ChatManage{PipelineState: PipelineState{
				RewriteQuery: "legacy knowledge question",
			}},
			want: "legacy knowledge question",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.cm.RetrievalQuery(); got != test.want {
				t.Fatalf("RetrievalQuery() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestRetrievalQueriesUseModelDecompositionWithoutDroppingCompatibility(t *testing.T) {
	cm := &ChatManage{PipelineState: PipelineState{
		RewriteQuery:    "full mixed task",
		EvidenceQuery:   "combined source question",
		EvidenceQueries: []string{"  first independent source question ", "second independent source question", "FIRST INDEPENDENT SOURCE QUESTION", ""},
	}}
	want := []string{"first independent source question", "second independent source question"}
	if got := cm.RetrievalQueries(); !reflect.DeepEqual(got, want) {
		t.Fatalf("RetrievalQueries() = %#v, want %#v", got, want)
	}
	if got := cm.RetrievalQuery(); got != "combined source question" {
		t.Fatalf("RetrievalQuery() = %q, want combined compatibility query", got)
	}

	legacy := &ChatManage{PipelineState: PipelineState{EvidenceQuery: "legacy evidence query"}}
	if got := legacy.RetrievalQueries(); !reflect.DeepEqual(got, []string{"legacy evidence query"}) {
		t.Fatalf("legacy RetrievalQueries() = %#v", got)
	}
}
