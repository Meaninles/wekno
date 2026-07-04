package tools

import (
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	clientResultLimit       = 20
	clientRowLimit          = 50
	clientColumnLimit       = 50
	clientListLimit         = 20
	clientTextPreviewLimit  = 800
	clientCellPreviewLimit  = 300
	clientSummaryTextLimit  = 1200
	clientThoughtTextLimit  = 2000
	clientFilenameTextLimit = 240
)

// persistStripFields lists bulky Data keys to drop before SSE replay / DB storage.
var persistStripFields = map[string][]string{
	"knowledge_chunks_list": {"chunks"},
	"grep_results":          {"chunk_results"},
	"db_schema":             {"semantic_context"},
}

// ShouldOmitRawToolOutput reports whether the raw XML/text Output should be
// excluded from SSE replay and persisted agent_steps. The full Output remains
// available in-memory for the current agent turn.
func ShouldOmitRawToolOutput(_ string, data map[string]interface{}) bool {
	if data == nil {
		return false
	}
	displayType, ok := data["display_type"].(string)
	return ok && displayType != ""
}

// SanitizeToolDataForPersist returns a copy of tool Data safe for DB / SSE replay.
func SanitizeToolDataForPersist(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	out := make(map[string]interface{}, len(data))
	for k, v := range data {
		out[k] = v
	}
	displayType := stringField(data, "display_type")
	for _, key := range persistStripFields[displayType] {
		delete(out, key)
	}
	return out
}

// SanitizeToolDataForClient returns the small, display-only subset that may be
// sent to browsers through SSE or history APIs. It intentionally keeps only
// fields currently needed by frontend renderers and caps user/content payloads.
func SanitizeToolDataForClient(data map[string]interface{}) map[string]interface{} {
	if data == nil {
		return nil
	}
	displayType := stringField(data, "display_type")
	switch displayType {
	case "web_search_results":
		return sanitizeWebSearchData(data)
	case "web_fetch_results":
		return sanitizeWebFetchData(data)
	case "search_results":
		return sanitizeSearchResultsData(data)
	case "graph_query_results":
		return sanitizeGraphResultsData(data)
	case "grep_results":
		return sanitizeGrepResultsData(data)
	case "knowledge_chunks_list":
		return sanitizeKnowledgeChunksListData(data)
	case "database_query":
		return sanitizeDatabaseQueryData(data)
	case "structured_analysis_result":
		return sanitizeStructuredAnalysisData(data)
	case "db_catalog", "db_schema":
		return sanitizeDatabaseMetadataData(data)
	case "document_info":
		return sanitizeDocumentInfoData(data)
	case "general_agent_artifacts":
		return sanitizeGeneralArtifactsData(data)
	case "plan":
		return sanitizePlanData(data)
	case "thinking":
		return sanitizeThinkingData(data)
	case "chunk_detail":
		return sanitizeChunkDetailData(data)
	case "related_chunks":
		return sanitizeRelatedChunksData(data)
	case "knowledge_base_list":
		return sanitizeKnowledgeBaseListData(data)
	case "wiki_write_page", "wiki_replace_text", "wiki_rename_page", "wiki_delete_page":
		return sanitizeWikiEditData(data)
	default:
		return sanitizeGenericToolData(data)
	}
}

// SanitizeToolResultForClient builds stream / persistence metadata for the UI.
func SanitizeToolResultForClient(_ string, result *types.ToolResult) map[string]interface{} {
	meta := map[string]interface{}{}
	if result == nil {
		return meta
	}
	if result.Data != nil {
		for k, v := range SanitizeToolDataForClient(result.Data) {
			meta[k] = v
		}
	}
	return meta
}

// StreamContentForToolResult is the short SSE Content field for tool results.
func StreamContentForToolResult(toolName string, success bool, errMsg string, data map[string]interface{}) string {
	if !success {
		return errMsg
	}
	return compactToolSummary(success, errMsg, data)
}

