package service

import (
	"context"
	"fmt"
	"time"

	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// Worker unit tests construct the application service without the custom
// bootstrap. Install a test-only resolver so those tests still exercise their
// model stubs while production remains fail-closed when the control plane is
// absent. An empty or unloadable derivative model is classified as a durable
// wait, matching derivativecontrol.Service.ResolveChatModel.
func init() {
	RegisterDerivativeChatResolver(func(
		ctx context.Context,
		modelService interfaces.ModelService,
		modelID string,
	) (chat.Chat, error) {
		if modelID == "" {
			return nil, &testDerivativeResolutionError{
				reason: "no derivative model is configured",
			}
		}
		if modelService == nil {
			return nil, &testDerivativeResolutionError{
				reason: "derivative model service is unavailable",
			}
		}
		instance, err := modelService.GetChatModel(ctx, modelID)
		if err != nil {
			return nil, &testDerivativeResolutionError{
				reason: "published derivative model is unavailable",
				cause:  err,
			}
		}
		return instance, nil
	})
}

type testDerivativeResolutionError struct {
	reason string
	cause  error
}

func (e *testDerivativeResolutionError) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %v", e.reason, e.cause)
	}
	return e.reason
}

func (e *testDerivativeResolutionError) Unwrap() error                  { return e.cause }
func (e *testDerivativeResolutionError) ModelWorkDeferred() bool        { return true }
func (e *testDerivativeResolutionError) ModelRetryAfter() time.Duration { return time.Minute }
