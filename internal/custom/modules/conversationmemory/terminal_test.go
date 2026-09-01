package conversationmemory

import "testing"

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
