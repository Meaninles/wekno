package conversationmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/chatretrieval"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"strings"
)

// ReadTool is bound to one authorized session; the model cannot select another.
type ReadTool struct {
	DB            *gorm.DB
	SessionID     string
	SearchTargets types.SearchTargets
}

func (*ReadTool) Name() string { return "read_conversation" }
func (*ReadTool) Description() string {
	return "Read persisted messages in this authorized conversation by source_id or page. Sections: text, tools, files, references (historical catalog), evidence (reuse exact source fragments after current scope/version/hash validation; requires assistant source_id). Assistant text is history, not evidence. Invalidated evidence requires a new source read."
}
func (*ReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"source_id":{"type":"string"},"tool_call_id":{"type":"string","description":"Read an archived tool payload from this active run; use section=tools."},"section":{"type":"string","enum":["text","tools","files","references","evidence"]},"page":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`)
}
func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage) (*types.ToolResult, error) {
	if err := toolcontract.Validate(raw, t.Parameters()); err != nil {
		return nil, err
	}
	if t.DB == nil || t.SessionID == "" {
		return nil, fmt.Errorf("conversation storage unavailable")
	}
	var input struct {
		ToolCallID string `json:"tool_call_id"`
		SourceID   string `json:"source_id"`
		Page       int    `json:"page"`
		Offset     int    `json:"offset"`
		Section    string `json:"section"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	tenantID, ok := types.TenantIDFromContext(ctx)
	if !ok || tenantID == 0 {
		return nil, fmt.Errorf("tenant context required")
	}
	userID, _ := types.UserIDFromContext(ctx)
	sessions := t.DB.WithContext(ctx).Model(&types.Session{}).Where("id = ? AND tenant_id = ?", t.SessionID, tenantID)
	if userID != "" {
		sessions = sessions.Where("user_id = ?", userID)
	}
	var count int64
	if err := sessions.Count(&count).Error; err != nil {
		return nil, err
	}
	if count != 1 {
		return nil, fmt.Errorf("conversation is not accessible")
	}
	if input.ToolCallID != "" {
		if input.Section != "tools" || input.SourceID != "" {
			return nil, fmt.Errorf("tool_call_id requires section=tools and no source_id")
		}
		content, ok := LiveToolsFromContext(ctx).Read(input.ToolCallID)
		if !ok {
			return nil, fmt.Errorf("tool result is not in this active run archive")
		}
		runes := []rune(content)
		start := min(input.Offset, len(runes))
		end := min(start+16000, len(runes))
		data := map[string]any{"tool_call_id": input.ToolCallID, "text": string(runes[start:end]), "offset": start, "complete": end == len(runes)}
		if end < len(runes) {
			data["next_offset"] = end
		}
		body, _ := json.Marshal(data)
		return &types.ToolResult{Success: true, Output: string(body), Data: data}, nil
	}
	q := t.DB.WithContext(ctx).Where("session_id = ?", t.SessionID)
	if input.SourceID != "" {
		id := strings.TrimPrefix(strings.TrimPrefix(input.SourceID, "user_message_"), "assistant_message_")
		q = q.Where("id = ?", id)
		if strings.HasPrefix(input.SourceID, "user_message_") {
			q = q.Where("role = ?", "user")
		}
		if strings.HasPrefix(input.SourceID, "assistant_message_") {
			q = q.Where("role = ?", "assistant")
		}
	} else {
		q = q.Offset((max(1, input.Page) - 1) * 10)
	}
	var rows []*types.Message
	if err := q.Order("created_at ASC, id ASC").Limit(10).Find(&rows).Error; err != nil {
		return nil, err
	}
	if input.Section == "evidence" {
		if input.SourceID == "" || len(rows) != 1 || rows[0].Role != "assistant" {
			return nil, fmt.Errorf("evidence requires an accessible assistant source_id")
		}
		refs := rows[0].KnowledgeReferences
		start := min((max(1, input.Page)-1)*EvidencePageSize, len(refs))
		end := min(start+EvidencePageSize, len(refs))
		valid, invalid, err := sourcerefs.ReuseEvidence(ctx, t.DB, t.SearchTargets, refs[start:end])
		if err != nil {
			return nil, err
		}
		data := map[string]any{"validated_evidence": valid, "invalidated_sources": invalid, "complete": end == len(refs)}
		if end < len(refs) {
			data["next_page"] = max(1, input.Page) + 1
		}
		status := map[string]any{"invalidated_sources": invalid, "complete": end == len(refs), "source_id": input.SourceID, "page": max(1, input.Page)}
		if next, ok := data["next_page"]; ok {
			status["next_page"] = next
		}
		output, _ := json.Marshal(status)
		evidence, _ := chatretrieval.FormatEvidence(valid, nil)
		return &types.ToolResult{Success: true, Output: string(output) + "\n\n" + evidence, Data: data, SourceReferences: valid}, nil
	}
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		content := row.Content
		var value any
		switch input.Section {
		case "tools":
			value = row.AgentSteps
		case "files":
			value = row.Attachments
		case "references":
			value = row.KnowledgeReferences
		}
		if input.Section != "" && input.Section != "text" {
			encoded, _ := json.Marshal(value)
			content = string(encoded)
		}
		runes := []rune(content)
		start := min(input.Offset, len(runes))
		limit := 16000
		if input.SourceID == "" {
			limit = 1600
		} // ten-message page shares one bounded output budget
		end := min(start+limit, len(runes))
		sourceID := MessageSourceID(row.ID)
		if row.Role == "assistant" {
			sourceID = AssistantSourceID(row.ID)
		}
		entry := map[string]any{"source_id": sourceID, "role": row.Role, "section": input.Section, "text": string(runes[start:end]), "created_at": row.CreatedAt, "offset": start, "complete": start == 0 && end == len(runes)}
		if end < len(runes) {
			entry["next_offset"] = end
		}
		entries = append(entries, entry)
	}
	data := map[string]any{"messages": entries, "page": max(1, input.Page)}
	if input.SourceID == "" && len(rows) == 10 {
		data["next_page"] = max(1, input.Page) + 1
	}
	output, _ := json.Marshal(data)
	return &types.ToolResult{Success: true, Output: string(output), Data: data}, nil
}
