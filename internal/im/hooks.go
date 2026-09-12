package im

import (
	"context"
	"time"
)

// AnswerDelivery describes an answer that has been handed to an IM adapter's
// final delivery method successfully. Hooks receive only stable identifiers so
// they can schedule follow-up work without retaining the QA worker's objects.
type AnswerDelivery struct {
	ChannelID          string
	TenantID           uint64
	Platform           Platform
	Mode               string
	UserID             string
	ChatID             string
	ChatType           ChatType
	SessionID          string
	RequestID          string
	UserMessageID      string
	AssistantMessageID string
	DeliveredAt        time.Time
}

// InteractiveEvent is a platform-neutral interactive IM event. At present
// only WeCom template-card events are routed through this hook.
type InteractiveEvent struct {
	ChannelID string
	TenantID  uint64
	Platform  Platform
	Mode      string

	EventType string
	EventKey  string
	TaskID    string
	RequestID string

	UserID   string
	ChatID   string
	ChatType ChatType
}

// FeedbackHook is the small native registration point used by custom
// feedback modules. Incoming hooks must stay lightweight; answer-delivery
// hooks are invoked asynchronously by the IM service.
type FeedbackHook interface {
	OnIMIncomingMessage(ctx context.Context, channelID string, channel *IMChannel, msg *IncomingMessage)
	OnIMAnswerDelivered(ctx context.Context, delivery AnswerDelivery)
	OnIMInteractiveEvent(ctx context.Context, event InteractiveEvent)
}

// InteractiveEventHandler receives events parsed by an adapter. The handler
// is installed by the IM service so the adapter remains independent of custom
// modules.
type InteractiveEventHandler func(ctx context.Context, event *InteractiveEvent) error

// InteractiveEventRegistrar is implemented by adapters that can emit
// interactive events, such as WeCom WebSocket template-card callbacks.
type InteractiveEventRegistrar interface {
	SetInteractiveEventHandler(handler InteractiveEventHandler)
}

// TemplateCardSender is implemented by adapters that support sending and
// updating template cards. cardBody is the JSON representation of the card
// object itself; the adapter adds the platform-specific message envelope.
type TemplateCardSender interface {
	SendTemplateCard(ctx context.Context, chatID string, cardBody []byte) error
	UpdateTemplateCard(ctx context.Context, requestID string, cardBody []byte) error
}
