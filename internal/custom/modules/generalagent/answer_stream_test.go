package generalagent

import (
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
	"testing"
)

func TestAnswerStreamChunkBoundariesAndSavedBytes(t *testing.T) {
	for _, body := range []string{"你好，第一段。\n\n下一段。", "正文<sup>999</sup>\n\n后续。", "```text\n<sup>999</sup>\n\n代码\n```\n\n正文。"} {
		expected, _, _ := sourcerefs.FilterAnswerCitations(body, nil)
		runes := []rune(body)
		for split := 0; split <= len(runes); split++ {
			var events []string
			s := answerStream{refs: func() []*types.SearchResult { return nil }, emit: func(v string) { events = append(events, v) }}
			s.push(string(runes[:split]), false)
			s.push(string(runes[split:]), true)
			if got := strings.Join(events, ""); got != expected || s.output.String() != got {
				t.Fatalf("split=%d got=%q want=%q", split, got, expected)
			}
		}
	}
}
