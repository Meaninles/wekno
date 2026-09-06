package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/common"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// responseVerdict captures the result of analyzing an LLM response to determine
// whether the agent loop should stop and what the final answer is (if any).
type responseVerdict struct {
	isDone       bool
	finalAnswer  string
	emptyContent bool // LLM returned stop with no tool calls and empty content
	step         types.AgentStep
}

// analyzeResponse inspects the LLM response for stop conditions:
//   - finish_reason == "stop"/"end_turn" with no tool calls → agent is done
//   - finish_reason == "content_filter" with no tool calls → agent is done (content filtered)
//
// The agent ends a turn by stopping naturally with its answer as plain
// assistant text (there is no dedicated final_answer tool). Any round that
// still requests tool calls is non-terminal and the caller continues the loop.
// It returns a responseVerdict. If isDone is true the caller should break out of the loop.
func (e *AgentEngine) analyzeResponse(
	ctx context.Context, response *types.ChatResponse,
	step types.AgentStep, iteration int, sessionID string, roundStart time.Time,
) responseVerdict {
	// Case 0: Content was blocked by the model's content filter.
	// Treat this as a terminal condition to avoid an infinite loop where
	// the same filtered response accumulates in the context.
	if response.FinishReason == "content_filter" && len(response.ToolCalls) == 0 {
		logger.Warnf(ctx, "[Agent][Round-%d] Content filter triggered, stopping agent loop (content=%d chars)",
			iteration+1, len(response.Content))
		common.PipelineWarn(ctx, "Agent", "content_filter_stop", map[string]interface{}{
			"iteration":   iteration,
			"round":       iteration + 1,
			"content_len": len(response.Content),
		})

		answer := response.Content
		if answer == "" {
			answer = "Sorry, this request was blocked by the content safety policy. Please try rephrasing your question."
		}

		answerID := generateEventID("answer")
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: answer,
				Done:    false,
			},
		})
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})

		return responseVerdict{
			isDone:      true,
			finalAnswer: answer,
			step:        step,
		}
	}

	// Case 1: LLM stopped naturally without requesting any tool calls
	if isSuccessfulTerminalFinishReason(response.FinishReason) && len(response.ToolCalls) == 0 {
		// Strip <think>…</think> blocks that some models embed in content
		// (DeepSeek, Qwen, etc.) before processing or displaying.
		response.Content = agenttools.StripThinkBlocks(response.Content)
		logger.Infof(ctx, "[Agent][Round-%d] Agent finished naturally: answer=%d chars, duration=%dms",
			iteration+1, len(response.Content), time.Since(roundStart).Milliseconds())
		common.PipelineInfo(ctx, "Agent", "round_final_answer", map[string]interface{}{
			"iteration":  iteration,
			"round":      iteration + 1,
			"answer_len": len(response.Content),
		})

		// Emit the final answer. The answer text reaches the UI by one of two
		// paths:
		//   (a) Already streamed live during the think phase — the common case
		//       now that plain assistant content is routed straight to
		//       EventAgentFinalAnswer (response.AnswerStreamed). Re-emitting the
		//       full content here would render it twice and produce the
		//       end-of-stream "jump from Thinking to Answer" the user reported,
		//       so we only close the existing stream with a Done marker on the
		//       same event ID.
		//   (b) Not streamed live (e.g. the content only surfaced in the
		//       accumulated result) — emit the full content, then Done.
		var answerID string
		if response.AnswerStreamed && response.AnswerEventID != "" {
			answerID = response.AnswerEventID
		} else {
			answerID = generateEventID("answer")
			if response.Content != "" {
				e.eventBus.Emit(ctx, event.Event{
					ID:        answerID,
					Type:      event.EventAgentFinalAnswer,
					SessionID: sessionID,
					Data: event.AgentFinalAnswerData{
						Content: response.Content,
						Done:    false,
					},
				})
			}
		}
		e.eventBus.Emit(ctx, event.Event{
			ID:        answerID,
			Type:      event.EventAgentFinalAnswer,
			SessionID: sessionID,
			Data: event.AgentFinalAnswerData{
				Content: "",
				Done:    true,
			},
		})

		return responseVerdict{
			isDone:       true,
			finalAnswer:  response.Content,
			emptyContent: response.Content == "",
			step:         step,
		}
	}

	// Any round that still requests tool calls is non-terminal: the caller
	// executes the tools and loops again. The agent only ends by stopping
	// naturally (Case 1) with its answer as plain assistant text.
	return responseVerdict{isDone: false, step: step}
}

