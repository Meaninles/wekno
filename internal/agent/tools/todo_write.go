package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

var todoWriteTool = BaseTool{
	name: ToolTodoWrite,
	description: `使用此工具为检索和研究任务创建并管理结构化任务列表。它能帮助你跟踪进展、组织复杂检索操作，并向用户体现工作完整性。

**关键 - 只关注检索任务**：
- 此工具用于跟踪检索和研究任务（例如搜索知识库、读取文档、收集信息）
- 不要在 todo_write 中包含总结或综合任务，这些由 thinking 工具处理
- 合适任务示例：“在知识库中搜索 X”“检索 Y 的信息”“比较 A 和 B”
- 应排除的任务示例：“总结发现”“生成最终回答”“综合结果”，这些属于 thinking 工具

## 何时使用此工具
在以下场景主动使用：

1. 复杂多步骤任务：任务需要 3 个或更多不同步骤或动作
2. 非平凡且复杂的任务：需要仔细规划或多项操作
3. 用户明确要求 todo 列表：用户直接要求使用任务列表
4. 用户提供多个任务：用户给出待办事项列表（编号或逗号分隔）
5. 收到新指令后：立即把用户要求记录为 todo
6. 开始处理任务时：开始前将其标记为 in_progress。理想情况下同一时间只有一个 todo 处于 in_progress
7. 完成任务后：将其标记为 completed，并添加实现过程中发现的新后续任务

## 何时不要使用此工具

以下场景跳过此工具：
1. 只有一个直接任务
2. 任务很简单，跟踪它没有组织收益
3. 任务纯属对话或信息问答

注意，如果只有一个简单任务，不应使用此工具，直接完成任务更合适。

## 应使用 Todo 列表的示例

<example>
User: 比较 WeKnora 和 LangChain、LlamaIndex 等其他 RAG 框架。
Assistant: 我会帮你比较 WeKnora 和其他 RAG 框架。先创建一个检索计划，收集每个框架的信息。
*创建包含检索任务的 todo 列表：1) 在知识库中搜索 WeKnora 的功能和架构，2) 使用 web_search 查找 LangChain 文档和功能，3) 使用 web_search 查找 LlamaIndex 文档和功能，4) 检索每个框架的详细比较点*
*注意：所有检索任务完成后，由 thinking 工具负责总结和综合*
Assistant: 我先从知识库中搜索 WeKnora 的详细信息。

<reasoning>
助手正确使用了 todo_write 工具，因为：
1. 该任务需要多个检索操作（在知识库中搜索 WeKnora、联网搜索 LangChain 和 LlamaIndex）
2. 每个检索任务都具体且可执行
3. todo 列表关注要检索什么，而不是如何总结
4. 所有检索完成后，总结和综合将由 thinking 工具单独处理
5. 这种分离确保任务边界清晰并避免重复
</reasoning>
</example>

<example>
User: 我需要研究 RAG 应用中向量数据库的最新进展。
Assistant: 我会帮你研究 RAG 的向量数据库。先创建一个检索计划，系统收集信息。
*创建包含检索任务的 todo 列表：1) 在知识库中搜索向量数据库信息，2) 使用 web_search 查找最新向量数据库技术，3) 检索性能比较信息，4) 收集集成方式信息*
*注意：所有检索任务完成后，thinking 工具会把发现综合成完整回答*
Assistant: 我先从知识库中搜索向量数据库信息。

<reasoning>
助手正确使用了 todo_write 工具，因为：
1. 研究任务需要多个检索操作（知识库搜索、联网搜索最新信息）
2. 每个任务都聚焦于检索具体信息
3. todo 列表跟踪检索进度，而不是综合过程
4. 检索完成后，summary 和 analysis 将由 thinking 工具处理
5. 这种方式把检索（todo_write）和综合（thinking 工具）分开
</reasoning>
</example>

## 不应使用 Todo 列表的示例

<example>
User: 如何在 Python 中打印 'Hello World'？
Assistant: 在 Python 中，可以用这段简单代码打印 "Hello World"：

print("Hello World")

执行后会在控制台输出文本 "Hello World"。</assistant>

<reasoning>
助手没有使用 todo 列表，因为这是一个可以一步完成的单一简单任务。如此直接的请求不需要跟踪多个任务或步骤。
</reasoning>
</example>

<example>
User: git status 命令有什么作用？
Assistant: git status 命令显示工作目录和暂存区的当前状态。它会显示哪些变更已暂存、哪些未暂存，以及哪些文件未被 Git 跟踪。这是在提交前了解仓库当前状态的基础命令。

<reasoning>
助手没有使用 todo 列表，因为这是信息解释请求，没有需要完成的实际编码任务。用户只是请求解释，而不是要求助手执行多个步骤或任务。
</reasoning>
</example>

## 任务状态与管理

1. **任务状态**：使用这些状态跟踪进展：
  - pending：任务尚未开始
  - in_progress：正在处理（同一时间限制为一个任务）
  - completed：任务已成功完成

2. **任务管理**：
  - 工作过程中实时更新任务状态
  - 完成后立即标记任务为 completed（不要批量完成）
  - 任意时刻只保留一个任务处于 in_progress
  - 开始新任务前完成当前任务
  - 从列表中完全移除不再相关的任务

3. **任务完成要求**：
  - 只有在完全完成任务后才标记为 completed
  - 如果遇到错误、阻塞或无法完成，保持任务为 in_progress
  - 遇到阻塞时，创建一个新任务描述需要解决的问题
  - 以下情况绝不要标记为 completed：
    - 测试失败
    - 实现不完整
    - 存在未解决错误
    - 找不到必要文件或依赖

4. **任务拆解**：
  - 创建具体、可执行的检索任务
  - 将复杂检索需求拆解为更小、可管理的步骤
  - 使用清晰、描述性的任务名称，聚焦于要检索或研究什么
  - **不要包含总结/综合任务**，这些由 thinking 工具单独处理

**重要**：在 todo_write 中完成所有检索任务后，使用 thinking 工具综合发现并生成最终回答。todo_write 工具跟踪要检索什么，thinking 工具处理如何综合和呈现信息。

不确定时，使用此工具。主动管理任务能体现认真程度，并确保完整完成所有检索需求。`,
	schema: utils.GenerateSchema[TodoWriteInput](),
}

