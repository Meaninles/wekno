package conversationmemory

import (
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
	if !strings.Contains(archive, "terminal-field-21") {
		t.Fatal("older message tail lost")
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
