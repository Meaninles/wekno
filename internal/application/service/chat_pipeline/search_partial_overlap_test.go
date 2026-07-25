package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestRemovePartialOverlapsDoesNotLetSummaryEvictRawEvidence(t *testing.T) {
	raw := &types.SearchResult{
		ID:        "raw",
		ChunkType: types.ChunkTypeText,
		Score:     0.7,
		Content: "涉密计算机禁止上网，禁止使用来源不明的U盘。" +
			"P2P下载长时间（30分钟以上）占用带宽属于违规。",
	}
	summary := &types.SearchResult{
		ID:        "summary",
		ChunkType: types.ChunkTypeSummary,
		Score:     0.99,
		Content: "涉密计算机禁止上网，禁止使用来源不明的U盘。" +
			"P2P下载长时间占用带宽属于违规。",
	}

	got := removePartialOverlaps(
		context.Background(),
		[]*types.SearchResult{summary, raw},
	)
	if len(got) != 2 {
		t.Fatalf("summary/raw result count = %d, want 2", len(got))
	}
}

func TestRemovePartialOverlapsStillDropsRedundantRawChunk(t *testing.T) {
	high := &types.SearchResult{
		ID:        "high",
		ChunkType: types.ChunkTypeText,
		Score:     0.9,
		Content:   "禁止使用来源不明的U盘，使用前必须完成安全检测。",
	}
	low := &types.SearchResult{
		ID:        "low",
		ChunkType: types.ChunkTypeText,
		Score:     0.5,
		Content:   "禁止使用来源不明的U盘。",
	}

	got := removePartialOverlaps(
		context.Background(),
		[]*types.SearchResult{high, low},
	)
	if len(got) != 1 || got[0].ID != "high" {
		t.Fatalf("raw overlap result = %#v, want only high", got)
	}
}
