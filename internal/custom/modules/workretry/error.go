package workretry

import (
	"errors"

	"github.com/Tencent/WeKnora/internal/custom/modules/modeladmission"
)

// budgetedModelError marks a delivery that reached the remote provider and
// failed there. The underlying provider/circuit error remains visible for
// Retry-After scheduling, but modeladmission no longer treats this delivery as
// retry-budget-free.
type budgetedModelError struct {
	cause error
}

func (e *budgetedModelError) Error() string {
	if e == nil || e.cause == nil {
		return "budgeted model attempt failed"
	}
	return e.cause.Error()
}

func (e *budgetedModelError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

// ConsumesModelRetryBudget is a structural marker read by modeladmission.
func (e *budgetedModelError) ConsumesModelRetryBudget() bool {
	return e != nil
}

// Consume wraps an error after the caller has proved that at least one real
// provider request was made during this business attempt.
func Consume(err error) error {
	if err == nil || ConsumesBudget(err) {
		return err
	}
	return &budgetedModelError{cause: err}
}

// ConsumeProviderFailure marks err only when it contains a real provider-call
// failure. A CircuitOpenError represents a pre-call rejection and therefore
// remains budget-free.
func ConsumeProviderFailure(err error) error {
	if err == nil || !modeladmission.IsProviderCallFailure(err) {
		return err
	}
	return Consume(err)
}

func ConsumesBudget(err error) bool {
	var marker interface {
		ConsumesModelRetryBudget() bool
	}
	return errors.As(err, &marker) && marker.ConsumesModelRetryBudget()
}
