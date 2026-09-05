package chatpipeline

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

// PluginChatCompletionStream implements streaming chat completion functionality
// as a plugin that can be registered to EventManager
type PluginChatCompletionStream struct {
	modelService interfaces.ModelService // Interface for model operations
}

// terminalStreamCollection is the buffered result of one provider stream.
// Provider errors and premature channel closure are kept private until the
// caller has had one chance to replace the entire candidate. This prevents a
// partial answer or transient error event from becoming the persisted/UI
// result while remaining independent of answer semantics.
type terminalStreamCollection struct {
	Answer         string
	Completed      bool
	FinishReason   string
	TransportError string
}

func collectTerminalStream(
	ctx context.Context,
	stream <-chan types.StreamResponse,
	onThinking func(string),
	onThinkingDone func(),
) terminalStreamCollection {
	projector := conversationmemory.NewTerminalAnswerProjector()
	var candidate strings.Builder
	result := terminalStreamCollection{}
	closeThinking := func() {
		if onThinkingDone != nil {
			onThinkingDone()
		}
	}
	for {
		select {
		case <-ctx.Done():
			closeThinking()
			result.TransportError = ctx.Err().Error()
			return result
		case response, ok := <-stream:
			if !ok {
				closeThinking()
				candidate.WriteString(projector.Flush())
				result.Answer = candidate.String()
				if result.TransportError == "" {
					result.TransportError = "stream closed before terminal completion"
				}
				return result
			}
			if response.FinishReason != "" {
				result.FinishReason = strings.ToLower(strings.TrimSpace(response.FinishReason))
			}
			switch response.ResponseType {
			case types.ResponseTypeError:
				if result.TransportError == "" {
					result.TransportError = strings.TrimSpace(response.Content)
					if result.TransportError == "" {
						result.TransportError = "provider stream error"
					}
				}
			case types.ResponseTypeThinking:
				if response.Content != "" && onThinking != nil {
					onThinking(response.Content)
				}
				if response.Done {
					closeThinking()
				}
			case types.ResponseTypeAnswer:
				closeThinking()
				candidate.WriteString(projector.Feed(response.Content))
				if response.Done {
					candidate.WriteString(projector.Flush())
					if result.FinishReason == "" {
						result.FinishReason = "stop"
					}
					result.Answer = candidate.String()
					result.Completed = result.FinishReason == "stop" || result.FinishReason == "end_turn"
					return result
				}
			}
		}
	}
}

// NewPluginChatCompletionStream creates a new PluginChatCompletionStream instance
// and registers it with the EventManager
func NewPluginChatCompletionStream(eventManager *EventManager,
	modelService interfaces.ModelService,
) *PluginChatCompletionStream {
	res := &PluginChatCompletionStream{
		modelService: modelService,
	}
	eventManager.Register(res)
	return res
}

// ActivationEvents returns the event types this plugin handles
func (p *PluginChatCompletionStream) ActivationEvents() []types.EventType {
	return []types.EventType{types.CHAT_COMPLETION_STREAM}
}

