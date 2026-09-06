package chat

import (
	"context"
	"fmt"
	"github.com/Tencent/WeKnora/internal/types"
	"strings"
)

// ValidateTerminal checks protocol completion only. A completed answer still
// requires a semantic review; text patterns cannot prove task success.
func ValidateTerminal(answer, finishReason string) error {
	switch strings.ToLower(strings.TrimSpace(finishReason)) {
	case "stop", "end_turn":
	default:
		return fmt.Errorf("model response did not complete: finish_reason=%q", finishReason)
	}
	if strings.TrimSpace(answer) == "" {
		return fmt.Errorf("model completed without answer text")
	}
	return nil
}

type TerminalStreamCollection struct {
	Answer         string
	Completed      bool
	FinishReason   string
	TransportError string
}

// CollectTerminalStream consumes typed provider events. It never parses a
// private text delimiter, guesses tool syntax or rewrites answer semantics.
func CollectTerminalStream(ctx context.Context, stream <-chan types.StreamResponse,
	onThinking func(string), onThinkingDone func()) TerminalStreamCollection {
	result := TerminalStreamCollection{}
	var answer strings.Builder
	closeThinking := func() {
		if onThinkingDone != nil {
			onThinkingDone()
		}
	}
	defer closeThinking()
	for {
		select {
		case <-ctx.Done():
			result.TransportError = ctx.Err().Error()
			return result
		case response, ok := <-stream:
			if !ok {
				result.Answer = answer.String()
				if result.TransportError == "" {
					result.TransportError = "stream closed before terminal completion"
				}
				return result
			}
			if response.FinishReason != "" {
				result.FinishReason = response.FinishReason
			}
			switch response.ResponseType {
			case types.ResponseTypeError:
				result.TransportError = response.Content
				if result.TransportError == "" {
					result.TransportError = "provider stream error"
				}
				result.Answer = answer.String()
				return result
			case types.ResponseTypeThinking:
				if response.Content != "" && onThinking != nil {
					onThinking(response.Content)
				}
				if response.Done {
					closeThinking()
				}
			case types.ResponseTypeAnswer:
				closeThinking()
				answer.WriteString(response.Content)
				if response.Done {
					result.Answer = answer.String()
					result.Completed = ValidateTerminal(result.Answer, result.FinishReason) == nil
					return result
				}
			}
		}
	}
}
