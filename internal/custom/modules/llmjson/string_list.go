// Package llmjson contains strict-but-tolerant decoders for common structured
// LLM response shapes. It accepts harmless representation variance while
// continuing to reject incompatible JSON types.
package llmjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// StringList accepts either a JSON string or an array of strings. Scalar
// values may contain comma/semicolon/newline-separated items because models
// commonly collapse short arrays such as aliases into one string.
type StringList []string

func (s *StringList) UnmarshalJSON(data []byte) error {
	if s == nil {
		return fmt.Errorf("llmjson.StringList: UnmarshalJSON on nil pointer")
	}
	data = bytes.TrimSpace(data)
	if bytes.Equal(data, []byte("null")) {
		*s = nil
		return nil
	}

	var list []string
	if err := json.Unmarshal(data, &list); err == nil {
		*s = normalizeStringList(list)
		return nil
	}

	var scalar string
	if err := json.Unmarshal(data, &scalar); err == nil {
		*s = normalizeStringList([]string{scalar})
		return nil
	}

	return fmt.Errorf("llmjson.StringList: expected string or string array, got %s", previewJSON(data))
}

func normalizeStringList(values []string) StringList {
	result := make(StringList, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		parts := strings.FieldsFunc(value, func(r rune) bool {
			switch r {
			case ',', '，', ';', '；', '\n', '\r':
				return true
			default:
				return false
			}
		})
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			key := strings.ToLower(part)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			result = append(result, part)
		}
	}
	return result
}

func previewJSON(data []byte) string {
	const limit = 120
	if len(data) <= limit {
		return string(data)
	}
	return string(data[:limit]) + "..."
}
