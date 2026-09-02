package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
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
	contextMessages ...[]chat.Message,
) error {
	e.syncCitationReferences(state)
	totalToolCalls := countTotalToolCalls(state.RoundSteps)
	logger.Infof(ctx, "[Agent][FinalAnswer] Synthesizing from %d steps, %d tool calls",
		len(state.RoundSteps), totalToolCalls)
	common.PipelineInfo(ctx, "Agent", "final_answer_start", map[string]interface{}{
		"session_id":   sessionID,
		"query":        query,
		"steps":        len(state.RoundSteps),
		"tool_results": totalToolCalls,
	})

	// Preserve the exact bounded conversation context used by the ReAct loop.
	// Final synthesis is a continuation of that turn, not a new one: rebuilding
	// only system + current query here would discard the user-authored state that
	// a long-context answer may need. The variadic argument keeps direct callers
	// and recovery paths that do not have a snapshot backwards-compatible.
	var messages []chat.Message
	if len(contextMessages) > 0 && len(contextMessages[0]) > 0 {
		messages = append([]chat.Message(nil), contextMessages[0]...)
	} else {
		messages = []chat.Message{
			{Role: "system", Content: e.buildSystemPrompt(ctx)},
			{Role: "user", Content: e.RenderUserTurnContent(sessionID, query)},
		}
	}
	citationContext, citationRefs := prepareFinalAnswerCitationContext(state)
	if len(citationRefs) > 0 {
		e.eventBus.Emit(ctx, event.Event{
			ID:        generateEventID("references"),
			Type:      event.EventAgentReferences,
			SessionID: sessionID,
			Data: event.AgentReferencesData{
				References: citationRefs,
			},
		})
	}

	// Direct callers without a loop snapshot still need the accumulated tool
	// evidence. Loop callers already have correctly paired assistant/tool
	// messages, so duplicating them as synthetic user claims would weaken role
	// provenance and waste context.
	toolResultCount := 0
	if len(contextMessages) == 0 || len(contextMessages[0]) == 0 {
		for stepIdx, step := range state.RoundSteps {
			for toolIdx, toolCall := range step.ToolCalls {
				if toolCall.Result == nil {
					continue
				}
				toolResultCount++
				messages = append(messages, chat.Message{
					Role:    "user",
					Content: fmt.Sprintf("Tool %s returned: %s", toolCall.Name, toolCall.Result.Output),
				})
				logger.Debugf(ctx, "[Agent][FinalAnswer] Added tool result [Step-%d][Tool-%d]: %s (output: %d chars)",
					stepIdx+1, toolIdx+1, toolCall.Name, len(toolCall.Result.Output))
			}
		}
	} else {
		toolResultCount = totalToolCalls
	}

	if citationContext != "" {
		messages = append(messages, chat.Message{
			Role: "user",
			Content: "Citation handles available for the final answer:\n" +
				citationContext +
				"\nUse these handles only when the cited statement is directly supported by the matching context.",
		})
	}

	logger.Debugf(ctx, "[Agent][FinalAnswer] Built context: %d messages, %d tool results",
		len(messages), toolResultCount)

	// Add final answer prompt
	finalPrompt := fmt.Sprintf(`Based on the conversation context and any tool call results above, generate a complete answer for the user's current question.

User question: %s

Requirements:
1. For conversation state, use only user-authored facts and preserve questions, requests, proposals, hypotheticals, negations, and unknown values as such. For external claims, use actually retrieved evidence.
2. When AVAILABLE_CITATIONS are provided, copy the matching cite_exactly value verbatim immediately after each directly supported sentence or paragraph. Each document source is one specific fragment, so choose the fragment that supports the adjacent claim and use a reasonable minimum.
3. Organize the answer in a structured format
4. If information is insufficient, honestly state so
5. IMPORTANT: Respond in the same language as the user's question

Now generate the final answer:`, query)
	finalPrompt = sourcerefs.PlaceTerminalCitationInstruction(finalPrompt, citationRefs)
	if outputDirective := conversationmemory.TerminalGenerationDirective(); outputDirective != "" {
		finalPrompt += "\n\n" + outputDirective
	}

	messages = append(messages, chat.Message{
		Role:    "user",
		Content: finalPrompt,
	})

	// Generate a single ID for this entire final answer stream
	answerID := generateEventID("answer")
	logger.Debugf(ctx, "[Agent][FinalAnswer] AnswerID: %s", answerID)
	thinking := false
	answerOptions := &chat.ChatOptions{
		Temperature:         e.config.Temperature,
		MaxCompletionTokens: e.config.MaxCompletionTokens,
		Thinking:            &thinking,
	}
	generateCandidate := func(candidateMessages []chat.Message) (string, error) {
		projector := conversationmemory.NewTerminalAnswerProjector()
		var projected strings.Builder
		llmResult, err := e.streamLLMToEventBus(
			ctx,
			candidateMessages,
			answerOptions,
			func(chunk *types.StreamResponse, fullContent string) {
				if chunk.ResponseType == types.ResponseTypeThinking {
					return
				}
				if chunk.Content != "" {
					projected.WriteString(projector.Feed(chunk.Content))
				}
			},
		)
		if err != nil {
			return "", err
		}
		projected.WriteString(projector.Flush())
		answer := strings.TrimSpace(projected.String())
		if answer == "" {
			answer = conversationmemory.ProjectTerminalAnswer(
				agenttools.StripThinkBlocks(llmResult.Content),
			)
		}
		return answer, nil
	}

	fullAnswer, err := generateCandidate(messages)
	if err != nil {
		logger.Errorf(ctx, "[Agent][FinalAnswer] Final answer generation failed: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_stream_failed", map[string]interface{}{
			"session_id": sessionID,
			"error":      err.Error(),
		})
		return err
	}
	if reason := conversationmemory.TerminalAnswerIntegrityReason(fullAnswer); reason != "" {
		logger.Warnf(ctx, "[Agent][FinalAnswer] Terminal answer integrity failure (%s), retrying once", reason)
		common.PipelineWarn(ctx, "Agent", "final_answer_integrity_retry", map[string]interface{}{
			"session_id": sessionID,
			"reason":     reason,
			"attempt":    1,
		})
		retryMessages := append([]chat.Message(nil), messages...)
		retryMessages = append(retryMessages, chat.Message{
			Role:    "user",
			Content: conversationmemory.TerminalIntegrityRetryDirective(),
		})
		retryMessages = agenttools.SanitizeMessages(retryMessages)
		repaired, retryErr := generateCandidate(retryMessages)
		if retryErr == nil && conversationmemory.TerminalAnswerIntegrityReason(repaired) == "" {
			fullAnswer = repaired
		} else {
			fullAnswer = conversationmemory.TerminalIntegrityFallback(
				types.LanguageNameFromContext(ctx),
			)
		}
	}
	// Canonical citation tags are a machine protocol. Filter unsupported,
	// malformed, or prior-turn handles before the production candidate is sent
	// anywhere, while leaving all claim text untouched. This keeps SSE,
	// persistence, and history replay byte-equivalent without an extra model
	// call or any Eval-only rewrite.
	filteredAnswer, citedRefs, citationReport := sourcerefs.FilterAnswerCitations(fullAnswer, citationRefs)
	fullAnswer = strings.TrimSpace(filteredAnswer)
	state.KnowledgeRefs = citedRefs
	if citationReport.ForbiddenTags > 0 || citationReport.IncompleteTags > 0 || len(citationReport.UnknownIDs) > 0 {
		logger.Warnf(ctx, "[Agent][Citations] filtered invalid terminal citation protocol: forbidden=%d incomplete=%d unknown=%v",
			citationReport.ForbiddenTags, citationReport.IncompleteTags, citationReport.UnknownIDs)
	}

	logger.Debugf(ctx, "[Agent][FinalAnswer] Emitting validated answer: %d chars", len(fullAnswer))
	e.eventBus.Emit(ctx, event.Event{
		ID:        answerID,
		Type:      event.EventAgentFinalAnswer,
		SessionID: sessionID,
		Data: event.AgentFinalAnswerData{
			Content: fullAnswer,
			Done:    false,
		},
	})
	e.eventBus.Emit(ctx, event.Event{
		ID:        answerID,
		Type:      event.EventAgentFinalAnswer,
		SessionID: sessionID,
		Data: event.AgentFinalAnswerData{
			Done: true,
		},
	})
	logger.Infof(ctx, "[Agent][FinalAnswer] Final answer generated: %d characters", len(fullAnswer))
	common.PipelineInfo(ctx, "Agent", "final_answer_done", map[string]interface{}{
		"session_id": sessionID,
		"answer_len": len(fullAnswer),
	})
	state.FinalAnswer = fullAnswer
	return nil
}

