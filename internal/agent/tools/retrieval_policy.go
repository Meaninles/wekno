package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/types"
)

// TargetedEvidenceRetrievalRedirect prevents a pinpoint or structured
// evidence request from dumping a whole pinned document into the bounded model
// context. Both the native ReAct engine and the general-agent sidecar bridge
// use this policy so they cannot diverge. Exact chunk reads and genuine
// exhaustive-review turns remain unchanged.
func TargetedEvidenceRetrievalRedirect(
	toolName string,
	args map[string]interface{},
	query string,
) *types.ToolResult {
	searches := conversationmemory.RequiredEvidenceSearches(query)
	if toolName != ToolListKnowledgeChunks || len(searches) == 0 || exhaustiveEvidenceReviewTurn(query) {
		return nil
	}
	stringArg := func(key string) string {
		value, _ := args[key].(string)
		return strings.TrimSpace(value)
	}
	if stringArg("chunk_id") != "" || stringArg("faq_id") != "" {
		return nil
	}
	knowledgeID := stringArg("knowledge_id")
	if knowledgeID == "" {
		return nil
	}
	encodedSearches, _ := json.Marshal(searches)
	return &types.ToolResult{
		Success: false,
		Error: fmt.Sprintf(
			"Whole-document listing was skipped because this request has focused evidence targets and bounded output could hide the relevant fragments. Use knowledge_search first inside the selected knowledge scope with these user-derived semantic queries: %s. Use grep_chunks only for a literal term when semantic search is insufficient. Then call list_knowledge_chunks with an exact chunk_id only when a supporting fragment needs deeper reading. Do not answer a requested factual section until it has direct evidence. knowledge_id=%q.",
			string(encodedSearches),
			knowledgeID,
		),
	}
}

func exhaustiveEvidenceReviewTurn(query string) bool {
	value := strings.ToLower(query)
	for _, marker := range []string{
		"全文阅读", "完整阅读", "通读全文", "逐章阅读", "逐节阅读", "逐条审查", "全文审查", "全量导出",
		"read the entire", "review the entire", "full-document review", "exhaustive review",
	} {
		if strings.Contains(value, marker) {
			return true
		}
	}
	return false
}
