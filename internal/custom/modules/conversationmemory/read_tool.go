package conversationmemory

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
	"github.com/Tencent/WeKnora/internal/types"
	"gorm.io/gorm"
	"strings"
)

// ReadTool is bound to one authorized session; the model cannot select another.
type ReadTool struct {
	DB        *gorm.DB
	SessionID string
}

func (*ReadTool) Name() string { return "read_conversation" }
func (*ReadTool) Description() string {
	return "Read original persisted user messages from this conversation. Use a source_id to recover a full source omitted from context, or page through earlier user messages. Results contain user text, never inferred facts from assistant output."
}
func (*ReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"source_id":{"type":"string"},"page":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`)
}
func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage) (*types.ToolResult, error) {
	if err := toolcontract.Validate(raw, t.Parameters()); err != nil {
		return nil, err
	}
	if t.DB == nil || t.SessionID == "" {
		return nil, fmt.Errorf("conversation storage unavailable")
	}
	var input struct {
		SourceID string `json:"source_id"`
		Page     int    `json:"page"`
		Offset   int    `json:"offset"`
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
	q := t.DB.WithContext(ctx).Where("session_id = ? AND role = ?", t.SessionID, "user")
	if input.SourceID != "" {
		q = q.Where("id = ?", strings.TrimPrefix(input.SourceID, "user_message_"))
	} else {
		q = q.Offset((max(1, input.Page) - 1) * 10)
	}
	var rows []*types.Message
	if err := q.Order("created_at ASC, id ASC").Limit(10).Find(&rows).Error; err != nil {
		return nil, err
	}
	entries := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		runes := []rune(row.Content)
		start := min(input.Offset, len(runes))
		end := min(start+16000, len(runes))
		entry := map[string]any{"source_id": MessageSourceID(row.ID), "text": string(runes[start:end]), "created_at": row.CreatedAt, "offset": start, "complete": start == 0 && end == len(runes)}
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