// indentLines prefixes every line of s with indent. Used to nest pre-rendered
// XML blocks inside the `<runtime_context>` envelope without losing readability.
func indentLines(s, indent string) string {
	if s == "" {
		return ""
	}
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		if line == "" {
			continue
		}
		lines[i] = indent + line
	}
	return strings.Join(lines, "\n")
}

// escapeXMLAttr escapes a string for safe inclusion in an XML attribute value.
// Titles and names may contain user-supplied characters like <, >, &, ".
func escapeXMLAttr(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, "\"", "&quot;")
	return s
}

// buildRuntimeContextBlock builds a metadata block with session info and the
// *active retrieval scope for this turn only*. It is injected
// into the current user message for the LLM call and is not persisted into
// conversation history — replayed user turns keep bare Content so stale scope
// snapshots do not steer follow-up questions.
//
// Emitted as an XML-ish block (not free prose) so it is a visually distinct,
// non-instruction envelope that is hard to conflate with user text and
// prompt-injection-safe.
func buildRuntimeContextBlock(
	sessionID string,
	kbs []*KnowledgeBaseInfo,
	docs []*SelectedDocumentInfo,
) string {
	var sb strings.Builder
	sb.WriteString("<runtime_context scope=\"this_turn\">\n")
	fmt.Fprintf(&sb, "  <session>%s</session>\n", escapeXMLAttr(sessionID))

	if len(kbs) > 0 {
		// Render the full bound-KB detail (capabilities + recent docs) so the
		// model has everything it needs to route its retrieval in one place.
		// `formatKnowledgeBaseList` already emits a `<knowledge_bases>…</knowledge_bases>`
		// envelope; we wrap it in `<bound_knowledge_bases>` to make the scope
		// semantics explicit and to match the naming the prompt templates use
		// when referring back to this block.
		sb.WriteString("  <bound_knowledge_bases>\n")
		sb.WriteString(indentLines(formatKnowledgeBaseList(kbs), "    "))
		sb.WriteString("\n  </bound_knowledge_bases>\n")
	}

	if len(docs) > 0 {
		sb.WriteString("  <pinned_documents scope=\"authoritative_for_this_turn\">\n")
		for _, d := range docs {
			if d == nil {
				continue
			}
			title := d.Title
			if title == "" {
				title = d.FileName
			}
			if title == "" {
				title = d.KnowledgeID
			}
			if d.FileType != "" {
				fmt.Fprintf(&sb, "    <document knowledge_id=\"%s\" title=\"%s\" file_type=\"%s\" />\n",
					escapeXMLAttr(d.KnowledgeID), escapeXMLAttr(title), escapeXMLAttr(d.FileType))
			} else {
				fmt.Fprintf(&sb, "    <document knowledge_id=\"%s\" title=\"%s\" />\n",
					escapeXMLAttr(d.KnowledgeID), escapeXMLAttr(title))
			}
		}
		sb.WriteString("  </pinned_documents>\n")
		sb.WriteString("  <note>Retrieval scope is limited to these selected documents.</note>\n")
	}

	sb.WriteString("</runtime_context>")
	return sb.String()
}

