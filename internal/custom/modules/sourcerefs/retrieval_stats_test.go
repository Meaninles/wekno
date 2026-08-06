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

func TestHasConfiguredEvidenceScopeSeparatesPlainChatFromZeroResultRetrieval(t *testing.T) {
	if HasConfiguredEvidenceScope(nil, nil, 0, false, nil) {
		t.Fatal("plain model-only chat must not be presented as a retrieval attempt")
	}
	if !HasConfiguredEvidenceScope([]string{"kb-1"}, nil, 0, false, nil) {
		t.Fatal("explicit knowledge base is a configured evidence scope")
	}
	if !HasConfiguredEvidenceScope(nil, []string{"doc-1"}, 0, false, nil) {
		t.Fatal("explicit document is a configured evidence scope")
	}
	if !HasConfiguredEvidenceScope(nil, nil, 1, false, nil) {
		t.Fatal("explicit tag scope is a configured evidence scope")
	}
	if !HasConfiguredEvidenceScope(nil, nil, 0, true, nil) {
		t.Fatal("enabled web search is a configured evidence scope")
	}
}

func TestHasConfiguredEvidenceScopeMirrorsAgentKBPolicy(t *testing.T) {
	selected := &types.CustomAgent{Config: types.CustomAgentConfig{
		KBSelectionMode: "selected",
		KnowledgeBases:  []string{"kb-1"},
	}}
	if !HasConfiguredEvidenceScope(nil, nil, 0, false, selected) {
		t.Fatal("selected agent knowledge base is a configured evidence scope")
	}

	builtinQuick := &types.CustomAgent{IsBuiltin: true, Config: types.CustomAgentConfig{
		KBSelectionMode: "all",
	}}
	if HasConfiguredEvidenceScope(nil, nil, 0, false, builtinQuick) {
		t.Fatal("implicit built-in scope without inspected evidence is a simple conversation")
	}
	if !HasConfiguredEvidenceScope([]string{"kb-1"}, nil, 0, false, builtinQuick) {
		t.Fatal("explicit KB selection remains visible for a built-in entry agent")
	}

	onMention := &types.CustomAgent{Config: types.CustomAgentConfig{
		KBSelectionMode:             "selected",
		KnowledgeBases:              []string{"kb-1"},
		RetrieveKBOnlyWhenMentioned: true,
	}}
	if HasConfiguredEvidenceScope(nil, nil, 0, false, onMention) {
		t.Fatal("mention-only agent without a mention must remain a simple conversation")
	}
	if !HasConfiguredEvidenceScope([]string{"kb-1"}, nil, 0, false, onMention) {
		t.Fatal("mention-only agent with an explicit KB has an evidence scope")
	}

	none := &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "none"}}
	if HasConfiguredEvidenceScope([]string{"stale-kb"}, nil, 0, false, none) {
		t.Fatal("KBSelectionMode=none ignores request knowledge targets")
	}

	data := &types.CustomAgent{Config: types.CustomAgentConfig{
		KBSelectionMode: "none",
		DBDataSources:   []string{"source-1"},
	}}
	if !HasConfiguredEvidenceScope(nil, nil, 0, false, data) {
		t.Fatal("configured data source is an evidence scope")
	}
}

func TestRetrievalToolClassificationIncludesDataInspectionTools(t *testing.T) {
	for _, name := range []string{
		"knowledge_search", "grep_chunks", "wiki_read_page", "web_fetch",
		"table_schema", "table_analysis", "db_catalog", "db_schema", "db_query",
	} {
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

func TestRetrievalStatsForAgentStepsCountsUniqueDataSources(t *testing.T) {
	steps := types.AgentSteps{
		{ToolCalls: []types.ToolCall{{
			Name: "mcp__weknora__db_catalog",
			Result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"display_type": "db_catalog",
				"sources": []interface{}{
					map[string]interface{}{"id": "db-1", "name": "销售库"},
					map[string]interface{}{"id": "db-2", "name": "库存库"},
				},
			}},
		}}},
		{ToolCalls: []types.ToolCall{{
			Name: "mcp__weknora__db_schema",
			Result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"display_type": "db_schema",
				"tables": []interface{}{
					map[string]interface{}{"source_id": "db-1", "table_name": "orders"},
				},
			}},
		}}},
		{ToolCalls: []types.ToolCall{{
			Name: "table_schema",
			Result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"data_source_ids": []string{"file-1"},
			}},
		}}},
	}

	got := RetrievalStatsForAgentSteps(types.RetrievalStats{}, steps)
	if !got.Attempted || got.Unit != RetrievalUnitDataSources || got.DataSources != 3 || got.Total != 3 {
		t.Fatalf("stats = %+v, want three unique inspected data sources", got)
	}
}

func TestAgentToolCallCountExcludesRetrievalAndFinalAnswer(t *testing.T) {
	steps := types.AgentSteps{
		{ToolCalls: []types.ToolCall{
			{Name: "prepare_original_input_file", Result: &types.ToolResult{Success: true, Data: map[string]interface{}{
				"agent_progress": true,
				"display_type":   "agent_progress",
			}}},
			{Name: "grep_chunks"},
			{Name: "wiki_search"},
			{Name: "web_fetch"},
			{Name: "Bash"},
			{Name: "final_answer"},
		}},
	}
	if got := AgentToolCallCount(steps); got != 2 {
		t.Fatalf("tool count = %d, want wiki catalog + Bash", got)
	}
}

func TestAgentToolCallCountExcludesSemanticProgressFromFutureAgents(t *testing.T) {
	steps := types.AgentSteps{{ToolCalls: []types.ToolCall{
		{Name: "future_agent_transport_step", Result: &types.ToolResult{Success: true, Data: map[string]interface{}{
			"display_type": "agent_progress",
		}}},
		{Name: "mcp__example__real_tool"},
	}}}
	if got := AgentToolCallCount(steps); got != 1 {
		t.Fatalf("tool count = %d, want only the real user-visible tool", got)
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
