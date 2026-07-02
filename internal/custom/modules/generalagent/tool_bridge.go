package generalagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

type activeRun struct {
	runID              string
	ctx                context.Context
	eventBus           *event.EventBus
	registry           interfaces.AgentToolRegistry
	sessionID          string
	assistantMessageID string
	requestID          string
	userID             string
	originalUserQuery  string
	toolExecTimeout    time.Duration

	mu       sync.Mutex
	stepSeq  int
	steps    []types.AgentStep
	progress map[string]progressRecord
}

type progressRecord struct {
	iteration int
	startedAt time.Time
	toolName  string
}

var activeRuns = struct {
	sync.RWMutex
	byID map[string]*activeRun
}{
	byID: map[string]*activeRun{},
}

func registerActiveRun(run *activeRun) func() {
	if run == nil || run.runID == "" {
		return func() {}
	}
	activeRuns.Lock()
	activeRuns.byID[run.runID] = run
	activeRuns.Unlock()
	return func() {
		activeRuns.Lock()
		delete(activeRuns.byID, run.runID)
		activeRuns.Unlock()
	}
}

func lookupActiveRun(runID string) *activeRun {
	activeRuns.RLock()
	defer activeRuns.RUnlock()
	return activeRuns.byID[runID]
}

func runtimeToolSpecs(registry interfaces.AgentToolRegistry) []RuntimeToolSpec {
	if registry == nil {
		return nil
	}
	defs := registry.GetFunctionDefinitions()
	out := make([]RuntimeToolSpec, 0, len(defs))
	for _, def := range defs {
		out = append(out, RuntimeToolSpec{
			Name:        def.Name,
			Description: def.Description,
			Parameters:  def.Parameters,
			Source:      classifyToolSource(def.Name),
		})
	}
	return out
}

func classifyToolSource(name string) string {
	switch {
	case strings.HasPrefix(name, "mcp__") || strings.HasPrefix(name, "mcp_"):
		return "mcp"
	case strings.HasPrefix(name, "db_"):
		return "database"
	case strings.HasPrefix(name, "wiki_"):
		return "wiki"
	case strings.Contains(name, "skill"):
		return "skill"
	case name == agenttools.ToolKnowledgeSearch ||
		name == agenttools.ToolGrepChunks ||
		name == agenttools.ToolListKnowledgeChunks ||
		name == agenttools.ToolGetDocumentInfo ||
		name == agenttools.ToolQueryKnowledgeGraph ||
		name == agenttools.ToolDatabaseQuery:
		return "knowledge"
	case name == agenttools.ToolWebSearch || name == agenttools.ToolWebFetch:
		return "web"
	default:
		return "native"
	}
}

func executeRuntimeTool(httpCtx context.Context, req ToolCallRequest) (*ToolCallResponse, error) {
	run := lookupActiveRun(req.RunID)
	if run == nil {
		return nil, fmt.Errorf("run %s is not active", req.RunID)
	}
	if run.registry == nil {
		return nil, fmt.Errorf("run %s has no tool registry", req.RunID)
	}
	toolCallID := strings.TrimSpace(req.ToolCallID)
	if toolCallID == "" {
		toolCallID = uuid.New().String()
	}
	var args map[string]any
	if len(req.Arguments) > 0 {
		_ = json.Unmarshal(req.Arguments, &args)
	}
	if args == nil {
		args = map[string]any{}
	}

	iteration := run.allocateIteration()
	run.eventBus.Emit(run.ctx, event.Event{
		Type:      event.EventAgentToolCall,
		SessionID: run.sessionID,
		RequestID: run.requestID,
		Data: event.AgentToolCallData{
			ToolCallID: toolCallID,
			ToolName:   req.ToolName,
			Arguments:  args,
			Iteration:  iteration,
		},
	})

	start := time.Now()
	execTimeout := run.toolTimeoutFor(req.ToolName)
	execCtx := agenttools.WithToolExecContext(run.ctx, &agenttools.ToolExecContext{
		SessionID:          run.sessionID,
		AssistantMessageID: run.assistantMessageID,
		RequestID:          run.requestID,
		ToolCallID:         toolCallID,
		UserID:             run.userID,
		EventBus:           run.eventBus,
		OriginalUserQuery:  run.originalUserQuery,
		ApprovalCtx:        run.ctx,
		ExecTimeout:        execTimeout,
	})
	execCtx, timeoutCancel := context.WithTimeout(execCtx, execTimeout)
	defer timeoutCancel()
	if httpCtx != nil {
		cancelCtx, cancel := context.WithCancel(execCtx)
		defer cancel()
		go func() {
			select {
			case <-httpCtx.Done():
				cancel()
			case <-cancelCtx.Done():
			}
		}()
		execCtx = cancelCtx
	}

	result, err := run.registry.ExecuteTool(execCtx, req.ToolName, req.Arguments)
	durationMs := time.Since(start).Milliseconds()
	if result == nil {
		result = &types.ToolResult{Success: false, Error: "tool returned no result"}
	}
	if err != nil && result.Error == "" {
		result.Error = err.Error()
		result.Success = false
	}
	run.recordToolCall(iteration, toolCallID, req.ToolName, args, result, durationMs)

	run.eventBus.Emit(run.ctx, event.Event{
		Type:      event.EventAgentToolResult,
		SessionID: run.sessionID,
		RequestID: run.requestID,
		Data: event.AgentToolResultData{
			ToolCallID: toolCallID,
			ToolName:   req.ToolName,
			Output:     result.Output,
			Error:      result.Error,
			Success:    result.Success,
			Duration:   durationMs,
			Iteration:  iteration,
			Data:       result.Data,
		},
	})
	if err != nil {
		logger.Warnf(run.ctx, "general-agent tool %s failed: %v", req.ToolName, err)
	}
	return &ToolCallResponse{
		Success: result.Success,
		Output:  result.Output,
		Error:   result.Error,
		Data:    result.Data,
		Images:  result.Images,
	}, nil
}

