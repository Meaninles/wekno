package conversationmemory

import (
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
	"testing"
	"time"
)

func TestHistoryPreservesInterruptedFactsAndStableIdentity(t *testing.T) {
	now := time.Now()
	rows := []*types.Message{{ID: "first", RequestID: "r1", Role: "user", Content: "original fact", CreatedAt: now}, {ID: "failed", RequestID: "r1", Role: "assistant", Content: "incomplete inference", IsCompleted: false}, {ID: "next", RequestID: "r2", Role: "user", Content: "correction", CreatedAt: now.Add(time.Second)}}
	recent, archive := BuildHistory(rows, 1)
	if len(recent) != 1 || recent[0].SourceID != "user_message_next" || !strings.Contains(archive, "original fact") || strings.Contains(archive, "incomplete inference") {
		t.Fatal(recent, archive)
	}
	recent2, _ := BuildHistory(rows[1:], 1)
	if recent2[0].SourceID != recent[0].SourceID {
		t.Fatal("source identity shifted")
	}
	recent, _ = BuildHistory(rows, 2, "next")
	if len(recent) != 1 || recent[0].Assistant != nil {
		t.Fatal("failed assistant or current user included")
	}
}
func TestHistoryWholeMessagesNotPrefixes(t *testing.T) {
	var rows []*types.Message
	for i := 0; i < 48; i++ {
		rows = append(rows, &types.Message{ID: fmt.Sprint(i), RequestID: fmt.Sprint(i), Role: "user", Content: strings.Repeat("x", 650) + fmt.Sprintf(" terminal-field-%d", i), CreatedAt: time.Unix(int64(i), 0)})
	}
	recent, archive := BuildHistory(rows, 5)
	for _, line := range strings.Split(archive, "\n") {
		var entry struct {
			SourceID     string `json:"source_id"`
			Text         string `json:"text"`
			ReadRequired bool   `json:"read_required"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.ReadRequired {
			if entry.Text != "" {
				t.Fatal("partial text presented as source")
			}
			continue
		}
		var index int
		if _, err := fmt.Sscanf(entry.SourceID, "user_message_%d", &index); err != nil {
			t.Fatal(err)
		}
		if entry.Text != rows[index].Content {
			t.Fatal("whole original message not preserved", entry.SourceID)
		}
	}
	if strings.Contains(archive, "terminal-field-47") || len(recent) != 5 {
		t.Fatal("recent users duplicated")
	}
	rows[0].Content = strings.Repeat("x", 70000)
	_, archive = BuildHistory(rows, 5)
	if !strings.Contains(archive, `"read_required":true`) || !strings.Contains(archive, `"source_id":"user_message_0"`) {
		t.Fatal("overflow lacks original-source read handle")
	}
}

func TestRepairHistoryBudgetKeepsFifteenUserTurnsAndReadableAnswers(t *testing.T) {
	var rows []*types.Message
	for i := 0; i < 15; i++ {
		rid := fmt.Sprint(i)
		rows = append(rows, &types.Message{ID: "u" + rid, RequestID: rid, Role: "user", Content: fmt.Sprintf("用户更正字段 %d，保留原文条件。", i), CreatedAt: time.Unix(int64(i), 0)}, &types.Message{ID: "a" + rid, RequestID: rid, Role: "assistant", Content: strings.Repeat("long generated answer ", 3000), IsCompleted: true})
	}
	turns, archive := BuildHistoryWithBudget(rows, 15, 4000)
	if len(turns) != 15 || archive != "" {
		t.Fatal("continuous history lost", len(turns))
	}
	tokens := 0
	for i, turn := range turns {
		if turn.User.Content != rows[i*2].Content {
			t.Fatal("user correction replaced", i)
		}
		if !strings.Contains(turn.Assistant.Content, AssistantSourceID(rows[i*2+1].ID)) {
			t.Fatal("oversized answer has no read handle", i)
		}
		if !strings.Contains(rows[i*2+1].Content, "long generated") {
			t.Fatal("stored answer changed")
		}
		tokens += EstimateTokens(turn.User.Content) + EstimateTokens(turn.Assistant.Content)
	}
	if tokens > 4000 {
		t.Fatal("history exceeded budget", tokens)
	}
}
