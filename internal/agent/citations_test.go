package agent

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

func TestNativeAgentNaturalCompletionKeepsCanonicalToolCitation(t *testing.T) {
	engine := &AgentEngine{eventBus: event.NewEventBus()}
	engine.citationState.reset()
	result := &types.ToolResult{
		Success: true,
		Output:  "工具返回：5.2.8规定投委会审议触发条件。",
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "doc-1",
			"knowledge_title": "投资管理办法.docx",
			"chunks": []map[string]interface{}{{
				"seq": 1, "chunk_id": "chunk-528", "knowledge_id": "doc-1",
				"knowledge_base_id": "kb-1", "content": "5.2.8规定投委会审议触发条件。",
			}},
		},
	}

	engine.exposeToolResultReferences(context.Background(), "session-1", "list_knowledge_chunks", result)
	if !strings.Contains(result.Output, `cite_exactly=<src id="S1" />`) {
		t.Fatalf("tool result did not expose canonical handle: %s", result.Output)
	}

	state := &types.AgentState{
		FinalAnswer: "触发条件如下。<src id=\"S1\" />\n不应保留旧标签。<kb doc=\"投资管理办法.docx\" />",
	}
	engine.emitCompletionEvent(context.Background(), state, "session-1", "message-1", time.Now())
	if !strings.Contains(state.FinalAnswer, `<src id="S1" />`) {
		t.Fatalf("canonical natural-stop citation was removed: %s", state.FinalAnswer)
	}
	if strings.Contains(state.FinalAnswer, "<kb") {
		t.Fatalf("legacy citation tag was not filtered: %s", state.FinalAnswer)
	}
	if len(state.KnowledgeRefs) != 1 || sourcerefs.CitationID(state.KnowledgeRefs[0]) != "S1" {
		t.Fatalf("completion references are not authoritative: %#v", state.KnowledgeRefs)
	}
}

func TestNativeAgentPlacesTerminalCitationInstructionAtGenerationBoundary(t *testing.T) {
	engine := &AgentEngine{eventBus: event.NewEventBus()}
	engine.citationState.reset()
	result := &types.ToolResult{
		Success: true,
		Output:  `<knowledge_chunks><chunk chunk_id="chunk-1"><content>证据</content></chunk></knowledge_chunks>`,
		Data: map[string]interface{}{
			"display_type":    "knowledge_chunks_list",
			"knowledge_id":    "doc-1",
			"knowledge_title": "制度.docx",
			"chunks": []map[string]interface{}{{
				"seq": 1, "chunk_id": "chunk-1", "knowledge_id": "doc-1",
				"knowledge_base_id": "kb-1", "content": "证据",
			}},
		},
	}
	engine.exposeToolResultReferences(context.Background(), "session-1", "list_knowledge_chunks", result)

	original := []chat.Message{
		{Role: "user", Content: "问题"},
		{Role: "tool", Content: result.Output, Name: "list_knowledge_chunks"},
	}
	prepared := engine.prepareCitationAwareGenerationMessages(original)
	if !strings.HasSuffix(prepared[len(prepared)-1].Content, sourcerefs.TerminalCitationInstruction()) {
		t.Fatalf("terminal citation instruction is not adjacent to generation: %s", prepared[len(prepared)-1].Content)
	}
	if strings.Count(prepared[len(prepared)-1].Content, "[CITATION_USE]") != 1 {
		t.Fatalf("terminal citation instruction was duplicated: %s", prepared[len(prepared)-1].Content)
	}
	if original[len(original)-1].Content != result.Output {
		t.Fatalf("generation preparation mutated persisted tool output")
	}

	preparedAgain := engine.prepareCitationAwareGenerationMessages(prepared)
	if preparedAgain[len(preparedAgain)-1].Content != prepared[len(prepared)-1].Content {
		t.Fatalf("generation preparation is not idempotent")
	}
}

func TestNativeAgentDoesNotAddTerminalCitationInstructionWithoutEvidence(t *testing.T) {
	engine := &AgentEngine{}
	engine.citationState.reset()
	messages := []chat.Message{{Role: "user", Content: "你好"}}
	prepared := engine.prepareCitationAwareGenerationMessages(messages)
	if prepared[0].Content != "你好" || strings.Contains(prepared[0].Content, "[CITATION_USE]") {
		t.Fatalf("no-evidence conversation was changed: %#v", prepared)
	}
}
