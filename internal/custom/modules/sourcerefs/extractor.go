package sourcerefs

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	SourceTypeKnowledge = "knowledge"
	SourceTypeWiki      = "wiki"
	SourceTypeWeb       = "web"
	SourceTypeData      = "data_source"
)

var wikiLinkRE = regexp.MustCompile(`\[\[([^\]|\n]+)\|([^\]\n]+)\]\]`)

// ExtractFromToolResult normalizes custom agent tool outputs into the existing
// SearchResult reference shape so the current SSE and message storage pipeline
// can persist and replay them without a schema migration.
func ExtractFromToolResult(toolName string, result *types.ToolResult) []*types.SearchResult {
	if result == nil || !result.Success || result.Data == nil {
		return nil
	}

	name := strings.ToLower(strings.TrimSpace(toolName))
	displayType := strings.ToLower(stringValue(result.Data["display_type"]))
	var refs []*types.SearchResult

	switch {
	case displayType == "web_search_results" || displayType == "web_fetch_results" || name == "web_search" || name == "web_fetch":
		refs = append(refs, extractWebReferences(result.Data)...)
	case name == "wiki_search" || name == "wiki_read_page" || strings.HasPrefix(name, "wiki_"):
		refs = append(refs, extractWikiReferences(result.Data, result.Output)...)
	case displayType == "db_catalog" || displayType == "db_schema" || displayType == "structured_analysis_result" ||
		name == "db_catalog" || name == "db_schema" || name == "db_query":
		refs = append(refs, extractDataSourceReferences(result.Data)...)
	case displayType == "search_results" || displayType == "grep_results" || displayType == "knowledge_chunks_list" ||
		displayType == "document_info" || name == "knowledge_search" || name == "search_knowledge" ||
		name == "grep_chunks" || name == "list_knowledge_chunks" || name == "get_document_info":
		refs = append(refs, extractKnowledgeReferences(result.Data)...)
	}

	return uniqueReferences(refs)
}

func ReferenceKey(ref *types.SearchResult) string {
	if ref == nil {
		return ""
	}
	sourceType := sourceTypeFromRef(ref)
	id := strings.TrimSpace(ref.ID)
	if id == "" {
		id = strings.TrimSpace(ref.KnowledgeID)
	}
	if id == "" {
		id = strings.TrimSpace(ref.KnowledgeTitle)
	}
	if sourceType == SourceTypeWiki {
		id = strings.TrimSpace(ref.Metadata["slug"]) + ":" + strings.TrimSpace(ref.KnowledgeBaseID)
	}
	if sourceType == SourceTypeWeb {
		id = firstNonEmpty(ref.Metadata["url"], ref.ID, ref.KnowledgeTitle)
	}
	if sourceType == SourceTypeData {
		id = firstNonEmpty(ref.Metadata["source_id"], ref.ID, ref.KnowledgeTitle)
	}
	if id == "" {
		return ""
	}
	return sourceType + ":" + strings.ToLower(id)
}

func sourceTypeFromRef(ref *types.SearchResult) string {
	if ref == nil {
		return SourceTypeKnowledge
	}
	if ref.Metadata != nil {
		if t := strings.TrimSpace(ref.Metadata["source_type"]); t != "" {
			return t
		}
	}
	switch ref.ChunkType {
	case "web_search":
		return SourceTypeWeb
	case "wiki_page":
		return SourceTypeWiki
	case "data_source":
		return SourceTypeData
	default:
		return SourceTypeKnowledge
	}
}

func extractKnowledgeReferences(data map[string]interface{}) []*types.SearchResult {
	displayType := strings.ToLower(stringValue(data["display_type"]))
	var refs []*types.SearchResult

	switch displayType {
	case "search_results":
		for _, item := range mapSlice(data["results"]) {
			refs = append(refs, knowledgeRefFromMap(item, data))
		}
	case "grep_results":
		chunks := mapSlice(data["chunk_results"])
		if len(chunks) > 0 {
			for _, item := range chunks {
				refs = append(refs, knowledgeRefFromMap(item, data))
			}
			break
		}
		for _, item := range mapSlice(data["knowledge_results"]) {
			refs = append(refs, knowledgeRefFromMap(item, data))
		}
	case "knowledge_chunks_list":
		chunks := mapSlice(data["chunks"])
		if len(chunks) == 0 {
			refs = append(refs, knowledgeRefFromMap(data, data))
			break
		}
		for _, item := range chunks {
			refs = append(refs, knowledgeRefFromMap(item, data))
		}
	case "document_info":
		for _, item := range mapSlice(data["documents"]) {
			refs = append(refs, knowledgeRefFromMap(item, data))
		}
	}

	return refs
}

