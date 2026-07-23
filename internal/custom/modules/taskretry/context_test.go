package taskretry

import (
	"context"
	"testing"
)

func TestMetadataRoundTripAndClamp(t *testing.T) {
	if _, _, ok := Metadata(context.Background()); ok {
		t.Fatal("plain context unexpectedly contains retry metadata")
	}

	ctx := WithMetadata(context.Background(), 2, 5)
	retry, maxRetry, ok := Metadata(ctx)
	if !ok || retry != 2 || maxRetry != 5 {
		t.Fatalf("Metadata() = (%d, %d, %v), want (2, 5, true)", retry, maxRetry, ok)
	}

	ctx = WithMetadata(context.Background(), -1, -2)
	retry, maxRetry, ok = Metadata(ctx)
	if !ok || retry != 0 || maxRetry != 0 {
		t.Fatalf("clamped Metadata() = (%d, %d, %v), want (0, 0, true)", retry, maxRetry, ok)
	}
}
