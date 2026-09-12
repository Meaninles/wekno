package answerfeedback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/Tencent/WeKnora/internal/im"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/google/uuid"
)

const (
	wecomFeedbackTaskStatePending   = "pending"
	wecomFeedbackTaskStateSending   = "sending"
	wecomFeedbackTaskStateSent      = "sent"
	wecomFeedbackTaskStateSubmitted = "submitted"
	wecomFeedbackTaskTTL            = 24 * time.Hour
	wecomFeedbackSendTimeout        = 8 * time.Second
	wecomFeedbackSendAttempts       = 3
)

// WeComCardSender is the small adapter seam needed by this custom module. The
// IM service supplies the implementation for the currently running channel.
type WeComCardSender interface {
	SendTemplateCard(ctx context.Context, channelID, chatID string, cardBody []byte) error
	UpdateTemplateCard(ctx context.Context, channelID, requestID string, cardBody []byte) error
}

type wecomFeedbackTask struct {
	taskID             string
	key                string
	channelID          string
	tenantID           uint64
	userID             string
	chatID             string
	chatType           im.ChatType
	actorKey           string
	sessionID          string
	requestID          string
	userMessageID      string
	assistantMessageID string
	generation         uint64
	state              string
	cardBody           []byte
	scheduledAt        time.Time
	ctx                context.Context
	cancel             context.CancelFunc
	timer              *time.Timer
	expiryTimer        *time.Timer
}

var _ im.FeedbackHook = (*Service)(nil)

// initWeComFeedbackState initializes the process-local state used only for the
// delayed follow-up window. No database or network work happens here.
func (s *Service) initWeComFeedbackState() {
	if s == nil {
		return
	}
	s.wecomTasksByKey = make(map[string]*wecomFeedbackTask)
	s.wecomTasksByID = make(map[string]*wecomFeedbackTask)
	s.wecomGenerations = make(map[string]uint64)
	s.wecomLastIncomingAt = make(map[string]time.Time)
}

// SetWeComCardSender wires the IM service after both custom and native
// services have been constructed during bootstrap.
func (s *Service) SetWeComCardSender(sender WeComCardSender) {
	if s == nil {
		return
	}
	s.wecomSenderMu.Lock()
	s.wecomSender = sender
	s.wecomSenderMu.Unlock()
}

func (s *Service) wecomCardSender() WeComCardSender {
	if s == nil {
		return nil
	}
	s.wecomSenderMu.RLock()
	defer s.wecomSenderMu.RUnlock()
	return s.wecomSender
}

// OnIMIncomingMessage cancels a pending follow-up as soon as the same WeCom
// user starts another request. It is intentionally constant-time and runs on
// the IM receive path, so it must never perform I/O.
func (s *Service) OnIMIncomingMessage(_ context.Context, channelID string, channel *im.IMChannel, msg *im.IncomingMessage) {
	if !isWeComFeedbackConversation(channel, msg) {
		return
	}
	key := wecomConversationKey(channelID, msg.UserID, msg.ChatID)
	now := time.Now()

	s.wecomMu.Lock()
	s.wecomGenerations[key]++
	s.wecomLastIncomingAt[key] = now
	if task := s.wecomTasksByKey[key]; task != nil {
		s.removeWeComTaskLocked(task, true)
	}
	s.wecomMu.Unlock()
}

// OnIMAnswerDelivered schedules the delayed card only after the adapter's
// final frame has been accepted. The timer callback and card send are both
// detached from the QA worker.
func (s *Service) OnIMAnswerDelivered(_ context.Context, delivery im.AnswerDelivery) {
	if s == nil || delivery.Platform != im.PlatformWeCom || delivery.Mode != "websocket" ||
		delivery.ChannelID == "" || delivery.UserID == "" || delivery.AssistantMessageID == "" {
		return
	}
	key := wecomConversationKey(delivery.ChannelID, delivery.UserID, delivery.ChatID)
	deliveredAt := delivery.DeliveredAt
	if deliveredAt.IsZero() {
		deliveredAt = time.Now()
	}

	// Do not retain the full QA request context for the 24-hour card lifetime;
	// the task only needs cancellation and the stable identifiers copied below.
	taskCtx, cancel := context.WithCancel(context.Background())
	task := &wecomFeedbackTask{
		taskID:             "wf_" + uuid.NewString(),
		key:                key,
		channelID:          delivery.ChannelID,
		tenantID:           delivery.TenantID,
		userID:             delivery.UserID,
		chatID:             delivery.ChatID,
		chatType:           delivery.ChatType,
		actorKey:           wecomActorKey(delivery.ChannelID, delivery.UserID, delivery.ChatID),
		sessionID:          delivery.SessionID,
		requestID:          delivery.RequestID,
		userMessageID:      delivery.UserMessageID,
		assistantMessageID: delivery.AssistantMessageID,
		state:              wecomFeedbackTaskStatePending,
		ctx:                taskCtx,
		cancel:             cancel,
	}
	task.cardBody = buildWeComFeedbackCard(task.taskID)

	s.wecomMu.Lock()
	if lastIncomingAt := s.wecomLastIncomingAt[key]; lastIncomingAt.After(deliveredAt) {
		s.wecomMu.Unlock()
		cancel()
		return
	}
	task.generation = s.wecomGenerations[key]
	if old := s.wecomTasksByKey[key]; old != nil {
		s.removeWeComTaskLocked(old, true)
	}
	s.wecomTasksByKey[key] = task
	s.wecomTasksByID[task.taskID] = task
	delay := s.cfg.WeComFeedbackDelay
	task.scheduledAt = time.Now()
	logger.Infof(context.Background(), "[answerfeedback] WeCom feedback card scheduled: task=%s wait=%s due_at=%s",
		task.taskID, delay, task.scheduledAt.Add(delay).Format(time.RFC3339Nano))
	task.timer = time.AfterFunc(delay, func() {
		s.fireWeComFeedbackTask(task)
	})
	s.wecomMu.Unlock()
}

