package agenteval

import "testing"

func TestResponseMaxCharsIsEvalOnlyAndBounded(t *testing.T) {
	contract := &ResponseContract{MaxResponseChars: 1200}

	production := Config{Mode: ModeProduction, CapturePolicy: CaptureMetadata}
	if got, err := production.ResponseMaxChars(contract); err != nil || got != 0 {
		t.Fatalf("production response contract = (%d, %v), want ignored", got, err)
	}

	eval := Config{Mode: ModeEval, CapturePolicy: CaptureFull}
	if got, err := eval.ResponseMaxChars(contract); err != nil || got != 1200 {
		t.Fatalf("eval response contract = (%d, %v), want 1200", got, err)
	}
	if _, err := eval.ResponseMaxChars(&ResponseContract{}); err == nil {
		t.Fatal("zero eval response limit was accepted")
	}
	if _, err := eval.ResponseMaxChars(&ResponseContract{MaxResponseChars: maxEvalResponseChars + 1}); err == nil {
		t.Fatal("unbounded eval response limit was accepted")
	}
}
