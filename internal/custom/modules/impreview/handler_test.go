package impreview

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/im/wecom"
)

func TestPreviewStreamingMatchesWeComApplicationAndBotModes(t *testing.T) {
	if previewStreaming("wecom", "webhook", "stream") {
		t.Fatal("WeCom application callback mode must use full delivery")
	}
	if !previewStreaming("wecom", "websocket", "stream") {
		t.Fatal("WeCom intelligent Bot websocket mode must preview streaming")
	}
	if previewStreaming("wecom", "websocket", "full") {
		t.Fatal("full output_mode must disable streaming for every integration")
	}
}

func TestBuildTransportPreviewUsesRealWeComApplicationAndBotShapes(t *testing.T) {
	longContent := strings.Repeat("制度要求。", 500) + `[\[1\]](https://example.com)`
	application := buildTransportPreview("wecom", "webhook", longContent)
	if len(application) < 2 || application[0].Kind != "application_markdown" {
		t.Fatalf("unexpected application payloads: %#v", application)
	}
	for _, payload := range application {
		body, ok := payload.Body.(map[string]interface{})
		if !ok || body["msgtype"] != "markdown" {
			t.Fatalf("unexpected application body: %#v", payload.Body)
		}
	}

	bot := buildTransportPreview("wecom", "websocket", "最终正文")
	if len(bot) != 1 || bot[0].Kind != "bot_stream_final" {
		t.Fatalf("unexpected bot payload: %#v", bot)
	}
	body := bot[0].Body.(wecom.BotStreamReplyBody)
	if !body.Stream.Finish || body.Stream.Content != "最终正文" {
		t.Fatalf("unexpected bot final frame: %#v", body)
	}
}
