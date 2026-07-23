package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/documentsplit"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestNormalizePhysicalPartMarkdownRemovesRepeatedTableHeader(t *testing.T) {
	content := strings.Join([]string{
		"# Sheet1",
		"",
		"| 编号 | 名称 |",
		"| --- | --- |",
		"| 101 | 中间分片首行 |",
	}, "\n")
	normalized, inherited := normalizePhysicalPartMarkdown(content, map[string]any{
		"kind":            "sheet_range",
		"row_start":       float64(100),
		"header_repeated": true,
		"header_context":  "A=编号；B=名称",
	})
	require.NotContains(t, normalized, "| 编号 | 名称 |")
	require.NotContains(t, normalized, "| --- | --- |")
	require.Contains(t, normalized, "中间分片首行")
	require.Equal(t, "列标题：A=编号；B=名称", inherited)
}

func TestNormalizePhysicalPartMarkdownCarriesHeadingBreadcrumb(t *testing.T) {
	normalized, inherited := normalizePhysicalPartMarkdown(
		"# 总则\n## 适用范围\n\n这里是续写正文。",
		map[string]any{
			"kind": "line_range", "line_start": float64(500),
		},
	)
	require.Equal(t, "这里是续写正文。", normalized)
	require.Equal(t, "章节路径：# 总则 / ## 适用范围", inherited)
}

func TestLogicalLocatorHeaderPreservesAllFormatCoordinates(t *testing.T) {
	require.Contains(t, logicalLocatorHeader(map[string]any{
		"kind": "image_tile", "frame_index": float64(2), "frame_count": float64(4),
		"x_start": float64(10), "x_end": float64(20),
		"y_start": float64(30), "y_end": float64(40),
	}), "原图帧：2/4")
	require.Contains(t, logicalLocatorHeader(map[string]any{
		"kind": "json_path_records", "path_start": `$["data"][0]`,
		"path_end": `$["data"][99]`,
	}), `$["data"][99]`)
	require.Contains(t, logicalLocatorHeader(map[string]any{
		"kind": "line_range", "line_start": float64(8), "line_end": float64(8),
		"character_start": float64(1024), "character_end": float64(2048),
	}), "字符：1024–2048")
	require.Contains(t, logicalLocatorHeader(map[string]any{
		"kind": "pages", "page_start": float64(4), "page_end": float64(9),
		"normalized_from": "docx",
	}), "由原始 DOCX 保真渲染")
	mhtmlResource := logicalLocatorHeader(map[string]any{
		"kind": "dom_units", "unit_start": float64(7), "unit_end": float64(7),
		"resource_continuation": true, "resource_index": float64(2),
		"frame_index": float64(1), "frame_count": float64(1),
		"x_start": float64(100), "x_end": float64(900),
		"y_start": float64(200), "y_end": float64(1200),
	})
	require.Contains(t, mhtmlResource, "嵌入资源：3")
	require.Contains(t, mhtmlResource, "x=100–900")
}

func TestTrimExactBoundaryOverlapOnlyForMeaningfulSuffix(t *testing.T) {
	trimmed, removed := trimExactBoundaryOverlap(
		"上一段完整内容，跨片重复语音",
		"跨片重复语音，下一段新内容",
		512,
	)
	require.Equal(t, "，下一段新内容", trimmed)
	require.Equal(t, len([]rune("跨片重复语音")), removed)

	unchanged := "短语，新的内容"
	trimmed, removed = trimExactBoundaryOverlap("上一段短语", unchanged, 512)
	require.Equal(t, unchanged, trimmed)
	require.Zero(t, removed)
}

func TestTemporalLocatorsOverlap(t *testing.T) {
	require.True(t, temporalLocatorsOverlap(
		types.JSON(`{"kind":"time_range","start_seconds":0,"end_seconds":12}`),
		types.JSON(`{"kind":"time_range","start_seconds":10,"end_seconds":20}`),
	))
	require.False(t, temporalLocatorsOverlap(
		types.JSON(`{"kind":"pages","page_start":1,"page_end":2}`),
		types.JSON(`{"kind":"pages","page_start":3,"page_end":4}`),
	))
}

