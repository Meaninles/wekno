package tools

import (
	"regexp"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCompileGrepRankingPatternsSplitsOnlyTopLevelAlternation(t *testing.T) {
	query := `公开采购|询比(?:采购|比选)|竞价\|拍卖|竞争谈判|规格[|]型号`
	fallback := regexp.MustCompile("(?i)" + query)
	parts, compiled := compileGrepRankingPatterns(query, fallback)
	if len(parts) != 5 || len(compiled) != 5 {
		t.Fatalf("ranking branches = %v (%d compiled), want 5", parts, len(compiled))
	}
	if parts[1] != `询比(?:采购|比选)` || parts[2] != `竞价\|拍卖` ||
		parts[3] != `竞争谈判` || parts[4] != `规格[|]型号` {
		t.Fatalf("unexpected top-level split: %#v", parts)
	}
}

func TestCompileGrepRankingPatternsObservedComparisonQuery(t *testing.T) {
	query := `公开采购|询比|竞价|竞争谈判|谈判|采购方式`
	fallback := regexp.MustCompile("(?i)" + query)
	parts, compiled := compileGrepRankingPatterns(query, fallback)
	if len(parts) != 6 || len(compiled) != 6 {
		t.Fatalf("ranking branches = %v (%d compiled), want 6", parts, len(compiled))
	}
	if limit := grepResultLimit(len(compiled)); limit != 12 {
		t.Fatalf("multi-topic result limit = %d, want 12", limit)
	}
}

func TestSelectGrepCoverageKeepsEveryNamedTopic(t *testing.T) {
	query := `公开采购|询比|竞价|竞争谈判`
	fallback := regexp.MustCompile("(?i)" + query)
	_, patterns := compileGrepRankingPatterns(query, fallback)
	tool := &GrepChunksTool{}
	results := []chunkWithTitle{
		{Chunk: grepTestChunk("public", 22, "公开采购应满足采购信息可以公开、采购时间允许等条件")},
		{Chunk: grepTestChunk("inquiry", 26, "询比采购要求采购需求确定")},
		{Chunk: grepTestChunk("auction", 27, "竞价采购要求采购需求明确")},
		{Chunk: grepTestChunk("negotiation", 29, "竞争谈判适用于技术复杂项目")},
		{Chunk: grepTestChunk("noise-1", 1, "公开采购背景说明")},
		{Chunk: grepTestChunk("noise-2", 2, "公开采购背景说明二")},
	}
	scored := tool.scoreChunks(nil, results, patterns)
	selected := selectGrepCoverage(scored, scored[:2], patterns, 4)
	if len(selected) != 4 {
		t.Fatalf("selected %d results, want 4", len(selected))
	}
	for index, pattern := range patterns {
		covered := false
		for _, result := range selected {
			if pattern.MatchString(result.Content) {
				covered = true
				break
			}
		}
		if !covered {
			t.Fatalf("coverage selection omitted pattern %d from %#v", index, selected)
		}
	}
}

func TestCalculateMatchScoreDoesNotSaturateSinglePatternHits(t *testing.T) {
	tool := &GrepChunksTool{}
	pattern := regexp.MustCompile(`(?i)公开采购`)
	one, _ := tool.calculateMatchScore("公开采购的背景说明", []*regexp.Regexp{pattern})
	repeated, _ := tool.calculateMatchScore("公开采购应满足条件；选择公开采购方式时逐项核对公开采购条件", []*regexp.Regexp{pattern})
	if one >= 1 || repeated <= one {
		t.Fatalf("single-pattern scores did not retain relevance headroom: one=%f repeated=%f", one, repeated)
	}
}

func grepTestChunk(id string, index int, content string) types.Chunk {
	return types.Chunk{ID: id, ChunkIndex: index, Content: content}
}
