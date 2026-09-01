package conversationmemory

import (
	"strings"
	"testing"
)

func TestProjectTerminalAnswer(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{name: "enveloped", raw: "private planning\n<weknora_final_response>\nVisible answer.\n</weknora_final_response>ignored", want: "Visible answer."},
		{name: "plain fallback", raw: "  Provider plain answer.  ", want: "Provider plain answer."},
		{name: "missing close", raw: "<weknora_final_response>partial but usable", want: "partial but usable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ProjectTerminalAnswer(tt.raw); got != tt.want {
				t.Fatalf("ProjectTerminalAnswer() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTerminalAnswerIntegrityReasonRejectsOnlyProtocolLevelCorruption(t *testing.T) {
	tests := []struct {
		name   string
		answer string
		want   string
	}{
		{name: "empty", answer: "  ", want: "empty_terminal_answer"},
		{name: "protocol residue", answer: "Useful prefix </weknora_final_placeholder>", want: "terminal_protocol_residue"},
		{name: "malformed source", answer: `Supported claim <src id 'S1' />`, want: "malformed_source_handle"},
		{name: "repeated short unit", answer: strings.Repeat("的。", 80), want: "degenerate_repetition"},
		{name: "repeated phrase", answer: "Now read the source " + strings.Repeat("to get the full text ", 3), want: "degenerate_repetition"},
		{name: "canonical citation", answer: `A concise supported claim.<src id="S12" />`, want: ""},
		{name: "normal repetition", answer: "Retry once, retry twice, then report the final outcome with evidence.", want: ""},
		{name: "markdown table", answer: "| Field | Value |\n| --- | --- |\n| owner | pending |\n| date | pending |\n| source | user |", want: ""},
		{name: "code with repeated syntax", answer: "```go\nif ready { run() }\nif pending { wait() }\nif failed { report() }\n```", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := TerminalAnswerIntegrityReason(tt.answer); got != tt.want {
				t.Fatalf("TerminalAnswerIntegrityReason() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTerminalAnswerProjectorHandlesSplitMarkersAndHidesSelfTalk(t *testing.T) {
	p := NewTerminalAnswerProjector()
	chunks := []string{
		"I should inspect the ledger first.\n<weknora_",
		"final_response>\nCurrent facts:\n- Owner: Noor",
		"\n- Status: pending\n</weknora_final_",
		"response>do not show this",
	}
	var streamed string
	for _, chunk := range chunks {
		streamed += p.Feed(chunk)
	}
	streamed += p.Flush()
	want := "Current facts:\n- Owner: Noor\n- Status: pending"
	if streamed != want {
		t.Fatalf("streamed = %q, want %q", streamed, want)
	}
	if p.Answer() != streamed {
		t.Fatalf("persisted = %q, streamed = %q", p.Answer(), streamed)
	}
}

func TestTerminalAnswerProjectorFailsOpenWithoutEnvelope(t *testing.T) {
	p := NewTerminalAnswerProjector()
	if got := p.Feed("  ordinary "); got != "" {
		t.Fatalf("Feed() leaked pre-envelope content: %q", got)
	}
	if got := p.Feed("answer  "); got != "" {
		t.Fatalf("Feed() leaked pre-envelope content: %q", got)
	}
	if got := p.Flush(); got != "ordinary answer" {
		t.Fatalf("Flush() = %q, want plain fallback", got)
	}
	if p.Answer() != "ordinary answer" {
		t.Fatalf("Answer() = %q", p.Answer())
	}
}