func prepareFinalAnswerCitationContext(state *types.AgentState) (string, []*types.SearchResult) {
	if state == nil {
		return "", nil
	}

	refs := state.KnowledgeRefs
	if len(refs) == 0 {
		refs = collectToolSourceReferences(state)
	}
	if len(refs) == 0 {
		return "", nil
	}
	missingHandle := false
	for _, ref := range refs {
		if sourcerefs.CitationID(ref) == "" {
			missingHandle = true
			break
		}
	}
	if missingHandle {
		sourcerefs.AssignCitationIDs(refs)
	}
	mergeStateKnowledgeReferences(state, refs)
	return renderFinalAnswerCitationContext(refs), refs
}

func collectToolSourceReferences(state *types.AgentState) []*types.SearchResult {
	refs := make([]*types.SearchResult, 0)
	seen := make(map[string]bool)
	for _, step := range state.RoundSteps {
		for _, toolCall := range step.ToolCalls {
			if toolCall.Result == nil {
				continue
			}
			for _, ref := range sourcerefs.ExtractFromToolResult(toolCall.Name, toolCall.Result) {
				if !sourcerefs.IsSupportedCitationReference(ref) {
					continue
				}
				key := sourcerefs.ReferenceKey(ref)
				if key == "" || seen[key] {
					continue
				}
				seen[key] = true
				refs = append(refs, ref)
			}
		}
	}
	return refs
}

