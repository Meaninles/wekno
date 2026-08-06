// Package questioncontract owns the machine-readable batch contract between
// question enrichment and an OpenAI-compatible chat provider. It keeps model
// quirks local: unknown record IDs are ignored, duplicate records are merged,
// malformed individual questions are skipped, and only genuinely omitted
// input records require a bounded recovery call.
package questioncontract

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/llmjson"
)

type Input struct {
	RecordID string `json:"record_id"`
	Content  string `json:"content"`
}

type Report struct {
	Results              map[string][]string
	MissingRecordIDs     []string
	UnknownRecordIDs     []string
	DuplicateRecordCount int
	InvalidQuestionCount int
	RecoveredTruncation  bool
}

func (r Report) HasDeviations() bool {
	return len(r.UnknownRecordIDs) > 0 || r.DuplicateRecordCount > 0 ||
		r.InvalidQuestionCount > 0 || r.RecoveredTruncation ||
		len(r.MissingRecordIDs) > 0
}

func (r Report) Detail() string {
	return fmt.Sprintf(
		"unknown=%d duplicate_records=%d invalid_questions=%d truncated_recovered=%t missing=%d",
		len(r.UnknownRecordIDs), r.DuplicateRecordCount,
		r.InvalidQuestionCount, r.RecoveredTruncation,
		len(r.MissingRecordIDs),
	)
}

type rawResponse struct {
	Results []rawResult `json:"results"`
}

type rawResult struct {
	RecordID  string            `json:"record_id"`
	Questions []json.RawMessage `json:"questions"`
}

// Schema binds record_id to the exact opaque IDs in the request. Numeric
// positions are intentionally absent, so a provider cannot return n for an
// input range 0..n-1 and accidentally link a question to the wrong chunk.
func Schema(inputs []Input) (json.RawMessage, error) {
	ids := uniqueInputIDs(inputs)
	schema := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"results": map[string]any{
				"type": "array",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"record_id": map[string]any{
							"type": "string",
							"enum": ids,
						},
						"questions": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
						},
					},
					"required":             []string{"record_id", "questions"},
					"additionalProperties": false,
				},
			},
		},
		"required":             []string{"results"},
		"additionalProperties": false,
	}
	raw, err := json.Marshal(schema)
	if err != nil {
		return nil, fmt.Errorf("encode question response schema: %w", err)
	}
	return json.RawMessage(raw), nil
}

func Parse(raw string, inputs []Input, questionCount int) (Report, error) {
	report := Report{Results: make(map[string][]string)}
	content := extractJSONPayload(raw)
	var response rawResponse
	decodeErr := decodeResponse([]byte(content), &response)
	if decodeErr != nil && strings.Contains(strings.ToLower(decodeErr.Error()), "unexpected end") {
		if recovered, _, ok := llmjson.RecoverTruncatedObjectArray(
			[]byte(content), "results",
		); ok {
			if err := decodeResponse(recovered, &response); err == nil {
				decodeErr = nil
				report.RecoveredTruncation = true
			}
		}
	}
	if decodeErr != nil {
		return Report{}, fmt.Errorf("decode question batch JSON: %w", decodeErr)
	}

	allowed := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		if id := strings.TrimSpace(input.RecordID); id != "" {
			allowed[id] = struct{}{}
		}
	}
	seen := make(map[string]struct{}, len(response.Results))
	unknown := make(map[string]struct{})
	seenQuestions := make(map[string]map[string]struct{}, len(response.Results))
	for _, item := range response.Results {
		recordID := strings.TrimSpace(item.RecordID)
		if _, ok := allowed[recordID]; !ok {
			unknown[recordID] = struct{}{}
			continue
		}
		if _, duplicate := seen[recordID]; duplicate {
			report.DuplicateRecordCount++
		} else {
			seen[recordID] = struct{}{}
			report.Results[recordID] = nil
			seenQuestions[recordID] = make(map[string]struct{})
		}
		for _, rawQuestion := range item.Questions {
			question, ok := decodeQuestion(rawQuestion)
			if !ok {
				report.InvalidQuestionCount++
				continue
			}
			question = Normalize(question)
			if len([]rune(question)) <= 5 {
				continue
			}
			key := strings.ToLower(question)
			if _, duplicate := seenQuestions[recordID][key]; duplicate {
				continue
			}
			seenQuestions[recordID][key] = struct{}{}
			if len(report.Results[recordID]) < questionCount {
				report.Results[recordID] = append(report.Results[recordID], question)
			}
		}
	}
	for id := range unknown {
		report.UnknownRecordIDs = append(report.UnknownRecordIDs, id)
	}
	sort.Strings(report.UnknownRecordIDs)
	for _, input := range inputs {
		id := strings.TrimSpace(input.RecordID)
		if _, present := report.Results[id]; !present {
			report.MissingRecordIDs = append(report.MissingRecordIDs, id)
		}
	}
	return report, nil
}

func extractJSONPayload(raw string) string {
	content := strings.TrimSpace(raw)
	if strings.HasPrefix(content, "```") {
		if newline := strings.IndexByte(content, '\n'); newline >= 0 {
			content = strings.TrimSpace(content[newline+1:])
		}
		content = strings.TrimSpace(strings.TrimSuffix(content, "```"))
	}
	objectAt := strings.IndexByte(content, '{')
	arrayAt := strings.IndexByte(content, '[')
	start := -1
	switch {
	case objectAt >= 0 && arrayAt >= 0:
		start = min(objectAt, arrayAt)
	case objectAt >= 0:
		start = objectAt
	case arrayAt >= 0:
		start = arrayAt
	}
	if start > 0 {
		content = strings.TrimSpace(content[start:])
	}
	if content == "" {
		return content
	}
	closing := byte('}')
	if content[0] == '[' {
		closing = ']'
	}
	if end := strings.LastIndexByte(content, closing); end >= 0 {
		content = content[:end+1]
	}
	return strings.TrimSpace(content)
}

func decodeResponse(content []byte, response *rawResponse) error {
	content = []byte(strings.TrimSpace(string(content)))
	if len(content) == 0 {
		return fmt.Errorf("empty response")
	}
	if content[0] == '[' {
		var results []rawResult
		if err := json.Unmarshal(content, &results); err != nil {
			return err
		}
		response.Results = results
		return nil
	}
	return json.Unmarshal(content, response)
}

func Normalize(raw string) string {
	question := strings.TrimSpace(raw)
	question = strings.TrimLeft(question, "0123456789.-*)、） ")
	return strings.TrimSpace(strings.Trim(question, `"'`))
}

func decodeQuestion(data json.RawMessage) (string, bool) {
	var direct string
	if err := json.Unmarshal(data, &direct); err == nil {
		return direct, true
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		return "", false
	}
	selected := ""
	found := false
	for _, key := range []string{"question", "text", "query"} {
		raw, exists := object[key]
		if !exists {
			continue
		}
		var candidate string
		if err := json.Unmarshal(raw, &candidate); err != nil {
			return "", false
		}
		if found && candidate != selected {
			return "", false
		}
		selected, found = candidate, true
	}
	return selected, found
}

func uniqueInputIDs(inputs []Input) []string {
	seen := make(map[string]struct{}, len(inputs))
	ids := make([]string, 0, len(inputs))
	for _, input := range inputs {
		id := strings.TrimSpace(input.RecordID)
		if id == "" {
			continue
		}
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}
