package sourcerefs

import (
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestRetrievalStatsCountsUniqueInspectedSources(t *testing.T) {
	refs := []*types.SearchResult{
		{ID: "c1", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1"},
		{ID: "c2", KnowledgeID: "doc-1", KnowledgeBaseID: "kb-1"},
		{ID: "c3", KnowledgeID: "doc-2", KnowledgeBaseID: "kb-1"},
		{ID: "wiki:kb-w:policy", KnowledgeBaseID: "kb-w", ChunkType: "wiki_page", Metadata: map[string]string{"source_type": "wiki", "slug": "policy"}},
		{ID: "https://Example.com/a#one", ChunkType: "web_search", Metadata: map[string]string{"source_type": "web", "url": "https://Example.com/a#one"}},
		{ID: "https://example.com/a#two", ChunkType: "web_search", Metadata: map[string]string{"source_type": "web", "url": "https://example.com/a#two"}},
		{ID: "query-1", ChunkType: "data_query_result", Metadata: map[string]string{"source_type": "data_source", "data_source_keys": `["销售库","库存库"]`, "data_source_count": "2"}},
		{ID: "query-2", ChunkType: "data_query_result", Metadata: map[string]string{"source_type": "data_source", "data_source_keys": `["销售库"]`, "data_source_count": "1"}},
	}

	got := RetrievalStatsFromReferences(refs, true)
	if got.Documents != 2 || got.Wiki != 1 || got.Web != 1 || got.DataSources != 2 || got.Total != 6 || !got.Attempted {
		t.Fatalf("stats = %+v, want documents=2 wiki=1 web=1 data_sources=2 total=6 attempted", got)
	}
}

func TestRetrievalStatsUsesDeclaredDataSourceCountWithoutInflatingRepeatedQueries(t *testing.T) {
	refs := []*types.SearchResult{
		{ID: "query-1", ChunkType: "data_query_result", Metadata: map[string]string{"source_type": "data_source", "data_source_count": "2"}},
		{ID: "query-2", ChunkType: "data_query_result", Metadata: map[string]string{"source_type": "data_source", "data_source_count": "2"}},
	}
	got := RetrievalStatsFromReferences(refs, true)
	if got.Documents != 0 || got.DataSources != 2 || got.Total != 2 {
		t.Fatalf("stats = %+v, want two stable data sources and no documents", got)
	}
	if got.Unit != RetrievalUnitDataSources {
		t.Fatalf("unit = %q, want data_sources", got.Unit)
	}
}

func TestRetrievalStatsKeepsAttemptedZeroResult(t *testing.T) {
	got := RetrievalStatsFromReferences(nil, true)
	if !got.Attempted || got.Total != 0 {
		t.Fatalf("stats = %+v, want attempted zero-result retrieval", got)
	}
}

func TestRetrievalToolClassificationExcludesCatalogOnlyTools(t *testing.T) {
	for _, name := range []string{"knowledge_search", "grep_chunks", "wiki_read_page", "web_fetch", "table_analysis", "db_query"} {
		if !IsRetrievalToolName(name) {
			t.Fatalf("%s should be a retrieval tool", name)
		}
	}
	for _, name := range []string{"wiki_search", "get_document_info", "query_knowledge_graph", "thinking"} {
		if IsRetrievalToolName(name) {
			t.Fatalf("%s should not count as inspected evidence", name)
		}
	}
}

func TestAgentToolCallCountExcludesRetrievalAndFinalAnswer(t *testing.T) {
	steps := types.AgentSteps{
		{ToolCalls: []types.ToolCall{
			{Name: "prepare_original_input_file"},
			{Name: "grep_chunks"},
			{Name: "wiki_search"},
			{Name: "web_fetch"},
			{Name: "Bash"},
			{Name: "final_answer"},
		}},
	}
	if got := AgentToolCallCount(steps); got != 3 {
		t.Fatalf("tool count = %d, want preparation + wiki catalog + Bash", got)
	}
}

func TestRetrievalStatsForAgentStepsKeepsDataSourceUnitAtZero(t *testing.T) {
	steps := types.AgentSteps{{ToolCalls: []types.ToolCall{{Name: "mcp__weknora__db_catalog"}}}}
	got := RetrievalStatsForAgentSteps(types.RetrievalStats{}, steps)
	if got.Total != 0 || got.Unit != RetrievalUnitDataSources {
		t.Fatalf("stats = %+v, want zero data_sources unit", got)
	}
}

func TestRetrievalStatsForAgentStepsDefaultsToDocumentUnitAtZero(t *testing.T) {
	got := RetrievalStatsForAgentSteps(types.RetrievalStats{}, nil)
	if got.Total != 0 || got.Unit != RetrievalUnitDocuments {
		t.Fatalf("stats = %+v, want zero documents unit", got)
	}
}
