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
	return "Read this authorized conversation. section=evidence restores original fragments retrieved in prior runs, including uncited fragments, after permission/version/hash validation. Copy an assistant source_id from the conversation navigation and page for exact reads, or provide query to select relevant recent evidence. section=references lists archived sources. Other sections: text, tools, files. Assistant prose is not source evidence."
}
func (*ReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"source_id":{"type":"string","description":"Copy an assistant source_id from conversation navigation for source reads."},"query":{"type":"string","description":"With section=evidence, select relevant archived fragments; source_id is optional."},"tool_call_id":{"type":"string","description":"Read an archived tool payload from this active run; use section=tools."},"section":{"type":"string","enum":["text","tools","files","references","evidence"]},"page":{"type":"integer","minimum":1},"offset":{"type":"integer","minimum":0}},"additionalProperties":false}`)
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
		Query      string `json:"query"`
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
	if input.Section == "evidence" && input.SourceID == "" && strings.TrimSpace(input.Query) != "" {
		q = q.Where("role = ? AND is_completed = ?", "assistant", true).Order("created_at DESC, id DESC")
	}
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
		if (input.SourceID == "" && strings.TrimSpace(input.Query) == "") || (input.SourceID != "" && (len(rows) != 1 || rows[0].Role != "assistant")) {
			return nil, fmt.Errorf("evidence requires an accessible assistant source_id")
		}
		ids := []string{}
		for _, row := range rows {
			ids = append(ids, row.ID)
		}
		archive, err := LoadEvidenceArchive(ctx, t.DB, t.SessionID, ids)
		if err != nil {
			return nil, err
		}
		refs := []*types.SearchResult{}
		for _, row := range rows {
			saved := row.KnowledgeReferences
			if stored, exists := archive[row.ID]; exists {
				saved = stored
			}
			for _, ref := range saved {
				if evidenceInScope(ref, t.SearchTargets) {
					refs = append(refs, ref)
				}
			}
		}
		if strings.TrimSpace(input.Query) != "" {
			refs = SelectEvidence(refs, input.Query, 4000)
		}
		refs, invalid, err := sourcerefs.ReuseEvidence(ctx, t.DB, t.SearchTargets, refs)
		if err != nil {
			return nil, err
		}
		start := min((max(1, input.Page)-1)*EvidencePageSize, len(refs))
		end := min(start+EvidencePageSize, len(refs))
		if input.Query != "" {
			start, end = 0, len(refs)
		}
		valid := refs[start:end]
		data := map[string]any{"validated_evidence": valid, "invalidated_sources": invalid, "complete": end == len(refs)}
		if end < len(refs) {
			data["next_page"] = max(1, input.Page) + 1
		}
		status := map[string]any{"invalidated_sources": invalid, "complete": end == len(refs), "source_id": input.SourceID, "page": max(1, input.Page)}
		if input.Query != "" {
			status["selection"] = "bounded relevant fragments; use source_id/page or retrieve for missing claims"
		}
		if next, ok := data["next_page"]; ok {
			status["next_page"] = next
		}
		output, _ := json.Marshal(status)
		evidence, _ := chatretrieval.FormatEvidence(valid, nil)
		return &types.ToolResult{Success: true, Output: string(output) + "\n\n" + evidence, Data: data, SourceReferences: valid}, nil
	}
	entries := make([]map[string]any, 0, len(rows))
	archive := map[string][]*types.SearchResult{}
	if input.Section == "references" {
		ids := []string{}
		for _, row := range rows {
			if row.Role == "assistant" {
				ids = append(ids, row.ID)
			}
		}
		var err error
		archive, err = LoadEvidenceArchive(ctx, t.DB, t.SessionID, ids)
		if err != nil {
			return nil, err
		}
	}
	for _, row := range rows {
		content := row.Content
		if row.Role == "assistant" && row.ErrorCode != "" {
			content = ""
		}
		var value any
		switch input.Section {
		case "tools":
			value = row.AgentSteps
		case "files":
			value = row.Attachments
		case "references":
			copy := *row
			valid, _, err := sourcerefs.ReuseEvidence(ctx, t.DB, t.SearchTargets, archive[row.ID])
			if err != nil {
				return nil, err
			}
			copy.KnowledgeReferences = valid
			value = json.RawMessage(AssistantMetadata(&copy))
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
		if row.Role == "assistant" && row.ErrorCode != "" {
			entry["outcome"] = "failed"
		}
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