func mergeStateKnowledgeReferences(state *types.AgentState, refs []*types.SearchResult) {
	if state == nil || len(refs) == 0 {
		return
	}
	seen := make(map[string]bool, len(state.KnowledgeRefs)+len(refs))
	for _, ref := range state.KnowledgeRefs {
		if key := sourcerefs.ReferenceKey(ref); key != "" {
			seen[key] = true
		}
	}
	for _, ref := range refs {
		key := sourcerefs.ReferenceKey(ref)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		state.KnowledgeRefs = append(state.KnowledgeRefs, ref)
	}
}

func renderFinalAnswerCitationContext(refs []*types.SearchResult) string {
	// Every claim-bearing tool result is already present in the final-answer
	// message list and carries its adjacent canonical handle. Repeating all
	// evidence here doubled prompt tokens and final-answer latency. The compact
	// catalog is sufficient to remind the model which opaque handles exist.
	return sourcerefs.RenderCitationCatalog(refs)
}

// handleMaxIterations generates a final answer when the agent loop exhausted all iterations
// without the LLM producing a natural stop. It marks state.IsComplete = true.
func (e *AgentEngine) handleMaxIterations(
	ctx context.Context, query string, state *types.AgentState, sessionID string,
	contextMessages ...[]chat.Message,
) {
	logger.Info(ctx, "Reached max iterations, generating final answer")
	common.PipelineWarn(ctx, "Agent", "max_iterations_reached", map[string]interface{}{
		"iterations": state.CurrentRound,
		"max":        e.config.MaxIterations,
	})

	// Stream final answer generation through EventBus
	if err := e.streamFinalAnswerToEventBus(ctx, query, state, sessionID, contextMessages...); err != nil {
		logger.Errorf(ctx, "Failed to synthesize final answer: %v", err)
		common.PipelineError(ctx, "Agent", "final_answer_failed", map[string]interface{}{
			"error": err.Error(),
		})
		state.FinalAnswer = "Sorry, I was unable to generate a complete answer."
	}
	state.IsComplete = true
}

// emitCompletionEvent emits the EventAgentComplete event with execution summary.
func (e *AgentEngine) emitCompletionEvent(
	ctx context.Context, state *types.AgentState, sessionID, messageID string, startTime time.Time,
) {
	e.syncCitationReferences(state)
	// The exact text already streamed to the user is the immutable production
	// candidate. Completion-time citation accounting may narrow the reference
	// registry, but must not rewrite that text before persistence/history replay.
	_, citedRefs, report := sourcerefs.FilterAnswerCitations(state.FinalAnswer, state.KnowledgeRefs)
	if report.ForbiddenTags > 0 || report.IncompleteTags > 0 || len(report.UnknownIDs) > 0 {
		logger.Warnf(ctx, "[Agent][Citations] observed invalid citation protocol: forbidden=%d incomplete=%d unknown=%v",
			report.ForbiddenTags, report.IncompleteTags, report.UnknownIDs)
	}
	if report.EvidenceAvailableUncited {
		logger.Warnf(ctx, "[Agent][Citations] final answer omitted all current-turn citation handles: available=%d",
			report.AvailableCount)
	}
	state.KnowledgeRefs = citedRefs
	e.eventBus.Emit(ctx, event.Event{
		ID:        generateEventID("complete"),
		Type:      event.EventAgentComplete,
		SessionID: sessionID,
		Data: event.AgentCompleteData{
			FinalAnswer:                state.FinalAnswer,
			KnowledgeRefs:              citedRefs,
			KnowledgeRefsAuthoritative: true,
			AgentSteps:                 state.RoundSteps, // Include detailed execution steps for message storage
			TotalSteps:                 len(state.RoundSteps),
			TotalDurationMs:            time.Since(startTime).Milliseconds(),
			MessageID:                  messageID, // Include message ID for proper message update
		},
	})

	logger.Infof(ctx, "Agent execution completed in %d rounds", state.CurrentRound)
}