// OnIMInteractiveEvent returns immediately to the adapter callback goroutine;
// validation, persistence enqueue, and card update happen in a short-lived
// background goroutine.
func (s *Service) OnIMInteractiveEvent(ctx context.Context, event im.InteractiveEvent) {
	if s == nil || event.Platform != im.PlatformWeCom || event.Mode != "websocket" ||
		event.EventType != "template_card_event" || event.TaskID == "" {
		return
	}
	go s.processWeComFeedbackEvent(context.WithoutCancel(ctx), event)
}

func (s *Service) fireWeComFeedbackTask(task *wecomFeedbackTask) {
	s.wecomMu.Lock()
	current := s.wecomTasksByID[task.taskID]
	if current != task || task.state != wecomFeedbackTaskStatePending ||
		s.wecomGenerations[task.key] != task.generation {
		s.wecomMu.Unlock()
		return
	}
	task.state = wecomFeedbackTaskStateSending
	s.wecomMu.Unlock()
	logger.Infof(context.Background(), "[answerfeedback] WeCom feedback card wait elapsed: task=%s waited=%s configured_wait=%s",
		task.taskID, time.Since(task.scheduledAt).Round(time.Millisecond), s.cfg.WeComFeedbackDelay)

	go s.sendWeComFeedbackTask(task)
}

func (s *Service) sendWeComFeedbackTask(task *wecomFeedbackTask) {
	sender := s.wecomCardSender()
	if sender == nil {
		s.failWeComFeedbackTask(task, "card sender is unavailable")
		return
	}

	var lastErr error
	for attempt := 0; attempt < wecomFeedbackSendAttempts; attempt++ {
		if err := task.ctx.Err(); err != nil {
			s.failWeComFeedbackTask(task, err.Error())
			return
		}
		sendCtx, cancel := context.WithTimeout(task.ctx, wecomFeedbackSendTimeout)
		lastErr = sender.SendTemplateCard(sendCtx, task.channelID, task.chatIDForSend(), task.cardBody)
		cancel()
		if lastErr == nil {
			s.markWeComFeedbackSent(task)
			return
		}
		if attempt+1 < wecomFeedbackSendAttempts {
			select {
			case <-task.ctx.Done():
				s.failWeComFeedbackTask(task, task.ctx.Err().Error())
				return
			case <-time.After(time.Duration(attempt+1) * 300 * time.Millisecond):
			}
		}
	}
	s.failWeComFeedbackTask(task, lastErr.Error())
}

func (s *Service) markWeComFeedbackSent(task *wecomFeedbackTask) {
	s.wecomMu.Lock()
	defer s.wecomMu.Unlock()
	if s.wecomTasksByID[task.taskID] != task || task.state != wecomFeedbackTaskStateSending || task.ctx.Err() != nil {
		return
	}
	task.state = wecomFeedbackTaskStateSent
	task.expiryTimer = time.AfterFunc(wecomFeedbackTaskTTL, func() {
		s.expireWeComFeedbackTask(task)
	})
}

func (s *Service) failWeComFeedbackTask(task *wecomFeedbackTask, reason string) {
	s.wecomMu.Lock()
	if s.wecomTasksByID[task.taskID] == task {
		s.removeWeComTaskLocked(task, true)
	}
	s.wecomMu.Unlock()
	logger.Warnf(context.Background(), "[answerfeedback] WeCom feedback card failed: task=%s reason=%s", task.taskID, reason)
}

func (s *Service) expireWeComFeedbackTask(task *wecomFeedbackTask) {
	s.wecomMu.Lock()
	if s.wecomTasksByID[task.taskID] == task {
		s.removeWeComTaskLocked(task, true)
	}
	s.wecomMu.Unlock()
}

