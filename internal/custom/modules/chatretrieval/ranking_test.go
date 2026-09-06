package chatretrieval

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRepairRankKeepsIndependentQueryCoverageAndScores(t *testing.T) {
	input := []*types.SearchResult{
		{ID: "a", Content: "a", Score: .03, MatchedQueries: []string{"q1"}},
		{ID: "b", Content: "b", Score: .02, MatchedQueries: []string{"q1"}},
		{ID: "c", Content: "c", Score: .01, MatchedQueries: []string{"q2"}},
	}
	score := func(_ context.Context, q string, passages []string) (map[int]float64, error) {
		require.Len(t, passages, 3)
		if q == "q1" {
			return map[int]float64{0: .9, 1: .89, 2: .01}, nil
		}
		require.Equal(t, "q2", q)
		return map[int]float64{0: .01, 1: .01, 2: .8}, nil
	}
	out, err := Rank(context.Background(), []string{"q1", "q2"}, input, score, .2, Budget{Fusion: 10, EvidenceCount: 2, EvidenceTokens: 1000})
	require.NoError(t, err)
	require.Len(t, out, 2)
	require.Equal(t, "a", out[0].ID)
	require.Equal(t, "c", out[1].ID)
	require.Equal(t, .03, input[0].Score)
	require.Nil(t, input[0].QueryScores)
	require.Equal(t, .01, out[0].QueryScores["q2"])
}

func TestRepairEvidenceBudgetIncludesMultimodalAndSkipsOversizedFirst(t *testing.T) {
	images, _ := json.Marshal([]types.ImageInfo{{OCRText: strings.Repeat("字", 200)}})
	input := []*types.SearchResult{{ID: "large", Content: "a", ImageInfo: string(images), Score: 1}, {ID: "small", Content: "b", Score: .5}}
	out := Select(input, 2, 150)
	require.Len(t, out, 1)
	require.Equal(t, "small", out[0].ID)
}

func TestRepairRankingRejectsFailuresAndDoesNotInventScores(t *testing.T) {
	input := []*types.SearchResult{{ID: "a", Content: "a", Score: .5}}
	for _, score := range []Scorer{
		func(context.Context, string, []string) (map[int]float64, error) {
			return nil, errors.New("provider failure")
		},
		func(context.Context, string, []string) (map[int]float64, error) { return map[int]float64{4: .9}, nil },
	} {
		_, err := Rank(context.Background(), []string{"q"}, input, score, 0, ResolveBudget(1, 1))
		require.Error(t, err)
	}
	out, err := Rank(context.Background(), []string{"q"}, input, func(context.Context, string, []string) (map[int]float64, error) { return map[int]float64{0: -.6}, nil }, -.7, ResolveBudget(1, 1))
	require.NoError(t, err)
	require.Len(t, out, 1)
	require.Equal(t, -.6, out[0].Score)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Rank(ctx, []string{"q"}, input, func(ctx context.Context, _ string, _ []string) (map[int]float64, error) { return nil, ctx.Err() }, 0, ResolveBudget(1, 1))
	require.ErrorIs(t, errors.Unwrap(err), context.Canceled)
}

func TestRepairPassagePreservesSourceButExcludesGeneratedQueries(t *testing.T) {
	faq, _ := json.Marshal(types.FAQChunkMetadata{StandardQuestion: "Q", Answers: []string{"A"}, SimilarQuestions: []string{"generated"}, NegativeQuestions: []string{"negative"}})
	p := Passage(&types.SearchResult{Content: "| item | value |\n| a | 1 |", MatchedContent: "query expansion", ChunkMetadata: faq})
	require.Contains(t, p, "| a | 1 |")
	require.Contains(t, p, "Question: Q")
	require.Contains(t, p, "Answer: A")
	for _, text := range []string{"generated", "negative", "query expansion"} {
		require.NotContains(t, p, text)
	}
}
