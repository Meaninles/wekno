package embedding

import (
	"context"
	"strings"
	"testing"
)

func TestPrepareZhipuEmbeddingInputsSplitsOversizedText(t *testing.T) {
	longText := strings.Repeat("中", maxZhipuEmbeddingInputRunes+3)

	expanded, indexes, err := prepareZhipuEmbeddingInputs(context.Background(), []string{"short", longText})
	if err != nil {
		t.Fatalf("prepareZhipuEmbeddingInputs returned error: %v", err)
	}
	if len(expanded) != 3 {
		t.Fatalf("expanded length = %d, want 3", len(expanded))
	}
	if len([]rune(expanded[1])) != maxZhipuEmbeddingInputRunes {
		t.Fatalf("first long chunk runes = %d, want %d", len([]rune(expanded[1])), maxZhipuEmbeddingInputRunes)
	}
	if len([]rune(expanded[2])) != 3 {
		t.Fatalf("second long chunk runes = %d, want 3", len([]rune(expanded[2])))
	}
	if got := indexes[0]; len(got) != 1 || got[0] != 0 {
		t.Fatalf("indexes[0] = %#v, want [0]", got)
	}
	if got := indexes[1]; len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("indexes[1] = %#v, want [1 2]", got)
	}
}

func TestPrepareZhipuEmbeddingInputsRejectsEmptyText(t *testing.T) {
	_, _, err := prepareZhipuEmbeddingInputs(context.Background(), []string{"   "})
	if err == nil {
		t.Fatalf("expected empty text to be rejected before calling provider")
	}
}

func TestAverageEmbeddingVectors(t *testing.T) {
	got, err := averageEmbeddingVectors([][]float32{{1, 3}, {3, 5}})
	if err != nil {
		t.Fatalf("averageEmbeddingVectors returned error: %v", err)
	}
	if len(got) != 2 || got[0] != 2 || got[1] != 4 {
		t.Fatalf("averageEmbeddingVectors = %#v, want [2 4]", got)
	}
}

func TestAverageEmbeddingVectorsWeighted(t *testing.T) {
	got, err := averageEmbeddingVectorsWeighted([][]float32{{0, 10}, {10, 0}}, []int{3, 1})
	if err != nil {
		t.Fatalf("averageEmbeddingVectorsWeighted returned error: %v", err)
	}
	if len(got) != 2 || got[0] != 2.5 || got[1] != 7.5 {
		t.Fatalf("averageEmbeddingVectorsWeighted = %#v, want [2.5 7.5]", got)
	}
}
