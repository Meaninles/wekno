package vlmguard

import (
	"errors"
	"fmt"
	"time"
)

type FailureKind string

const (
	FailureFirstTokenTimeout FailureKind = "first_token_timeout"
	FailureIdleTimeout       FailureKind = "stream_idle_timeout"
	FailureStreamTruncated   FailureKind = "stream_truncated"
	FailureTotalBudget       FailureKind = "total_budget_exceeded"
	FailureRunaway           FailureKind = "runaway_repetition"
	FailureOutputLimit       FailureKind = "output_limit_reached"
)

// Error classifies a VLM request that was terminated by the local progress
// guard. Only no-progress failures count against provider health. A request
// that kept generating until a local budget, repetition guard or token limit
// was reached is unhealthy work, not proof that the whole provider is down.
type Error struct {
	Kind          FailureKind
	Operation     Operation
	Limit         time.Duration
	Elapsed       time.Duration
	ProgressChars int
	FinishReason  string
	Cause         error
}

func (err *Error) Error() string {
	if err == nil {
		return "VLM progress guard failure"
	}
	message := fmt.Sprintf(
		"VLM %s (operation=%s, elapsed=%s, progress_chars=%d",
		err.Kind,
		normalizeOperation(err.Operation),
		err.Elapsed.Round(time.Millisecond),
		err.ProgressChars,
	)
	if err.Limit > 0 {
		message += fmt.Sprintf(", limit=%s", err.Limit)
	}
	if err.FinishReason != "" {
		message += fmt.Sprintf(", finish_reason=%s", err.FinishReason)
	}
	message += ")"
	if err.Cause != nil {
		message += ": " + err.Cause.Error()
	}
	return message
}

func (err *Error) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.Cause
}

func (err *Error) CountsAsProviderFailure() bool {
	if err == nil {
		return false
	}
	return err.Kind == FailureFirstTokenTimeout ||
		err.Kind == FailureIdleTimeout ||
		err.Kind == FailureStreamTruncated
}

func ProviderCircuitFailure(err error) (failure bool, classified bool) {
	var guardErr *Error
	if !errors.As(err, &guardErr) {
		return false, false
	}
	return guardErr.CountsAsProviderFailure(), true
}

func FailureKindOf(err error) (FailureKind, bool) {
	var guardErr *Error
	if !errors.As(err, &guardErr) {
		return "", false
	}
	return guardErr.Kind, true
}
