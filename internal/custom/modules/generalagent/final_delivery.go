package generalagent

import "strings"

// terminalDeliveryBuffer keeps Claude SDK answer events private until the
// sidecar result has arrived. The result answer is then replayed once through
// the ordinary event path, so persistence and the user-visible text consume
// the same immutable string.
type terminalDeliveryBuffer struct {
	content strings.Builder
}

func (b *terminalDeliveryBuffer) append(evt StreamEvent) bool {
	if evt.Type != "answer_delta" {
		return false
	}
	if evt.Content != "" {
		b.content.WriteString(evt.Content)
	}
	return true
}

func (b *terminalDeliveryBuffer) finalAnswer(result *ChatResult) string {
	if result != nil {
		if answer := strings.TrimSpace(result.Answer); answer != "" {
			return answer
		}
	}
	return strings.TrimSpace(b.content.String())
}

func (b *terminalDeliveryBuffer) replay(
	answer string,
	answerID string,
	emit func(StreamEvent),
) {
	if emit == nil || answer == "" {
		return
	}
	emit(StreamEvent{
		ID:      answerID,
		Type:    "answer_delta",
		Content: answer,
	})
	emit(StreamEvent{
		ID:   answerID,
		Type: "answer_delta",
		Done: true,
	})
}
