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
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/logger"
	"github.com/Tencent/WeKnora/internal/models/chat"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/google/uuid"
)

type activeRun struct {
	chatModel          chat.Chat
	agentConfig        *types.AgentConfig
	runID              string
	client             *Client
	originalInputFiles map[string]OriginalInputFileSpec
	ctx                context.Context
	eventBus           *event.EventBus
	registry           interfaces.AgentToolRegistry
	sessionID          string
	assistantMessageID string
	requestID          string
	userID             string
	originalUserQuery  string
	toolExecTimeout    time.Duration

	mu           sync.Mutex
	stepSeq      int
	steps        []types.AgentStep
	runtimeTools map[string]runtimeToolRecord
	refSeen      map[string]bool
	sources      *sourcerefs.Registry
}

type runtimeToolRecord struct {
	iteration int
	startedAt time.Time
	toolName  string
	args      map[string]any
	done      bool
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
	if req.ToolName == "inspect_image" {
		args = imageTransportAuditArgs(args)
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
		RunID:              run.runID,
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
	sourceReferences := run.registerSourceReferences(req.ToolName, result)
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
	citationOutputContract := ""
	if run.hasCitableEvidence() {
		citationOutputContract = sourcerefs.TerminalCitationInstruction()

	}
	return &ToolCallResponse{
		Success:                result.Success,
		Output:                 result.Output,
		Error:                  result.Error,
		Data:                   result.Data,
		Images:                 result.Images,
		SourceReferences:       sourceReferences,
		CitationOutputContract: citationOutputContract,
	}, nil
}

func (r *activeRun) registerSourceReferences(toolName string, result *types.ToolResult) []*sourcerefs.CitationSource {
	r.mu.Lock()
	if r.sources == nil {
		r.sources = sourcerefs.NewRegistry()
	}
	refs, sources := sourcerefs.RegisterToolResult(r.sources, toolName, result)
	if len(refs) > 0 {
		// Use the same shared model-visible annotation as the native ReAct loop.
		// The sidecar transport only serializes this result; it does not infer or
		// convert a second citation protocol.
		result.Output = sourcerefs.AppendCitationCatalog(result.Output, refs)
	}
	if r.refSeen == nil {
		r.refSeen = make(map[string]bool, len(refs))
	}
	newRefs := make([]*types.SearchResult, 0, len(refs))
	for _, ref := range refs {
		key := sourcerefs.ReferenceKey(ref)
		if key == "" || r.refSeen[key] {
			continue
		}
		r.refSeen[key] = true
		newRefs = append(newRefs, ref)
	}
	r.mu.Unlock()
	if len(refs) == 0 {
		return nil
	}
	if len(newRefs) > 0 {
		r.eventBus.Emit(r.ctx, event.Event{
			Type:      event.EventAgentReferences,
			SessionID: r.sessionID,
			RequestID: r.requestID,
			Data: event.AgentReferencesData{
				References: newRefs,
			},
		})
	}

	return sources
}

func (r *activeRun) snapshotSourceReferences() []*types.SearchResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sources == nil {
		return nil
	}
	return r.sources.SnapshotReferences()
}

func (r *activeRun) hasCitableEvidence() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.refSeen) > 0
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

// Runtime-selected tools use the same event and history contract as platform
// callbacks. Status-only sidecar events never become synthetic tool calls.
func (r *activeRun) recordRuntimeToolEvent(ctx context.Context, bus *event.EventBus, evt sidecarProgressData) {
	if evt.ToolCallID == "" || evt.ToolName == "" {
		return
	}
	r.mu.Lock()
	if r.runtimeTools == nil {
		r.runtimeTools = make(map[string]runtimeToolRecord)
	}
	rec, exists := r.runtimeTools[evt.ToolCallID]
	if rec.done {
		r.mu.Unlock()
		return
	}
	if !exists {
		args, _ := evt.Metadata["arguments"].(map[string]any)
		rec = runtimeToolRecord{iteration: r.stepSeq, startedAt: time.Now(), toolName: evt.ToolName, args: args}
		r.stepSeq++
		r.runtimeTools[evt.ToolCallID] = rec
	}
	terminal := evt.Phase == "success" || evt.Phase == "error"
	if terminal {
		rec.done = true
		r.runtimeTools[evt.ToolCallID] = rec
	}
	r.mu.Unlock()
	if !exists {
		bus.Emit(ctx, event.Event{Type: event.EventAgentToolCall, SessionID: r.sessionID, RequestID: r.requestID,
			Data: event.AgentToolCallData{ToolCallID: evt.ToolCallID, ToolName: rec.toolName, Arguments: rec.args, Iteration: rec.iteration}})
	}
	if !terminal {
		return
	}
	output := evt.Content
	if value, ok := evt.Metadata["output"]; ok {
		if text, ok := value.(string); ok {
			output = text
		} else if encoded, err := json.Marshal(value); err == nil {
			output = string(encoded)
		}
	}
	result := &types.ToolResult{Success: evt.Phase == "success", Output: output}
	if result.Success && rec.toolName == "create_artifact" {
		var item SidecarArtifact
		if json.Unmarshal([]byte(output), &item) == nil && item.Persisted && item.ArtifactID != "" {
			result.Data = map[string]any{"display_type": displayTypeArtifacts,
				"artifacts": artifactResultMaps([]ArtifactResult{{ArtifactID: item.ArtifactID,
					FileName: item.FileName, FileType: item.FileType, FileSize: item.FileSize,
					SHA256: item.SHA256, DownloadURL: item.DownloadURL}})}
		}
	}
	if !result.Success {
		result.Error = output
	}
	duration := time.Since(rec.startedAt).Milliseconds()
	if measured, ok := evt.Metadata["duration_ms"].(float64); ok {
		duration = int64(measured)
	}
	r.recordToolCall(rec.iteration, evt.ToolCallID, rec.toolName, rec.args, result, duration)
	bus.Emit(ctx, event.Event{Type: event.EventAgentToolResult, SessionID: r.sessionID, RequestID: r.requestID,
		Data: event.AgentToolResultData{ToolCallID: evt.ToolCallID, ToolName: rec.toolName, Output: result.Output,
			Error: result.Error, Success: result.Success, Duration: duration, Iteration: rec.iteration, Data: result.Data}})
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
