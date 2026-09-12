package answerfeedback

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
)

func TestNormalizeFeedback(t *testing.T) {
	tests := map[string]struct {
		input string
		want  string
		ok    bool
	}{
		"legacy like":    {input: "like", want: FeedbackSolved, ok: true},
		"legacy dislike": {input: "dislike", want: FeedbackUnsolved, ok: true},
		"solved":         {input: FeedbackSolved, want: FeedbackSolved, ok: true},
		"off topic":      {input: FeedbackOffTopic, want: FeedbackOffTopic, ok: true},
		"inaccurate":     {input: FeedbackInaccurate, want: FeedbackInaccurate, ok: true},
		"unsolved":       {input: FeedbackUnsolved, want: FeedbackUnsolved, ok: true},
		"clear":          {input: "none", want: FeedbackNone, ok: true},
		"invalid":        {input: "other", want: "", ok: false},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			got, ok := normalizeFeedback(tt.input)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("normalizeFeedback(%q) = (%q, %v), want (%q, %v)", tt.input, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestBuildWeComFeedbackCard(t *testing.T) {
	var card map[string]any
	if err := json.Unmarshal(buildWeComFeedbackCard("task-1"), &card); err != nil {
		t.Fatalf("unmarshal card: %v", err)
	}
	if card["card_type"] != "button_interaction" || card["task_id"] != "task-1" {
		t.Fatalf("unexpected card header: %#v", card)
	}
	mainTitle, ok := card["main_title"].(map[string]any)
	if !ok || mainTitle["title"] != "这次回答解决您的问题了吗？" {
		t.Fatalf("unexpected card title: %#v", card["main_title"])
	}
	if _, hasDesc := mainTitle["desc"]; hasDesc {
		t.Fatalf("feedback card should not include a subtitle: %#v", mainTitle)
	}
	buttons, ok := card["button_list"].([]any)
	if !ok || len(buttons) != 4 {
		t.Fatalf("button count = %d, want 4", len(buttons))
	}
	for i, expected := range []struct {
		key  string
		text string
	}{
		{key: FeedbackSolved, text: "✓ 已解决"},
		{key: FeedbackOffTopic, text: "? 没答到点上"},
		{key: FeedbackInaccurate, text: "! 内容不准确"},
		{key: FeedbackUnsolved, text: "× 未解决"},
	} {
		button, ok := buttons[i].(map[string]any)
		if !ok || button["key"] != expected.key || button["text"] != expected.text || button["style"] != float64(2) {
			t.Fatalf("button %d = %#v, want key %q text %q style 2", i, buttons[i], expected.key, expected.text)
		}
	}
}

func TestWeComFeedbackTaskIsCancelledByNewMessage(t *testing.T) {
	s := NewService(nil, Config{WeComFeedbackDelay: 15 * time.Millisecond})
	sender := &recordingWeComCardSender{sent: make(chan struct{}, 1)}
	s.SetWeComCardSender(sender)
	channel := &im.IMChannel{Platform: string(im.PlatformWeCom), Mode: "websocket"}
	message := &im.IncomingMessage{Platform: im.PlatformWeCom, UserID: "user-1"}
	s.OnIMIncomingMessage(context.Background(), "channel-1", channel, message)
	s.OnIMAnswerDelivered(context.Background(), im.AnswerDelivery{
		ChannelID:          "channel-1",
		TenantID:           1,
		Platform:           im.PlatformWeCom,
		Mode:               "websocket",
		UserID:             "user-1",
		AssistantMessageID: "assistant-1",
		DeliveredAt:        time.Now(),
	})
	s.OnIMIncomingMessage(context.Background(), "channel-1", channel, message)

	select {
	case <-sender.sent:
		t.Fatal("feedback card was sent after a new message")
	case <-time.After(40 * time.Millisecond):
	}
}

func TestWeComFeedbackCardClickUsesSharedFeedbackQueue(t *testing.T) {
	s := NewService(nil, Config{WeComFeedbackDelay: 1 * time.Millisecond})
	sender := &recordingWeComCardSender{sent: make(chan struct{}, 1), updated: make(chan struct{}, 1)}
	s.SetWeComCardSender(sender)
	channel := &im.IMChannel{Platform: string(im.PlatformWeCom), Mode: "websocket"}
	s.OnIMIncomingMessage(context.Background(), "channel-1", channel, &im.IncomingMessage{
		Platform: im.PlatformWeCom,
		UserID:   "user-1",
	})
	s.OnIMAnswerDelivered(context.Background(), im.AnswerDelivery{
		ChannelID:          "channel-1",
		TenantID:           1,
		Platform:           im.PlatformWeCom,
		Mode:               "websocket",
		UserID:             "user-1",
		SessionID:          "session-1",
		RequestID:          "request-1",
		AssistantMessageID: "assistant-1",
		DeliveredAt:        time.Now(),
	})
	select {
	case <-sender.sent:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("feedback card was not sent")
	}

	var card map[string]any
	if err := json.Unmarshal(sender.lastSent(), &card); err != nil {
		t.Fatalf("unmarshal sent card: %v", err)
	}
	s.OnIMInteractiveEvent(context.Background(), im.InteractiveEvent{
		ChannelID: "channel-1",
		TenantID:  1,
		Platform:  im.PlatformWeCom,
		Mode:      "websocket",
		EventType: "template_card_event",
		EventKey:  FeedbackInaccurate,
		TaskID:    card["task_id"].(string),
		RequestID: "event-request-1",
		UserID:    "user-1",
	})
	select {
	case <-sender.updated:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("feedback card was not updated")
	}

	select {
	case key := <-s.feedbackQueue:
		task, ok := s.pendingFeedback[key]
		if !ok {
			t.Fatal("shared feedback task disappeared before persistence")
		}
		if task.Feedback != FeedbackInaccurate || task.Source != "wecom_feedback_card" {
			t.Fatalf("shared feedback task = %#v, want inaccurate from WeCom card", task)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("shared feedback task was not queued")
	}
}

type recordingWeComCardSender struct {
	mu      sync.Mutex
	sent    chan struct{}
	updated chan struct{}
	body    []byte
}

func (s *recordingWeComCardSender) SendTemplateCard(_ context.Context, _, _ string, body []byte) error {
	s.mu.Lock()
	s.body = append([]byte(nil), body...)
	s.mu.Unlock()
	s.sent <- struct{}{}
	return nil
}

func (s *recordingWeComCardSender) UpdateTemplateCard(_ context.Context, _, _ string, _ []byte) error {
	s.updated <- struct{}{}
	return nil
}

func (s *recordingWeComCardSender) lastSent() []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]byte(nil), s.body...)
}
