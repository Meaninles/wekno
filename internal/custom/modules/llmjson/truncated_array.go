package llmjson

import (
	"bytes"
	"encoding/json"
)

// RecoverTruncatedObjectArray preserves only fully closed object items from a
// JSON array whose tail was cut off by an LLM output-token limit. The input
// may be either a raw array or an object containing the named array field.
//
// It deliberately does not synthesize or repair a partial object. Callers
// must still unmarshal and validate the returned JSON before accepting it.
func RecoverTruncatedObjectArray(
	raw []byte,
	field string,
) (recovered []byte, completeItems int, ok bool) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || field == "" || json.Valid(raw) {
		return nil, 0, false
	}

	arrayStart := -1
	wrapped := false
	switch raw[0] {
	case '[':
		arrayStart = 0
	case '{':
		fieldJSON, err := json.Marshal(field)
		if err != nil {
			return nil, 0, false
		}
		keyAt := bytes.Index(raw, fieldJSON)
		if keyAt < 0 {
			return nil, 0, false
		}
		colonAt := bytes.IndexByte(raw[keyAt+len(fieldJSON):], ':')
		if colonAt < 0 {
			return nil, 0, false
		}
		colonAt += keyAt + len(fieldJSON)
		arrayOffset := bytes.IndexByte(raw[colonAt+1:], '[')
		if arrayOffset < 0 {
			return nil, 0, false
		}
		arrayStart = colonAt + 1 + arrayOffset
		wrapped = true
	default:
		return nil, 0, false
	}

	firstItem := -1
	lastComplete := -1
	objectDepth := 0
	inString := false
	escaped := false
	for index := arrayStart + 1; index < len(raw); index++ {
		value := raw[index]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch value {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}

		switch value {
		case '"':
			inString = true
		case '{':
			if objectDepth == 0 && firstItem < 0 {
				firstItem = index
			}
			objectDepth++
		case '}':
			if objectDepth == 0 {
				return nil, 0, false
			}
			objectDepth--
			if objectDepth == 0 {
				lastComplete = index
				completeItems++
			}
		case ']':
			if objectDepth == 0 {
				// The item array itself is complete. Recovery is still useful
				// when only the outer wrapper's final brace was truncated.
				index = len(raw)
			}
		}
	}
	if completeItems == 0 || firstItem < 0 || lastComplete < firstItem {
		return nil, 0, false
	}

	items := bytes.TrimSpace(raw[firstItem : lastComplete+1])
	if wrapped {
		fieldJSON, _ := json.Marshal(field)
		recovered = make([]byte, 0, len(fieldJSON)+len(items)+6)
		recovered = append(recovered, '{')
		recovered = append(recovered, fieldJSON...)
		recovered = append(recovered, ':', '[')
		recovered = append(recovered, items...)
		recovered = append(recovered, ']', '}')
	} else {
		recovered = make([]byte, 0, len(items)+2)
		recovered = append(recovered, '[')
		recovered = append(recovered, items...)
		recovered = append(recovered, ']')
	}
	if !json.Valid(recovered) {
		return nil, 0, false
	}
	return recovered, completeItems, true
}
