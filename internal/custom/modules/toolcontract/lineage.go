package toolcontract

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
)

// QueryLineage describes the execution's real dataset/query boundary. It never
// claims a cell/row mapping for an aggregation or model-authored literal.
func QueryLineage(scope any, query string, columns, rows any, truncated bool) map[string]any {
	result, _ := json.Marshal(struct{ Columns, Rows any }{columns, rows})
	return map[string]any{"owner": "executor", "granularity": "dataset_and_query",
		"input_scope": scope, "executed_query": query, "query_sha256": fmt.Sprintf("%x", sha256.Sum256([]byte(query))),
		"result_sha256": fmt.Sprintf("%x", sha256.Sum256(result)), "result_truncated": truncated,
		"cell_mapping": nil, "semantic_validation": "not_performed"}
}
