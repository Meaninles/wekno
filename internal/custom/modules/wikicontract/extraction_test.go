package wikicontract

import (
	"encoding/json"
	"testing"
)

func TestExtractionSchemaIsStrict(t *testing.T) {
	raw := ExtractionSchema()
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema["additionalProperties"] != false {
		t.Fatalf("schema is not strict: %s", raw)
	}
}
