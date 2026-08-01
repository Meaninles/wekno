package wikillm

import (
	"context"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
	"github.com/Tencent/WeKnora/internal/models/chat"
)

func TestTypedProviderUnavailableWinsOverNested403(t *testing.T) {
	err := &modeladmission.ProviderUnavailableError{
		Kind: modeladmission.KindChat, RetryAfter: 15 * time.Second,
		Cause: &chat.HTTPStatusError{StatusCode: 403, Body: "forbidden"},
	}
	if got := Classify(context.Background(), err); got != ClassProviderTransient {
		t.Fatalf("class=%s, want transient", got)
	}
}

func TestRaw403SeparatesIntermediaryFromCredentials(t *testing.T) {
	gateway := &chat.HTTPStatusError{
		StatusCode: 403,
		Body:       "<!DOCTYPE html><html>ERROR CODE 403 - request could not be satisfied</html>",
	}
	if got := Classify(context.Background(), gateway); got != ClassProviderTransient {
		t.Fatalf("gateway class=%s", got)
	}
	credential := &chat.HTTPStatusError{StatusCode: 403, Body: "{\"error\":\"invalid api key scope\"}"}
	if got := Classify(context.Background(), credential); got != ClassProviderPermanent {
		t.Fatalf("credential class=%s", got)
	}
}
