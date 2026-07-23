package tools

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

var testSheetLocator = types.JSON(
	`{"kind":"sheet_range","sheet":"数据源表","row_start":180001,"row_end":202500,"physical_part_index":10}`,
)

func TestListKnowledgeChunksOutputDistinguishesLogicalIndexFromSourceCoordinate(t *testing.T) {
	tool := &ListKnowledgeChunksTool{}
	output := tool.buildOutput(
		"knowledge-1",
		"source.xlsx",
		1,
		1,
		0,
		[]*types.Chunk{{
			ID:            "chunk-1",
			ChunkIndex:    30300,
			ChunkType:     types.ChunkTypeText,
			Content:       "A: 202499,B: TEMQ_LGTQK_1633",
			SourceLocator: testSheetLocator,
		}},
	)

	for _, expected := range []string{
		"chunk_index is a logical chunk ordinal only",
		"<source_locator>",
		`&quot;sheet&quot;:&quot;数据源表&quot;`,
		`&quot;row_start&quot;:180001`,
		`&quot;row_end&quot;:202500`,
		"A: 202499",
	} {
		if !strings.Contains(output, expected) {
			t.Fatalf("output missing %q:\n%s", expected, output)
		}
	}
}

func TestGrepChunksOutputCarriesOriginalSourceLocator(t *testing.T) {
	tool := &GrepChunksTool{seenChunks: make(map[string]bool)}
	pattern := regexp.MustCompile("TEMQ_LGTQK_1633")
	output := tool.formatOutput(
		context.Background(),
		[]chunkWithTitle{{
			Chunk: types.Chunk{
				ID:              "chunk-1",
				KnowledgeID:     "knowledge-1",
				KnowledgeBaseID: "kb-1",
				ChunkIndex:      30300,
				ChunkType:       types.ChunkTypeText,
				Content:         "A: 202499,B: TEMQ_LGTQK_1633",
				SourceLocator:   testSheetLocator,
			},
			KnowledgeTitle: "source.xlsx",
			MatchScore:     1,
		}},
		[]string{"TEMQ_LGTQK_1633"},
		[]*regexp.Regexp{pattern},
	)

	if !strings.Contains(output, "chunk_index is a logical chunk ordinal") {
		t.Fatalf("logical coordinate warning missing:\n%s", output)
	}
	if !strings.Contains(output, `&quot;row_start&quot;:180001`) ||
		!strings.Contains(output, `&quot;row_end&quot;:202500`) {
		t.Fatalf("original source locator missing:\n%s", output)
	}
}

func TestWriteModelSourceLocator(t *testing.T) {
	var builder strings.Builder
	writeModelSourceLocator(&builder, testSheetLocator)
	output := builder.String()
	if !strings.Contains(output, "<source_locator>") ||
		!strings.Contains(output, `&quot;row_start&quot;:180001`) {
		t.Fatalf("source locator not rendered: %s", output)
	}
	if strings.Contains(output, "physical_part_index") {
		t.Fatalf("physical split internals leaked to model: %s", output)
	}
}
