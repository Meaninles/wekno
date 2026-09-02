package chatpipeline

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/models/rerank"
)

func TestRetainRerankRecallFloorIsDomainIndependent(t *testing.T) {
	input := []rerank.RankResult{
		{Index: 2, RelevanceScore: 0.91},
		{Index: 1, RelevanceScore: 0.22},
		{Index: 0, RelevanceScore: 0.11},
		{Index: 3, RelevanceScore: 0.04},
	}

	got, floorApplied := retainRerankRecallFloor(input, 4, 0.8, 5)
	if !floorApplied {
		t.Fatal("expected the rank-based recall floor to admit below-threshold candidates")
	}
	if len(got) != 3 {
		t.Fatalf("got %d results, want the bounded top-three floor", len(got))
	}
	for index, wantCandidate := range []int{2, 1, 0} {
		if got[index].Index != wantCandidate {
			t.Fatalf("rank %d candidate=%d, want %d", index, got[index].Index, wantCandidate)
		}
	}
}

func TestRetainRerankRecallFloorKeepsThresholdMatchesBeyondFloor(t *testing.T) {
	input := []rerank.RankResult{
		{Index: 0, RelevanceScore: 0.95},
		{Index: 1, RelevanceScore: 0.20},
		{Index: 2, RelevanceScore: 0.10},
		{Index: 3, RelevanceScore: 0.90},
	}

	got, floorApplied := retainRerankRecallFloor(input, 4, 0.8, 5)
	if !floorApplied {
		t.Fatal("expected below-threshold candidates inside the recall floor")
	}
	if len(got) != 4 || got[3].Index != 3 {
		t.Fatalf("threshold-qualified candidate outside floor was lost: %#v", got)
	}
}

func TestRetainRerankRecallFloorHonorsConfiguredContextBudget(t *testing.T) {
	input := []rerank.RankResult{
		{Index: 0, RelevanceScore: 0.40},
		{Index: 1, RelevanceScore: 0.30},
		{Index: 2, RelevanceScore: 0.20},
	}

	got, floorApplied := retainRerankRecallFloor(input, 3, 0.8, 1)
	if !floorApplied || len(got) != 1 || got[0].Index != 0 {
		t.Fatalf("topK=1 must bound the recall floor: applied=%v results=%#v", floorApplied, got)
	}
}

func TestRetainRerankRecallFloorDropsInvalidAndDuplicateIndices(t *testing.T) {
	input := []rerank.RankResult{
		{Index: -1, RelevanceScore: 1},
		{Index: 1, RelevanceScore: 0.7},
		{Index: 1, RelevanceScore: 0.6},
		{Index: 4, RelevanceScore: 1},
		{Index: 0, RelevanceScore: 0.5},
	}

	got, _ := retainRerankRecallFloor(input, 2, 0.9, 5)
	if len(got) != 2 || got[0].Index != 1 || got[1].Index != 0 {
		t.Fatalf("unexpected validated results: %#v", got)
	}
}