// SanitizeAgentStepsForClient strips historical agent_steps down to the same
// browser-facing contract used by the live SSE stream.
func SanitizeAgentStepsForClient(steps []types.AgentStep) []types.AgentStep {
	if len(steps) == 0 {
		return steps
	}
	out := make([]types.AgentStep, len(steps))
	for i, step := range steps {
		out[i] = step
		if len(step.ToolCalls) == 0 {
			continue
		}
		toolCalls := make([]types.ToolCall, len(step.ToolCalls))
		for j, tc := range step.ToolCalls {
			toolCalls[j] = tc
			toolCalls[j].Args = nil
			toolCalls[j].ProviderMetadata = nil
			toolCalls[j].Reflection = ""
			if tc.Result == nil {
				continue
			}
			result := *tc.Result
			result.Output = compactToolSummary(result.Success, result.Error, result.Data)
			result.Data = SanitizeToolDataForClient(result.Data)
			result.Images = nil
			toolCalls[j].Result = &result
		}
		out[i].ToolCalls = toolCalls
	}
	return out
}

// SanitizeMessageForClient returns a shallow message copy with agent steps
// minimized for browser display.
func SanitizeMessageForClient(message *types.Message) *types.Message {
	if message == nil {
		return nil
	}
	out := *message
	if len(message.AgentSteps) > 0 {
		out.AgentSteps = types.AgentSteps(SanitizeAgentStepsForClient(message.AgentSteps))
	}
	return &out
}

// SanitizeMessagesForClient returns browser-safe message copies.
func SanitizeMessagesForClient(messages []*types.Message) []*types.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]*types.Message, len(messages))
	for i, message := range messages {
		out[i] = SanitizeMessageForClient(message)
	}
	return out
}

// SanitizeAgentStepsForStorage strips LLM-only payloads from persisted steps.
func SanitizeAgentStepsForStorage(steps []types.AgentStep) []types.AgentStep {
	if len(steps) == 0 {
		return steps
	}
	out := make([]types.AgentStep, len(steps))
	for i, step := range steps {
		out[i] = step
		if len(step.ToolCalls) == 0 {
			continue
		}
		toolCalls := make([]types.ToolCall, len(step.ToolCalls))
		for j, tc := range step.ToolCalls {
			toolCalls[j] = tc
			if tc.Result == nil {
				continue
			}
			result := *tc.Result
			if ShouldOmitRawToolOutput(tc.Name, result.Data) {
				result.Output = compactToolSummary(result.Success, result.Error, result.Data)
				result.Data = SanitizeToolDataForPersist(result.Data)
			}
			toolCalls[j].Result = &result
		}
		out[i].ToolCalls = toolCalls
	}
	return out
}

// CompactToolOutputForHistory rebuilds a short tool message when replaying history.
func CompactToolOutputForHistory(toolName string, result *types.ToolResult) string {
	if result == nil {
		return ""
	}
	if !result.Success {
		if result.Error != "" {
			return "Error: " + result.Error
		}
		return "Error: tool call failed"
	}
	if result.Output != "" && !ShouldOmitRawToolOutput(toolName, result.Data) {
		return result.Output
	}
	return compactToolSummary(result.Success, result.Error, result.Data)
}

func compactToolSummary(success bool, errMsg string, data map[string]interface{}) string {
	if !success {
		if errMsg != "" {
			return "Error: " + errMsg
		}
		return "Error: tool call failed"
	}
	switch stringField(data, "display_type") {
	case "knowledge_chunks_list":
		title := stringField(data, "knowledge_title")
		if title == "" {
			title = stringField(data, "knowledge_id")
		}
		fetched := intField(data, "fetched_chunks")
		total := intField(data, "total_chunks")
		if q := stringField(data, "faq_question"); q != "" {
			return fmt.Sprintf("Loaded FAQ entry: %s (content omitted from history)", q)
		}
		if title != "" && total > 0 {
			return fmt.Sprintf("Listed %d/%d chunks from %s (content omitted from history)", fetched, total, title)
		}
		if title != "" {
			return fmt.Sprintf("Listed chunks from %s (content omitted from history)", title)
		}
	case "grep_results":
		chunks := intField(data, "total_matches")
		docs := intField(data, "document_count")
		if docs == 0 {
			docs = intField(data, "result_count")
		}
		if chunks > 0 {
			return fmt.Sprintf("Keyword search found %d matching chunks across %d document(s) (details omitted from history)", chunks, docs)
		}
	case "search_results":
		count := intField(data, "result_count")
		if count == 0 {
			count = intField(data, "count")
		}
		if count > 0 {
			return fmt.Sprintf("Semantic search returned %d result(s) (details omitted from history)", count)
		}
	case "db_catalog":
		count := intField(data, "count")
		return fmt.Sprintf("Database catalog matched %d table(s)", count)
	case "db_schema":
		count := intField(data, "count")
		return fmt.Sprintf("Loaded database schema for %d table(s)", count)
	case "structured_analysis_result":
		rows := intField(data, "row_count")
		return fmt.Sprintf("Structured data analysis returned %d row(s)", rows)
	}
	if displayType := stringField(data, "display_type"); displayType != "" {
		return fmt.Sprintf("Tool completed (%s; payload omitted from history)", displayType)
	}
	return "Tool completed (payload omitted from history)"
}

