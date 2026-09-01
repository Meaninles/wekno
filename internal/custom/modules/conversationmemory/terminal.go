package conversationmemory

import (
	"regexp"
	"strings"
	"unicode"
)

const (
	TerminalAnswerOpen  = "<weknora_final_response>"
	TerminalAnswerClose = "</weknora_final_response>"
)

var terminalCitationPattern = regexp.MustCompile(`<src\s+id="S[1-9][0-9]*"\s*/>`)

// TerminalIntegrityRetryDirective asks for a fresh terminal rendering using
// only already available context/evidence and explicitly disables tools. It is
// a production reliability instruction, not an Eval repair or semantic judge.
func TerminalIntegrityRetryDirective() string {
	return "The previous terminal response was not shown because it contained a transport-level output-integrity failure. Produce one fresh, self-contained final answer to the same current user request using only the existing conversation and current-turn tool evidence. Do not call any tool or describe this retry. Do not repeat protocol markers, planning, or self-talk. Return the complete user-visible answer inside exactly one <weknora_final_response>...</weknora_final_response> envelope."
}

// TerminalIntegrityFallback is used only after the single integrity retry also
// fails. It exposes no protocol, validation, scoring, or repair details.
func TerminalIntegrityFallback(language string) string {
	normalized := strings.ToLower(strings.TrimSpace(language))
	if strings.HasPrefix(normalized, "zh") || strings.HasPrefix(normalized, "chinese") {
		return "本次回答未能可靠生成，请重试。"
	}
	return "The response could not be generated reliably. Please try again."
}

// TerminalAnswerIntegrityReason returns an empty string for a usable terminal
// answer. It deliberately checks only transport/protocol integrity signals,
// never business facts, reference answers, Eval rubrics, or domain wording.
// This makes it safe to share across production agents without turning it into
// a hidden answer scorer.
func TerminalAnswerIntegrityReason(answer string) string {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return "empty_terminal_answer"
	}

	lower := strings.ToLower(trimmed)
	for _, marker := range []string{
		"<weknora_",
		"</weknora_",
		"weknora_final_placeholder",
	} {
		if strings.Contains(lower, marker) {
			return "terminal_protocol_residue"
		}
	}

	// A source handle is model/runtime protocol, not ordinary markup. If the
	// answer contains a handle prefix, every occurrence must be canonical.
	withoutValidCitations := terminalCitationPattern.ReplaceAllString(trimmed, "")
	if strings.Contains(strings.ToLower(withoutValidCitations), "<src") {
		return "malformed_source_handle"
	}

	compact := make([]rune, 0, len([]rune(trimmed)))
	for _, r := range []rune(trimmed) {
		if !unicode.IsSpace(r) {
			compact = append(compact, r)
		}
	}
	if hasDominantConsecutiveRepeat(compact) {
		return "degenerate_repetition"
	}
	return ""
}

// hasDominantConsecutiveRepeat catches provider degeneration such as a short
// phrase repeated for most of the response. Requiring the repeated span to
// cover at least half the answer avoids treating normal headings, tables,
// refrains, or intentionally repeated examples as corruption.
func hasDominantConsecutiveRepeat(value []rune) bool {
	if len(value) < 24 {
		return false
	}
	maxUnit := 64
	if len(value)/3 < maxUnit {
		maxUnit = len(value) / 3
	}
	for unit := 1; unit <= maxUnit; unit++ {
		requiredRepeats := 8
		if unit >= 10 {
			requiredRepeats = 3
		} else if unit >= 4 {
			requiredRepeats = 4
		}
		for start := 0; start+unit*requiredRepeats <= len(value); start++ {
			if unit >= 4 && distinctAlphaNumeric(value[start:start+unit]) < 2 {
				continue
			}
			repeats := 1
			for start+(repeats+1)*unit <= len(value) &&
				equalRunes(
					value[start:start+unit],
					value[start+repeats*unit:start+(repeats+1)*unit],
				) {
				repeats++
			}
			span := repeats * unit
			if repeats >= requiredRepeats && span*2 >= len(value) {
				return true
			}
		}
	}
	return false
}

func distinctAlphaNumeric(value []rune) int {
	seen := make(map[rune]struct{})
	for _, r := range value {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			seen[unicode.ToLower(r)] = struct{}{}
		}
	}
	return len(seen)
}

func equalRunes(left, right []rune) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

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
