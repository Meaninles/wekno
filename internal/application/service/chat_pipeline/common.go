package chatpipeline

import (
	"context"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/tracing/langfuse"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

var regThinkTags = regexp.MustCompile(`(?s)<think>.*?</think>`)

// pipelineInfo logs pipeline info level entries.
func pipelineInfo(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineInfo(ctx, stage, action, fields)
}

// pipelineWarn logs pipeline warning level entries.
func pipelineWarn(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineWarn(ctx, stage, action, fields)
}

// pipelineError logs pipeline error level entries.
func pipelineError(ctx context.Context, stage, action string, fields map[string]interface{}) {
	common.PipelineError(ctx, stage, action, fields)
}

// recordEvalPromptLayout emits exact prompt/history size diagnostics only when
// eval mode has explicitly enabled full capture. Production mode returns at the
// guard and performs no message traversal, serialization, or synchronous I/O.
func recordEvalPromptLayout(
	ctx context.Context,
	name string,
	chatManage *types.ChatManage,
	messages []chat.Message,
) {
	mgr := langfuse.GetManager()
	if mgr == nil || !mgr.CaptureContent() || !mgr.EnabledFor(ctx) {
		return
	}
	roleMessages := map[string]int{}
	roleChars := map[string]int{}
	for _, message := range messages {
		roleMessages[message.Role]++
		roleChars[message.Role] += utf8.RuneCountInString(message.Content)
	}
	historyUserChars := 0
	historyAssistantChars := 0
	for _, history := range chatManage.History {
		if history == nil {
			continue
		}
		historyUserChars += utf8.RuneCountInString(history.Query)
		historyAssistantChars += utf8.RuneCountInString(history.Answer)
	}
	observation := map[string]interface{}{
		"configured_history_rounds": chatManage.MaxRounds,
		"actual_history_rounds":     len(chatManage.History),
		"history_user_chars":        historyUserChars,
		"history_assistant_chars":   historyAssistantChars,
		"rendered_context_chars":    utf8.RuneCountInString(chatManage.RenderedContexts),
		"current_user_chars":        utf8.RuneCountInString(chatManage.UserContent),
		"message_count":             len(messages),
		"message_count_by_role":     roleMessages,
		"message_chars_by_role":     roleChars,
	}
	_, span := mgr.StartSpan(ctx, langfuse.SpanOptions{
		Name:     name,
		Input:    observation,
		Metadata: map[string]interface{}{"eval_only": true},
	})
	span.Finish(observation, map[string]interface{}{"eval_only": true}, nil)
}

// prepareChatModel shared logic to prepare chat model and options
// it gets the chat model and sets up the chat options based on the chat manage.
func prepareChatModel(ctx context.Context, modelService interfaces.ModelService,
	chatManage *types.ChatManage,
) (chat.Chat, *chat.ChatOptions, error) {
	chatModel, err := modelService.GetChatModel(ctx, chatManage.ChatModelID)
	if err != nil {
		logger.Errorf(ctx, "Failed to get chat model: %v", err)
		return nil, nil, err
	}

	opt := &chat.ChatOptions{
		Temperature:         chatManage.SummaryConfig.Temperature,
		TopP:                chatManage.SummaryConfig.TopP,
		Seed:                chatManage.SummaryConfig.Seed,
		MaxTokens:           chatManage.SummaryConfig.MaxTokens,
		MaxCompletionTokens: chatManage.SummaryConfig.MaxCompletionTokens,
		FrequencyPenalty:    chatManage.SummaryConfig.FrequencyPenalty,
		PresencePenalty:     chatManage.SummaryConfig.PresencePenalty,
		Thinking:            chatManage.SummaryConfig.Thinking,
	}
	if opt.Thinking != nil {
		pipelineInfo(ctx, "Stream", "thinking_option", map[string]interface{}{
			"enabled": *opt.Thinking,
		})
	}

	return chatModel, opt, nil
}

// prepareMessagesWithHistory prepare complete messages including history.
// When SystemPromptOverride is set (e.g. by intent-specific prompt logic),
// it takes precedence over the default SummaryConfig.Prompt.
func prepareMessagesWithHistory(chatManage *types.ChatManage) []chat.Message {
	base := chatManage.SummaryConfig.Prompt
	if chatManage.SystemPromptOverride != "" {
		base = chatManage.SystemPromptOverride
	}
	systemPrompt := types.RenderPromptPlaceholders(base, types.PlaceholderValues{
		"query":    chatManage.Query,
		"language": chatManage.Language,
		"contexts": chatManage.RenderedContexts,
	})
	// Apply the same lightweight contract to every normal-QA turn. It is inert
	// when no evidence handles exist, while keeping current-turn precedence and
	// citation syntax consistent for present and future retrieval paths.
	systemPrompt = sourcerefs.EnsureGenerationContract(systemPrompt)
	if skillContext := strings.TrimSpace(chatManage.LightweightSkillContext); skillContext != "" {
		systemPrompt += "\n\n" + skillContext
	}

	chatMessages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}

	chatMessages = AppendHistoryMessages(chatMessages, chatManage.History)

	// Add current user message. Only include images when the chat model supports
	// vision; non-vision models rely on the text description in UserContent.
	userMsg := chat.Message{Role: "user", Content: chatManage.UserContent}
	if chatManage.ChatModelSupportsVision && len(chatManage.Images) > 0 {
		userMsg.Images = chatManage.Images
	}
	chatMessages = append(chatMessages, userMsg)

	return chatMessages
}