func stringField(data map[string]interface{}, key string) string {
	if data == nil {
		return ""
	}
	v, ok := data[key]
	if !ok || v == nil {
		return ""
	}
	return strings.TrimSpace(fmt.Sprint(v))
}

func intField(data map[string]interface{}, key string) int {
	if data == nil {
		return 0
	}
	v, ok := data[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case int:
		return n
	case int32:
		return int(n)
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}

func sanitizeWebSearchData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "query", "count")
	out["results"] = sanitizeMapList(data["results"], clientResultLimit, []string{
		"result_index", "title", "url", "source", "published_at",
	}, map[string]int{
		"title": 240,
		"url":   500,
	})
	return out
}

func sanitizeWebFetchData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "count")
	out["results"] = sanitizeMapList(data["results"], clientResultLimit, []string{
		"url", "prompt", "summary", "content_length", "method", "error",
	}, map[string]int{
		"url":     500,
		"prompt":  500,
		"summary": clientSummaryTextLimit,
		"error":   clientSummaryTextLimit,
	})
	return out
}

func sanitizeSearchResultsData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "query", "count", "result_count", "knowledge_base_id")
	out["results"] = sanitizeMapList(data["results"], clientResultLimit, []string{
		"result_index", "chunk_id", "chunk_index", "index", "content",
		"knowledge_id", "knowledge_title", "knowledge_base_type", "faq_id",
		"faq_standard_question",
	}, map[string]int{
		"content":               clientTextPreviewLimit,
		"knowledge_title":       240,
		"faq_standard_question": 300,
	})
	return out
}

func sanitizeGraphResultsData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "query", "count", "has_graph_config")
	if cfg := sanitizeGraphConfig(data["graph_config"]); len(cfg) > 0 {
		out["graph_config"] = cfg
	}
	out["results"] = sanitizeMapList(data["results"], clientResultLimit, []string{
		"result_index", "chunk_id", "content", "score", "relevance_level",
		"knowledge_id", "knowledge_title", "match_type",
	}, map[string]int{
		"content":         clientTextPreviewLimit,
		"knowledge_title": 240,
	})
	return out
}

func sanitizeGrepResultsData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data,
		"display_type", "query", "patterns", "result_count", "document_count",
		"total_matches", "limit", "max_results",
	)
	out["knowledge_results"] = sanitizeMapList(data["knowledge_results"], clientResultLimit, []string{
		"knowledge_id", "knowledge_base_id", "knowledge_title", "faq_question",
		"title_match", "chunk_hit_count", "match_snippet", "total_pattern_hits",
		"distinct_patterns",
	}, map[string]int{
		"knowledge_title": 240,
		"faq_question":    300,
		"match_snippet":   clientTextPreviewLimit,
	})
	return out
}

func sanitizeKnowledgeChunksListData(data map[string]interface{}) map[string]interface{} {
	return copyFields(data,
		"display_type", "knowledge_id", "knowledge_title", "total_chunks",
		"fetched_chunks", "page", "page_size", "faq_question", "faq_id",
		"single_chunk",
	)
}

func sanitizeDatabaseQueryData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "row_count")
	out["columns"] = sanitizeStringList(data["columns"], clientColumnLimit, 120)
	out["rows"] = sanitizeRows(data["rows"], clientRowLimit)
	if intField(data, "row_count") > clientRowLimit {
		out["client_truncated"] = true
		out["client_row_limit"] = clientRowLimit
	}
	return out
}

func sanitizeStructuredAnalysisData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data,
		"display_type", "analysis_type", "query", "row_count",
		"chart_requested", "limits",
	)
	out["columns"] = sanitizeColumns(data["columns"], clientColumnLimit)
	out["rows"] = sanitizeRows(data["rows"], clientRowLimit)
	if chart := sanitizeChartSpec(data["chart"]); len(chart) > 0 {
		out["chart"] = chart
	}
	if intField(data, "row_count") > clientRowLimit {
		out["client_truncated"] = true
		out["client_row_limit"] = clientRowLimit
	}
	return out
}

