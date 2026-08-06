package wecom

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBotStreamReplyBodyPreservesClickableBracketCitation(t *testing.T) {
	content := `结论。[[1](https://knora.example.com/platform/knowledge-bases/kb?chunk_id=c1&knowledge_id=d1)]`
	body := NewBotStreamReplyBody("stream-1", content, true)
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal Bot stream body: %v", err)
	}
	var decoded BotStreamReplyBody
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("unmarshal Bot stream body: %v", err)
	}
	if decoded.MsgType != "stream" || decoded.Stream.ID != "stream-1" || !decoded.Stream.Finish {
		t.Fatalf("unexpected Bot frame metadata: %#v", decoded)
	}
	if decoded.Stream.Content != content || !strings.Contains(decoded.Stream.Content, `[[1](https://`) {
		t.Fatalf("Bot stream.content changed citation Markdown: %q", decoded.Stream.Content)
	}
}
