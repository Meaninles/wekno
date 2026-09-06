package service

import (
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/stretchr/testify/assert"
)

// Request-local evidence envelopes must not be replayed into later turns.
func TestBuildUserHistoryMessage_IgnoresRenderedEvidence(t *testing.T) {
	msg := &types.Message{
		Role:            "user",
		Content:         "what about the chart?",
		RenderedContent: "what about the chart? [augmented]",
		Images: types.MessageImages{
			{Caption: "a bar chart"},
		},
	}
	got := buildUserHistoryMessage(msg)
	assert.Equal(t, "user", got.Role)
	assert.Contains(t, got.Content, "what about the chart?")
	assert.Contains(t, got.Content, `authority="model_derived_not_verbatim_user_text"`)
	assert.Contains(t, got.Content, "a bar chart")
	assert.NotContains(t, got.Content, "[augmented]")
}

func TestBuildUserHistoryMessage_FallsBackToContentWithCaptions(t *testing.T) {
	msg := &types.Message{
		Role:    "user",
		Content: "look at this",
		Images: types.MessageImages{
			{Caption: "a bar chart"},
			{Caption: "a pie chart"},
		},
	}
	got := buildUserHistoryMessage(msg)
	assert.Equal(t, "user", got.Role)
	assert.Contains(t, got.Content, "look at this")
	assert.Contains(t, got.Content, `authority="model_derived_not_verbatim_user_text"`)
	assert.Contains(t, got.Content, "a bar chart\na pie chart")
}

func TestBuildUserHistoryMessage_LabelsVerbatimSourceSeparately(t *testing.T) {
	msg := &types.Message{
		Role:    "user",
		Content: "the owner is Lin",
		Images:  types.MessageImages{{Caption: "diagram suggests a different owner"}},
	}

	got := buildUserHistoryMessage(msg, "user_turn_007")

	assert.Contains(t, got.Content, `<historical_user_input source_id="user_turn_007" authority="user_authored">`)
	assert.Contains(t, got.Content, "the owner is Lin")
	assert.Contains(t, got.Content, `authority="model_derived_not_verbatim_user_text"`)
}

// TestBuildUserHistoryMessage_AppendsAttachmentsWhenNoRenderedContent covers
// the Agent-mode multi-turn path: AgentQA does not persist RenderedContent, so
// the next turn's history must reconstruct the original attachment prompt from
// the stored Attachments column. Otherwise, follow-up questions like "what is
// in there?" lose all reference to the uploaded file.
func TestBuildUserHistoryMessage_AppendsAttachmentsWhenNoRenderedContent(t *testing.T) {
	msg := &types.Message{
		Role:    "user",
		Content: "summarize this",
		Attachments: types.MessageAttachments{
			{
				FileName: "report.pdf",
				FileType: ".pdf",
				FileSize: 2048,
				Content:  "hello world",
			},
		},
	}
	got := buildUserHistoryMessage(msg)
	assert.Equal(t, "user", got.Role)
	assert.Contains(t, got.Content, "summarize this")
	assert.Contains(t, got.Content, `<attachment index="1" name="report.pdf">`)
	assert.Contains(t, got.Content, "hello world")
	assert.Contains(t, got.Content, `authority="user_supplied_file_evidence_not_chat_assertion"`)
}

func TestBuildUserHistoryMessage_RenderedContentDoesNotReplaceCanonicalAttachment(t *testing.T) {
	msg := &types.Message{
		Role:            "user",
		Content:         "summarize this",
		RenderedContent: "summarize this [with retrieval context already included]",
		Attachments: types.MessageAttachments{
			{FileName: "report.pdf", FileType: ".pdf", Content: "hello"},
		},
	}
	got := buildUserHistoryMessage(msg)
	assert.Contains(t, got.Content, "summarize this")
	assert.NotContains(t, got.Content, "retrieval context")
	assert.Contains(t, got.Content, "<attachment")
}

// TestBuildAssistantHistoryMessages_NaturalFinishEmitsSingleAnswer covers the
// most common path: a turn with no tool calls (model answered directly). The
// result must be a single assistant message holding the canonical answer —
// duplicates would inflate token usage every turn.
func TestBuildAssistantHistoryMessages_NaturalFinishEmitsSingleAnswer(t *testing.T) {
	msg := &types.Message{
		Role:    "assistant",
		Content: "Hello, nice to meet you!",
		AgentSteps: types.AgentSteps{
			{Iteration: 0, Thought: "Hello, nice to meet you!", ToolCalls: nil},
		},
	}
	got := buildAssistantHistoryMessages(msg)
	if assert.Len(t, got, 1) {
		assert.Equal(t, "assistant", got[0].Role)
		assert.Contains(t, got[0].Content, `authority="model_output_not_evidence"`)
		assert.Contains(t, got[0].Content, "Hello, nice to meet you!")
		assert.Empty(t, got[0].ToolCalls)
	}
}

// TestBuildAssistantHistoryMessages_StripsThinkBlocks ensures the trailing
// final-answer assistant message has any <think>…</think> blocks stripped, so
// internal-reasoning text doesn't leak into the next turn's context.
func TestBuildAssistantHistoryMessages_StripsThinkBlocks(t *testing.T) {
	msg := &types.Message{
		Role:    "assistant",
		Content: "<think>plotting...</think>The answer is 42.",
	}
	got := buildAssistantHistoryMessages(msg)
	if assert.Len(t, got, 1) {
		assert.Contains(t, got[0].Content, "The answer is 42.")
		assert.NotContains(t, got[0].Content, "plotting")
	}
}

// TestBuildAssistantHistoryMessages_ToolCallsExpandIntoOpenAIShape covers the
// option-B replay: non-terminal tool calls from AgentSteps become proper
// assistant_with_tool_calls + tool messages, and the canonical final answer is
// appended last. final_answer entries are filtered because they're terminal
// signals — the trailing assistant message already carries the answer.

func TestRepairHistoryDoesNotReplayTools(t *testing.T) {
	m := &types.Message{ID: "answer-1", Content: "final response", AgentSteps: types.AgentSteps{{Thought: "old private thought", ToolCalls: []types.ToolCall{{ID: "call-1", Name: "read_file", Result: &types.ToolResult{Success: true, Output: "old full file"}}}}}}
	messages := buildAssistantHistoryMessages(m)
	if len(messages) != 1 || len(messages[0].ToolCalls) != 0 || strings.Contains(messages[0].Content, "old full file") || !strings.Contains(messages[0].Content, "assistant_message_answer-1") {
		t.Fatal(messages)
	}
	if len(m.AgentSteps) != 1 {
		t.Fatal("persisted record mutated")
	}
}