// AppendHistoryMessages appends prior Q&A rounds in chronological order.
// History is already filtered and truncated upstream by the load_history plugin.
func AppendHistoryMessages(messages []chat.Message, history []*types.History) []chat.Message {
	for _, history := range history {
		messages = append(messages, chat.Message{Role: "user", Content: history.Query})
		messages = append(messages, chat.Message{Role: "assistant", Content: sourcerefs.StripCitationProtocol(history.Answer)})
	}
	return messages
}

// loadAndProcessHistory fetches recent messages, groups them into Q&A pairs,
// strips <think> tags from assistant answers, sorts by recency, and limits to maxRounds.
// fetchCount controls how many raw messages to fetch (typically maxRounds*2+10).
func loadAndProcessHistory(
	ctx context.Context,
	messageService interfaces.MessageService,
	sessionID string,
	maxRounds int,
	fetchCount int,
) ([]*types.History, error) {
	history, err := messageService.GetRecentMessagesBySession(ctx, sessionID, fetchCount)
	if err != nil {
		return nil, err
	}

	historyMap := make(map[string]*types.History)
	for _, message := range history {
		h, ok := historyMap[message.RequestID]
		if !ok {
			h = &types.History{}
		}
		if message.Role == "user" {
			// RenderedContent contains the previous turn's retrieval envelope,
			// including request-local citation IDs. Replaying it would expose stale
			// evidence as a new user message and an old S1 could collide with the
			// current turn's S1. Rebuild history from the original user input only.
			h.Query = message.Content
			h.CreateAt = message.CreatedAt
			if desc := extractImageCaptions(message.Images); desc != "" {
				h.Query += "\n\n[用户上传图片内容]\n" + desc
			}
			if len(message.Attachments) > 0 {
				h.Query += message.Attachments.BuildPrompt()
			}
		} else {
			h.Answer = sourcerefs.StripCitationProtocol(regThinkTags.ReplaceAllString(message.Content, ""))
			h.KnowledgeReferences = message.KnowledgeReferences
		}
		historyMap[message.RequestID] = h
	}

	historyList := make([]*types.History, 0, len(historyMap))
	for _, h := range historyMap {
		if h.Answer != "" && h.Query != "" {
			historyList = append(historyList, h)
		}
	}

	sort.Slice(historyList, func(i, j int) bool {
		return historyList[i].CreateAt.After(historyList[j].CreateAt)
	})

	if len(historyList) > maxRounds {
		historyList = historyList[:maxRounds]
	}

	slices.Reverse(historyList)
	return historyList, nil
}

// extractImageCaptions concatenates non-empty Caption fields from stored
// message images. Used when loading history so that previous turns' image
// descriptions are visible to the model.
func extractImageCaptions(images types.MessageImages) string {
	var parts []string
	for _, img := range images {
		if img.Caption != "" {
			parts = append(parts, img.Caption)
		}
	}
	return strings.Join(parts, "\n")
}

// ---------------------------------------------------------------------------
// Concurrency utilities
// ---------------------------------------------------------------------------

// ParallelTask represents a named unit of concurrent work.
type ParallelTask struct {
	Name string
	Run  func() *PluginError
}

// RunParallel executes tasks concurrently.
// Returns a map of task name → error for tasks that returned non-nil errors.
func RunParallel(tasks ...ParallelTask) map[string]*PluginError {
	errs := make(map[string]*PluginError)
	var mu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(len(tasks))
	for _, task := range tasks {
		go func(t ParallelTask) {
			defer wg.Done()
			if err := t.Run(); err != nil {
				mu.Lock()
				errs[t.Name] = err
				mu.Unlock()
			}
		}(task)
	}
	wg.Wait()
	return errs
}

// ParallelMap applies fn to each element of items concurrently (up to
// maxWorkers goroutines) and returns results in the same order as items.
// If maxWorkers <= 0, concurrency is unbounded (one goroutine per item).
func ParallelMap[T, R any](items []T, maxWorkers int, fn func(int, T) R) []R {
	n := len(items)
	if n == 0 {
		return nil
	}
	results := make([]R, n)

	if maxWorkers <= 0 || maxWorkers > n {
		maxWorkers = n
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, maxWorkers)

	for i, item := range items {
		wg.Add(1)
		sem <- struct{}{}
		go func(idx int, it T) {
			defer func() { <-sem; wg.Done() }()
			results[idx] = fn(idx, it)
		}(i, item)
	}
	wg.Wait()
	return results
}
