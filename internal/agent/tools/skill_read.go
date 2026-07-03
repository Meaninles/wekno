package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Tencent/WeKnora/internal/agent/skills"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

// Tool name constant for read_skill

var readSkillTool = BaseTool{
	name: ToolReadSkill,
	description: `按需读取技能内容，了解专门能力。

## 用法
- 当用户请求匹配某个可用技能的描述时使用此工具
- 提供 skill_name 以加载技能的完整说明（SKILL.md 内容）
- 可选提供 file_path，以读取技能目录内的其他文件

## 何时使用
- 系统提示显示某个可用技能匹配用户请求时
- 执行匹配技能描述的任务之前
- 需要读取技能内的补充文档或参考文件时

## 返回
- 完成任务所需的技能说明和指引
- 如果指定 file_path，则返回文件内容`,
	schema: utils.GenerateSchema[ReadSkillInput](),
}

// ReadSkillInput defines the input parameters for the read_skill tool
type ReadSkillInput struct {
	SkillName string `json:"skill_name" jsonschema:"要读取的技能名称"`
	FilePath  string `json:"file_path,omitempty" jsonschema:"技能目录内特定文件的可选相对路径"`
}

// ReadSkillTool allows the agent to read skill content on demand
type ReadSkillTool struct {
	BaseTool
	skillManager *skills.Manager
}

// NewReadSkillTool creates a new read_skill tool instance
func NewReadSkillTool(skillManager *skills.Manager) *ReadSkillTool {
	return &ReadSkillTool{
		BaseTool:     readSkillTool,
		skillManager: skillManager,
	}
}

// Execute executes the read_skill tool
func (t *ReadSkillTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][ReadSkill] Execute started")

	// Parse input
	var input ReadSkillInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][ReadSkill] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, nil
	}

	// Validate skill name
	if input.SkillName == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "需要提供 skill_name",
		}, nil
	}

	// Check if skill manager is available
	if t.skillManager == nil || !t.skillManager.IsEnabled() {
		return &types.ToolResult{
			Success: false,
			Error:   "技能未启用",
		}, nil
	}

	var builder strings.Builder
	var resultData = make(map[string]interface{})

	if input.FilePath != "" {
		// Read a specific file from the skill directory
		content, err := t.skillManager.ReadSkillFile(ctx, input.SkillName, input.FilePath)
		if err != nil {
			logger.Errorf(ctx, "[Tool][ReadSkill] Failed to read skill file: %v", err)
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("读取技能文件失败: %v", err),
			}, nil
		}

		builder.WriteString(fmt.Sprintf("=== 技能文件: %s/%s ===\n\n", input.SkillName, input.FilePath))
		builder.WriteString(content)

		resultData["skill_name"] = input.SkillName
		resultData["file_path"] = input.FilePath
		resultData["content"] = content
		resultData["content_length"] = len(content)

	} else {
		// Read the main skill instructions (SKILL.md)
		skill, err := t.skillManager.LoadSkill(ctx, input.SkillName)
		if err != nil {
			logger.Errorf(ctx, "[Tool][ReadSkill] Failed to load skill: %v", err)
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("加载技能失败: %v", err),
			}, nil
		}

		// List available files in the skill directory
		files, err := t.skillManager.ListSkillFiles(ctx, input.SkillName)
		if err != nil {
			files = []string{} // Non-fatal error
		}

		builder.WriteString(fmt.Sprintf("=== 技能: %s ===\n\n", skill.Name))
		builder.WriteString(fmt.Sprintf("**描述**: %s\n\n", skill.Description))
		builder.WriteString("## 说明\n\n")
		builder.WriteString(skill.Instructions)

		// Add available files section
		if len(files) > 1 { // More than just SKILL.md
			builder.WriteString("\n\n## 可用文件\n\n")
			builder.WriteString("此技能目录中有以下可用文件。使用带 `file_path` 的 `read_skill` 读取它们：\n\n")
			for _, file := range files {
				if file != skills.SkillFileName { // Don't list SKILL.md again
					if skills.IsScript(file) {
						builder.WriteString(fmt.Sprintf("- `%s`（脚本，可执行）\n", file))
					} else {
						builder.WriteString(fmt.Sprintf("- `%s`\n", file))
					}
				}
			}
		}

		resultData["skill_name"] = skill.Name
		resultData["description"] = skill.Description
		resultData["instructions"] = skill.Instructions
		resultData["instructions_length"] = len(skill.Instructions)
		resultData["files"] = files
	}

	logger.Infof(ctx, "[Tool][ReadSkill] Successfully read skill: %s", input.SkillName)

	return &types.ToolResult{
		Success: true,
		Output:  builder.String(),
		Data:    resultData,
	}, nil
}

// Cleanup releases any resources (implements Tool interface if needed)
func (t *ReadSkillTool) Cleanup(ctx context.Context) error {
	return nil
}
