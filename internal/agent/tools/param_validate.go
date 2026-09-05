package tools

import (
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
	"strings"
)

type ValidationError struct {
	Param   string
	Message string
}

// ValidateParams enforces the tool's full schema, including nested/composed schemas.
func ValidateParams(args json.RawMessage, schema json.RawMessage) []ValidationError {
	if err := toolcontract.Validate(args, schema); err != nil {
		return []ValidationError{{Message: err.Error()}}
	}
	return nil
}

// FormatValidationErrors formats a list of validation errors into a human-readable string.
func FormatValidationErrors(errs []ValidationError) string {
	if len(errs) == 0 {
		return ""
	}
	msgs := make([]string, len(errs))
	for i, e := range errs {
		msgs[i] = e.Message
	}
	return "Parameter validation failed: " + strings.Join(msgs, "; ")
}
