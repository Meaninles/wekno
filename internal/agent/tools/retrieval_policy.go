package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/types"
)

// TargetedEvidenceRetrievalRedirect prevents a multi-topic comparison from
// dumping a whole pinned document into the bounded model context. Both the
// native ReAct engine and the general-agent sidecar bridge use this policy so
// they cannot diverge. Exact chunk reads and genuine exhaustive-review turns
// remain unchanged.
func TargetedEvidenceRetrievalRedirect(
	toolName string,
	args map[string]interface{},
	query string,
) *types.ToolResult {
	if toolName != ToolListKnowledgeChunks || len(conversationmemory.RequiredEvidenceTopics(query)) < 2 {
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
	topics := conversationmemory.RequiredEvidenceTopics(query)
	encodedTopics, _ := json.Marshal(topics)
	searches := conversationmemory.EvidenceRetrievalQueries(query)
	encodedSearches, _ := json.Marshal(searches)
	return &types.ToolResult{
		Success: false,
		Error: fmt.Sprintf(
			"Whole-document listing was skipped because this request requires direct evidence for multiple named topics and bounded output could hide later chunks. Use grep_chunks (preferred) or knowledge_search inside knowledge_id %q with one query for each topic in %s (keep every query focused). Suggested user-derived queries: %s. Then call list_knowledge_chunks with each exact chunk_id that supports a claim. For applicability questions, a definition, amount threshold, evaluation-start threshold, neighboring procedure, or parent category does not replace the named topic's complete condition passage. Do not answer until every named topic has direct evidence.",
			knowledgeID,
			string(encodedTopics),
			string(encodedSearches),
		),
	}
}
