package conversationmemory

import (
	"encoding/json"
	"github.com/Tencent/WeKnora/internal/types"
	"sort"
	"strings"
)

// Turn keeps persisted user input independent of the outcome of the assistant.
type Turn struct {
	User, Assistant *types.Message
	SourceID        string
}

func MessageSourceID(id string) string { return "user_message_" + id }

// BuildHistory is the sole selection policy for chat, native agents and SDK agents.
// Recent turns and older user sources are disjoint. Older messages are kept whole:
// a budget overflow is an explicit read handle, never a truncated quotation.
func BuildHistory(rows []*types.Message, recentRounds int, excludedIDs ...string) ([]Turn, string) {
	if recentRounds <= 0 {
		return nil, ""
	}
	excluded := make(map[string]bool, len(excludedIDs))
	for _, id := range excludedIDs {
		if id != "" {
			excluded[id] = true
		}
	}
	users := make(map[string]*types.Message)
	assistants := make(map[string]*types.Message)
	for _, m := range rows {
		if m == nil || excluded[m.ID] {
			continue
		}
		if m.Role == "user" {
			users[m.ID] = m
		}
		if m.Role == "assistant" && m.IsCompleted {
			old := assistants[m.RequestID]
			if old == nil || m.CreatedAt.After(old.CreatedAt) {
				assistants[m.RequestID] = m
			}
		}
	}
	turns := make([]Turn, 0, len(users))
	for _, m := range users {
		turns = append(turns, Turn{User: m, Assistant: assistants[m.RequestID], SourceID: MessageSourceID(m.ID)})
	}
	sort.Slice(turns, func(i, j int) bool {
		if turns[i].User.CreatedAt.Equal(turns[j].User.CreatedAt) {
			return turns[i].User.ID < turns[j].User.ID
		}
		return turns[i].User.CreatedAt.Before(turns[j].User.CreatedAt)
	})
	olderCount := max(0, len(turns)-recentRounds)
	if olderCount == 0 {
		return turns, ""
	}
	entries := make([]string, olderCount)
	remaining := 64000 // user-text rune budget, with no per-message prefix clipping
	for i := olderCount - 1; i >= 0; i-- {
		turn := turns[i]
		entry := map[string]any{"source_id": turn.SourceID, "created_at": turn.User.CreatedAt, "text": turn.User.Content}
		size := len([]rune(turn.User.Content))
		if size > remaining {
			delete(entry, "text")
			entry["read_required"] = true
			entry["message_id"] = turn.User.ID
		} else {
			remaining -= size
		}
		encoded, _ := json.Marshal(entry)
		entries[i] = string(encoded)
	}
	return turns[olderCount:], strings.Join(entries, "\n")
}