func sanitizeDatabaseMetadataData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "query", "count")
	tables := sanitizeMapList(data["tables"], clientResultLimit, []string{
		"source_id", "source_name", "source_type", "schema_name", "table_name",
		"sql_table_name", "object_type", "description", "row_estimate", "columns",
	}, map[string]int{
		"source_name":    160,
		"schema_name":    160,
		"table_name":     160,
		"sql_table_name": 240,
		"description":    500,
	})
	for _, table := range tables {
		table["columns"] = sanitizeMapList(table["columns"], clientColumnLimit, []string{
			"name", "type", "description", "semantic_type",
		}, map[string]int{
			"name":        160,
			"type":        120,
			"description": 300,
		})
	}
	out["tables"] = tables
	return out
}

func sanitizeDocumentInfoData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "total_docs", "requested", "title")
	out["documents"] = sanitizeMapList(data["documents"], clientResultLimit, []string{
		"knowledge_id", "faq_id", "chunk_id", "title", "faq_question",
		"faq_answers", "faq_similar_questions", "is_faq", "description", "type",
		"source", "channel", "file_name", "file_type", "file_size",
		"parse_status", "chunk_count", "type_icon",
	}, map[string]int{
		"title":        240,
		"faq_question": 300,
		"description":  500,
		"source":       500,
		"file_name":    clientFilenameTextLimit,
	})
	if errors := sanitizeStringList(data["errors"], clientListLimit, 500); len(errors) > 0 {
		out["errors"] = errors
	}
	return out
}

func sanitizeGeneralArtifactsData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data,
		"display_type", "summary", "notice", "artifact_original_count",
		"artifact_returned_count", "artifact_dropped_count",
		"artifact_returned_size", "artifact_limit_bytes",
	)
	out["artifacts"] = sanitizeMapList(data["artifacts"], clientListLimit, []string{
		"artifact_id", "filename", "file_type", "file_size", "sha256", "download_url",
	}, map[string]int{
		"filename":     clientFilenameTextLimit,
		"download_url": 1000,
	})
	return out
}

func sanitizePlanData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "task", "total_steps", "plan_created")
	out["steps"] = sanitizeMapList(data["steps"], 100, []string{
		"id", "description", "tools_to_use", "status",
	}, map[string]int{
		"description": 500,
	})
	return out
}

func sanitizeThinkingData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data,
		"display_type", "thought_number", "total_thoughts",
		"next_thought_needed", "thought_history_length", "incomplete_steps",
	)
	if thought := stringField(data, "thought"); thought != "" {
		out["thought"] = truncateText(thought, clientThoughtTextLimit)
	}
	return out
}

func sanitizeChunkDetailData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "chunk_id", "chunk_index", "knowledge_id", "content_length")
	if content := stringField(data, "content"); content != "" {
		out["content"] = truncateText(content, clientTextPreviewLimit)
	}
	return out
}

func sanitizeRelatedChunksData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "chunk_id", "relation_type", "count")
	out["chunks"] = sanitizeMapList(data["chunks"], clientResultLimit, []string{
		"index", "chunk_id", "chunk_index", "content", "knowledge_id",
	}, map[string]int{
		"content": clientTextPreviewLimit,
	})
	return out
}

func sanitizeKnowledgeBaseListData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "count")
	out["knowledge_bases"] = sanitizeMapList(data["knowledge_bases"], clientResultLimit, []string{
		"index", "id", "name", "description",
	}, map[string]int{
		"name":        240,
		"description": 500,
	})
	return out
}

func sanitizeWikiEditData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data, "display_type", "action", "slug", "title", "page_type", "summary", "old_slug", "new_slug", "updated_count")
	if oldText := stringField(data, "old_text"); oldText != "" {
		out["old_text"] = truncateText(oldText, 500)
	}
	if newText := stringField(data, "new_text"); newText != "" {
		out["new_text"] = truncateText(newText, 500)
	}
	if affected := sanitizeStringList(data["affected_pages"], clientListLimit, 240); len(affected) > 0 {
		out["affected_pages"] = affected
	}
	return out
}

func sanitizeGenericToolData(data map[string]interface{}) map[string]interface{} {
	out := copyFields(data,
		"display_type", "tool_name", "success", "count", "result_count",
		"row_count", "file_count", "total_matches", "document_count",
		"mode", "phase", "message", "title", "query",
	)
	return out
}

