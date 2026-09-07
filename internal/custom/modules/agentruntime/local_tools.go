package agentruntime

import (
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/event"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Workspace execution belongs to the SDK. These receipts only project its
// existing lifecycle into the same durable steps and UI as business tools.
func recordLocalTool(tx *gorm.DB, row *RunRecord, item StreamEvent) error {
	var data struct {
		CallID    string         `json:"tool_call_id"`
		Name      string         `json:"tool_name"`
		Arguments map[string]any `json:"arguments"`
		Output    string         `json:"output"`
		Success   bool           `json:"success"`
		Duration  int64          `json:"duration"`
	}
	if err := json.Unmarshal(item.Data, &data); err != nil {
		return err
	}
	if data.CallID == "" || data.Name == "" {
		return fmt.Errorf("missing SDK tool identity")
	}
	var projected event.Event
	if item.Type == "local_tool_start" {
		args, err := json.Marshal(data.Arguments)
		if err != nil {
			return err
		}
		receipt := ToolReceipt{RunID: row.ID, CallID: data.CallID, Name: data.Name, OwnerEpoch: row.OwnerEpoch,
			Arguments: args, Status: "executing", Local: true}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&receipt).Error; err != nil {
			return err
		}
		var prior ToolReceipt
		if err := tx.First(&prior, "run_id = ? AND call_id = ?", row.ID, data.CallID).Error; err != nil {
			return err
		}
		if !prior.Local || prior.Name != data.Name || !sameJSON(prior.Arguments, args) {
			return fmt.Errorf("SDK tool identity was reused with different arguments")
		}
		projected = event.Event{Type: event.EventAgentToolCall, Data: event.AgentToolCallData{ToolCallID: data.CallID, ToolName: data.Name, Arguments: data.Arguments}}
	} else {
		result := ToolCallResponse{Success: data.Success, Output: data.Output}
		if !data.Success {
			result.Error = data.Output
		}
		if data.Name == "publish_artifact" && data.Success {
			var published SidecarArtifact
			if err := json.Unmarshal([]byte(data.Output), &published); err != nil {
				return err
			}
			var artifact Artifact
			if err := tx.Where("id = ? AND run_id = ? AND tenant_id = ? AND storage_state = ?", published.ArtifactID,
				row.ID, row.TenantID, artifactStorageStateReady).First(&artifact).Error; err != nil {
				return err
			}
			result.Data = map[string]any{"display_type": displayTypeArtifacts, "artifacts": []*SidecarArtifact{sidecarArtifactFromRow(&artifact)}}
		}
		encoded, err := json.Marshal(result)
		if err != nil {
			return err
		}
		updated := tx.Model(&ToolReceipt{}).Where("run_id = ? AND call_id = ? AND name = ? AND local = true", row.ID, data.CallID, data.Name).
			Updates(map[string]any{"status": "completed", "response": encoded, "duration_ms": data.Duration, "owner_epoch": row.OwnerEpoch})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return fmt.Errorf("SDK tool receipt is unavailable")
		}
		projected = event.Event{Type: event.EventAgentToolResult, Data: event.AgentToolResultData{ToolCallID: data.CallID,
			ToolName: data.Name, Output: data.Output, Success: data.Success, Error: result.Error, Duration: data.Duration, Data: result.Data}}
	}
	projected.SessionID = row.SessionID
	body, err := json.Marshal(projected)
	if err != nil {
		return err
	}
	return outbox(tx, row, StreamEvent{Type: "bus", Data: body})
}
