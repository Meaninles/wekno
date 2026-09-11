package service

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestResolveSearchMatchCountByKnowledgeBaseScope(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name          string
		knowledgeBase int
		want          int
	}{
		{name: "single KB", knowledgeBase: 1, want: singleKnowledgeBaseCandidateTopK},
		{name: "two KBs", knowledgeBase: 2, want: multiKnowledgeBaseCandidateTopK},
		{name: "multiple KBs", knowledgeBase: 12, want: multiKnowledgeBaseCandidateTopK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveSearchMatchCount(tc.knowledgeBase)
			if got != tc.want {
				t.Fatalf("resolveSearchMatchCount() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestResolveSearchOutputLimitUsesUnifiedMaximum(t *testing.T) {
	cases := []struct {
		name   string
		params types.SearchParams
		want   int
	}{
		{name: "requested candidate count above maximum", params: types.SearchParams{CandidateCount: 500, MatchCount: 5}, want: types.MaxRetrievalTopK},
		{name: "requested match count above maximum", params: types.SearchParams{MatchCount: 500}, want: types.MaxRetrievalTopK},
		{name: "normal requested match count", params: types.SearchParams{MatchCount: 10}, want: 10},
		{name: "missing requested count", params: types.SearchParams{}, want: types.MaxRetrievalTopK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolveSearchOutputLimit(tc.params); got != tc.want {
				t.Fatalf("resolveSearchOutputLimit() = %d, want %d", got, tc.want)
			}
		})
	}
}
