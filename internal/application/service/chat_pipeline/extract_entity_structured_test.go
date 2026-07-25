package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/require"
)

func TestGraphFormatterParsesStructuredWrapper(t *testing.T) {
	formatter := NewFormater()
	graph, err := formatter.ParseGraph(context.Background(), `{
		"extractions": [
			{"entity": "采购部门", "entity_attributes": ["负责需求审查"]},
			{"entity": "财务部门", "entity_attributes": ["负责预算复核"]},
			{"entity1": "采购部门", "entity2": "财务部门", "relation": "协同"}
		]
	}`)
	require.NoError(t, err)
	require.Len(t, graph.Node, 2)
	require.Len(t, graph.Relation, 1)
	require.Equal(t, "采购部门", graph.Relation[0].Node1)
	require.Equal(t, "财务部门", graph.Relation[0].Node2)
}

func TestGraphFormatterExampleUsesSameStructuredContract(t *testing.T) {
	formatter := NewFormater()
	formatted, err := formatter.formatExtraction(nil, nil)
	require.NoError(t, err)
	require.Contains(t, formatted, `"extractions"`)
	graph, err := formatter.ParseGraph(context.Background(), formatted)
	require.NoError(t, err)
	require.Empty(t, graph.Node)
	require.Empty(t, graph.Relation)
}

func TestGraphFormatterRecoversOnlyCompleteItemsFromTokenTruncatedTail(t *testing.T) {
	formatter := NewFormater()
	graph, err := formatter.ParseGraph(context.Background(), `{
		"extractions": [
			{"entity": "采购部门", "entity_attributes": ["负责需求审查"]},
			{"entity": "财务部门", "entity_attributes": ["负责预算复核"]},
			{"entity1": "采购部门", "entity2": "财务部门", "relation": "协同"},
			{"entity": "被截断`)
	require.NoError(t, err)
	require.Len(t, graph.Node, 2)
	require.Len(t, graph.Relation, 1)
	require.Equal(t, "采购部门", graph.Relation[0].Node1)
	require.Equal(t, "财务部门", graph.Relation[0].Node2)
}

func TestGraphExtractorBoundsOutputAndKeepsTruncationSafetyMargin(t *testing.T) {
	template := &types.PromptTemplateStructured{Description: "extract graph"}
	extractor := NewExtractor(nil, template)
	require.Equal(t, 8192, extractor.chatOpt.MaxTokens)

	system := NewQAPromptGenerator(NewFormater(), template).System(context.Background())
	require.Contains(t, system, "at most 48 extraction items")
	require.Contains(t, system, "always close every string, array, and object")
	require.Contains(t, system, "at most 4 concise attributes")
}
