package toolcontract

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestQueryOutputPreservesExecutableEvidenceAndChart(t *testing.T) {
	rows := make([]map[string]any, 37)
	for i := range rows {
		rows[i] = map[string]any{"group": fmt.Sprintf("row-%d", i), "amount": i + 1}
	}
	data := map[string]any{
		"query": "SELECT group, amount FROM source", "rows": rows, "row_count": len(rows),
		"source":     map[string]any{"knowledge_id": "source-id"},
		"limits":     map[string]any{"truncated": true, "max_rows": 37},
		"lineage":    QueryLineage("source-id", "SELECT group, amount FROM source", nil, rows, true),
		"chart":      map[string]any{"eligible": true, "id": "chart_abc123", "contract": map[string]any{"type": "bar"}},
		"session_id": "ui-only",
	}
	output, err := QueryOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatal(err)
	}
	if got["chart_handle"] != "{{chart:chart_abc123}}" || len(got["rows"].([]any)) != len(rows) {
		t.Fatalf("chart handle or result rows missing: %s", output)
	}
	for _, key := range []string{"query", "rows", "row_count", "source", "limits", "lineage", "chart"} {
		want, _ := json.Marshal(data[key])
		actual, _ := json.Marshal(got[key])
		if string(want) != string(actual) {
			t.Fatalf("model projection changed %s: %s != %s", key, actual, want)
		}
	}
	if _, exists := got["session_id"]; exists {
		t.Fatal("UI routing metadata must not be part of query evidence")
	}
}

func TestQueryOutputDoesNotInventUnavailableChart(t *testing.T) {
	data := map[string]any{"chart_requested": true, "chart": map[string]any{
		"eligible": false, "id": "", "reason": "no numeric metric column",
	}}
	output, err := QueryOutput(data)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(output), &got); err != nil {
		t.Fatal(err)
	}
	if _, exists := got["chart_handle"]; exists || got["chart"].(map[string]any)["reason"] != "no numeric metric column" {
		t.Fatalf("unavailable chart lost its actual status: %s", output)
	}
	if _, err := QueryOutput(map[string]any{"rows": make(chan int)}); err == nil {
		t.Fatal("unserializable execution result must fail explicitly")
	}
}