func TestBuildPhysicalPartChunksLinksParentChildTextSequence(t *testing.T) {
	chunks, _, _, err := buildPhysicalPartChunks(
		&types.Knowledge{
			ID: "knowledge", TenantID: 7, KnowledgeBaseID: "kb",
			ProcessingGeneration: "generation",
		},
		&documentsplit.Part{
			PartIndex: 2,
			Locator: types.JSON(
				`{"kind":"line_range","line_start":100,"line_end":300}`,
			),
		},
		types.EffectiveProcessConfig{ChunkingConfig: types.ChunkingConfig{
			EnableParentChild: true,
			ParentChunkSize:   240,
			ChildChunkSize:    80,
			ChunkOverlap:      10,
		}},
		&types.ReadResult{MarkdownContent: strings.Repeat("复杂段落内容，包含实体、字段和关系。", 200)},
		nil,
	)
	require.NoError(t, err)
	var textChunks []*types.Chunk
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeText {
			textChunks = append(textChunks, chunk)
		}
	}
	require.Greater(t, len(textChunks), 2)
	for index, chunk := range textChunks {
		require.Equal(t, "generation", chunk.ProcessingGeneration)
		require.Equal(t, 2, chunk.SplitPartIndex)
		require.NotEmpty(t, chunk.ParentChunkID)
		if index > 0 {
			require.Equal(t, textChunks[index-1].ID, chunk.PreChunkID)
		}
		if index+1 < len(textChunks) {
			require.Equal(t, textChunks[index+1].ID, chunk.NextChunkID)
		}
	}
}

func TestBuildPhysicalPartChunksCoordinatesCoverConfiguredMaximumPart(t *testing.T) {
	const maximumPartIndex = 9_999
	chunks, _, _, err := buildPhysicalPartChunks(
		&types.Knowledge{
			ID: "knowledge", TenantID: 7, KnowledgeBaseID: "kb",
			ProcessingGeneration: "generation",
		},
		&documentsplit.Part{
			PartIndex: maximumPartIndex,
			Locator: types.JSON(
				`{"kind":"line_range","line_start":999900,"line_end":999999}`,
			),
		},
		types.EffectiveProcessConfig{ChunkingConfig: types.ChunkingConfig{
			ChunkSize: 80, ChunkOverlap: 10,
		}},
		&types.ReadResult{MarkdownContent: strings.Repeat("全局坐标必须跨越全部物理分片。", 20)},
		nil,
	)
	require.NoError(t, err)
	require.NotEmpty(t, chunks)
	require.Greater(t, chunks[0].ChunkIndex, 2_147_483_647)
	require.Greater(t, chunks[0].StartAt, 2_147_483_647)
	require.Equal(t, maximumPartIndex, chunks[0].SplitPartIndex)
}

func TestSplitEnrichmentStrataCapsAreFormatAware(t *testing.T) {
	cfg := documentsplit.Config{
		QuestionStrata:      256,
		GraphStrata:         512,
		TableQuestionStrata: 64,
		TableGraphStrata:    128,
	}
	question, graph := splitEnrichmentStrataCaps(
		&documentsplit.Plan{SourceType: "xlsx"}, cfg,
	)
	require.Equal(t, int64(64), question)
	require.Equal(t, int64(128), graph)

	question, graph = splitEnrichmentStrataCaps(
		&documentsplit.Plan{SourceType: ".DOCX"}, cfg,
	)
	require.Equal(t, int64(256), question)
	require.Equal(t, int64(512), graph)
}

func TestDurablePagedEnrichmentRejectsQuestionCountMismatch(t *testing.T) {
	payload := types.KnowledgePostProcessPayload{
		TenantID: 1, KnowledgeID: "knowledge", KnowledgeBaseID: "kb",
		ProcessingGeneration: "generation",
	}
	plan := durableEnrichmentFanout{
		Stage: durableEnrichmentPlanStage, Version: 2,
		TenantID: 1, KnowledgeID: "knowledge", KnowledgeBaseID: "kb",
		ProcessingGeneration: "generation",
		QuestionBatchCount:   1,
	}
	require.Error(t, plan.validate(payload))
}
