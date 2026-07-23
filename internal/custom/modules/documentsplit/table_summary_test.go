package documentsplit

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestBuildTableSummaryCorpusCoversLogicalExtents(t *testing.T) {
	plan := &Plan{
		SourceName: "资产目录.xlsx", SourceType: "xlsx",
		SourceSize: 16_000_000, PartCount: 23,
	}
	knowledge := &types.Knowledge{
		FileName: "资产目录.xlsx", Title: "资产目录",
	}
	samples := []*types.Chunk{
		{
			ChunkIndex: 0, Content: "编号 | 部门 | 资产名称\n1 | 财务部 | 总账",
			SourceLocator: types.JSON(`{"kind":"sheet_range","sheet":"总表","row_start":1,"row_end":1000,"column_start":1,"column_end":3,"header_context":"A=编号；B=部门；C=资产名称"}`),
		},
		{
			ChunkIndex: 9_000_000, Content: "90001 | 运营部 | 统一用户中心",
			SourceLocator: types.JSON(`{"kind":"sheet_range","sheet":"总表","row_start":90001,"row_end":100000,"column_start":1,"column_end":3,"header_context":"A=编号；B=部门；C=资产名称"}`),
		},
		{
			ChunkIndex: 22_000_000, Content: "权限编码 | 权限名称\nP-999 | 归档查询",
			SourceLocator: types.JSON(`{"kind":"sheet_range","sheet":"权限字典","row_start":4001,"row_end":5000,"column_start":1,"column_end":2,"header_context":"A=权限编码；B=权限名称"}`),
		},
	}

	corpus, err := BuildTableSummaryCorpus(plan, knowledge, samples, 1_600, 8_000)
	require.NoError(t, err)
	require.Equal(t, "资产目录.xlsx", corpus.TableName)
	require.Contains(t, corpus.SchemaDescription, `Sheet "总表"`)
	require.Contains(t, corpus.SchemaDescription, `Sheet "权限字典"`)
	require.Contains(t, corpus.SchemaDescription, "Logical text chunks: 1600")
	require.Contains(t, corpus.SchemaDescription, "A=编号")
	require.Contains(t, corpus.SampleDescription, "统一用户中心")
	require.Contains(t, corpus.SampleDescription, "归档查询")
	require.LessOrEqual(t, len([]rune(corpus.SampleDescription)), 8_000)
}

func TestBuildTableSummaryCorpusBoundsLongSamples(t *testing.T) {
	plan := &Plan{
		SourceName: "large.csv", SourceType: "csv",
		SourceSize: 100_000_000, PartCount: 40,
	}
	knowledge := &types.Knowledge{FileName: "large.csv"}
	samples := make([]*types.Chunk, 0, 64)
	for index := 0; index < 64; index++ {
		samples = append(samples, &types.Chunk{
			ChunkIndex: index * 1_000_000,
			Content:    strings.Repeat("字段值", 2_000),
			SourceLocator: types.JSON(
				`{"kind":"record_range","row_start":1,"row_end":100,"column_start":1,"column_end":20}`,
			),
		})
	}
	corpus, err := BuildTableSummaryCorpus(plan, knowledge, samples, 10_000, 12_000)
	require.NoError(t, err)
	require.LessOrEqual(t, len([]rune(corpus.SampleDescription)), 12_000)
	require.Contains(t, corpus.SampleDescription, "[…]")
}
