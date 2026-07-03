package agent

import (
	"context"
	"fmt"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// streamFinalAnswerToEventBus streams the final answer generation through EventBus
func (e *AgentEngine) streamFinalAnswerToEventBus(
	ctx context.Context,
	query string,
	state *types.AgentState,
	sessionID string,
) error {
	totalToolCalls := countTotalToolCalls(state.RoundSteps)
	logger.Infof(ctx, "[Agent][FinalAnswer] Synthesizing from %d steps, %d tool calls",
		len(state.RoundSteps), totalToolCalls)
	common.PipelineInfo(ctx, "Agent", "final_answer_start", map[string]interface{}{
		"session_id":   sessionID,
		"query":        query,
		"steps":        len(state.RoundSteps),
		"tool_results": totalToolCalls,
	})

	// Build messages with all context
	systemPrompt := e.buildSystemPrompt(ctx)
	userTurn := e.RenderUserTurnContent(sessionID, query)

	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userTurn},
	}

	// Add all tool call results as context
	toolResultCount := 0
	for stepIdx, step := range state.RoundSteps {
		for toolIdx, toolCall := range step.ToolCalls {
			toolResultCount++
			messages = append(messages, chat.Message{
				Role:    "user",
				Content: fmt.Sprintf("工具 %s 返回：%s", toolCall.Name, toolCall.Result.Output),
			})
			logger.Debugf(ctx, "[Agent][FinalAnswer] Added tool result [Step-%d][Tool-%d]: %s (output: %d chars)",
				stepIdx+1, toolIdx+1, toolCall.Name, len(toolCall.Result.Output))
		}
	}

	logger.Debugf(ctx, "[Agent][FinalAnswer] Built context: %d messages, %d tool results",
		len(messages), toolResultCount)

	// Add final answer prompt
	finalPrompt := fmt.Sprintf(`基于以上工具调用结果，为用户问题生成完整回答。

用户问题：%s

要求：
1. 基于实际检索到的内容回答
2. 清楚标注信息来源（chunk_id、文档名）
3. 用结构化格式组织回答
4. 如果信息不足，请如实说明
5. 重要：使用与用户问题相同的语言回答

现在生成最终回答：`, query)

	messages = append(messages, chat.Message{
		Role:    "user",
		Content: finalPrompt,
	})

	// Generate a single ID for this entire final answer stream
	answerID := generateEventID("answer")
	logger.Debugf(ctx, "[Agent][FinalAnswer] AnswerID: %s", answerID)
	answerDoneEmitted := false

	llmResult, err := e.streamLLMToEventBus(
		ctx,
		messages,
		&chat.ChatOptions{Temperature: e.config.Temperature}, // Thinking disabled for final answer synthesis
		func(chunk *types.StreamResponse, fullContent string) {
			// Defensive filter: only emit answer content, skip thinking chunks
			if chunk.ResponseType == types.ResponseTypeThinking {
				return
			}
			if chunk.Content != "" {
				logger.Debugf(ctx, "[Agent][FinalAnswer] Emitting answer chunk: %d chars", len(chunk.Content))
				e.eventBus.Emit(ctx, event.Event{
					ID:        answerID,
					Type:      event.EventAgentFinalAnswer,
					SessionID: sessionID,
					Data: event.AgentFinalAnswerData{
						Content: chunk.Content,
						Done:    chunk.Done,
					},
				})
				if chunk.Done {
					answerDoneEmitted = true
				}
			}
		},
	)
	if err != nil {
		logger.Errorf(ctx, "[Agent][FinalAnswer] Final answer generation failed: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_stream_failed", map[string]interface{}{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		return err
	}

	if !answerDoneEmitted {
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})
	}

	// Safety net: strip any residual <think> blocks that may have leaked through
	fullAnswer := agenttools.StripThinkBlocks(llmResult.Content)
	logger.Infof(ctx, "[Agent][FinalAnswer] Final answer generated: %d characters", len(fullAnswer))
	common.PipelineInfo(ctx, "Agent", "final_answer_done", map[string]interface{}{
		"session_id": sessionID,
		"answer_len": len(fullAnswer),
	})
	state.FinalAnswer = fullAnswer
	return nil
}

// handleMaxIterations generates a final answer when the agent loop exhausted all iterations
// without the LLM producing a natural stop. It marks state.IsComplete = true.
func (e *AgentEngine) handleMaxIterations(
	ctx context.Context, query string, state *types.AgentState, sessionID string,
) {
	logger.Info(ctx, "Reached max iterations, generating final answer")
	common.PipelineWarn(ctx, "Agent", "max_iterations_reached", map[string]interface{}{
		"iterations": state.CurrentRound,
		"max":        e.config.MaxIterations,
	})

	// Stream final answer generation through EventBus
	if err := e.streamFinalAnswerToEventBus(ctx, query, state, sessionID); err != nil {
		logger.Errorf(ctx, "Failed to synthesize final answer: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_failed", map[string]interface{}{
			"error": err.Error(),
		})
		state.FinalAnswer = "抱歉，我无法生成完整回答。"
	}
	state.IsComplete = true
}

// emitCompletionEvent emits the EventAgentComplete event with execution summary.
func (e *AgentEngine) emitCompletionEvent(
	ctx context.Context, state *types.AgentState, sessionID, messageID string, startTime time.Time,
) {
	// Convert knowledge refs to interface{} slice for event data
	knowledgeRefsInterface := make([]interface{}, 0, len(state.KnowledgeRefs))
	for _, ref := range state.KnowledgeRefs {
		knowledgeRefsInterface = append(knowledgeRefsInterface, ref)
	}

	e.eventBus.Emit(ctx, event.Event{
		ID:        generateEventID("complete"),
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		Data: event.AgentCompleteData{
			FinalAnswer:     state.FinalAnswer,
			KnowledgeRefs:   knowledgeRefsInterface,
			AgentSteps:      state.RoundSteps, // Include detailed execution steps for message storage
			TotalSteps:      len(state.RoundSteps),
			TotalDurationMs: time.Since(startTime).Milliseconds(),
			MessageID:       messageID, // Include message ID for proper message update
		},
	})

	logger.Infof(ctx, "Agent execution completed in %d rounds", state.CurrentRound)
}
