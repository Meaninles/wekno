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

// Tool name constant for execute_skill_script

var executeSkillScriptTool = BaseTool{
	name: ToolExecuteSkillScript,
	description: `在沙箱环境中执行技能中的脚本。

## 用法
- 使用此工具运行技能附带的工具脚本
- 为了安全，脚本会在隔离沙箱中执行
- 只能执行已加载技能中的脚本

## 何时使用
- 当技能说明引用工具脚本时（例如“运行 scripts/analyze_form.py”）
- 当技能工作流需要自动化或数据处理时
- 当确定性操作用脚本执行比生成代码更可靠时

## 安全
- 脚本在权限受限的沙箱环境中运行
- 默认禁用网络访问
- 文件访问限制在技能目录内

## 返回
- 脚本的 stdout 和 stderr 输出
- 表示成功（0）或失败（非 0）的退出码`,
	schema: utils.GenerateSchema[ExecuteSkillScriptInput](),
}

// ExecuteSkillScriptInput defines the input parameters for the execute_skill_script tool
type ExecuteSkillScriptInput struct {
	SkillName  string   `json:"skill_name" jsonschema:"包含该脚本的技能名称"`
	ScriptPath string   `json:"script_path" jsonschema:"技能目录内脚本的相对路径（例如 scripts/analyze.py）"`
	Args       []string `json:"args,omitempty" jsonschema:"传递给脚本的可选命令行参数。注意：如果使用 --file 标志，必须提供技能目录中实际存在的文件路径。如果数据在内存中（不是文件），请改用 'input' 参数。"`
	Input      string   `json:"input,omitempty" jsonschema:"通过 stdin 传递给脚本的可选输入数据。当内存中有需要脚本处理的数据（例如 JSON 字符串）时使用。等价于管道输入：echo 'data' | python script.py"`
}

// ExecuteSkillScriptTool allows the agent to execute skill scripts in a sandbox
type ExecuteSkillScriptTool struct {
	BaseTool
	skillManager *skills.Manager
}

// NewExecuteSkillScriptTool creates a new execute_skill_script tool instance
func NewExecuteSkillScriptTool(skillManager *skills.Manager) *ExecuteSkillScriptTool {
	return &ExecuteSkillScriptTool{
		BaseTool:     executeSkillScriptTool,
		skillManager: skillManager,
	}
}

// Execute executes the execute_skill_script tool
func (t *ExecuteSkillScriptTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Execute started")

	// Parse input
	var input ExecuteSkillScriptInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][ExecuteSkillScript] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, nil
	}

	// Validate required fields
	if input.SkillName == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "需要提供 skill_name",
		}, nil
	}

	if input.ScriptPath == "" {
		return &types.ToolResult{
			Success: false,
			Error:   "需要提供 script_path",
		}, nil
	}

	// Check if skill manager is available
	if t.skillManager == nil || !t.skillManager.IsEnabled() {
		return &types.ToolResult{
			Success: false,
			Error:   "技能未启用",
		}, nil
	}

	// Execute the script in sandbox
	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Executing script: %s/%s with args: %v, input length: %d",
		input.SkillName, input.ScriptPath, input.Args, len(input.Input))

	result, err := t.skillManager.ExecuteScript(ctx, input.SkillName, input.ScriptPath, input.Args, input.Input)
	if err != nil {
		logger.Errorf(ctx, "[Tool][ExecuteSkillScript] Script execution failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("脚本执行失败: %v", err),
		}, nil
	}

	// Build output
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("=== 脚本执行: %s/%s ===\n\n", input.SkillName, input.ScriptPath))

	if len(input.Args) > 0 {
		builder.WriteString(fmt.Sprintf("**参数**: %v\n", input.Args))
	}

	builder.WriteString(fmt.Sprintf("**退出码**: %d\n", result.ExitCode))
	builder.WriteString(fmt.Sprintf("**耗时**: %v\n\n", result.Duration))

	if result.Killed {
		builder.WriteString("**警告**: 脚本已终止（超时或被杀死）\n\n")
	}

	if result.Stdout != "" {
		builder.WriteString("## 标准输出\n\n")
		builder.WriteString("```\n")
		builder.WriteString(result.Stdout)
		if !strings.HasSuffix(result.Stdout, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("```\n\n")
	}

	if result.Stderr != "" {
		builder.WriteString("## 标准错误\n\n")
		builder.WriteString("```\n")
		builder.WriteString(result.Stderr)
		if !strings.HasSuffix(result.Stderr, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("```\n\n")
	}

	if result.Error != "" {
		builder.WriteString("## 错误\n\n")
		builder.WriteString(result.Error)
		builder.WriteString("\n")
	}

	// Determine success based on exit code
	success := result.IsSuccess()

	resultData := map[string]interface{}{
		"skill_name":  input.SkillName,
		"script_path": input.ScriptPath,
		"args":        input.Args,
		"exit_code":   result.ExitCode,
		"stdout":      result.Stdout,
		"stderr":      result.Stderr,
		"duration_ms": result.Duration.Milliseconds(),
		"killed":      result.Killed,
	}

	logger.Infof(ctx, "[Tool][ExecuteSkillScript] Script completed with exit code: %d", result.ExitCode)

	return &types.ToolResult{
		Success: success,
		Output:  builder.String(),
		Data:    resultData,
		Error: func() string {
			if !success {
				if result.Error != "" {
					return result.Error
				}
				return fmt.Sprintf("Script exited with code %d", result.ExitCode)
			}
			return ""
		}(),
	}, nil
}

// Cleanup releases any resources
func (t *ExecuteSkillScriptTool) Cleanup(ctx context.Context) error {
	return nil
}
