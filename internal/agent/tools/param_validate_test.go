package tools

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateParams(t *testing.T) {
	schema := json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","minLength":1},"limit":{"type":"integer","minimum":1,"maximum":100},"mode":{"enum":["fast","deep"]},"ids":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}}},"required":["query"]}`)
	for _, args := range []string{`{"query":"你好","limit":100}`, `{"query":"ok","ids":["id"]}`, `{"query":"x","extra":true}`} {
		assert.Empty(t, ValidateParams(json.RawMessage(args), schema), args)
	}
	for _, args := range []string{"", `{`, `null`, `[]`, `{}`, `{"query":null}`, `{"query":1}`, `{"query":""}`, `{"query":"ok","limit":0}`, `{"query":"ok","limit":101}`, `{"query":"ok","limit":1.5}`, `{"query":"ok","mode":"other"}`, `{"query":"ok","ids":[]}`, `{"query":"ok","ids":[123]}`} {
		assert.NotEmpty(t, ValidateParams(json.RawMessage(args), schema), args)
	}
	assert.NotEmpty(t, ValidateParams(json.RawMessage(`{}`), json.RawMessage(`{`)))
	assert.Empty(t, ValidateParams(json.RawMessage(`{}`), nil))
}

func TestDocumentInfoContract(t *testing.T) {
	for _, args := range []string{`{}`, `{"knowledge_ids":[]}`, `{"knowledge_ids":[123]}`, `{"faq_ids":[""]}`, `{`} {
		assert.NotEmpty(t, ValidateParams(json.RawMessage(args), getDocumentInfoTool.Parameters()), args)
	}
	for _, args := range []string{`{"knowledge_ids":["doc"]}`, `{"knowledge_base_ids":["kb"]}`, `{"faq_ids":["faq"]}`} {
		assert.Empty(t, ValidateParams(json.RawMessage(args), getDocumentInfoTool.Parameters()), args)
	}
}

func TestFormatValidationErrors(t *testing.T) {
	t.Run("empty errors", func(t *testing.T) {
		assert.Equal(t, "", FormatValidationErrors(nil))
	})

	t.Run("single error", func(t *testing.T) {
		errs := []ValidationError{{Param: "q", Message: "required parameter 'q' is missing"}}
		result := FormatValidationErrors(errs)
		assert.Contains(t, result, "Parameter validation failed")
		assert.Contains(t, result, "required parameter 'q' is missing")
	})

	t.Run("multiple errors joined", func(t *testing.T) {
		errs := []ValidationError{
			{Param: "a", Message: "error a"},
			{Param: "b", Message: "error b"},
		}
		result := FormatValidationErrors(errs)
		assert.Contains(t, result, "error a; error b")
	})
}
