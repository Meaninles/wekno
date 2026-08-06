package sourcerefs

import (
	"encoding/json"
	"fmt"
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
	dataSourceIDs := make(map[string]struct{})
	declaredDataSourceCount := 0
	dataSourceToolSeen := stats.Unit == RetrievalUnitDataSources || stats.DataSources > 0
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if !IsDataSourceToolName(call.Name) {
				continue
			}
			dataSourceToolSeen = true
			if call.Result == nil || !call.Result.Success {
				continue
			}
			stats.Attempted = true
			ids, declared := dataSourceIdentities(call.Result.Data)
			for _, id := range ids {
				dataSourceIDs[id] = struct{}{}
			}
			if declared > declaredDataSourceCount {
				declaredDataSourceCount = declared
			}
		}
	}
	if len(dataSourceIDs) > stats.DataSources {
		stats.DataSources = len(dataSourceIDs)
	}
	if declaredDataSourceCount > stats.DataSources {
		stats.DataSources = declaredDataSourceCount
	}
	if dataSourceToolSeen {
		stats.Unit = RetrievalUnitDataSources
	}
	return NormalizeRetrievalStats(stats)
}

// dataSourceIdentities reads compact, authoritative source coordinates from
// structured-data tool results. It intentionally understands only the output
// contract of data tools; a knowledge_id appearing in a document-search result
// must never be counted as a database/table source.
func dataSourceIdentities(data map[string]interface{}) ([]string, int) {
	if data == nil {
		return nil, 0
	}
	identities := make(map[string]struct{})
	add := func(value interface{}) {
		identity := strings.TrimSpace(stringValue(value))
		if identity != "" {
			identities[identity] = struct{}{}
		}
	}
	addList := func(value interface{}) {
		for _, identity := range interfaceStringSlice(value) {
			add(identity)
		}
	}

	addList(data["data_source_ids"])
	addList(data["source_ids"])
	declared := intValue(data["data_source_count"])

	if source, ok := interfaceMap(data["source"]); ok {
		add(source["data_source_id"])
		add(source["source_id"])
		add(source["knowledge_id"])
		addList(source["data_source_ids"])
		addList(source["source_ids"])
		// Some connectors expose only stable display names. They still identify
		// the actually queried sources more accurately than an anonymous count.
		if len(identities) == 0 {
			addList(source["source_names"])
			add(source["source_name"])
		}
		if count := intValue(source["source_count"]); count > declared {
			declared = count
		}
	}

	for _, table := range interfaceMapSlice(data["tables"]) {
		add(table["source_id"])
	}
	for _, source := range interfaceMapSlice(data["sources"]) {
		before := len(identities)
		add(source["source_id"])
		add(source["id"])
		if len(identities) == before {
			add(source["name"])
		}
	}

	out := make([]string, 0, len(identities))
	for identity := range identities {
		out = append(out, identity)
	}
	return out, declared
}

func interfaceMap(value interface{}) (map[string]interface{}, bool) {
	switch typed := value.(type) {
	case map[string]interface{}:
		return typed, true
	case map[string]string:
		out := make(map[string]interface{}, len(typed))
		for key, item := range typed {
			out[key] = item
		}
		return out, true
	default:
		return nil, false
	}
}

func interfaceMapSlice(value interface{}) []map[string]interface{} {
	switch typed := value.(type) {
	case []map[string]interface{}:
		return typed
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(typed))
		for _, item := range typed {
			if mapped, ok := interfaceMap(item); ok {
				out = append(out, mapped)
			}
		}
		return out
	default:
		return nil
	}
}

func interfaceStringSlice(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if text := strings.TrimSpace(stringValue(item)); text != "" {
				out = append(out, text)
			}
		}
		return out
	default:
		return nil
	}
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
	if IsDataSourceToolName(name) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "knowledge_search", "search_knowledge", "grep_chunks", "list_knowledge_chunks",
		"wiki_read_page", "wiki_read_source_doc", "web_search", "web_fetch":
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

// HasConfiguredEvidenceScope reports whether a normal (non-ReAct) answer had
// any user- or agent-configured source it could meaningfully retrieve from.
// The quick-answer pipeline always emits a knowledge_search progress step,
// even for a plain model-only conversation. Treating that transport step as a
// real retrieval would make simple conversations claim "未检索文档" despite
// having no selected knowledge source. Keep the decision beside the shared
// retrieval telemetry rules so Web, mobile, embed and future agent types all
// consume the same persisted truth.
func HasConfiguredEvidenceScope(
	knowledgeBaseIDs, knowledgeIDs []string,
	tagScopeCount int,
	webSearchEnabled bool,
	customAgent *types.CustomAgent,
) bool {
	if webSearchEnabled {
		return true
	}

	// A custom agent configured with no KB capability ignores even stale or
	// forged request targets; mirror the runtime resolver here.
	if customAgent != nil && customAgent.Config.KBSelectionMode == "none" {
		return len(customAgent.Config.DBDataSources) > 0
	}

	if len(knowledgeBaseIDs) > 0 || len(knowledgeIDs) > 0 || tagScopeCount > 0 {
		return true
	}
	if customAgent == nil {
		return false
	}
	if len(customAgent.Config.DBDataSources) > 0 {
		return true
	}
	// Built-in entry agents are default chat surfaces, not an explicit source
	// selection. Quick Answer may search its tenant-wide scope speculatively;
	// when reranking supplies no evidence to the model, the completed turn is a
	// simple model conversation and should show only its duration. Explicit
	// KB/file/tag selections and actual inspected references are handled above
	// and by RetrievalStatsFromReferences respectively.
	if customAgent.IsBuiltin {
		return false
	}
	if customAgent.Config.RetrieveKBOnlyWhenMentioned {
		return false
	}
	switch customAgent.Config.KBSelectionMode {
	case "all":
		return true
	case "selected", "":
		return len(customAgent.Config.KnowledgeBases) > 0
	default:
		return len(customAgent.Config.KnowledgeBases) > 0
	}
}

// AgentToolCallCount reports user-visible ReAct tool usage without counting
// retrieval calls a second time. Retrieval breadth is already represented by
// RetrievalStats, while file preparation, MCP, skills and other tools remain
// visible here. The calculation is O(tool calls) and requires no model work.
func AgentToolCallCount(steps types.AgentSteps) int {
	count := 0
	for _, step := range steps {
		for _, call := range step.ToolCalls {
			if strings.EqualFold(strings.TrimSpace(call.Name), "final_answer") ||
				IsRetrievalToolName(call.Name) || isInternalProgressToolCall(call) {
				continue
			}
			count++
		}
	}
	return count
}

// isInternalProgressToolCall keeps transport/progress bookkeeping out of the
// user-visible tool count. General-agent sidecars use these synthetic calls to
// report preparation progress, but the model did not choose or invoke them as
// answer tools. Prefer the semantic result marker so future agent types can
// reuse the rule without adding another name-specific exception.
func isInternalProgressToolCall(call types.ToolCall) bool {
	name := strings.ToLower(strings.TrimSpace(call.Name))
	if name == "general_agent_progress" || strings.HasPrefix(name, "prepare_original_input_file") {
		return true
	}
	if call.Result == nil || call.Result.Data == nil {
		return false
	}
	if progress, ok := call.Result.Data["agent_progress"].(bool); ok && progress {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(fmt.Sprint(call.Result.Data["display_type"])), "agent_progress")
}
