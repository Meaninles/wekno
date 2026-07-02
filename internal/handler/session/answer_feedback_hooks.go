package session

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

// AssistantRunSnapshot describes the stable request/runtime context for one
// completed assistant answer. Custom modules can subscribe to this without
// pushing training-data persistence into the native session flow.
type AssistantRunSnapshot struct {
	Session             *types.Session
	AssistantMessage    *types.Message
	UserMessageID       string
	UserQuery           string
	CustomAgent         *types.CustomAgent
	KnowledgeBaseIDs    []string
	KnowledgeIDs        []string
	SkillNames          []string
	SummaryModelID      string
	WebSearchEnabled    bool
	EnableMemory        bool
	EffectiveTenantID   uint64
	Channel             string
	MentionedItems      types.MentionedItems
	Images              []ImageAttachment
	Attachments         types.MessageAttachments
	RequestAgentID      string
	RequestAgentEnabled bool
}

// AssistantRunSnapshotHook receives completed-answer metadata. Hooks must be
// non-blocking and must not return user-facing errors.
type AssistantRunSnapshotHook func(context.Context, AssistantRunSnapshot)

var assistantRunSnapshotHooks struct {
	sync.RWMutex
	items []AssistantRunSnapshotHook
}

// RegisterAssistantRunSnapshotHook registers a custom completed-answer hook.
func RegisterAssistantRunSnapshotHook(hook AssistantRunSnapshotHook) {
	if hook == nil {
		return
	}
	assistantRunSnapshotHooks.Lock()
	defer assistantRunSnapshotHooks.Unlock()
	assistantRunSnapshotHooks.items = append(assistantRunSnapshotHooks.items, hook)
}

func emitAssistantRunSnapshot(ctx context.Context, snapshot AssistantRunSnapshot) {
	assistantRunSnapshotHooks.RLock()
	hooks := append([]AssistantRunSnapshotHook(nil), assistantRunSnapshotHooks.items...)
	assistantRunSnapshotHooks.RUnlock()
	for _, hook := range hooks {
		func(h AssistantRunSnapshotHook) {
			defer func() {
				if r := recover(); r != nil {
					logger.Warnf(ctx, "assistant run snapshot hook panicked: %v", r)
				}
			}()
			h(ctx, snapshot)
		}(hook)
	}
}
