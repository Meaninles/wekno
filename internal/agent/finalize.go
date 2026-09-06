package agent

import (
	"context"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

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
	completionExtra := map[string]interface{}{}
	if state.TerminalFinishReason != "" {
		completionExtra["terminal_finish_reason"] = state.TerminalFinishReason
	}
	if state.TerminalRecoveryReason != "" {
		completionExtra["terminal_recovery_reason"] = state.TerminalRecoveryReason
		completionExtra["terminal_recovery_attempts"] = state.TerminalRecoveryAttempts
		completionExtra["terminal_recovery_failed"] = state.TerminalRecoveryFailed
	}
	if len(completionExtra) == 0 {
		completionExtra = nil
	}
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
			Extra:                      completionExtra,
		},
	})

	logger.Infof(ctx, "Agent execution completed in %d rounds", state.CurrentRound)
}
