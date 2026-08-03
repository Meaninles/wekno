package agent

import (
	"context"
	"sync"

	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
)

// agentCitationState is run-scoped. AgentEngine instances are normally
// request-scoped, but reset keeps a reused engine from leaking handles across
// turns. Registration is serialized so parallel tool calls retain the
// caller-established result order.
type agentCitationState struct {
	mu       sync.Mutex
	registry *sourcerefs.Registry
	seen     map[string]bool
}

func (s *agentCitationState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.registry = sourcerefs.NewRegistry()
	s.seen = make(map[string]bool)
}

func (s *agentCitationState) register(
	toolName string,
	result *types.ToolResult,
) (refs, newRefs []*types.SearchResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registry == nil {
		s.registry = sourcerefs.NewRegistry()
	}
	var sources []*sourcerefs.CitationSource
	refs, sources = sourcerefs.RegisterToolResult(s.registry, toolName, result)
	sourcerefs.AttachToolResultSources(result, sources)
	for _, ref := range refs {
		key := sourcerefs.ReferenceKey(ref)
		if key == "" || s.seen[key] {
			continue
		}
		s.seen[key] = true
		newRefs = append(newRefs, ref)
	}
	return refs, newRefs
}

func (s *agentCitationState) snapshot() []*types.SearchResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.registry == nil {
		return nil
	}
	return s.registry.SnapshotReferences()
}

// exposeToolResultReferences is the native-agent registration point for the
// shared citation capability. It runs after a tool call and before that result
// is appended to model context or emitted to the UI.
func (e *AgentEngine) exposeToolResultReferences(
	ctx context.Context,
	sessionID, toolName string,
	result *types.ToolResult,
) {
	refs, newRefs := e.citationState.register(toolName, result)
	if len(refs) == 0 {
		return
	}
	result.Output = sourcerefs.AppendCitationCatalog(result.Output, refs)
	if len(newRefs) == 0 || e.eventBus == nil {
		return
	}
	e.eventBus.Emit(ctx, event.Event{
		Type:      event.EventAgentReferences,
		SessionID: sessionID,
		Data: event.AgentReferencesData{
			References: newRefs,
		},
	})
}

func (e *AgentEngine) syncCitationReferences(state *types.AgentState) []*types.SearchResult {
	if state == nil {
		return nil
	}
	refs := e.citationState.snapshot()
	if len(refs) > 0 {
		state.KnowledgeRefs = refs
	}
	return state.KnowledgeRefs
}

// prepareCitationAwareGenerationMessages places the current-turn terminal
// citation reminder at the final model-input boundary once evidence exists.
// It edits a shallow copy so persisted ReAct history and tool outputs remain
// unchanged, and it does not add another model request or alter tool choice.
func (e *AgentEngine) prepareCitationAwareGenerationMessages(messages []chat.Message) []chat.Message {
	refs := e.citationState.snapshot()
	if len(messages) == 0 || !sourcerefs.HasCitableReferences(refs) {
		return messages
	}
	out := append([]chat.Message(nil), messages...)
	last := len(out) - 1
	out[last].Content = sourcerefs.PlaceTerminalCitationInstruction(out[last].Content, refs)
	return out
}
