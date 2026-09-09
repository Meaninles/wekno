package agentruntime

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
)

func processEventID(runID string, item StreamEvent) string {
	return fmt.Sprintf("%s:%d:%s:%s", runID, item.Revision, item.Type, item.ID)
}

// Project the durable trace, never the SDK checkpoint, into display steps.
// Receipts remain the authority for outcomes; outbox sequence preserves the
// interleaving of reasoning, commentary and parallel tool starts.
func processSteps(db *gorm.DB, id string, calls []ToolReceipt) ([]types.AgentStep, error) {
	var records []RunOutbox
	if err := db.Where("run_id = ?", id).Order("seq").Find(&records).Error; err != nil {
		return nil, err
	}
	return projectProcessSteps(id, calls, records)
}

func projectProcessSteps(id string, calls []ToolReceipt, records []RunOutbox) ([]types.AgentStep, error) {
	steps := []types.AgentStep{}
	byID := map[string]int{}
	receipts := map[string]ToolReceipt{}
	for _, call := range calls {
		receipts[call.CallID] = call
	}
	appendCall := func(call ToolReceipt) error {
		key := "tool:" + call.CallID
		if _, exists := byID[key]; exists {
			return nil
		}
		var args map[string]any
		var result ToolCallResponse
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return err
		}
		if len(call.Response) > 0 {
			if err := json.Unmarshal(call.Response, &result); err != nil {
				return err
			}
		}
		result.Data = withProcessSources(result.Data)
		byID[key] = len(steps)
		steps = append(steps, types.AgentStep{EventID: key, Timestamp: call.CreatedAt, ToolCalls: []types.ToolCall{{ID: call.CallID, Name: call.Name, Args: args, Duration: call.DurationMs, Result: &types.ToolResult{Success: result.Success, Output: result.Output, Error: result.Error, Data: result.Data}}}})
		if call.Status != "completed" {
			steps[len(steps)-1].ProcessStatus = "stopped"
		}
		return nil
	}
	for _, record := range records {
		var item StreamEvent
		if err := json.Unmarshal(record.Body, &item); err != nil {
			return nil, err
		}
		switch item.Type {
		case "thought_delta", "commentary", "process_status":
			key := processEventID(id, item)
			index, found := byID[key]
			if !found {
				index = len(steps)
				byID[key] = index
				steps = append(steps, types.AgentStep{EventID: key, ProcessKind: item.Type, Timestamp: record.CreatedAt, ProcessStatus: "running"})
			}
			step := &steps[index]
			// Bound display text, independently from the lossless provider checkpoint.
			if item.Type == "thought_delta" {
				step.ReasoningContent = displayText(step.ReasoningContent+item.Content, 12000)
			} else {
				step.Thought = displayText(step.Thought+item.Content, 12000)
			}
			if item.Done {
				step.ProcessStatus = "success"
			}
		case "bus":
			var evt event.Event
			if err := json.Unmarshal(item.Data, &evt); err != nil {
				return nil, err
			}
			if evt.Type != event.EventAgentToolCall {
				continue
			}
			raw, _ := json.Marshal(evt.Data)
			var data event.AgentToolCallData
			if err := json.Unmarshal(raw, &data); err != nil {
				return nil, err
			}
			if call, ok := receipts[data.ToolCallID]; ok {
				if err := appendCall(call); err != nil {
					return nil, err
				}
			}
		}
	}
	// A receipt can precede its start outbox on an interrupted execution.
	sort.SliceStable(calls, func(i, j int) bool { return calls[i].CreatedAt.Before(calls[j].CreatedAt) })
	for _, call := range calls {
		if err := appendCall(call); err != nil {
			return nil, err
		}
	}
	for i := range steps {
		steps[i].Iteration = i
	}
	return steps, nil
}

func displayText(text string, limit int) string {
	chars := []rune(text)
	if len(chars) > limit {
		return string(chars[:limit]) + "…"
	}
	return text
}
