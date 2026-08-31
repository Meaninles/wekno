package agenteval

import "fmt"

const maxEvalResponseChars = 20000

// ResponseContract contains presentation-only constraints supplied by the
// external Eval runner. It deliberately excludes claims, evidence anchors,
// rubrics and reference answers so evaluation expectations cannot leak into
// the system under test.
type ResponseContract struct {
	MaxResponseChars int `json:"max_response_chars,omitempty"`
}

// ResponseMaxChars accepts an Eval response limit only when full-content Eval
// mode is active. Production remains record-only: even a client-supplied
// contract is ignored and cannot change generation or add work to the hot path.
func (c Config) ResponseMaxChars(contract *ResponseContract) (int, error) {
	if contract == nil || !c.CaptureContent() {
		return 0, nil
	}
	if contract.MaxResponseChars < 1 || contract.MaxResponseChars > maxEvalResponseChars {
		return 0, fmt.Errorf(
			"eval_response_contract.max_response_chars must be between 1 and %d",
			maxEvalResponseChars,
		)
	}
	return contract.MaxResponseChars, nil
}