func (s *Service) processWeComFeedbackEvent(ctx context.Context, event im.InteractiveEvent) {
	s.wecomMu.Lock()
	task := s.wecomTasksByID[event.TaskID]
	if task == nil || task.state != wecomFeedbackTaskStateSent ||
		task.channelID != event.ChannelID || task.tenantID != event.TenantID ||
		task.userID != event.UserID || task.chatID != event.ChatID {
		s.wecomMu.Unlock()
		return
	}
	task.state = wecomFeedbackTaskStateSubmitted
	s.removeWeComTaskLocked(task, true)
	s.wecomMu.Unlock()

	feedback, ok := normalizeFeedback(event.EventKey)
	if !ok || feedback == FeedbackNone {
		return
	}
	metadata := types.JSONMap{
		"source":       "wecom_feedback_card",
		"card_task_id": task.taskID,
		"event_key":    strings.TrimSpace(event.EventKey),
		"channel_id":   task.channelID,
		"chat_id":      task.chatID,
		"user_id":      task.userID,
	}
	if !s.SetFeedback(ctx, FeedbackInput{
		TenantID:           task.tenantID,
		UserID:             task.userID,
		ActorKey:           task.actorKey,
		SessionID:          task.sessionID,
		RequestID:          task.requestID,
		AssistantMessageID: task.assistantMessageID,
		Feedback:           feedback,
		Channel:            "wecom_bot",
		Source:             "wecom_feedback_card",
		Metadata:           metadata,
	}) {
		logger.Warnf(ctx, "[answerfeedback] WeCom feedback was not queued: task=%s message=%s", task.taskID, task.assistantMessageID)
	}

	if strings.TrimSpace(event.RequestID) == "" {
		return
	}
	sender := s.wecomCardSender()
	if sender == nil {
		return
	}
	updateCtx, cancel := context.WithTimeout(ctx, wecomFeedbackSendTimeout)
	err := sender.UpdateTemplateCard(updateCtx, event.ChannelID, event.RequestID, buildWeComFeedbackResultCard(task.taskID, feedback))
	cancel()
	if err != nil {
		logger.Warnf(ctx, "[answerfeedback] WeCom feedback card update failed: task=%s err=%v", task.taskID, err)
	}
}

func (s *Service) removeWeComTaskLocked(task *wecomFeedbackTask, cancel bool) {
	if task == nil {
		return
	}
	if task.timer != nil {
		task.timer.Stop()
	}
	if task.expiryTimer != nil {
		task.expiryTimer.Stop()
	}
	if cancel && task.cancel != nil {
		task.cancel()
	}
	if s.wecomTasksByKey[task.key] == task {
		delete(s.wecomTasksByKey, task.key)
	}
	if s.wecomTasksByID[task.taskID] == task {
		delete(s.wecomTasksByID, task.taskID)
	}
}

func (task *wecomFeedbackTask) chatIDForSend() string {
	if task.chatID != "" {
		return task.chatID
	}
	return task.userID
}

func isWeComFeedbackConversation(channel *im.IMChannel, msg *im.IncomingMessage) bool {
	return channel != nil && msg != nil && channel.Platform == string(im.PlatformWeCom) && channel.Mode == "websocket" &&
		msg.Platform == im.PlatformWeCom && strings.TrimSpace(msg.UserID) != ""
}

func wecomConversationKey(channelID, userID, chatID string) string {
	return channelID + "\x00" + userID + "\x00" + chatID
}

func wecomActorKey(channelID, userID, chatID string) string {
	sum := sha256.Sum256([]byte(wecomConversationKey(channelID, userID, chatID)))
	return "wecom:" + hex.EncodeToString(sum[:])
}

func buildWeComFeedbackCard(taskID string) []byte {
	return mustMarshalWeComCard(map[string]any{
		"card_type": "button_interaction",
		"main_title": map[string]string{
			"title": "这次回答解决您的问题了吗？",
		},
		"task_id": taskID,
		"button_list": []map[string]any{
			{"text": "✓ 已解决", "style": 2, "key": FeedbackSolved},
			{"text": "? 没答到点上", "style": 2, "key": FeedbackOffTopic},
			{"text": "! 内容不准确", "style": 2, "key": FeedbackInaccurate},
			{"text": "× 未解决", "style": 2, "key": FeedbackUnsolved},
		},
	})
}

func buildWeComFeedbackResultCard(taskID, feedback string) []byte {
	labels := map[string]string{
		FeedbackSolved:     "已解决",
		FeedbackOffTopic:   "没答到点上",
		FeedbackInaccurate: "内容不准确",
		FeedbackUnsolved:   "未解决",
	}
	return mustMarshalWeComCard(map[string]any{
		"card_type": "text_notice",
		"main_title": map[string]string{
			"title": "感谢您的反馈",
			"desc":  "您选择了：" + labels[feedback],
		},
		"sub_title_text": "我们会持续优化回答质量。",
		"task_id":        taskID,
	})
}

func mustMarshalWeComCard(value any) []byte {
	body, err := json.Marshal(value)
	if err != nil {
		// All values above are static maps, so this is only a defensive fallback.
		return []byte(`{"card_type":"text_notice","main_title":{"title":"感谢您的反馈"}}`)
	}
	return body
}