// TodoWriteTool implements a planning tool for complex tasks
// This is an optional tool that helps organize multi-step research
type TodoWriteTool struct {
	BaseTool
}

// TodoWriteInput defines the input parameters for todo_write tool
type TodoWriteInput struct {
	Task  string     `json:"task,omitempty" jsonschema:"需要创建计划的复杂任务或问题"`
	Steps []PlanStep `json:"steps" jsonschema:"带状态跟踪的研究计划步骤数组"`
}

// PlanStep represents a single step in the research plan
type PlanStep struct {
	ID          string `json:"id" jsonschema:"此步骤的唯一标识符（例如 'step1'、'step2'）"`
	Description string `json:"description" jsonschema:"清晰描述此步骤要调查或完成什么"`
	Status      string `json:"status" jsonschema:"当前状态：pending（未开始）、in_progress（执行中）、completed（已完成）"`
}

// NewTodoWriteTool creates a new todo_write tool instance
func NewTodoWriteTool() *TodoWriteTool {
	return &TodoWriteTool{
		BaseTool: todoWriteTool,
	}
}

// Execute executes the todo_write tool
func (t *TodoWriteTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	// Parse args from json.RawMessage
	var input TodoWriteInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, err
	}

	if input.Task == "" {
		input.Task = "未提供任务描述"
	}

	// Parse plan steps
	planSteps := input.Steps

	// Generate formatted output
	output := generatePlanOutput(input.Task, planSteps)

	// Prepare structured data for response
	stepsJSON, _ := json.Marshal(planSteps)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"task":         input.Task,
			"steps":        planSteps,
			"steps_json":   string(stepsJSON),
			"total_steps":  len(planSteps),
			"plan_created": true,
			"display_type": "plan",
		},
	}, nil
}

// Helper function to safely get string field from map
func getStringField(m map[string]interface{}, key string) string {
	if val, ok := m[key].(string); ok {
		return val
	}
	return ""
}

