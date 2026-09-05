// Package toolcontract validates the same JSON Schema that is supplied to the model.
package toolcontract

import (
	"encoding/json"
	"fmt"
	"github.com/google/jsonschema-go/jsonschema"
)

func Validate(args, schema json.RawMessage) error {
	var value any
	if err := json.Unmarshal(args, &value); err != nil {
		return fmt.Errorf("invalid argument JSON: %w", err)
	}
	if len(schema) == 0 {
		return nil
	}
	var definition jsonschema.Schema
	if err := json.Unmarshal(schema, &definition); err != nil {
		return fmt.Errorf("invalid tool schema: %w", err)
	}
	resolved, err := definition.Resolve(nil)
	if err != nil {
		return fmt.Errorf("invalid tool schema: %w", err)
	}
	return resolved.Validate(value)
}
