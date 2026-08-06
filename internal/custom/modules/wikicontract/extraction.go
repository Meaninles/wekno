// Package wikicontract owns machine-readable schemas used by Wiki synthesis.
package wikicontract

import "encoding/json"

func ExtractionSchema() json.RawMessage {
	item := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"name":        map[string]any{"type": "string"},
			"slug":        map[string]any{"type": "string"},
			"aliases":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"description": map[string]any{"type": "string"},
			"details":     map[string]any{"type": "string"},
		},
		"required":             []string{"name", "slug", "aliases", "description", "details"},
		"additionalProperties": false,
	}
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"entities": map[string]any{"type": "array", "items": item},
			"concepts": map[string]any{"type": "array", "items": item},
		},
		"required":             []string{"entities", "concepts"},
		"additionalProperties": false,
	}
	raw, _ := json.Marshal(schema)
	return raw
}
