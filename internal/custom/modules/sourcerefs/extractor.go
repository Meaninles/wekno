package sourcerefs

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/Tencent/WeKnora/internal/types"
)

const (
	SourceTypeKnowledge = "knowledge"
	SourceTypeWiki      = "wiki"
	SourceTypeWeb       = "web"
	SourceTypeData      = "data_source"
)

var (
	wikiLinkRE    = regexp.MustCompile(`\[\[([^\]|\n]+)\|([^\]\n]+)\]\]`)
	wikiPageRE    = regexp.MustCompile(`(?s)<wiki_page>.*?</wiki_page>`)
	wikiKBRE      = regexp.MustCompile(`(?s)<knowledge_base_id>\s*([^<]+?)\s*</knowledge_base_id>`)
	wikiSummaryRE = regexp.MustCompile(`(?s)<summary>\s*(.*?)\s*</summary>`)
	wikiContentRE = regexp.MustCompile(`(?s)<content>\s*(.*?)\s*</content>`)
)

// ExtractFromToolResult normalizes custom agent tool outputs into the existing
// SearchResult reference shape so the current SSE and message storage pipeline
// can persist and replay them without a schema migration.
func ExtractFromToolResult(toolName string, result *types.ToolResult) []*types.SearchResult {
	if result == nil || result.Data == nil {
		return nil
	}

	name := strings.ToLower(strings.TrimSpace(toolName))
	displayType := strings.ToLower(stringValue(result.Data["display_type"]))
	var refs []*types.SearchResult

	switch {
	case displayType == "web_search_results" || displayType == "web_fetch_results" || name == "web_search" || name == "web_fetch":
		refs = append(refs, extractWebReferences(result.Data)...)
	case name == "wiki_read_page":
		refs = append(refs, extractWikiReferences(result.Output)...)
	case displayType == "structured_analysis_result" && result.Success:
		if ref := extractStructuredAnalysisReference(result.Data, result.Output); ref != nil {
			refs = append(refs, ref)
		}
	case displayType == "search_results" || displayType == "grep_results" || displayType == "knowledge_chunks_list" ||
		name == "knowledge_search" || name == "search_knowledge" ||
		name == "grep_chunks" || name == "list_knowledge_chunks":
		refs = append(refs, extractKnowledgeReferences(result.Data)...)
	}

	return uniqueReferences(refs)
}

func ReferenceKey(ref *types.SearchResult) string {
	return CitationKey(ref)
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
		// knowledge_results are document/catalog hits, not claim-bearing
		// fragments. A follow-up chunk read/search is required before citation.
	case "knowledge_chunks_list":
		chunks := mapSlice(data["chunks"])
		if len(chunks) == 0 {
			refs = append(refs, knowledgeRefFromMap(data, data))
			break
		}
		for _, item := range chunks {
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
	if id == "" || content == "" {
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
		SourceLocator:     jsonValue(item["source_locator"]),
	}
}

func jsonValue(value interface{}) types.JSON {
	switch typed := value.(type) {
	case nil:
		return nil
	case json.RawMessage:
		if json.Valid(typed) {
			return append(types.JSON(nil), typed...)
		}
	case types.JSON:
		if json.Valid(typed) {
			return append(types.JSON(nil), typed...)
		}
	case []byte:
		if json.Valid(typed) {
			return append(types.JSON(nil), typed...)
		}
	case string:
		raw := []byte(strings.TrimSpace(typed))
		if json.Valid(raw) {
			return types.JSON(raw)
		}
	default:
		raw, err := json.Marshal(typed)
		if err == nil && json.Valid(raw) {
			return types.JSON(raw)
		}
	}
	return nil
}

func extractWebReferences(data map[string]interface{}) []*types.SearchResult {
	var refs []*types.SearchResult
	for _, item := range mapSlice(data["results"]) {
		rawURL := stringValue(item["url"])
		title := firstNonEmpty(stringValue(item["title"]), hostFromURL(rawURL), rawURL)
		content := firstNonEmpty(stringValue(item["snippet"]), stringValue(item["summary"]), stringValue(item["content"]))
		if rawURL == "" || content == "" {
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

func extractWikiReferences(output string) []*types.SearchResult {
	var refs []*types.SearchResult
	for _, block := range wikiPageRE.FindAllString(output, -1) {
		link := wikiLinkRE.FindStringSubmatch(block)
		if len(link) < 3 {
			continue
		}
		slug := strings.TrimSpace(link[1])
		title := strings.TrimSpace(link[2])
		kbID := firstSubmatch(wikiKBRE, block)
		summary := firstSubmatch(wikiSummaryRE, block)
		content := firstSubmatch(wikiContentRE, block)
		if slug == "" || kbID == "" || strings.TrimSpace(summary+content) == "" {
			continue
		}
		refs = append(refs, &types.SearchResult{
			ID:              "wiki:" + kbID + ":" + slug,
			Content:         strings.TrimSpace(summary + "\n\n" + content),
			KnowledgeTitle:  firstNonEmpty(title, slug),
			KnowledgeBaseID: kbID,
			ChunkType:       "wiki_page",
			Metadata: map[string]string{
				"source_type":       SourceTypeWiki,
				"slug":              slug,
				"knowledge_base_id": kbID,
			},
		})
	}
	return refs
}

func extractStructuredAnalysisReference(data map[string]interface{}, output string) *types.SearchResult {
	query := stringValue(data["query"])
	rows := data["rows"]
	if query == "" || len(mapSlice(rows)) == 0 {
		return nil
	}
	snapshot := map[string]interface{}{
		"query":   query,
		"columns": data["columns"],
		"rows":    rows,
		"limits":  data["limits"],
	}
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil
	}
	content := strings.TrimSpace(output)
	if content == "" {
		content = string(raw)
	}
	sum := sha256.Sum256(raw)
	evidenceID := fmt.Sprintf("query-%x", sum[:16])
	analysisType := firstNonEmpty(stringValue(data["analysis_type"]), "table")
	knowledgeID := ""
	title := "查询结果"
	if source, ok := data["source"].(map[string]interface{}); ok {
		knowledgeID = stringValue(source["knowledge_id"])
		title = firstNonEmpty(stringValue(source["table_name"]), title)
	}
	return &types.SearchResult{
		ID:             evidenceID,
		Content:        content,
		KnowledgeID:    knowledgeID,
		KnowledgeTitle: title,
		ChunkType:      "data_query_result",
		Metadata: map[string]string{
			"source_type":     SourceTypeKnowledge,
			"evidence_origin": analysisType + "_query",
			"query":           query,
			MetadataChunkID:   evidenceID,
		},
	}
}

func firstSubmatch(re *regexp.Regexp, value string) string {
	match := re.FindStringSubmatch(value)
	if len(match) < 2 {
		return ""
	}
	return strings.TrimSpace(match[1])
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
