package tools

import (
	"regexp"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCompileGrepRankingPatternsSplitsOnlyTopLevelAlternation(t *testing.T) {
	query := `onboarding|incident(?:response|review)|gamma\|delta|calibration|model[|]variant`
	fallback := regexp.MustCompile("(?i)" + query)
	parts, compiled := compileGrepRankingPatterns(query, fallback)
	if len(parts) != 5 || len(compiled) != 5 {
		t.Fatalf("ranking branches = %v (%d compiled), want 5", parts, len(compiled))
	}
	if parts[1] != `incident(?:response|review)` || parts[2] != `gamma\|delta` ||
		parts[3] != `calibration` || parts[4] != `model[|]variant` {
		t.Fatalf("unexpected top-level split: %#v", parts)
	}
}

func TestCompileGrepRankingPatternsCrossDomainComparisonQuery(t *testing.T) {
	query := `onboarding|incident|calibration|retention|handoff|escalation`
	fallback := regexp.MustCompile("(?i)" + query)
	parts, compiled := compileGrepRankingPatterns(query, fallback)
	if len(parts) != 6 || len(compiled) != 6 {
		t.Fatalf("ranking branches = %v (%d compiled), want 6", parts, len(compiled))
	}
	if limit := grepResultLimit(len(compiled)); limit != 12 {
		t.Fatalf("multi-topic result limit = %d, want 12", limit)
	}
}

func TestGrepResultLimitScalesWithQueryBreadth(t *testing.T) {
	tests := []struct {
		patterns int
		want     int
	}{
		{patterns: 1, want: 30},
		{patterns: 2, want: 18},
		{patterns: 3, want: 18},
		{patterns: 4, want: 12},
		{patterns: 8, want: 12},
	}
	for _, test := range tests {
		if got := grepResultLimit(test.patterns); got != test.want {
			t.Fatalf("grepResultLimit(%d) = %d, want %d", test.patterns, got, test.want)
		}
	}
}

func TestSelectGrepCoverageKeepsEveryNamedTopic(t *testing.T) {
	query := `onboarding|incident|calibration|retention`
	fallback := regexp.MustCompile("(?i)" + query)
	_, patterns := compileGrepRankingPatterns(query, fallback)
	tool := &GrepChunksTool{}
	results := []chunkWithTitle{
		{Chunk: grepTestChunk("hr", 22, "The onboarding checklist names the responsible coordinator.")},
		{Chunk: grepTestChunk("ops", 26, "Incident response requires an owner and severity review.")},
		{Chunk: grepTestChunk("manual", 27, "Calibration starts after the status light becomes steady.")},
		{Chunk: grepTestChunk("policy", 29, "Retention periods differ by record category.")},
		{Chunk: grepTestChunk("noise-1", 1, "Onboarding background and glossary.")},
		{Chunk: grepTestChunk("noise-2", 2, "A second onboarding overview.")},
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
	pattern := regexp.MustCompile(`(?i)calibration`)
	one, _ := tool.calculateMatchScore("Calibration overview", []*regexp.Regexp{pattern})
	repeated, _ := tool.calculateMatchScore("Calibration begins after startup; repeat calibration only after the device cools.", []*regexp.Regexp{pattern})
	if one >= 1 || repeated <= one {
		t.Fatalf("single-pattern scores did not retain relevance headroom: one=%f repeated=%f", one, repeated)
	}
}

func grepTestChunk(id string, index int, content string) types.Chunk {
	return types.Chunk{ID: id, ChunkIndex: index, Content: content}
}