func copyFields(data map[string]interface{}, keys ...string) map[string]interface{} {
	out := map[string]interface{}{}
	for _, key := range keys {
		v, ok := data[key]
		if !ok || v == nil {
			continue
		}
		out[key] = sanitizeClientScalar(v, clientSummaryTextLimit)
	}
	return out
}

func sanitizeMapList(value interface{}, maxItems int, keys []string, textLimits map[string]int) []map[string]interface{} {
	items := listMapItems(value)
	if maxItems > 0 && len(items) > maxItems {
		items = items[:maxItems]
	}
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		next := map[string]interface{}{}
		for _, key := range keys {
			v, ok := item[key]
			if !ok || v == nil {
				continue
			}
			if key == "columns" {
				next[key] = v
				continue
			}
			limit := clientSummaryTextLimit
			if textLimits != nil && textLimits[key] > 0 {
				limit = textLimits[key]
			}
			next[key] = sanitizeClientScalar(v, limit)
		}
		out = append(out, next)
	}
	return out
}

func sanitizeRows(value interface{}, maxRows int) []map[string]interface{} {
	rows := listMapItems(value)
	if maxRows > 0 && len(rows) > maxRows {
		rows = rows[:maxRows]
	}
	out := make([]map[string]interface{}, 0, len(rows))
	for _, row := range rows {
		next := make(map[string]interface{}, len(row))
		for k, v := range row {
			next[k] = sanitizeClientScalar(v, clientCellPreviewLimit)
		}
		out = append(out, next)
	}
	return out
}

func sanitizeColumns(value interface{}, maxItems int) []map[string]interface{} {
	if columns := sanitizeMapList(value, maxItems, []string{"name", "type", "semantic_type"}, map[string]int{
		"name":          160,
		"type":          120,
		"semantic_type": 120,
	}); len(columns) > 0 {
		return columns
	}
	if stringsList := sanitizeStringList(value, maxItems, 120); len(stringsList) > 0 {
		out := make([]map[string]interface{}, 0, len(stringsList))
		for _, name := range stringsList {
			out = append(out, map[string]interface{}{"name": name})
		}
		return out
	}
	return nil
}

func sanitizeChartSpec(value interface{}) map[string]interface{} {
	m, ok := mapItem(value)
	if !ok {
		return nil
	}
	out := copyFields(m,
		"eligible", "id", "type", "default_type", "x", "group", "value",
		"reason", "language", "labels_locale", "table_visible", "explicit_chart",
		"chart_title", "title",
	)
	if y := sanitizeStringList(m["y"], clientListLimit, 160); len(y) > 0 {
		out["y"] = y
	}
	if secondaryY := sanitizeStringList(m["secondary_y"], clientListLimit, 160); len(secondaryY) > 0 {
		out["secondary_y"] = secondaryY
	}
	if values := sanitizeStringList(m["values"], clientListLimit, 160); len(values) > 0 {
		out["values"] = values
	}
	if contract := sanitizeChartContract(m["contract"]); len(contract) > 0 {
		out["contract"] = contract
	}
	if validation := sanitizeChartValidation(m["validation"]); len(validation) > 0 {
		out["validation"] = validation
	}
	return out
}

func sanitizeChartContract(value interface{}) map[string]interface{} {
	m, ok := mapItem(value)
	if !ok {
		return nil
	}
	out := copyFields(m, "id", "type")
	for _, key := range []string{
		"intent", "encoding", "transform", "visual_scope", "evidence_scope", "display", "metadata",
	} {
		if sanitized := sanitizeJSONLike(m[key], 4); sanitized != nil {
			out[key] = sanitized
		}
	}
	return out
}

func sanitizeChartValidation(value interface{}) map[string]interface{} {
	m, ok := mapItem(value)
	if !ok {
		return nil
	}
	out := copyFields(m, "status")
	if issues := sanitizeStringList(m["issues"], clientListLimit, 500); len(issues) > 0 {
		out["issues"] = issues
	}
	return out
}

