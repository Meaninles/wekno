package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
)

var sequentialThinkingTool = BaseTool{
	name: ToolThinking,
	description: `用于通过思考进行动态、反思式问题解决的详细工具。

此工具通过可调整、可演化的灵活思考过程帮助分析问题。

随着理解加深，每一步思考都可以基于、质疑或修正此前洞察。

## 何时使用此工具

- 将复杂问题拆解为步骤
- 需要保留修正空间的规划和设计
- 可能需要调整方向的分析
- 初始范围不完全清楚的问题
- 需要多步骤解决的问题
- 需要在多个步骤中维持上下文的任务
- 需要过滤无关信息的场景

## 关键特性

- 可以随着进展上调或下调 total_thoughts
- 可以质疑或修正此前思考
- 即使到达看似结束的位置，也可以继续添加思考
- 可以表达不确定性并探索替代方案
- 思考不必线性推进，可以分支或回溯
- 生成解决方案假设
- 基于思考链步骤验证假设
- 重复此过程直到满意
- 当思考完成后，用普通回复直接给出答案并停止（不要再调用工具）。绝不要把最终答案直接写进 thought。

## 参数说明

- **thought**：当前思考步骤，可包含：
  * 常规分析步骤
  * 对此前思考的修正
  * 对此前决定的疑问
  * 意识到需要更多分析
  * 方法变化
  * 假设生成
  * 假设验证
  
  **关键 - 用户友好的思考**：用自然、面向用户的语言写 thought。思考过程中绝不要提到工具名（例如 "grep_chunks"、"knowledge_search"、"web_search" 等）。请改用通俗语言描述行动：
  - ❌ 不好：“我会使用 grep_chunks 搜索关键词，然后用 knowledge_search 做语义理解”
  - ✅ 好：“我会先在知识库中搜索关键术语，再探索相关概念”
  - ❌ 不好：“grep_chunks 返回结果后，我会使用 knowledge_search”
  - ✅ 好：“找到相关文档后，我会继续搜索语义相关内容”
  
  写 thought 时要像在向用户解释推理，而不是记录技术步骤。关注你要找什么以及为什么，而不是如何找（会用哪些工具）。

- **next_thought_needed**：如果还需要更多思考则为 true，即使当前位置看似已经结束
- **thought_number**：当前序号（需要时可超过初始总数）
- **total_thoughts**：当前预计需要的思考总数（可上调/下调）
- **is_revision**：此 thought 是否修正此前思考
- **revises_thought**：如果 is_revision 为 true，表示正在重新考虑哪一步 thought
- **branch_from_thought**：如果发生分支，表示从哪一步 thought 分支
- **branch_id**：当前分支标识（如有）
- **needs_more_thoughts**：到达末尾但意识到还需要更多思考时使用

## 最佳实践

1. 先估计所需思考步数，但随时准备调整
2. 可以质疑或修正此前思考
3. 如果需要，即使在“末尾”也不要犹豫添加更多思考
4. 存在不确定性时明确表达
5. 标记修正此前思考或分支到新路径的 thought
6. 忽略与当前步骤无关的信息
7. 适当时生成解决方案假设
8. 基于思考链步骤验证假设
9. 重复此过程直到对方案满意
10. 只有真正完成且得到满意答案时，才将 next_thought_needed 设为 false
11. 绝不要在 thought 内容中包含最终答案。思考完成后，用普通回复直接给出最终答案并停止（不要再调用工具）`,
	schema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "thought": {
      "type": "string",
      "description": "当前思考步骤。请使用自然、面向用户的语言。绝不要提到工具名（例如 \"grep_chunks\"、\"knowledge_search\"、\"web_search\" 等）。请改用通俗语言描述行动（例如说“我会搜索关键术语”，不要说“我会使用 grep_chunks”）。关注你要找什么以及为什么，而不是如何找（会使用哪些工具）。"
    },
    "next_thought_needed": {
      "type": "boolean",
      "description": "是否还需要下一步思考"
    },
    "thought_number": {
      "type": "integer",
      "description": "当前思考序号（数字，例如 1、2、3）",
      "minimum": 1
    },
    "total_thoughts": {
      "type": "integer",
      "description": "预计需要的思考总数（数字，例如 5、10）",
      "minimum": 1
    },
    "is_revision": {
      "type": "boolean",
      "description": "此 thought 是否修正此前思考"
    },
    "revises_thought": {
      "type": "integer",
      "description": "正在重新考虑哪一步 thought",
      "minimum": 1
    },
    "branch_from_thought": {
      "type": "integer",
      "description": "分支点 thought 序号",
      "minimum": 1
    },
    "branch_id": {
      "type": "string",
      "description": "分支标识"
    },
    "needs_more_thoughts": {
      "type": "boolean",
      "description": "是否需要更多思考"
    }
  },
  "required": ["thought", "next_thought_needed", "thought_number", "total_thoughts"]
}`),
}

// SequentialThinkingTool is a dynamic and reflective problem-solving tool
// This tool helps analyze problems through a flexible thinking process that can adapt and evolve
type SequentialThinkingTool struct {
	BaseTool
	thoughtHistory []SequentialThinkingInput
	branches       map[string][]SequentialThinkingInput
}

// SequentialThinkingInput defines the input parameters for sequential thinking tool
type SequentialThinkingInput struct {
	Thought           string `json:"thought"`
	NextThoughtNeeded bool   `json:"next_thought_needed"`
	ThoughtNumber     int    `json:"thought_number"`
	TotalThoughts     int    `json:"total_thoughts"`
	IsRevision        bool   `json:"is_revision,omitempty"`
	RevisesThought    *int   `json:"revises_thought,omitempty"`
	BranchFromThought *int   `json:"branch_from_thought,omitempty"`
	BranchID          string `json:"branch_id,omitempty"`
	NeedsMoreThoughts bool   `json:"needs_more_thoughts,omitempty"`
}

// NewSequentialThinkingTool creates a new sequential thinking tool instance
func NewSequentialThinkingTool() *SequentialThinkingTool {
	return &SequentialThinkingTool{
		BaseTool:       sequentialThinkingTool,
		thoughtHistory: make([]SequentialThinkingInput, 0),
		branches:       make(map[string][]SequentialThinkingInput),
	}
}

// Execute executes the sequential thinking tool
func (t *SequentialThinkingTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	logger.Infof(ctx, "[Tool][SequentialThinking] Execute started")

	// Parse args from json.RawMessage
	var input SequentialThinkingInput
	if err := json.Unmarshal(args, &input); err != nil {
		logger.Errorf(ctx, "[Tool][SequentialThinking] Failed to parse args: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("解析参数失败: %v", err),
		}, err
	}

	// Validate and parse input
	if err := t.validate(input); err != nil {
		logger.Errorf(ctx, "[Tool][SequentialThinking] Validation failed: %v", err)
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("校验失败: %v", err),
		}, err
	}

	// Adjust totalThoughts if thoughtNumber exceeds it
	if input.ThoughtNumber > input.TotalThoughts {
		input.TotalThoughts = input.ThoughtNumber
	}

	// Add to thought history
	t.thoughtHistory = append(t.thoughtHistory, input)

	// Handle branching
	if input.BranchFromThought != nil && input.BranchID != "" {
		if t.branches[input.BranchID] == nil {
			t.branches[input.BranchID] = make([]SequentialThinkingInput, 0)
		}
		t.branches[input.BranchID] = append(t.branches[input.BranchID], input)
	}

	logger.Debugf(ctx, "[Tool][SequentialThinking] %s", input.Thought)

	// Prepare response data
	branchKeys := make([]string, 0, len(t.branches))
	for k := range t.branches {
		branchKeys = append(branchKeys, k)
	}

	incomplete := input.NextThoughtNeeded || input.NeedsMoreThoughts ||
		input.ThoughtNumber < input.TotalThoughts

	responseData := map[string]interface{}{
		"thought_number":         input.ThoughtNumber,
		"total_thoughts":         input.TotalThoughts,
		"next_thought_needed":    input.NextThoughtNeeded,
		"branches":               branchKeys,
		"thought_history_length": len(t.thoughtHistory),
		"display_type":           "thinking",
		"thought":                input.Thought,
		"incomplete_steps":       incomplete,
	}

	logger.Infof(
		ctx,
		"[Tool][SequentialThinking] Execute completed - Thought %d/%d",
		input.ThoughtNumber,
		input.TotalThoughts,
	)

	outputMsg := "思考过程已记录"
	if incomplete {
		outputMsg = "思考过程已记录，还有未完成步骤，请继续探索并调用工具"
	}

	return &types.ToolResult{
		Success: true,
		Output:  outputMsg,
		Data:    responseData,
	}, nil
}

// validate validates the input thought data
func (t *SequentialThinkingTool) validate(data SequentialThinkingInput) error {
	// Validate thought (required)
	if data.Thought == "" {
		return fmt.Errorf("invalid thought: must be a non-empty string")
	}

	// Validate thoughtNumber (required)
	if data.ThoughtNumber < 1 {
		return fmt.Errorf("invalid thoughtNumber: must be >= 1")
	}

	// Validate totalThoughts (required)
	if data.TotalThoughts < 1 {
		return fmt.Errorf("invalid totalThoughts: must be >= 1")
	}

	return nil
}
