package rerank

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

type repairWindowScorer struct {
	calls    int
	maxChars int
	seenTail bool
	invalid  bool
}

func (*repairWindowScorer) GetModelID() string   { return "test" }
func (*repairWindowScorer) GetModelName() string { return "test" }
func (s *repairWindowScorer) Rerank(ctx context.Context, _ string, docs []string) ([]RankResult, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	s.calls++
	if s.invalid {
		return []RankResult{{Index: len(docs)}}, nil
	}
	var out []RankResult
	for i, d := range docs {
		if len([]rune(d)) > s.maxChars {
			return nil, fmt.Errorf("unbounded window")
		}
		score := .1
		if strings.Contains(d, "尾标记") {
			score = .9
			s.seenTail = true
		}
		out = append(out, RankResult{Index: i, RelevanceScore: score})
	}
	return out, nil
}
func TestRepairWindowedRerankerScoresTailAndKeepsOriginalIdentity(t *testing.T) {
	scorer := &repairWindowScorer{maxChars: 63}
	r := &windowedReranker{Reranker: scorer, maxTokens: 128}
	long := strings.Repeat("甲", 5000) + "尾标记"
	out, err := r.Rerank(context.Background(), "问", []string{long, "乙"})
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, 0, out[0].Index)
	require.Equal(t, .9, out[0].RelevanceScore)
	require.Equal(t, long, out[0].Document.Text)
	require.True(t, scorer.seenTail)
	require.Greater(t, scorer.calls, 1)
}
func TestRepairWindowedRerankerRejectsInvalidBudgetIndexAndCancellation(t *testing.T) {
	scorer := &repairWindowScorer{maxChars: 100, invalid: true}
	r := &windowedReranker{Reranker: scorer, maxTokens: 128}
	_, err := r.Rerank(context.Background(), "q", []string{"a"})
	require.ErrorContains(t, err, "invalid window index")
	_, err = r.Rerank(context.Background(), strings.Repeat("问", 100), []string{"a"})
	require.ErrorContains(t, err, "query exceeds")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Rerank(ctx, "q", []string{"a"})
	require.ErrorIs(t, err, context.Canceled)
}
func TestRepairPassageWindowBoundaryHasNoMissingCharacters(t *testing.T) {
	var source strings.Builder
	for i := 0; i < 300; i++ {
		source.WriteRune(rune(0x4e00 + i))
		if i%11 == 0 {
			source.WriteByte('\n')
		}
	}
	text := source.String()
	for _, size := range []int{8, 17, 31, 80} {
		previousStart, previousEnd := 0, 0
		for _, w := range passageWindows(text, size) {
			require.LessOrEqual(t, len(w), size)
			position := strings.Index(text[previousStart:], w)
			require.GreaterOrEqual(t, position, 0)
			start := previousStart + position
			end := start + len(w)
			if start > previousEnd {
				require.Empty(t, strings.TrimSpace(text[previousEnd:start]), "window lost original source characters")
			}
			require.Greater(t, end, previousEnd)
			previousStart = start
			previousEnd = end
		}
		require.Empty(t, strings.TrimSpace(text[previousEnd:]))
	}
}
