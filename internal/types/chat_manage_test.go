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
