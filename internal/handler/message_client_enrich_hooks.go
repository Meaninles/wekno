package handler

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// MessageClientEnricher lets custom modules append non-native presentation
// fields to messages before they are returned to browser clients.
type MessageClientEnricher func(context.Context, []*types.Message)

var messageClientEnrichers struct {
	sync.RWMutex
	items []MessageClientEnricher
}

// RegisterMessageClientEnricher registers a custom message response enricher.
func RegisterMessageClientEnricher(enricher MessageClientEnricher) {
	if enricher == nil {
		return
	}
	messageClientEnrichers.Lock()
	defer messageClientEnrichers.Unlock()
	messageClientEnrichers.items = append(messageClientEnrichers.items, enricher)
}

func enrichMessagesForClient(ctx context.Context, messages []*types.Message) {
	messageClientEnrichers.RLock()
	enrichers := append([]MessageClientEnricher(nil), messageClientEnrichers.items...)
	messageClientEnrichers.RUnlock()
	for _, enricher := range enrichers {
		func(fn MessageClientEnricher) {
			defer func() {
				if r := recover(); r != nil {
					logger.Warnf(ctx, "message client enricher panicked: %v", r)
				}
			}()
			fn(ctx, messages)
		}(enricher)
	}
}
