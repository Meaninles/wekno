package toolcontract

import (
	"encoding/json"
	"fmt"
)

// QueryOutput is the model-facing projection of an executed query. Both SQL
// tools use this boundary, so chart handles, provenance and truncation state
// cannot be visible to the UI but absent from the model's evidence. Row limits
// belong to the executor; conversation memory owns the overall context budget.
func QueryOutput(data map[string]any) (string, error) {
	output := make(map[string]any)
	for _, key := range []string{"analysis_type", "source", "query", "columns", "rows", "row_count", "limits", "lineage", "chart_requested", "chart"} {
		if value, ok := data[key]; ok {
			output[key] = value
		}
	}
	if chart, ok := data["chart"].(map[string]any); ok && chart["eligible"] == true {
		if id, ok := chart["id"].(string); ok && id != "" {
			output["chart_handle"] = "{{chart:" + id + "}}"
		}
	}
	encoded, err := json.Marshal(output)
	if err != nil {
		return "", fmt.Errorf("encode executed query result: %w", err)
	}
	return string(encoded), nil
}
