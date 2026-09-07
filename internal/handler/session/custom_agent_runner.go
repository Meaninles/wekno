package session

import (
	"context"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

func RunAgentQA(ctx context.Context, service interfaces.SessionService, req *types.QARequest, bus *event.EventBus) error {
	return service.AgentQA(ctx, req, bus)
}