func (r *activeRun) toolTimeoutFor(toolName string) time.Duration {
	timeout := r.toolExecTimeout
	if timeout <= 0 {
		timeout = envDurationSeconds("CUSTOM_GENERAL_AGENT_TOOL_EXEC_TIMEOUT_SEC", 15*time.Minute)
	}
	return timeout
}

func (r *activeRun) allocateIteration() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	iteration := r.stepSeq
	r.stepSeq++
	return iteration
}

func (r *activeRun) recordToolCall(iteration int, id, name string, args map[string]any, result *types.ToolResult, durationMs int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	step := types.AgentStep{
		Iteration: iteration,
		ToolCalls: []types.ToolCall{{
			ID:       id,
			Name:     name,
			Args:     args,
			Result:   result,
			Duration: durationMs,
		}},
		Timestamp: time.Now(),
	}
	r.steps = append(r.steps, step)
}

func (r *activeRun) recordProgressStatus(id, name, phase, message string) {
	id = strings.TrimSpace(id)
	name = strings.TrimSpace(name)
	phase = strings.TrimSpace(phase)
	message = strings.TrimSpace(message)
	if id == "" && name == "" && message == "" {
		return
	}
	if id == "" {
		id = fmt.Sprintf("agent-progress-%s", name)
	}
	if name == "" {
		name = "general_agent_progress"
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if r.progress == nil {
		r.progress = make(map[string]progressRecord)
	}
	rec, exists := r.progress[id]
	if !exists {
		rec = progressRecord{
			iteration: r.stepSeq,
			startedAt: time.Now(),
			toolName:  name,
		}
		r.stepSeq++
		r.progress[id] = rec
	}
	if phase != "success" && phase != "error" {
		return
	}

	success := phase != "error"
	result := &types.ToolResult{
		Success: success,
		Output:  message,
		Data: map[string]interface{}{
			"agent_progress":         true,
			"agent_progress_message": message,
			"phase":                  phase,
			"display_type":           "agent_progress",
		},
	}
	if !success {
		result.Error = message
	}
	durationMs := int64(0)
	if !rec.startedAt.IsZero() {
		durationMs = time.Since(rec.startedAt).Milliseconds()
	}
	r.steps = append(r.steps, types.AgentStep{
		Iteration: rec.iteration,
		ToolCalls: []types.ToolCall{{
			ID:       id,
			Name:     rec.toolName,
			Args:     map[string]interface{}{},
			Result:   result,
			Duration: durationMs,
		}},
		Timestamp: time.Now(),
	})
	delete(r.progress, id)
}

func (r *activeRun) snapshotSteps(finalAnswer string) []types.AgentStep {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]types.AgentStep, len(r.steps))
	copy(out, r.steps)
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Iteration < out[j].Iteration
	})
	if finalAnswer != "" {
		out = append(out, types.AgentStep{
			Iteration: r.stepSeq,
			Thought:   finalAnswer,
			Timestamp: time.Now(),
		})
	}
	return out
}
