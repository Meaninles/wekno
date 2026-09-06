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

func AssistantSourceID(id string) string { return "assistant_message_" + id }

// HistoryTokenBudget is separate from the model's total context: current input,
// active tool exchanges and validated evidence have their own allocations.
const HistoryTokenBudget = 12000

// EstimateTokens is a conservative mixed-script estimate, not provider usage.
func EstimateTokens(value string) int {
	ascii, other := 0, 0
	for _, r := range value {
		if r < 128 {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func archivedMessage(m *types.Message, id string) *types.Message {
	copy := *m
	data, _ := json.Marshal(map[string]any{"source_id": id, "role": m.Role, "read_required": true})
	copy.Content = string(data)
	copy.AgentSteps = nil
	copy.Images = nil
	copy.Attachments = nil
	return &copy
}

// BuildHistory is the sole selection policy for chat, native agents and SDK agents.
// Recent turns and older user sources are disjoint. Older messages are kept whole:
// a budget overflow is an explicit read handle, never a truncated quotation.
func BuildHistory(rows []*types.Message, recentRounds int, excludedIDs ...string) ([]Turn, string) {
	return BuildHistoryWithBudget(rows, recentRounds, HistoryTokenBudget, excludedIDs...)
}

func BuildHistoryWithBudget(rows []*types.Message, recentRounds, tokenBudget int, excludedIDs ...string) ([]Turn, string) {
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
	if tokenBudget <= 0 {
		return nil, ""
	}
	// Start with read handles, whose real serialized cost is part of the budget.
	originals := append([]Turn(nil), turns...)
	archived := map[*types.Message]bool{}
	for i := range turns {
		turns[i].User = archivedMessage(turns[i].User, turns[i].SourceID)
		archived[turns[i].User] = true
		if turns[i].Assistant != nil {
			turns[i].Assistant = archivedMessage(turns[i].Assistant, AssistantSourceID(turns[i].Assistant.ID))
		}
	}
	omitted := 0
	renderLedger := func() string {
		var lines []string
		if omitted > 0 {
			data, _ := json.Marshal(map[string]any{"history_archive": true, "omitted_turns": omitted, "read_tool": "read_conversation", "offset": 0})
			lines = append(lines, string(data))
		}
		for _, turn := range turns[:olderCount] {
			entry := map[string]any{"source_id": turn.SourceID, "created_at": turn.User.CreatedAt, "text": turn.User.Content}
			if archived[turn.User] {
				delete(entry, "text")
				entry["read_required"] = true
				entry["read_tool"] = "read_conversation"
			}
			if turn.Assistant != nil {
				entry["assistant_source_id"] = AssistantSourceID(turn.Assistant.ID)
			}
			data, _ := json.Marshal(entry)
			lines = append(lines, string(data))
		}
		return strings.Join(lines, "\n")
	}
	messageCosts := map[*types.Message]int{}
	messageCost := func(m *types.Message) int {
		if cost, ok := messageCosts[m]; ok {
			return cost
		}
		cost := historyMessageCost(m)
		messageCosts[m] = cost
		return cost
	}
	cost := func() int {
		total := EstimateTokens(renderLedger())
		for _, turn := range turns[olderCount:] {
			total += messageCost(turn.User) + messageCost(turn.Assistant) + 24
		}
		return total
	}
	// An arbitrarily long archive is one paginated read capability, not an
	// unbounded list of placeholder messages. Never silently truncate quotations.
	for len(turns) > 0 && cost() > tokenBudget {
		turns = turns[1:]
		originals = originals[1:]
		omitted++
		olderCount = max(0, olderCount-1)
	}
	if cost() > tokenBudget {
		return nil, ""
	}
	for i := len(turns) - 1; i >= 0; i-- {
		before := turns[i].User
		candidate := *originals[i].User
		candidate.AgentSteps = nil
		turns[i].User = &candidate
		if cost() > tokenBudget {
			turns[i].User = before
		}
	}
	for i := len(turns) - 1; i >= olderCount; i-- {
		if originals[i].Assistant == nil {
			continue
		}
		before := turns[i].Assistant
		candidate := *originals[i].Assistant
		candidate.AgentSteps = nil
		turns[i].Assistant = &candidate
		if cost() > tokenBudget {
			turns[i].Assistant = before
		}
	}
	return turns[olderCount:], renderLedger()
}

func historyMessageCost(m *types.Message) int {
	if m == nil {
		return 0
	}
	content := m.Content
	if m.Role == "assistant" {
		content = AssistantRecord(m)
	}
	data, _ := json.Marshal(map[string]any{"id": m.ID, "role": m.Role, "content": content, "images": m.Images, "attachments": m.Attachments})
	return EstimateTokens(string(data))
}
