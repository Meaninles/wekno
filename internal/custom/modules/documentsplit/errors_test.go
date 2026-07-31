package documentsplit

import (
	"errors"
	"fmt"
	"testing"
)

func TestRemoteErrorPermanentClassificationSurvivesWrapping(t *testing.T) {
	permanent := &RemoteError{
		Code:      "too_many_parts",
		Message:   "requires 10770 parts",
		Retryable: false,
	}
	if !IsPermanent(fmt.Errorf("split preparation: %w", permanent)) {
		t.Fatal("wrapped non-retryable remote error was not classified permanent")
	}
	if IsPermanent(&RemoteError{Code: "backend_busy", Retryable: true}) {
		t.Fatal("retryable remote error was classified permanent")
	}
	if IsPermanent(errors.New("unknown transport failure")) {
		t.Fatal("untyped transport error was classified permanent")
	}
}