// buildMustUseBlock emits a short per-turn hint when the user @mentioned MCP/Skill.
// Tool names are not listed here — they are already in the function-calling schema.
func buildMustUseBlock(mcpServices []*PinnedMCPServiceInfo, skills []*PinnedSkillInfo) string {
	var lines []string
	for _, svc := range mcpServices {
		if svc == nil {
			continue
		}
		prefix := mcpToolNamePrefix(svc)
		if prefix == "" {
			continue
		}
		display := sanitizeMustUseField(svc.Name)
		if display == "" {
			display = sanitizeMustUseField(svc.ID)
		}
		lines = append(lines, fmt.Sprintf("Must use MCP tools whose names start with %s (@%s) to answer the question below.", prefix, display))
	}
	for _, skill := range skills {
		if skill == nil || skill.Name == "" {
			continue
		}
		name := sanitizeMustUseField(skill.Name)
		lines = append(lines, fmt.Sprintf("Must call read_skill(skill_name=\"%s\") for @Skill \"%s\" before answering.", name, name))
	}
	if len(lines) == 0 {
		return ""
	}
	return "<must_use>\n" + strings.Join(lines, "\n") + "\n</must_use>"
}

// sanitizeMustUseField strips newlines and angle brackets so an MCP/skill name
// cannot break out of the <must_use> block or inject extra instruction lines.
func sanitizeMustUseField(s string) string {
	replacer := strings.NewReplacer("\n", " ", "\r", " ", "<", " ", ">", " ")
	return strings.TrimSpace(replacer.Replace(s))
}

// mcpToolNamePrefix returns the shared prefix for an MCP service's registered
// tools (e.g. mcp_iwiki_ from mcp_iwiki_getdocument). Tool names are
// mcp_{sanitized_service_name}_{tool}, and the service slug itself may contain
// underscores (sanitizeName turns spaces/hyphens into "_"), so we take the
// longest common prefix across the service's tools and trim it back to the last
// segment boundary instead of naively cutting at the first underscore.
func mcpToolNamePrefix(svc *PinnedMCPServiceInfo) string {
	if svc == nil || len(svc.ToolNames) == 0 {
		return ""
	}
	const head = "mcp_"
	var mcpNames []string
	for _, toolName := range svc.ToolNames {
		if strings.HasPrefix(toolName, head) {
			mcpNames = append(mcpNames, toolName)
		}
	}
	if len(mcpNames) == 0 {
		return ""
	}
	prefix := mcpNames[0]
	for _, name := range mcpNames[1:] {
		prefix = commonStringPrefix(prefix, name)
	}
	// Trim to the last underscore so the hint names the service prefix
	// (mcp_{service}_) rather than a partial tool name.
	if idx := strings.LastIndex(prefix, "_"); idx >= len(head)-1 {
		prefix = prefix[:idx+1]
	}
	if len(prefix) <= len(head) {
		return ""
	}
	return prefix
}

func commonStringPrefix(a, b string) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	i := 0
	for i < n && a[i] == b[i] {
		i++
	}
	return a[:i]
}

// RenderUserTurnContent builds the user-turn payload for the current LLM call
// (runtime_context + must_use + current-task envelope). Used by Execute and finalize paths only;
// not written to rendered_content / history.
func (e *AgentEngine) RenderUserTurnContent(sessionID, query string) string {
	runtimeCtx := buildRuntimeContextBlock(sessionID, e.knowledgeBasesInfo, e.selectedDocs)
	mustUse := buildMustUseBlock(e.pinnedMCPServices, e.pinnedSkills)
	return composeUserTurnContent(runtimeCtx, mustUse, e.currentTurnContext, buildCurrentTaskBlocks(query))
}

func composeUserTurnContent(parts ...string) string {
	nonEmpty := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			nonEmpty = append(nonEmpty, part)
		}
	}
	return strings.Join(nonEmpty, "\n\n")
}

// buildCurrentTaskBlocks places the authoritative current request at the tail
// of the current user message, after all runtime metadata. Conversation history
// remains available for genuine follow-ups, but cannot silently replace a new
// topic merely because the previous answer was longer or more detailed.
//
// This is shared by every Go ReAct agent (built-in and future custom prompt
// types). It adds no model call and only a small, constant prompt prefix.
func buildCurrentTaskBlocks(query string) string {
	return "<user_request verbatim=\"true\">" + query + "</user_request>"
}

