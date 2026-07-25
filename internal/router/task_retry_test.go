package router

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/application/service"
	"github.com/Tencent/WeKnora/internal/custom/modules/documentqueue"
	"github.com/hibiken/asynq"
)

func TestAsynqIsFailureFunc(t *testing.T) {
	realFailure := errors.New("redis unavailable")
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "wiki lock conflict", err: service.ErrWikiIngestConcurrent, want: false},
		{name: "wrapped wiki lock conflict", err: fmt.Errorf("defer worker: %w", service.ErrWikiIngestConcurrent), want: false},
		{name: "document lease conflict", err: documentqueue.ErrAlreadyLeased, want: false},
		{name: "wrapped document lease conflict", err: fmt.Errorf("defer duplicate: %w", documentqueue.ErrAlreadyLeased), want: false},
		{name: "document durable capacity", err: documentqueue.ErrInstanceCapacity, want: false},
		{name: "wrapped document durable capacity", err: fmt.Errorf("defer capacity: %w", documentqueue.ErrInstanceCapacity), want: false},
		{name: "document instance fenced", err: documentqueue.ErrInstanceFenced, want: false},
		{name: "wrapped document instance fenced", err: fmt.Errorf("defer old boot: %w", documentqueue.ErrInstanceFenced), want: false},
		{name: "real failure", err: realFailure, want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := asynqIsFailureFunc(tt.err); got != tt.want {
				t.Fatalf("asynqIsFailureFunc(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestAsynqRetryDelayFuncUsesShortBudgetFreeDocumentOwnershipPoll(t *testing.T) {
	task := asynq.NewTask("document:test", nil)
	for _, err := range []error{
		documentqueue.ErrAlreadyLeased,
		fmt.Errorf("duplicate delivery: %w", documentqueue.ErrAlreadyLeased),
		documentqueue.ErrInstanceCapacity,
		fmt.Errorf("durable capacity: %w", documentqueue.ErrInstanceCapacity),
		documentqueue.ErrInstanceFenced,
		fmt.Errorf("old instance: %w", documentqueue.ErrInstanceFenced),
	} {
		if got := asynqRetryDelayFunc(99, err, task); got != 2*time.Second {
			t.Fatalf("document lease retry delay = %s, want 2s", got)
		}
	}
}
