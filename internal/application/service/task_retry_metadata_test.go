package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/custom/modules/taskretry"
)

func TestIsFinalAsynqAttemptUsesLiteRetryMetadata(t *testing.T) {
	if isFinalAsynqAttempt(context.Background()) {
		t.Fatal("plain context must not be treated as a final worker attempt")
	}
	if isFinalAsynqAttempt(taskretry.WithMetadata(context.Background(), 1, 2)) {
		t.Fatal("retry 1/2 must not be terminal")
	}
	if !isFinalAsynqAttempt(taskretry.WithMetadata(context.Background(), 2, 2)) {
		t.Fatal("retry 2/2 must be terminal")
	}
}
