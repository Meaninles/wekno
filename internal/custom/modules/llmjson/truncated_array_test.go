package llmjson

import (
	"encoding/json"
	"testing"
)

func TestRecoverTruncatedObjectArrayKeepsOnlyCompleteItems(t *testing.T) {
	raw := []byte(`{
		"extractions": [
			{"entity":"采购部","entity_attributes":["负责\"采购\""]},
			{"entity1":"采购部","entity2":"财务部","relation":"协同"},
			{"entity":"未完成`)

	recovered, count, ok := RecoverTruncatedObjectArray(raw, "extractions")
	if !ok || count != 2 {
		t.Fatalf("recovery = ok:%v count:%d, want true/2", ok, count)
	}
	var parsed struct {
		Extractions []map[string]any `json:"extractions"`
	}
	if err := json.Unmarshal(recovered, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Extractions) != 2 {
		t.Fatalf("recovered items = %d, want 2", len(parsed.Extractions))
	}
	if parsed.Extractions[0]["entity"] != "采购部" {
		t.Fatalf("first complete item changed: %#v", parsed.Extractions[0])
	}
}

func TestRecoverTruncatedRawObjectArray(t *testing.T) {
	recovered, count, ok := RecoverTruncatedObjectArray(
		[]byte(`[{"entity":"A"},{"entity":"B"},{"entity":"C"`),
		"extractions",
	)
	if !ok || count != 2 || string(recovered) != `[{"entity":"A"},{"entity":"B"}]` {
		t.Fatalf("raw recovery = %s count=%d ok=%v", recovered, count, ok)
	}
}

func TestRecoverTruncatedObjectArrayRejectsUnsafeOrUnnecessaryRepair(t *testing.T) {
	for _, raw := range [][]byte{
		[]byte(`{"extractions":[{"entity":"complete"}]}`),
		[]byte(`{"other":[{"entity":"A"}`),
		[]byte(`{"extractions":[{"entity":`),
		[]byte(`not-json`),
	} {
		if recovered, count, ok := RecoverTruncatedObjectArray(raw, "extractions"); ok {
			t.Fatalf("unexpected recovery for %q: %s count=%d", raw, recovered, count)
		}
	}
}