func knowledgeRefFromMap(item map[string]interface{}, parent map[string]interface{}) *types.SearchResult {
	id := firstNonEmpty(
		stringValue(item["chunk_id"]),
		stringValue(item["faq_id"]),
		stringValue(item["id"]),
		stringValue(item["knowledge_id"]),
	)
	knowledgeID := firstNonEmpty(stringValue(item["knowledge_id"]), stringValue(parent["knowledge_id"]))
	title := firstNonEmpty(
		stringValue(item["knowledge_title"]),
		stringValue(item["title"]),
		stringValue(item["knowledge_filename"]),
		stringValue(parent["knowledge_title"]),
		stringValue(parent["title"]),
		knowledgeID,
	)
	content := firstNonEmpty(
		stringValue(item["content"]),
		stringValue(item["match_snippet"]),
		stringValue(item["description"]),
		stringValue(item["faq_question"]),
	)
	if id == "" && knowledgeID == "" && title == "" {
		return nil
	}
	chunkType := firstNonEmpty(stringValue(item["chunk_type"]), "text")
	if stringValue(item["faq_id"]) != "" {
		chunkType = "faq"
	}

	metadata := map[string]string{
		"source_type": SourceTypeKnowledge,
	}
	copySelectedMetadata(metadata, item, "source_query", "query_type", "knowledge_base_type", "file_name", "file_type", "chunk_count")
	return &types.SearchResult{
		ID:                id,
		Content:           content,
		KnowledgeID:       knowledgeID,
		KnowledgeTitle:    title,
		KnowledgeBaseID:   firstNonEmpty(stringValue(item["knowledge_base_id"]), stringValue(parent["knowledge_base_id"])),
		KnowledgeFilename: firstNonEmpty(stringValue(item["knowledge_filename"]), stringValue(item["file_name"])),
		ChunkIndex:        intValue(firstNonEmptyValue(item["chunk_index"], item["index"])),
		Score:             floatValue(item["score"]),
		ChunkType:         chunkType,
		Metadata:          metadata,
	}
}

func extractWebReferences(data map[string]interface{}) []*types.SearchResult {
	var refs []*types.SearchResult
	for _, item := range mapSlice(data["results"]) {
		rawURL := stringValue(item["url"])
		title := firstNonEmpty(stringValue(item["title"]), hostFromURL(rawURL), rawURL)
		content := firstNonEmpty(stringValue(item["snippet"]), stringValue(item["summary"]), stringValue(item["content"]))
		if rawURL == "" && title == "" {
			continue
		}
		metadata := map[string]string{
			"source_type": SourceTypeWeb,
			"url":         rawURL,
		}
		copySelectedMetadata(metadata, item, "source", "published_at", "prompt", "method")
		refs = append(refs, &types.SearchResult{
			ID:             firstNonEmpty(rawURL, title),
			Content:        content,
			KnowledgeTitle: title,
			ChunkType:      "web_search",
			Metadata:       metadata,
		})
	}
	return refs
}

func extractWikiReferences(data map[string]interface{}, output string) []*types.SearchResult {
	foundKBs, ok := data["found_kbs"].(map[string]interface{})
	if !ok {
		if typed, typedOK := data["found_kbs"].(map[string][]string); typedOK {
			foundKBs = make(map[string]interface{}, len(typed))
			for slug, kbIDs := range typed {
				foundKBs[slug] = kbIDs
			}
		}
	}
	if len(foundKBs) == 0 {
		return nil
	}

	titleBySlug := wikiTitlesBySlug(output)
	slugs := make([]string, 0, len(foundKBs))
	for slug := range foundKBs {
		slugs = append(slugs, slug)
	}
	sort.Strings(slugs)

	var refs []*types.SearchResult
	for _, slug := range slugs {
		kbIDs := stringSlice(foundKBs[slug])
		if len(kbIDs) == 0 {
			kbIDs = []string{""}
		}
		title := firstNonEmpty(titleBySlug[slug], slug)
		for _, kbID := range kbIDs {
			metadata := map[string]string{
				"source_type": SourceTypeWiki,
				"slug":        slug,
			}
			if kbID != "" {
				metadata["knowledge_base_id"] = kbID
			}
			refs = append(refs, &types.SearchResult{
				ID:              "wiki:" + kbID + ":" + slug,
				Content:         title,
				KnowledgeTitle:  title,
				KnowledgeBaseID: kbID,
				ChunkType:       "wiki_page",
				Metadata:        metadata,
			})
		}
	}
	return refs
}

