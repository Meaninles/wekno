package generalagent

import (
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

// answerStream projects each completed text segment once. Citation-bearing
// Markdown waits for a paragraph boundary; ordinary text streams immediately.
// The saved answer is assembled from exactly the segments sent to the client.
type answerStream struct {
	pending string
	output  strings.Builder
	refs    func() []*types.SearchResult
	emit    func(string)
}

func (s *answerStream) write(text string) {
	if text == "" {
		return
	}
	s.output.WriteString(text)
	s.emit(text)
}
func (s *answerStream) push(text string, done bool) {
	s.pending += text
	refs := s.refs()
	if len(refs) == 0 && !strings.ContainsAny(s.pending, "<`~") {
		s.write(s.pending)
		s.pending = ""
		return
	}
	// Keep fenced code intact so literal citation examples are not interpreted.
	if strings.Count(s.pending, "```")%2 == 0 && strings.Count(s.pending, "~~~")%2 == 0 {
		if boundary := strings.LastIndex(s.pending, "\n\n"); boundary >= 0 {
			segment, _, _ := sourcerefs.FilterAnswerCitations(s.pending[:boundary+2], refs)
			s.write(segment)
			s.pending = s.pending[boundary+2:]
		}
	}
	if done {
		segment, _, _ := sourcerefs.FilterAnswerCitations(s.pending, refs)
		s.write(segment)
		s.pending = ""
	}
}