// Helper function to safely get string array field from map
func getStringArrayField(m map[string]interface{}, key string) []string {
	if val, ok := m[key].([]interface{}); ok {
		result := make([]string, 0, len(val))
		for _, item := range val {
			if str, ok := item.(string); ok {
				result = append(result, str)
			}
		}
		return result
	}
	// Handle legacy string format for backward compatibility
	if val, ok := m[key].(string); ok && val != "" {
		return []string{val}
	}
	return []string{}
}

// generatePlanOutput generates a formatted plan output
func generatePlanOutput(task string, steps []PlanStep) string {
	output := "计划已创建\n\n"
	output += fmt.Sprintf("**任务**: %s\n\n", task)

	if len(steps) == 0 {
		output += "注意：未提供具体步骤。建议创建 3 到 7 个检索任务，以便系统研究。\n\n"
		output += "建议的检索工作流（聚焦检索任务，不包含总结）：\n"
		output += "1. 使用 grep_chunks 搜索关键词并定位相关文档\n"
		output += "2. 使用 knowledge_search 做语义搜索以检索相关内容\n"
		output += "3. 使用 list_knowledge_chunks 获取关键文档的完整内容\n"
		output += "4. 如有需要，使用 web_search 获取补充信息\n"
		output += "\n注意：总结和综合由 thinking 工具处理。不要在这里添加总结任务。\n"
		return output
	}

	// Count task statuses
	pendingCount := 0
	inProgressCount := 0
	completedCount := 0
	for _, step := range steps {
		switch step.Status {
		case "pending":
			pendingCount++
		case "in_progress":
			inProgressCount++
		case "completed":
			completedCount++
		}
	}
	totalCount := len(steps)
	remainingCount := pendingCount + inProgressCount

	output += "**计划步骤**:\n\n"

	// Display all steps in order
	for i, step := range steps {
		output += formatPlanStep(i+1, step)
	}

	// Add summary and emphasis on remaining tasks
	output += "\n=== 任务进度 ===\n"
	output += fmt.Sprintf("总计: %d 个任务\n", totalCount)
	output += fmt.Sprintf("✅ 已完成: %d\n", completedCount)
	output += fmt.Sprintf("🔄 进行中: %d\n", inProgressCount)
	output += fmt.Sprintf("⏳ 待处理: %d\n", pendingCount)

	output += "\n=== ⚠️ 重要提醒 ===\n"
	if remainingCount > 0 {
		output += fmt.Sprintf("**仍有 %d 个任务未完成！**\n\n", remainingCount)
		output += "**总结或得出结论前，必须完成所有任务。**\n\n"
		output += "下一步：\n"
		if inProgressCount > 0 {
			output += "- 继续完成当前进行中的任务\n"
		}
		if pendingCount > 0 {
			output += fmt.Sprintf("- 开始处理 %d 个待处理任务\n", pendingCount)
			output += "- 按顺序完成每个任务，不要跳过\n"
		}
		output += "- 每完成一个任务后，更新 todo_write 将其标记为 completed\n"
		output += "- 只有在所有任务完成后才生成最终总结\n"
	} else {
		output += "✅ **所有任务已完成！**\n\n"
		output += "现在可以：\n"
		output += "- 综合所有任务的发现\n"
		output += "- 生成完整最终回答或报告\n"
		output += "- 确保所有方面都已充分研究\n"
	}

	return output
}

// formatPlanStep formats a single plan step for output
func formatPlanStep(index int, step PlanStep) string {
	statusEmoji := map[string]string{
		"pending":     "⏳",
		"in_progress": "🔄",
		"completed":   "✅",
		"skipped":     "⏭️",
	}

	emoji, ok := statusEmoji[step.Status]
	if !ok {
		emoji = "⏳"
	}

	output := fmt.Sprintf("  %d. %s [%s] %s\n", index, emoji, step.Status, step.Description)

	// if len(step.ToolsToUse) > 0 {
	// 	output += fmt.Sprintf("     工具: %s\n", strings.Join(step.ToolsToUse, ", "))
	// }

	return output
}
