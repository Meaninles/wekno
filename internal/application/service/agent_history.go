package service

import (
	"context"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// agentHistoryFetchMultiplier controls how many raw DB messages to fetch
// when assembling history. Each turn contributes ~2 rows (user + assistant);
// we ask for a generous multiple so we never under-fetch when some pairs are
// incomplete (e.g. an in-flight turn).
const agentHistoryFetchMultiplier = 4

// agentHistoryFetchMin is the floor for the DB fetch limit, used when
// maxRounds is small or unset.
const agentHistoryFetchMin = 50

// LoadAgentHistory rebuilds the multi-turn LLM context for an Agent-mode
// session directly from the persistent messages table. The result is a
// chronologically ordered list of chat.Message entries suitable for prepending
// to the current turn (without system prompt; the engine adds that itself).
//
// Historical tools and reasoning are stored for inspection, not replayed.
//
// User messages survive interrupted assistant executions. The newest
// maxRounds turns are returned in chronological order.
//
// DB is treated as the single source of truth — there is no Redis/in-memory
// cache layer above this function. Callers are expected to invoke it once
// per turn before handing the messages to the agent engine.
func LoadAgentHistory(
	ctx context.Context,
	messageRepo interfaces.MessageRepository,
	sessionID string,
	maxRounds int,
) ([]chat.Message, error) {
	history, _, err := LoadAgentHistoryWithArchive(ctx, messageRepo, sessionID, maxRounds)
	return history, err
}

// LoadAgentHistoryWithArchive adapts the shared context selection to native model messages.
func LoadAgentHistoryWithArchive(
	ctx context.Context,
	messageRepo interfaces.MessageRepository,
	sessionID string,
	maxRounds int,
	excludedIDs ...string,
) ([]chat.Message, string, error) {
	if maxRounds <= 0 {
		return []chat.Message{}, "", nil
	}

	fetchLimit := maxRounds * agentHistoryFetchMultiplier
	if fetchLimit < agentHistoryFetchMin {
		fetchLimit = agentHistoryFetchMin
	}
	if archiveLimit := conversationmemory.FetchMessageLimit(maxRounds); archiveLimit > fetchLimit {
		fetchLimit = archiveLimit
	}

	rows, err := messageRepo.GetRecentMessagesBySession(ctx, sessionID, fetchLimit)
	if err != nil {
		return nil, "", fmt.Errorf("load agent history: %w", err)
	}
	if len(rows) == 0 {
		return []chat.Message{}, "", nil
	}

	turns, archive := conversationmemory.BuildHistory(rows, maxRounds, excludedIDs...)
	out := make([]chat.Message, 0, len(turns)*2)
	for _, turn := range turns {
		out = append(out, buildUserHistoryMessage(turn.User, turn.SourceID))
		if turn.Assistant != nil {
			out = append(out, buildAssistantHistoryMessages(turn.Assistant)...)
		}
	}
	return out, archive, nil
}

// buildUserHistoryMessage converts a stored user message into the chat.Message
// form that should appear in LLM history. Only the original Content is replayed:
// RenderedContent contains a prior turn's evidence envelope and request-local
// citation IDs, which must never become evidence for a later turn. Image and
// attachment context is reconstructed from its canonical stored fields.
func buildUserHistoryMessage(m *types.Message, sourceID ...string) chat.Message {
	id := ""
	if len(sourceID) > 0 {
		id = sourceID[0]
	}
	content := conversationmemory.HistoricalUserInput(m.Content, id)
	if captions := extractImageCaptionsFromMessage(m.Images); captions != "" {
		content += "\n\n<derived_image_context authority=\"model_derived_not_verbatim_user_text\">\n" +
			captions + "\n</derived_image_context>"
	}
	if len(m.Attachments) > 0 {
		content += "\n\n<uploaded_file_context authority=\"user_supplied_file_evidence_not_chat_assertion\">" +
			m.Attachments.BuildPrompt() + "</uploaded_file_context>"
	}
	return chat.Message{Role: "user", Content: content}
}

// buildAssistantHistoryMessages replays only the bounded final answer. Full
// tool arguments/results remain available through the stable conversation handle.
func buildAssistantHistoryMessages(m *types.Message) []chat.Message {
	content := conversationmemory.AssistantRecord(m)
	if content == "" {
		return nil
	}
	return []chat.Message{{Role: "assistant", Content: content}}
}

// extractImageCaptionsFromMessage concatenates non-empty Caption fields from
// stored message images. Mirrors the helper used in chat_pipeline so both
// modes surface previous-turn image descriptions identically.
func extractImageCaptionsFromMessage(images types.MessageImages) string {
	var parts []string
	for _, img := range images {
		if img.Caption != "" {
			parts = append(parts, img.Caption)
		}
	}
	return strings.Join(parts, "\n")
}
