package session

import (
	"context"
	"net/http"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
)

const (
	ChatQueueRejectPoolFull         = "CHAT_QUEUE_FULL"
	ChatQueueRejectUserLimit        = "CHAT_QUEUE_USER_LIMIT"
	ChatQueueRejectUnavailable      = "CHAT_QUEUE_UNAVAILABLE"
	ChatQueueRejectModelUnavailable = "CHAT_QUEUE_MODEL_UNAVAILABLE"
)

// ChatQueueAdmissionRequest is the native-to-custom registration boundary.
// The custom queue module owns all queue logic; the session handler only
// supplies already-authorized request context and renders the result.
type ChatQueueAdmissionRequest struct {
	// Surface selects an independent conversation-capacity domain. Empty is
	// the normal Web conversation queue; IM integrations pass "im".
	Surface          string
	TenantID         uint64
	PrincipalID      string
	RequestID        string
	SessionID        string
	SummaryModelID   string
	AgentModelID     string
	KnowledgeBaseIDs []string
	KnowledgeIDs     []string
}

// ChatQueueSnapshot is sent through SSE while an accepted conversation waits.
type ChatQueueSnapshot struct {
	Surface        string `json:"surface"`
	State          string `json:"state"`
	ModelID        string `json:"model_id"`
	ResourcePoolID string `json:"resource_pool_id"`
	Position       int64  `json:"position"`
	Waiting        int64  `json:"waiting"`
	Active         int64  `json:"active"`
	MaxConcurrent  int    `json:"max_concurrent"`
	MaxWaiting     int    `json:"max_waiting"`
	QueuedAtUnix   int64  `json:"queued_at_unix,omitempty"`
}

// ChatQueueRejection is returned before user/assistant messages are persisted.
// This lets clients preserve the composer draft without creating ghost turns.
type ChatQueueRejection struct {
	Surface        string `json:"surface,omitempty"`
	Code           string `json:"code"`
	Message        string `json:"message"`
	ModelID        string `json:"model_id,omitempty"`
	ResourcePoolID string `json:"resource_pool_id,omitempty"`
	Waiting        int64  `json:"waiting,omitempty"`
	Active         int64  `json:"active,omitempty"`
	MaxConcurrent  int    `json:"max_concurrent,omitempty"`
	MaxWaiting     int    `json:"max_waiting,omitempty"`
	UserWaiting    int64  `json:"user_waiting,omitempty"`
	UserMaxWaiting int    `json:"user_max_waiting,omitempty"`
}

// ChatQueueTicket represents either an immediately active conversation or an
// accepted FIFO waiter. Release and Cancel must be idempotent.
type ChatQueueTicket interface {
	Queued() bool
	Wait(context.Context, func(ChatQueueSnapshot)) error
	Release(context.Context)
	Cancel(context.Context)
}

type chatQueueAdmissionHook func(
	context.Context,
	ChatQueueAdmissionRequest,
) (ChatQueueTicket, *ChatQueueRejection, error)

var chatQueueHookRegistry struct {
	sync.RWMutex
	hook chatQueueAdmissionHook
}

// RegisterChatQueueAdmissionHook installs the custom conversation admission
// module. Passing nil is supported by tests and restores native behavior.
func RegisterChatQueueAdmissionHook(hook chatQueueAdmissionHook) {
	chatQueueHookRegistry.Lock()
	chatQueueHookRegistry.hook = hook
	chatQueueHookRegistry.Unlock()
}

func reserveChatQueue(
	ctx context.Context,
	request ChatQueueAdmissionRequest,
) (ChatQueueTicket, *ChatQueueRejection, error) {
	chatQueueHookRegistry.RLock()
	hook := chatQueueHookRegistry.hook
	chatQueueHookRegistry.RUnlock()
	if hook == nil {
		return nil, nil, nil
	}
	return hook(ctx, request)
}

func writeChatQueueRejection(c *gin.Context, rejection *ChatQueueRejection, err error) {
	status := http.StatusTooManyRequests
	if rejection == nil {
		rejection = &ChatQueueRejection{
			Code:    ChatQueueRejectUnavailable,
			Message: "聊天排队服务暂时不可用，请稍后重试",
		}
		status = http.StatusServiceUnavailable
	}
	if rejection.Code == ChatQueueRejectUnavailable ||
		rejection.Code == ChatQueueRejectModelUnavailable {
		status = http.StatusServiceUnavailable
	}
	if strings.TrimSpace(rejection.Message) == "" {
		rejection.Message = "当前请求暂时无法进入聊天队列"
	}
	payload := gin.H{
		"success": false,
		"code":    rejection.Code,
		"error":   rejection.Message,
		"message": rejection.Message,
		"data":    rejection,
	}
	if err != nil {
		payload["retryable"] = true
	}
	c.AbortWithStatusJSON(status, payload)
}
