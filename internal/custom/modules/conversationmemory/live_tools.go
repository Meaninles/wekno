package conversationmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"strings"
	"sync"
)

type liveToolKey struct{}
type LiveTools struct {
	mu           sync.RWMutex
	outputs      map[string]string
	recoveryUsed bool
}

func WithLiveTools(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveToolKey{}, &LiveTools{outputs: map[string]string{}})
}
func LiveToolsFromContext(ctx context.Context) *LiveTools {
	a, _ := ctx.Value(liveToolKey{}).(*LiveTools)
	return a
}
func (a *LiveTools) Read(id string) (string, bool) {
	if a == nil {
		return "", false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	v, ok := a.outputs[id]
	return v, ok
}

// BoundMessages retains exact user messages and protocol pairs. Only old tool
// payloads can be replaced by explicit run-scoped read handles. No model-written
// summary, generated "fact" or hidden semantic rewrite enters the transcript.
func BoundMessages(ctx context.Context, messages []chat.Message, tools []chat.Tool, maxContext, outputReserve int) ([]chat.Message, error) {
	archive := LiveToolsFromContext(ctx)
	out := append([]chat.Message(nil), messages...)
	canRead := false
	for _, tool := range tools {
		canRead = canRead || tool.Function.Name == "read_conversation"
	}
	// Bound each oversized result before it fills the window. The exact payload
	// and source identifiers remain in the authorized run archive. Paged reads
	// must not be offloaded again, otherwise the model can never reach the text.
	if archive != nil && canRead {
		archive.mu.Lock()
		for i, m := range out {
			if m.Role != "tool" || m.ToolCallID == "" || m.Name == "read_conversation" {
				continue
			}
			runes := []rune(m.Content)
			if len(runes) <= 24000 {
				continue
			}
			if _, exists := archive.outputs[m.ToolCallID]; !exists {
				archive.outputs[m.ToolCallID] = m.Content
			}
			handle, _ := json.Marshal(map[string]any{"archived_tool_result": true, "complete": false,
				"read_tool": "read_conversation", "section": "tools", "tool_call_id": m.ToolCallID,
				"offset": 0, "total_characters": len(runes), "preview": string(runes[:2000]),
				"notice": "Preview only. Read the exact archived result for omitted evidence and details."})
			out[i].Content = string(handle)
		}
		archive.mu.Unlock()
	}
	if maxContext <= 0 {
		return out, nil
	}
	budget := maxContext - max(0, outputReserve)
	cost := func(rows []chat.Message) int {
		imageCost := 0
		textRows := append([]chat.Message(nil), rows...)
		for i, row := range textRows {
			imageCost += len(row.Images) * 4096
			textRows[i].Images = nil
			textRows[i].MultiContent = nil
			for _, part := range row.MultiContent {
				if part.Type == "image_url" {
					imageCost += 4096
				} else {
					textRows[i].MultiContent = append(textRows[i].MultiContent, part)
				}
			}
		}
		b, _ := json.Marshal(struct {
			Messages []chat.Message
			Tools    []chat.Tool
		}{textRows, tools})
		return EstimateTokens(string(b)) + imageCost
	}
	if cost(out) <= budget {
		return out, nil
	}
	if archive == nil || !canRead {
		return nil, fmt.Errorf("context exceeds configured budget without a readable tool archive")
	}
	archive.mu.Lock()
	defer archive.mu.Unlock()
	for i, m := range out {
		if m.Role != "tool" || m.ToolCallID == "" || m.Name == "read_conversation" {
			continue
		}
		handle, _ := json.Marshal(map[string]any{"archived_tool_result": true, "read_tool": "read_conversation", "section": "tools", "tool_call_id": m.ToolCallID})
		if EstimateTokens(m.Content) <= EstimateTokens(string(handle)) {
			continue
		}
		if _, exists := archive.outputs[m.ToolCallID]; !exists {
			archive.outputs[m.ToolCallID] = m.Content
		}
		out[i].Content = string(handle)
		if cost(out) <= budget {
			return out, nil
		}
	}
	return nil, fmt.Errorf("user context and required protocol metadata exceed configured input budget %d", budget)
}

// ResponseRecoveryReason classifies provider completion, never generated prose.
func ResponseRecoveryReason(finish, text string, hasTools bool) string {
	switch strings.ToLower(strings.TrimSpace(finish)) {
	case "length", "max_tokens", "max_output_tokens":
		return "output_limit"
	case "stop", "end_turn":
		if !hasTools && strings.TrimSpace(text) == "" {
			return "empty_response"
		}
	}
	return ""
}

func RecoverResponse(ctx context.Context, messages []chat.Message, reason string) ([]chat.Message, bool) {
	archive := LiveToolsFromContext(ctx)
	if archive == nil || reason == "" || ctx.Err() != nil {
		return messages, false
	}
	archive.mu.Lock()
	defer archive.mu.Unlock()
	if archive.recoveryUsed {
		return messages, false
	}
	archive.recoveryUsed = true
	out := append([]chat.Message(nil), messages...)
	return append(out, chat.Message{Role: "user", Content: "Runtime status: the previous model response ended without a complete answer or executable tool call. Its incomplete output was not executed. Previously completed tools and their results remain available. Continue from those results with the next complete tool call or answer; do not repeat completed operations."}), true
}