// listToolNames returns tool.function names for logging
func listToolNames(ts []chat.Tool) []string {
	names := make([]string, 0, len(ts))
	for _, t := range ts {
		names = append(names, t.Function.Name)
	}
	return names
}

// buildToolsForLLM builds the tools list for LLM function calling
func (e *AgentEngine) buildToolsForLLM() []chat.Tool {
	functionDefs := e.toolRegistry.GetFunctionDefinitions()
	tools := make([]chat.Tool, 0, len(functionDefs))
	for _, def := range functionDefs {
		tools = append(tools, chat.Tool{
			Type: "function",
			Function: chat.FunctionDef{
				Name:        def.Name,
				Description: def.Description,
				Parameters:  def.Parameters,
			},
		})
	}

	return tools
}

// appendToolResults adds tool results to the in-turn message history following
// OpenAI's tool-calling format. Cross-turn persistence is handled separately:
// the final AgentSteps are written to the assistant message by the SSE handler,
// and rebuilt from DB on the next turn by service.LoadAgentHistory.
func (e *AgentEngine) appendToolResults(
	messages []chat.Message,
	step types.AgentStep,
	currentQuery string,
) []chat.Message {
	// Add assistant message with tool calls (if any)
	if step.Thought != "" || len(step.ToolCalls) > 0 || step.ReasoningContent != "" {
		assistantMsg := chat.Message{
			Role:             "assistant",
			Content:          step.Thought,
			ReasoningContent: step.ReasoningContent,
		}

		// Add tool calls to assistant message (following OpenAI format)
		if len(step.ToolCalls) > 0 {
			assistantMsg.ToolCalls = make([]chat.ToolCall, 0, len(step.ToolCalls))
			for _, tc := range step.ToolCalls {
				// Convert arguments back to JSON string
				argsJSON, _ := json.Marshal(tc.Args)

				assistantMsg.ToolCalls = append(assistantMsg.ToolCalls, chat.ToolCall{
					ID:               tc.ID,
					Type:             "function",
					ProviderMetadata: tc.ProviderMetadata,
					Function: chat.FunctionCall{
						Name:      tc.Name,
						Arguments: string(argsJSON),
					},
				})
			}
		}

		messages = append(messages, assistantMsg)
	}

	// Add tool result messages (role: "tool", following OpenAI format)
	for _, toolCall := range step.ToolCalls {
		resultContent := toolCall.Result.Output
		if !toolCall.Result.Success {
			resultContent = fmt.Sprintf("Error: %s", toolCall.Result.Error)
		}

		toolMsg := chat.Message{
			Role:       "tool",
			Content:    resultContent,
			ToolCallID: toolCall.ID,
			Name:       toolCall.Name,
		}

		messages = append(messages, toolMsg)
	}

	return messages
}

// countTotalToolCalls counts total tool calls across all steps
func countTotalToolCalls(steps []types.AgentStep) int {
	total := 0
	for _, step := range steps {
		total += len(step.ToolCalls)
	}
	return total
}

// buildMessagesWithLLMContext builds the message array with LLM context
func (e *AgentEngine) buildMessagesWithLLMContext(
	systemPrompt, currentQuery, sessionID string,
	llmContext []chat.Message,
	imageURLs []string,
) []chat.Message {
	messages := []chat.Message{
		{Role: "system", Content: systemPrompt},
	}

	if len(llmContext) > 0 {
		for _, msg := range llmContext {
			if msg.Role == "system" {
				continue
			}
			if msg.Role == "user" || msg.Role == "assistant" || msg.Role == "tool" {
				messages = append(messages, msg)
			}
		}
	}

	// Build user message with per-turn scope envelopes (current turn only).
	// Historical user messages in llmContext stay as bare Content from the DB.
	userMsg := chat.Message{
		Role:    "user",
		Content: e.RenderUserTurnContent(sessionID, currentQuery),
		Images:  imageURLs,
	}
	messages = append(messages, userMsg)

	return messages
}