// OnEvent handles streaming chat completion events
// It prepares the chat model, messages, and initiates streaming response
func (p *PluginChatCompletionStream) OnEvent(ctx context.Context,
	eventType types.EventType, chatManage *types.ChatManage, next func() *PluginError,
) *PluginError {
	pipelineInfo(ctx, "Stream", "input", map[string]interface{}{
		"session_id":     chatManage.SessionID,
		"user_question":  chatManage.UserContent,
		"history_rounds": len(chatManage.History),
		"chat_model":     chatManage.ChatModelID,
	})

	// Prepare chat model and options
	chatModel, opt, err := prepareChatModel(ctx, p.modelService, chatManage)
	if err != nil {
		return ErrGetChatModel.WithError(err)
	}

	// Prepare base messages without history

	chatMessages := prepareMessagesWithHistory(chatManage)
	recordEvalPromptLayout(ctx, "rag.prompt_layout", chatManage, chatMessages)
	pipelineInfo(ctx, "Stream", "messages_ready", map[string]interface{}{
		"message_count": len(chatMessages),
		"system_prompt": chatMessages[0].Content,
	})
	pipelineInfo(ctx, "Stream", "user_message", map[string]interface{}{
		"content": chatMessages[len(chatMessages)-1].Content,
	})
	// EventBus is required for event-driven streaming
	if chatManage.EventBus == nil {
		pipelineError(ctx, "Stream", "eventbus_missing", map[string]interface{}{
			"session_id": chatManage.SessionID,
		})
		return ErrModelCall.WithError(errors.New("EventBus is required for streaming"))
	}
	eventBus := chatManage.EventBus
	pipelineInfo(ctx, "Stream", "eventbus_ready", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})

	// Initiate streaming chat model call with independent context
	pipelineInfo(ctx, "Stream", "model_call", map[string]interface{}{
		"chat_model": chatManage.ChatModelID,
	})
	responseChan, err := chatModel.ChatStream(ctx, chatMessages, opt)
	if err != nil {
		pipelineError(ctx, "Stream", "model_call", map[string]interface{}{
			"chat_model": chatManage.ChatModelID,
			"error":      err.Error(),
		})
		return ErrModelCall.WithError(err)
	}
	if responseChan == nil {
		pipelineError(ctx, "Stream", "model_call", map[string]interface{}{
			"chat_model": chatManage.ChatModelID,
			"error":      "nil_channel",
		})
		return ErrModelCall.WithError(errors.New("chat stream returned nil channel"))
	}

	pipelineInfo(ctx, "Stream", "model_started", map[string]interface{}{
		"session_id": chatManage.SessionID,
	})

	// Start goroutine to consume the channel. Reasoning remains live, while the
	// terminal answer is buffered until a protocol-only integrity check succeeds.
	// This keeps SSE, persistence, and later history on one production candidate.
	go func() {
		thinkingID := fmt.Sprintf("%s-thinking", uuid.New().String()[:8])
		answerID := fmt.Sprintf("%s-answer", uuid.New().String()[:8])
		thinkingOpen := false
		answerDone := false

		closeThinking := func() {
			if !thinkingOpen {
				return
			}
			eventBus.Emit(ctx, types.Event{
				ID:        thinkingID,
				Type:      types.EventType(event.EventAgentThought),
				SessionID: chatManage.SessionID,
				Data: event.AgentThoughtData{
					Done: true,
				},
			})
			thinkingOpen = false
		}
		emitAnswer := func(content string, done bool) {
			if answerDone {
				return
			}
			eventBus.Emit(ctx, types.Event{
				ID:        answerID,
				Type:      types.EventType(event.EventAgentFinalAnswer),
				SessionID: chatManage.SessionID,
				Data: event.AgentFinalAnswerData{
					Content: content,
					Done:    done,
				},
			})
			if done {
				answerDone = true
			}
		}
		collect := func(stream <-chan types.StreamResponse) terminalStreamCollection {
			return collectTerminalStream(
				ctx,
				stream,
				func(content string) {
					thinkingOpen = true
					eventBus.Emit(ctx, types.Event{
						ID:        thinkingID,
						Type:      types.EventType(event.EventAgentThought),
						SessionID: chatManage.SessionID,
						Data:      event.AgentThoughtData{Content: content, Done: false},
					})
				},
				closeThinking,
			)
		}

		result := collect(responseChan)
		answer, completed, finishReason := result.Answer, result.Completed, result.FinishReason
		if ctx.Err() != nil {
			pipelineInfo(ctx, "Stream", "context_cancelled", map[string]interface{}{
				"session_id": chatManage.SessionID,
			})
			return
		}
		recoveryReason := ""
		recoveryDirective := ""
		switch {
		case result.TransportError != "":
			recoveryReason = "transport_error"
			recoveryDirective = conversationmemory.TerminalIntegrityRetryDirective()
			pipelineError(ctx, "Stream", "stream_error_buffered", map[string]interface{}{
				"session_id": chatManage.SessionID,
				"error":      result.TransportError,
			})
		case finishReason == "length" || finishReason == "max_tokens" || finishReason == "max_output_tokens":
			recoveryReason = "output_limit"
			recoveryDirective = conversationmemory.TerminalOutputLimitRetryDirective()
		case !completed:
			recoveryReason = "incomplete_stream"
			recoveryDirective = conversationmemory.TerminalIntegrityRetryDirective()
		default:
			if reason := conversationmemory.TerminalAnswerIntegrityReason(answer); reason != "" {
				recoveryReason = reason
				recoveryDirective = conversationmemory.TerminalIntegrityRetryDirective()
			}
		}
		if recoveryReason != "" {
			pipelineInfo(ctx, "Stream", "terminal_candidate_retry", map[string]interface{}{
				"session_id":    chatManage.SessionID,
				"reason":        recoveryReason,
				"finish_reason": finishReason,
				"attempt":       1,
			})
			retryMessages := append([]chat.Message(nil), chatMessages...)
			last := len(retryMessages) - 1
			retryMessages[last].Content += "\n\n" + recoveryDirective
			retryOpt := *opt
			thinking := false
			retryOpt.Thinking = &thinking
			retryStream, retryErr := chatModel.ChatStream(ctx, retryMessages, &retryOpt)
			if retryErr == nil && retryStream != nil {
				replacement := collect(retryStream)
				if replacement.TransportError == "" && replacement.Completed &&
					conversationmemory.TerminalAnswerIntegrityReason(replacement.Answer) == "" {
					answer = replacement.Answer
					completed = true
					finishReason = replacement.FinishReason
				} else {
					answer = conversationmemory.TerminalIntegrityFallback(chatManage.Language)
					completed = false
				}
			} else {
				answer = conversationmemory.TerminalIntegrityFallback(chatManage.Language)
				completed = false
			}
		}
		// Citation markup is a transport protocol, not answer semantics. Validate
		// it before the single production candidate is emitted so SSE, database
		// persistence, and history replay receive identical bytes. Unsupported or
		// prior-turn handles are removed; no claim text is rewritten and no model
		// call is added.
		answer = sourcerefs.RepairAnswerCitations(answer, chatManage.CitationResult)
		answer, _, citationReport := sourcerefs.FilterAnswerCitations(answer, chatManage.CitationResult)
		answer = strings.TrimSpace(answer)
		if citationReport.ForbiddenTags > 0 || citationReport.IncompleteTags > 0 || len(citationReport.UnknownIDs) > 0 {
			pipelineInfo(ctx, "Stream", "terminal_citation_protocol_filtered", map[string]interface{}{
				"session_id": chatManage.SessionID,
				"forbidden":  citationReport.ForbiddenTags,
				"incomplete": citationReport.IncompleteTags,
				"unknown":    citationReport.UnknownIDs,
			})
		}
		emitAnswer(answer, false)
		emitAnswer("", true)
		pipelineInfo(ctx, "Stream", "terminal_answer_done", map[string]interface{}{
			"session_id":       chatManage.SessionID,
			"stream_completed": completed,
			"finish_reason":    finishReason,
			"answer_len":       len(answer),
		})
	}()

	return next()
}
