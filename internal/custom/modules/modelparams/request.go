// Package modelparams preserves explicit request controls across SDK encoders.
// Sampling recommendations belong to the configured gateway route, not the app.
package modelparams

import "encoding/json"

func Apply(body any, fields map[string]any) (any, error) {
	if len(fields) == 0 {
		return body, nil
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, err
	}
	for key, value := range fields {
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		result[key] = encoded
	}
	return result, nil
}
