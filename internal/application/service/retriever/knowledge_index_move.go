package retriever

import (
	"errors"
)

// knowledgeIndexMoveError records whether a failed move is known to have been
// restored to its source side. Callers may safely restore their database claim
// only when rollbackComplete is true; otherwise they must leave the lifecycle
// non-terminal for deletion/recovery cleanup.
type knowledgeIndexMoveError struct {
	cause            error
	rollbackComplete bool
}

func (e *knowledgeIndexMoveError) Error() string { return e.cause.Error() }
func (e *knowledgeIndexMoveError) Unwrap() error { return e.cause }

func (e *knowledgeIndexMoveError) RollbackComplete() bool {
	return e != nil && e.rollbackComplete
}

func newKnowledgeIndexMoveError(cause error, rollbackComplete bool) error {
	if cause == nil {
		return nil
	}
	return &knowledgeIndexMoveError{cause: cause, rollbackComplete: rollbackComplete}
}

// KnowledgeIndexMoveRollbackComplete reports whether a failed scoped index
// move explicitly guarantees that all mutations were restored to the source.
// Unknown errors are treated as incomplete (fail closed).
func KnowledgeIndexMoveRollbackComplete(err error) bool {
	if err == nil {
		return true
	}
	var moveErr interface{ RollbackComplete() bool }
	return errors.As(err, &moveErr) && moveErr.RollbackComplete()
}
