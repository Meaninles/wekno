package chunker

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

func TestRepairAbsoluteStructureReplacesInheritedSiblingAndIsIdempotent(t *testing.T) {
	text := "# Manual\n## First section\nFirst material.\n## Second section\n| Field | Value |\n| --- | --- |\n| Owner | Current |\n"
	body := "| Owner | Current |\n"
	start := len([]rune(text)) - len([]rune(body))
	chunks := []Chunk{{Content: body, Start: start, End: len([]rune(text)),
		ContextHeader: "# Manual\n## First section\n# Manual\n## Second section"}}
	once := annotateStructure(text, chunks)
	require.Equal(t, "# Manual\n## Second section\n| Field | Value |", once[0].ContextHeader)
	twice := annotateStructure(text, once)
	require.Equal(t, once, twice)
	require.Equal(t, body, twice[0].Content, "the source text and positions are not rewritten")
}

func TestRepairStructureHeadingStackHandlesSkippedLevels(t *testing.T) {
	for _, text := range []string{
		"第一章 范围\n第一条 甲\n第二条 乙\n", "# Scope\n### A\n### B\n",
	} {
		parts := strings.SplitAfter(text, "\n")
		start := len([]rune(parts[0] + parts[1]))
		out := annotateStructure(text, []Chunk{{Content: parts[2], Start: start, End: start + len([]rune(parts[2]))}})
		var locator map[string]any
		require.NoError(t, json.Unmarshal(out[0].SourceLocator, &locator))
		path := locator["heading_path"].([]any)
		require.Len(t, path, 2)
		require.NotContains(t, out[0].ContextHeader, strings.TrimSpace(parts[1]))
		require.Equal(t, parts[2], out[0].Content)
	}
}
func TestRepairStructurePreservesLiteralTableSlicesAndHeader(t *testing.T) {
	text := "# A\n\n| Key | Value |\n| --- | --- |\n" + strings.Repeat("| item | value |\n", 20)
	out := Split(text, SplitterConfig{ChunkSize: 70, ChunkOverlap: 0, Strategy: "auto"})
	for _, c := range out {
		require.Equal(t, string([]rune(text)[c.Start:c.End]), c.Content)
		if strings.Contains(c.Content, "| item |") {
			require.Contains(t, c.EmbeddingContent(), "| Key | Value |")
			require.NotEmpty(t, c.SourceLocator)
		}
	}
}

func TestNumberedParagraphAncestrySurvivesFragmentBoundary(t *testing.T) {
	text := "第二章 项目职责\n第四条 职责分工\n（一）发起方\n1. 提出需求。\n（二）审核方\n1. 核对材料。\n2. 作出审核决定。\n（三）执行方\n1. 执行决定。\n第五条 记录管理\n1. 保存记录。\n"
	for _, sample := range []struct{ body, owner string }{
		{"2. 作出审核决定。\n", "（二）审核方"},
		{"1. 执行决定。\n", "（三）执行方"},
		{"1. 保存记录。\n", "第五条 记录管理"},
	} {
		start := len([]rune(text[:strings.Index(text, sample.body)]))
		out := annotateStructure(text, []Chunk{{Content: sample.body, Start: start, End: start + len([]rune(sample.body))}})
		require.Equal(t, sample.body, out[0].Content)
		require.Contains(t, out[0].ContextHeader, sample.owner)
		require.NotContains(t, out[0].ContextHeader, "发起方")
		if sample.owner != "（二）审核方" {
			require.NotContains(t, out[0].ContextHeader, "审核方")
		}
	}
}
