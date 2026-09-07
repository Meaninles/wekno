// Package usererrors is the shared public failure vocabulary. Raw diagnostics
// belong in server logs/run records, never in the public message or stream.
package usererrors

import (
	_ "embed"
	"encoding/json"
	"strings"
)

//go:embed catalog.json
var catalogJSON []byte

type Failure struct {
	Code     string   `json:"code"`
	Message  string   `json:"message"`
	Patterns []string `json:"-"`
}

var catalog = func() []Failure {
	var rows []struct {
		Code     string
		Message  string
		Patterns []string
	}
	if err := json.Unmarshal(catalogJSON, &rows); err != nil {
		panic(err)
	}
	result := make([]Failure, 0, len(rows))
	for _, row := range rows {
		result = append(result, Failure{row.Code, row.Message, row.Patterns})
	}
	return result
}()

// Classify accepts a known code first, then conservative provider-boundary
// matching. Unrecognized text is never returned to users.
func Classify(code, detail string) Failure {
	for _, item := range catalog {
		if item.Code == code {
			return item
		}
	}
	lower := strings.ToLower(strings.TrimSpace(detail))
	for _, item := range catalog {
		if detail == item.Message {
			return item
		}
		for _, pattern := range item.Patterns {
			if strings.Contains(lower, pattern) {
				return item
			}
		}
	}
	return catalog[len(catalog)-1]
}

func Message(detail string) string { return Classify("", detail).Message }
