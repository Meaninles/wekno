package sourcerefs

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	RetrievalUnitDocuments   = "documents"
	RetrievalUnitDataSources = "data_sources"
)

// RetrievalStatsFromReferences counts the unique claim-bearing sources that
// were actually exposed to the answering model. It deliberately does not use
// final citation occurrence: inspected sources and cited sources are separate
// product concepts.
func RetrievalStatsFromReferences(refs []*types.SearchResult, attempted bool) types.RetrievalStats {
	documents := make(map[string]struct{})
	wikiPages := make(map[string]struct{})
	webPages := make(map[string]struct{})
	dataSources := make(map[string]struct{})

	for _, ref := range refs {
		if ref == nil {
			continue
		}
		switch SourceTypeFromRef(ref) {
		case SourceTypeWiki:
			key := normalizedKey(
				SourceTypeWiki,
				firstNonEmpty(ref.KnowledgeBaseID, ref.Metadata["knowledge_base_id"]),
				firstNonEmpty(ref.Metadata["slug"], ref.ID, ref.KnowledgeTitle),
			)
			if key != "wiki::" {
				wikiPages[key] = struct{}{}
			}
		case SourceTypeWeb:
			key := normalizeURL(firstNonEmpty(ref.Metadata["url"], ref.ID))
			if key == "" {
				key = strings.TrimSpace(ref.KnowledgeTitle)
			}
			if key != "" {
				webPages[key] = struct{}{}
			}
		case SourceTypeData:
			var keys []string
			_ = json.Unmarshal([]byte(ref.Metadata["data_source_keys"]), &keys)
			for _, identity := range keys {
				if identity = strings.TrimSpace(identity); identity != "" {
					dataSources[normalizedKey(SourceTypeData, identity)] = struct{}{}
				}
			}
			if len(keys) == 0 {
				identity := firstNonEmpty(ref.KnowledgeID, ref.Metadata["knowledge_id"], ref.Metadata["data_source_id"])
				if identity != "" {
					dataSources[normalizedKey(SourceTypeData, identity)] = struct{}{}
				}
			}
			declaredCount, _ := strconv.Atoi(strings.TrimSpace(ref.Metadata["data_source_count"]))
			missingCount := declaredCount - len(keys)
			for index := 0; index < missingCount; index++ {
				// Stable placeholders preserve an authoritative count even for a
				// connector that cannot expose source names. Repeated queries do
				// not inflate it.
				dataSources[normalizedKey(SourceTypeData, "declared", strconv.Itoa(index+1))] = struct{}{}
			}
		default:
			// A document is counted once even when several independently
			// citable chunks were inspected.
			identity := firstNonEmpty(
				ref.KnowledgeID,
				ref.Metadata["knowledge_id"],
				ref.KnowledgeFilename,
				ref.KnowledgeTitle,
				ref.ID,
			)
			if identity != "" {
				documents[normalizedKey(SourceTypeKnowledge, ref.KnowledgeBaseID, identity)] = struct{}{}
			}
		}
	}

	stats := types.RetrievalStats{
		Attempted:   attempted || len(documents)+len(wikiPages)+len(webPages)+len(dataSources) > 0,
		Documents:   len(documents),
		Wiki:        len(wikiPages),
		Web:         len(webPages),
		DataSources: len(dataSources),
	}
	if stats.DataSources > 0 && stats.Documents+stats.Wiki+stats.Web == 0 {
		stats.Unit = RetrievalUnitDataSources
	} else {
		stats.Unit = RetrievalUnitDocuments
	}
	stats.Total = stats.Documents + stats.Wiki + stats.Web + stats.DataSources
	return stats
}

// NormalizeRetrievalStats enforces the total invariant at persistence and SSE
// boundaries, so no frontend needs to repair producer output.
func NormalizeRetrievalStats(stats types.RetrievalStats) types.RetrievalStats {
	if stats.Documents < 0 {
		stats.Documents = 0
	}
	if stats.Wiki < 0 {
		stats.Wiki = 0
	}
	if stats.Web < 0 {
		stats.Web = 0
	}
	if stats.DataSources < 0 {
		stats.DataSources = 0
	}
	stats.Total = stats.Documents + stats.Wiki + stats.Web + stats.DataSources
	if stats.Total > 0 {
		stats.Attempted = true
	}
	if stats.Unit == "" && stats.DataSources > 0 && stats.Documents+stats.Wiki+stats.Web == 0 {
		stats.Unit = RetrievalUnitDataSources
	} else if stats.Unit != RetrievalUnitDataSources {
		stats.Unit = RetrievalUnitDocuments
	}
	return stats
}

// RetrievalStatsForAgentSteps preserves a semantic display unit for empty
// results. It is based on tool capabilities rather than agent IDs, so custom
// and future agents automatically reuse the same behavior.
func RetrievalStatsForAgentSteps(stats types.RetrievalStats, steps types.AgentSteps) types.RetrievalStats {
	stats = NormalizeRetrievalStats(stats)
	if stats.Total > 0 {
		return stats
	}
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if IsDataSourceToolName(call.Name) {
				stats.Unit = RetrievalUnitDataSources
				return stats
			}
		}
	}
	return stats
}

// IsDataSourceToolName recognizes structured-data catalog/schema/query tools.
// Suffix matching covers MCP-qualified names without enumerating servers.
func IsDataSourceToolName(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, suffix := range []string{
		"data_analysis", "list_data_sources", "db_catalog", "db_schema", "db_query",
		"table_schema", "table_analysis",
	} {
		if name == suffix || strings.HasSuffix(name, "__"+suffix) || strings.HasSuffix(name, "_"+suffix) {
			return true
		}
	}
	return false
}

// IsRetrievalToolName identifies tools whose successful execution can expose
// claim-bearing document, Wiki, web, or structured-data evidence. Catalog-only
// tools such as wiki_search and get_document_info intentionally do not count.
func IsRetrievalToolName(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "knowledge_search", "search_knowledge", "grep_chunks", "list_knowledge_chunks",
		"wiki_read_page", "wiki_read_source_doc", "web_search", "web_fetch",
		"data_analysis", "table_analysis", "db_query":
		return true
	default:
		return false
	}
}

// AgentStepsAttemptedRetrieval is used by the quick-answer persistence path,
// whose completion callback runs before the generic completion SSE handler.
func AgentStepsAttemptedRetrieval(steps types.AgentSteps) bool {
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if IsRetrievalToolName(call.Name) {
				return true
			}
		}
	}
	return false
}

// AgentToolCallCount reports user-visible ReAct tool usage without counting
// retrieval calls a second time. Retrieval breadth is already represented by
// RetrievalStats, while file preparation, MCP, skills and other tools remain
// visible here. The calculation is O(tool calls) and requires no model work.
func AgentToolCallCount(steps types.AgentSteps) int {
	count := 0
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if strings.EqualFold(strings.TrimSpace(call.Name), "final_answer") || IsRetrievalToolName(call.Name) {
				continue
			}
			count++
		}
	}
	return count
}
