package generalagent

import "testing"

func TestTerminalDeliveryBufferUsesResultAsAuthoritativeAnswer(t *testing.T) {
	var buffer terminalDeliveryBuffer
	if !buffer.append(StreamEvent{Type: "answer_delta", Content: "早期候选"}) {
		t.Fatal("answer_delta was not buffered")
	}
	if buffer.append(StreamEvent{Type: "progress", Content: "工具执行中"}) {
		t.Fatal("non-answer event was buffered")
	}

	answer := buffer.finalAnswer(&ChatResult{Answer: "完整最终回答"})
	if answer != "完整最终回答" {
		t.Fatalf("unexpected authoritative answer: %q", answer)
	}
}

func TestTerminalDeliveryBufferFallsBackToBufferedAnswer(t *testing.T) {
	var buffer terminalDeliveryBuffer
	buffer.append(StreamEvent{Type: "answer_delta", Content: "完整的"})
	buffer.append(StreamEvent{Type: "answer_delta", Content: "候选回答"})

	answer := buffer.finalAnswer(&ChatResult{})
	if answer != "完整的候选回答" {
		t.Fatalf("unexpected fallback answer: %q", answer)
	}
}

func TestTerminalDeliveryReplayEmitsOneImmutableAnswerAndDone(t *testing.T) {
	var buffer terminalDeliveryBuffer
	var events []StreamEvent
	buffer.replay("完整最终回答", "answer-1", func(evt StreamEvent) {
		events = append(events, evt)
	})

	if len(events) != 2 {
		t.Fatalf("unexpected replay event count: %d", len(events))
	}
	if events[0].ID != "answer-1" || events[0].Content != "完整最终回答" || events[0].Done {
		t.Fatalf("unexpected answer event: %#v", events[0])
	}
	if events[1].ID != "answer-1" || events[1].Content != "" || !events[1].Done {
		t.Fatalf("unexpected done event: %#v", events[1])
	}
}

func TestTerminalDeliveryReplayAllowsEmptyNormalOutput(t *testing.T) {
	var buffer terminalDeliveryBuffer
	called := false
	buffer.replay("", "answer-1", func(evt StreamEvent) {
		called = true
	})
	if called {
		t.Fatal("empty answer should not emit a synthetic event")
	}
}
