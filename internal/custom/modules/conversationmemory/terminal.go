package conversationmemory

import "strings"

const (
	TerminalAnswerOpen  = "<weknora_final_response>"
	TerminalAnswerClose = "</weknora_final_response>"
)

// ProjectTerminalAnswer removes the model-only final-answer envelope. If a
// provider ignores the envelope contract, its complete plain-text response is
// returned unchanged apart from surrounding whitespace, so the production
// path fails open instead of dropping a valid answer.
func ProjectTerminalAnswer(raw string) string {
	if start := strings.Index(raw, TerminalAnswerOpen); start >= 0 {
		content := raw[start+len(TerminalAnswerOpen):]
		if end := strings.Index(content, TerminalAnswerClose); end >= 0 {
			content = content[:end]
		}
		return strings.TrimSpace(content)
	}
	return strings.TrimSpace(raw)
}

// TerminalAnswerProjector incrementally exposes only content inside the
// terminal envelope. Text before or after the envelope stays private. Until an
// opening marker is observed, content is buffered; Flush falls back to the
// provider's full plain-text response when the marker is absent. This adds no
// model call and keeps SSE aggregation identical to the persisted candidate.
type TerminalAnswerProjector struct {
	raw                strings.Builder
	beforeOpen         string
	pending            string
	trailingWhitespace string
	answer             strings.Builder
	opened             bool
	closed             bool
	finished           bool
	started            bool
}

func NewTerminalAnswerProjector() *TerminalAnswerProjector {
	return &TerminalAnswerProjector{}
}

func (p *TerminalAnswerProjector) Feed(chunk string) string {
	if p == nil || p.finished || chunk == "" {
		return ""
	}
	p.raw.WriteString(chunk)
	if p.closed {
		return ""
	}
	if !p.opened {
		p.beforeOpen += chunk
		start := strings.Index(p.beforeOpen, TerminalAnswerOpen)
		if start < 0 {
			return ""
		}
		p.opened = true
		p.pending = p.beforeOpen[start+len(TerminalAnswerOpen):]
		p.beforeOpen = ""
	} else {
		p.pending += chunk
	}
	return p.drain(false)
}

func (p *TerminalAnswerProjector) Flush() string {
	if p == nil || p.finished {
		return ""
	}
	p.finished = true
	if !p.opened {
		fallback := strings.TrimSpace(p.raw.String())
		p.answer.WriteString(fallback)
		return fallback
	}
	out := p.drain(true)
	// Surrounding whitespace is not part of the canonical answer. Inter-token
	// whitespace was already emitted once a later non-space character proved it
	// was internal rather than trailing.
	p.trailingWhitespace = ""
	return out
}

func (p *TerminalAnswerProjector) Answer() string {
	if p == nil {
		return ""
	}
	return p.answer.String()
}

func (p *TerminalAnswerProjector) drain(flush bool) string {
	if p.closed {
		return ""
	}
	content := ""
	if end := strings.Index(p.pending, TerminalAnswerClose); end >= 0 {
		content = p.pending[:end]
		p.pending = ""
		p.closed = true
	} else if flush {
		content = p.pending
		p.pending = ""
	} else {
		hold := terminalMarkerSuffixLength(p.pending, TerminalAnswerClose)
		content = p.pending[:len(p.pending)-hold]
		p.pending = p.pending[len(p.pending)-hold:]
	}
	return p.emitCanonical(content)
}

func (p *TerminalAnswerProjector) emitCanonical(content string) string {
	if content == "" {
		return ""
	}
	combined := p.trailingWhitespace + content
	p.trailingWhitespace = ""
	if !p.started {
		combined = strings.TrimLeft(combined, " \t\r\n")
		if combined == "" {
			return ""
		}
		p.started = true
	}
	last := len(combined) - 1
	for last >= 0 {
		switch combined[last] {
		case ' ', '\t', '\r', '\n':
			last--
		default:
			goto found
		}
	}
found:
	if last < 0 {
		p.trailingWhitespace = combined
		return ""
	}
	out := combined[:last+1]
	p.trailingWhitespace = combined[last+1:]
	p.answer.WriteString(out)
	return out
}

func terminalMarkerSuffixLength(value, marker string) int {
	limit := len(marker) - 1
	if len(value) < limit {
		limit = len(value)
	}
	for size := limit; size > 0; size-- {
		if strings.HasSuffix(value, marker[:size]) {
			return size
		}
	}
	return 0
}