func sanitizeJSONLike(value interface{}, depth int) interface{} {
	if depth <= 0 || value == nil {
		return nil
	}
	switch v := value.(type) {
	case string:
		return truncateText(v, clientSummaryTextLimit)
	case bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	case map[string]interface{}:
		return sanitizeJSONMap(v, depth)
	case map[string]string:
		m := make(map[string]interface{}, len(v))
		for key, val := range v {
			m[key] = val
		}
		return sanitizeJSONMap(m, depth)
	case []interface{}:
		return sanitizeJSONList(v, depth)
	case []string:
		out := make([]interface{}, 0, len(v))
		for _, item := range v {
			out = append(out, truncateText(item, clientSummaryTextLimit))
		}
		return out
	case []map[string]interface{}:
		items := make([]interface{}, 0, len(v))
		for _, item := range v {
			items = append(items, item)
		}
		return sanitizeJSONList(items, depth)
	default:
		return truncateText(fmt.Sprint(v), clientSummaryTextLimit)
	}
}

func sanitizeJSONMap(m map[string]interface{}, depth int) map[string]interface{} {
	if depth <= 0 {
		return nil
	}
	out := make(map[string]interface{}, len(m))
	count := 0
	for key, val := range m {
		if count >= clientColumnLimit {
			break
		}
		cleanKey := truncateText(key, 120)
		if cleanKey == "" {
			continue
		}
		sanitized := sanitizeJSONLike(val, depth-1)
		if sanitized == nil {
			continue
		}
		out[cleanKey] = sanitized
		count++
	}
	return out
}

func sanitizeJSONList(items []interface{}, depth int) []interface{} {
	if depth <= 0 {
		return nil
	}
	if len(items) > clientListLimit {
		items = items[:clientListLimit]
	}
	out := make([]interface{}, 0, len(items))
	for _, item := range items {
		sanitized := sanitizeJSONLike(item, depth-1)
		if sanitized == nil {
			continue
		}
		out = append(out, sanitized)
	}
	return out
}

func sanitizeGraphConfig(value interface{}) map[string]interface{} {
	m, ok := mapItem(value)
	if !ok {
		return nil
	}
	out := map[string]interface{}{}
	if nodes := sanitizeStringList(m["nodes"], clientListLimit, 120); len(nodes) > 0 {
		out["nodes"] = nodes
	}
	if relations := sanitizeStringList(m["relations"], clientListLimit, 120); len(relations) > 0 {
		out["relations"] = relations
	}
	return out
}

func sanitizeStringList(value interface{}, maxItems, maxChars int) []string {
	var items []string
	switch v := value.(type) {
	case []string:
		items = append(items, v...)
	case []interface{}:
		for _, item := range v {
			switch item.(type) {
			case map[string]interface{}, map[string]string, []interface{}, []map[string]interface{}:
				continue
			}
			items = append(items, fmt.Sprint(item))
		}
	default:
		if value == nil {
			return nil
		}
		return nil
	}
	if maxItems > 0 && len(items) > maxItems {
		items = items[:maxItems]
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, truncateText(item, maxChars))
	}
	return out
}

func listMapItems(value interface{}) []map[string]interface{} {
	switch v := value.(type) {
	case []map[string]interface{}:
		return v
	case []map[string]string:
		out := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			next := make(map[string]interface{}, len(item))
			for k, val := range item {
				next[k] = val
			}
			out = append(out, next)
		}
		return out
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(v))
		for _, item := range v {
			if m, ok := mapItem(item); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		return nil
	}
}

func mapItem(value interface{}) (map[string]interface{}, bool) {
	switch v := value.(type) {
	case map[string]interface{}:
		return v, true
	case map[string]string:
		out := make(map[string]interface{}, len(v))
		for k, val := range v {
			out[k] = val
		}
		return out, true
	default:
		return nil, false
	}
}

func sanitizeClientScalar(value interface{}, textLimit int) interface{} {
	switch v := value.(type) {
	case string:
		return truncateText(v, textLimit)
	case []string:
		return sanitizeStringList(v, clientListLimit, textLimit)
	case []interface{}:
		strings := make([]string, 0, len(v))
		for _, item := range v {
			switch item.(type) {
			case map[string]interface{}, map[string]string, []interface{}:
				continue
			default:
				strings = append(strings, fmt.Sprint(item))
			}
		}
		if len(strings) == 0 {
			return nil
		}
		return sanitizeStringList(strings, clientListLimit, textLimit)
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64:
		return v
	default:
		return truncateText(fmt.Sprint(v), textLimit)
	}
}

func truncateText(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	runes := []rune(strings.TrimSpace(s))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes]) + "..."
}
