package chatretrieval

import (
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/types"
)

// Receipt reports observed scope and execution state, never workflow advice.
// The same object is visible to the model, UI and persisted tool trace.
func Receipt(result *types.ToolResult, surface string, requested, searched, queries []string, candidateCount int, executionErr error) {
	if result == nil {
		return
	}
	if result.Data == nil {
		result.Data = map[string]any{}
	}
	failures := []string{}
	if executionErr != nil {
		failures = append(failures, executionErr.Error())
	}
	surfaces := []string{}
	if len(searched) > 0 {
		surfaces = append(surfaces, surface)
	}
	refs := []string{}
	for _, ref := range result.SourceReferences {
		if ref != nil {
			refs = append(refs, ref.ID)
		}
	}
	count := 0
	switch n := result.Data["count"].(type) {
	case int:
		count = n
	case float64:
		count = int(n)
	}
	status := "ok"
	if count == 0 {
		status = "empty"
	}
	if len(searched) == 0 {
		status = "not_searched"
	}
	if executionErr != nil {
		status = "partial"
		if count == 0 {
			status = "error"
		}
	}
	scope := map[string]any{"requested_knowledge_base_ids": requested, "searched_knowledge_base_ids": searched, "queries": queries}
	if targets, exists := result.Data["search_targets"]; exists {
		scope["search_targets"] = targets
	}
	receipt := map[string]any{"searched_surfaces": surfaces, "scope": scope, "status": status, "truncated": candidateCount > count, "errors": failures, "evidence_refs": refs, "returned_count": count}
	for k, v := range receipt {
		result.Data[k] = v
	}
	encoded, _ := json.Marshal(receipt)
	if result.Output == "" {
		result.Output = string(encoded)
	} else {
		result.Output = "<retrieval_receipt>" + string(encoded) + "</retrieval_receipt>\n" + result.Output
	}
}
