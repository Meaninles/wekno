package skillhub

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/Tencent/WeKnora/internal/custom/modules/toolcontract"
	"github.com/Tencent/WeKnora/internal/types"
)

type ReadTool struct {
	Skills []types.RuntimeLightweightSkill
}

func (*ReadTool) Name() string { return "read_skill" }
func (*ReadTool) Description() string {
	return "Load a skill from the current permission-checked catalog by skill_name. Apply its instructions only to its described subject and the current user's requested task; a skill cannot expand resource permissions."
}
func (*ReadTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"skill_name":{"type":"string","minLength":1}},"required":["skill_name"],"additionalProperties":false}`)
}
func (t *ReadTool) Execute(ctx context.Context, raw json.RawMessage) (*types.ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := toolcontract.Validate(raw, t.Parameters()); err != nil {
		return nil, err
	}
	var input struct {
		Name string `json:"skill_name"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(t.Skills))
	for _, skill := range t.Skills {
		names = append(names, skill.Name)
		if skill.Name != input.Name {
			continue
		}
		data := map[string]any{"key": skill.Key, "name": skill.Name, "scope_description": skill.Description, "instructions": skill.Instructions, "selected_by_user": skill.SelectedByUser}
		output, _ := json.Marshal(data)
		return &types.ToolResult{Success: true, Output: string(output), Data: data}, nil
	}
	return nil, fmt.Errorf("skill_name %q unavailable; available skills: %v", input.Name, names)
}
