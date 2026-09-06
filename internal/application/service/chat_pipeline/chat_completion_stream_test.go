package chatpipeline

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestCollectTerminalStreamKeepsPartialTransportFailurePrivate(t *testing.T) {
	stream := make(chan types.StreamResponse, 3)
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: "partial "}
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeError, Content: "unexpected EOF"}
	close(stream)

	result := collectTerminalStream(context.Background(), stream, nil, nil)
	if result.Answer != "partial " {
		t.Fatalf("answer = %q, want buffered partial", result.Answer)
	}
	if result.Completed {
		t.Fatal("transport-corrupted stream must not be complete")
	}
	if result.TransportError != "unexpected EOF" {
		t.Fatalf("transport error = %q", result.TransportError)
	}
}

func TestCollectTerminalStreamRequiresExplicitTerminalCompletion(t *testing.T) {
	stream := make(chan types.StreamResponse, 1)
	stream <- types.StreamResponse{
		ResponseType: types.ResponseTypeAnswer,
		Content:      "complete answer",
		Done:         true,
		FinishReason: "stop",
	}
	close(stream)

	result := collectTerminalStream(context.Background(), stream, nil, nil)
	if !result.Completed || result.TransportError != "" || result.Answer != "complete answer" {
		t.Fatalf("unexpected terminal result: %#v", result)
	}
}

func TestCollectTerminalStreamMarksPrematureCloseWithoutProviderError(t *testing.T) {
	stream := make(chan types.StreamResponse, 1)
	stream <- types.StreamResponse{ResponseType: types.ResponseTypeAnswer, Content: "cut off"}
	close(stream)

	result := collectTerminalStream(context.Background(), stream, nil, nil)
	if result.Completed || result.TransportError == "" {
		t.Fatalf("premature close not detected: %#v", result)
	}
}
