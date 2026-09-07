package agentruntime

import "github.com/Tencent/WeKnora/internal/types"

type executionError struct {
	cause error
	steps []types.AgentStep
}

func (e *executionError) Error() string                          { return e.cause.Error() }
func (e *executionError) Unwrap() error                          { return e.cause }
func (e *executionError) AgentExecutionSteps() []types.AgentStep { return e.steps }