func extractDataSourceReferences(data map[string]interface{}) []*types.SearchResult {
	sources := mapSlice(data["sources"])
	if len(sources) == 0 {
		if source, ok := data["source"].(map[string]interface{}); ok {
			sources = []map[string]interface{}{source}
		}
	}

	var refs []*types.SearchResult
	for _, item := range sources {
		sourceID := stringValue(item["id"])
		name := firstNonEmpty(stringValue(item["name"]), sourceID)
		if sourceID == "" && name == "" {
			continue
		}
		metadata := map[string]string{
			"source_type": SourceTypeData,
		}
		copySelectedMetadata(metadata, item, "id", "name", "type", "description")
		if sourceID != "" {
			metadata["source_id"] = sourceID
		}
		if name != "" {
			metadata["source_name"] = name
		}
		if dbType := stringValue(item["type"]); dbType != "" {
			metadata["database_type"] = dbType
		}
		refs = append(refs, &types.SearchResult{
			ID:             "data_source:" + firstNonEmpty(sourceID, name),
			Content:        stringValue(item["description"]),
			KnowledgeTitle: name,
			ChunkType:      "data_source",
			Metadata:       metadata,
		})
	}
	return refs
}

func wikiTitlesBySlug(output string) map[string]string {
	out := make(map[string]string)
	for _, match := range wikiLinkRE.FindAllStringSubmatch(output, -1) {
		if len(match) < 3 {
			continue
		}
		slug := strings.TrimSpace(match[1])
		title := strings.TrimSpace(match[2])
		if slug != "" && title != "" {
			out[slug] = title
		}
	}
	return out
}

func uniqueReferences(refs []*types.SearchResult) []*types.SearchResult {
	out := make([]*types.SearchResult, 0, len(refs))
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if ref == nil {
			continue
		}
		key := ReferenceKey(ref)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ref)
	}
	return out
}

func mapSlice(value interface{}) []map[string]interface{} {
	switch typed := value.(type) {
	case []map[string]interface{}:
		return typed
	case []interface{}:
		out := make([]map[string]interface{}, 0, len(typed))
		for _, item := range typed {
			if m, ok := item.(map[string]interface{}); ok {
				out = append(out, m)
			}
		}
		return out
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var out []map[string]interface{}
		if err := json.Unmarshal(data, &out); err != nil {
			return nil
		}
		return out
	}
}

func stringSlice(value interface{}) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []interface{}:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s := strings.TrimSpace(stringValue(item)); s != "" {
				out = append(out, s)
			}
		}
		return out
	case string:
		if strings.TrimSpace(typed) == "" {
			return nil
		}
		return []string{strings.TrimSpace(typed)}
	default:
		data, err := json.Marshal(value)
		if err != nil {
			return nil
		}
		var out []string
		if err := json.Unmarshal(data, &out); err == nil {
			return out
		}
		return nil
	}
}

func copySelectedMetadata(dst map[string]string, src map[string]interface{}, keys ...string) {
	for _, key := range keys {
		if value := strings.TrimSpace(stringValue(src[key])); value != "" {
			dst[key] = value
		}
	}
}

func stringValue(value interface{}) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case fmt.Stringer:
		return strings.TrimSpace(typed.String())
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
}

func intValue(value interface{}) int {
	switch typed := value.(type) {
	case int:
		return typed
	case int64:
		return int(typed)
	case float64:
		return int(typed)
	case string:
		var out int
		_, _ = fmt.Sscanf(typed, "%d", &out)
		return out
	default:
		return 0
	}
}

func floatValue(value interface{}) float64 {
	switch typed := value.(type) {
	case float64:
		return typed
	case float32:
		return float64(typed)
	case int:
		return float64(typed)
	case int64:
		return float64(typed)
	case string:
		var out float64
		_, _ = fmt.Sscanf(typed, "%f", &out)
		return out
	default:
		return 0
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func firstNonEmptyValue(values ...interface{}) interface{} {
	for _, value := range values {
		if strings.TrimSpace(stringValue(value)) != "" {
			return value
		}
	}
	return nil
}

func hostFromURL(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" {
		return ""
	}
	return parsed.Host
}
