package agentruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/custom/modules/conversationmemory"
	"github.com/Tencent/WeKnora/internal/custom/modules/sourcerefs"
	"github.com/Tencent/WeKnora/internal/event"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"gorm.io/gorm"
)

func toolReadOnly(name string) bool {
	switch name {
	case "knowledge_search", "grep_chunks", "list_knowledge_chunks", "get_document_info", "query_knowledge_graph", "web_search", "web_fetch", "wiki_search", "wiki_read_page", "wiki_read_source_doc", "read_skill", "get_conversation_history", "read_conversation", "db_catalog", "db_schema", "db_query", "table_schema", "table_analysis", "data_schema", "data_analysis", ToolTranscribeInputFile:
		return true
	default:
		return false
	}
}

// Hydration is request-scoped and uses the durable authorized scope. Live tool
// implementations still enforce tenant, document, MCP and mutation permissions.
func (s *Service) toolRegistry(ctx context.Context, row *RunRecord) (context.Context, interfaces.AgentToolRegistry, *types.AgentConfig, error) {
	var scope RunScope
	if err := json.Unmarshal(row.Scope, &scope); err != nil {
		return ctx, nil, nil, err
	}
	config := scope.config()
	ctx, hydrateErr := s.executionContext(ctx, row)
	if hydrateErr != nil {
		return ctx, nil, nil, hydrateErr
	}
	ctx = conversationmemory.WithLiveTools(ctx)
	if scope.NonInteractiveOAuth {
		ctx = types.WithMCPOAuthNonInteractive(ctx)
	}
	modelCtx := context.WithValue(ctx, types.TenantIDContextKey, scope.ModelTenantID)
	var err error
	// Rerank selection is a tool setting, never an additional answering model.
	var request types.QARequest
	var payload ChatPayload
	if err = json.Unmarshal(row.Payload, &payload); err != nil {
		return ctx, nil, nil, err
	}
	request.CustomAgent = &types.CustomAgent{}
	request.CustomAgent.Config.RerankModelID = payload.RuntimeConfig.RerankModelID
	ranker, err := s.resolveRerankModel(modelCtx, &request, config)
	if err != nil {
		return ctx, nil, nil, err
	}
	registry, err := s.agentService.CreateToolRegistry(ctx, config, ranker, row.SessionID)
	if err != nil {
		return ctx, nil, nil, err
	}
	return ctx, registry, config, nil
}

func (s *Service) callTool(ctx context.Context, req ToolCallRequest) (*ToolCallResponse, error) {
	if req.ToolCallID == "" || req.ToolName == "" {
		return nil, fmt.Errorf("SDK tool call identity is required")
	}
	var arguments map[string]any
	if err := json.Unmarshal(req.Arguments, &arguments); err != nil {
		return nil, err
	}
	normalized, err := json.Marshal(arguments)
	if err != nil {
		return nil, err
	}
	req.Arguments = normalized
	var row *RunRecord
	var cached *ToolCallResponse
	err = s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var e error
		row, e = lockRun(tx, req.RunID)
		if e != nil {
			return e
		}
		if e = owned(row, req.OwnerEpoch); e != nil {
			return e
		}
		var prior ToolReceipt
		e = tx.First(&prior, "run_id = ? AND call_id = ?", row.ID, req.ToolCallID).Error
		if e == nil {
			var original map[string]any
			if e = json.Unmarshal(prior.Arguments, &original); e != nil {
				return e
			}
			if prior.Name != req.ToolName || !reflect.DeepEqual(original, arguments) {
				return fmt.Errorf("tool call identity reused with different arguments")
			}
			if prior.Status == "completed" {
				cached = &ToolCallResponse{}
				return json.Unmarshal(prior.Response, cached)
			}
			if prior.OwnerEpoch == req.OwnerEpoch {
				return fmt.Errorf("tool call is already executing")
			}
			if !prior.ReadOnly {
				return fmt.Errorf("side-effect outcome is uncertain; refusing duplicate execution of %s", req.ToolCallID)
			}
			return tx.Model(&prior).Updates(map[string]any{"owner_epoch": req.OwnerEpoch, "status": "executing"}).Error
		}
		if !errors.Is(e, gorm.ErrRecordNotFound) {
			return e
		}
		var payload ChatPayload
		if e = json.Unmarshal(row.Payload, &payload); e != nil {
			return e
		}
		allowed := false
		for _, tool := range payload.Tools {
			if tool.Name == req.ToolName {
				allowed = true
				break
			}
		}
		if !allowed {
			return fmt.Errorf("tool is outside the authorized run catalog")
		}
		if !checkpointHasToolCall(row.Checkpoint, req.ToolCallID, req.ToolName, arguments) {
			return fmt.Errorf("tool decision has not been checkpointed")
		}
		return tx.Create(&ToolReceipt{RunID: row.ID, CallID: req.ToolCallID, OwnerEpoch: req.OwnerEpoch, Name: req.ToolName, Arguments: req.Arguments, Status: "executing", ReadOnly: toolReadOnly(req.ToolName)}).Error
	})
	if err != nil {
		return nil, err
	}
	if cached != nil {
		return cached, nil
	}
	toolCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	toolCtx, registry, config, err := s.toolRegistry(toolCtx, row)
	if err != nil {
		return nil, err
	}
	defer registry.Cleanup(context.WithoutCancel(toolCtx))
	bus := s.toolEventBus(row, req.OwnerEpoch)
	var requestPayload ChatPayload
	if err = json.Unmarshal(row.Payload, &requestPayload); err != nil {
		return nil, err
	}
	timeout := generalAgentToolExecTimeout(config)
	toolCtx = agenttools.WithToolExecContext(toolCtx, &agenttools.ToolExecContext{RunID: row.ID, RequestID: requestPayload.RequestID, SessionID: row.SessionID, AssistantMessageID: row.MessageID, ToolCallID: req.ToolCallID, UserID: row.UserID, OriginalUserQuery: payloadQuery(row), EventBus: bus, ApprovalCtx: toolCtx, ExecTimeout: timeout})
	toolCtx, deadlineCancel := context.WithTimeout(toolCtx, timeout)
	defer deadlineCancel()
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-toolCtx.Done():
				return
			case <-ticker.C:
				if _, e := s.ownedRun(toolCtx, row.ID, req.OwnerEpoch); e != nil {
					cancel()
					return
				}
			}
		}
	}()
	if err = bus.Emit(toolCtx, event.Event{Type: event.EventAgentToolCall, SessionID: row.SessionID, Data: event.AgentToolCallData{ToolCallID: req.ToolCallID, ToolName: req.ToolName, Arguments: arguments}}); err != nil {
		return nil, err
	}
	started := time.Now()
	var result *types.ToolResult
	var callErr error
	if req.ToolName == ToolTranscribeInputFile {
		result, callErr = s.transcribeInputFile(toolCtx, row, requestPayload, arguments, config)
	} else {
		result, callErr = registry.ExecuteTool(toolCtx, req.ToolName, req.Arguments)
	}
	if result == nil {
		result = &types.ToolResult{Success: false, Error: "tool returned no result"}
	}
	if callErr != nil {
		result.Success = false
		result.Error = callErr.Error()
	}
	duration := time.Since(started).Milliseconds()
	var response *ToolCallResponse
	persistCtx, finish := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer finish()
	err = s.db.WithContext(persistCtx).Transaction(func(tx *gorm.DB) error {
		current, e := lockRun(tx, row.ID)
		if e != nil {
			return e
		}
		if e = owned(current, req.OwnerEpoch); e != nil {
			return e
		}
		sources := sourcerefs.RestoreRegistry(current.References)
		refs, citations := sourcerefs.RegisterToolResult(sources, req.ToolName, result)
		if len(refs) > 0 {
			result.Output = sourcerefs.AppendCitationCatalog(result.Output, refs)
		}
		current.References = sources.SnapshotReferences()
		response = &ToolCallResponse{Success: result.Success, Output: result.Output, Error: result.Error, Data: result.Data, Images: result.Images, SourceReferences: citations}
		if len(current.References) > 0 {
			response.CitationOutputContract = "Use current source IDs in GenerateStructuredOutput.citations; copy exact answer text into text. Do not embed cite_exactly tags in answer. The authoritative complete current-run list is in [CURRENT_RUN_SOURCES]."
		}
		encoded, e := json.Marshal(response)
		if e != nil {
			return e
		}
		updated := tx.Model(&ToolReceipt{}).Where("run_id = ? AND call_id = ? AND owner_epoch = ?", row.ID, req.ToolCallID, req.OwnerEpoch).Updates(map[string]any{"status": "completed", "response": encoded, "duration_ms": duration})
		if updated.Error != nil {
			return updated.Error
		}
		if updated.RowsAffected != 1 {
			return errRunFenced
		}
		emitted := event.Event{Type: event.EventAgentToolResult, SessionID: row.SessionID, Data: event.AgentToolResultData{ToolCallID: req.ToolCallID, ToolName: req.ToolName, Output: result.Output, Error: result.Error, Success: result.Success, Duration: duration, Data: result.Data}}
		body, e := json.Marshal(emitted)
		if e != nil {
			return e
		}
		if e = outbox(tx, current, StreamEvent{Type: "bus", Data: body}); e != nil {
			return e
		}
		if len(refs) > 0 {
			body, e = json.Marshal(event.Event{Type: event.EventAgentReferences, SessionID: row.SessionID, Data: event.AgentReferencesData{References: refs}})
			if e != nil {
				return e
			}
			if e = outbox(tx, current, StreamEvent{Type: "bus", Data: body}); e != nil {
				return e
			}
		}
		return tx.Save(current).Error
	})
	return response, err
}

func checkpointHasToolCall(checkpoint json.RawMessage, id, name string, arguments map[string]any) bool {
	var root any
	if json.Unmarshal(checkpoint, &root) != nil {
		return false
	}
	var visit func(any) bool
	visit = func(node any) bool {
		switch value := node.(type) {
		case map[string]any:
			if value["type"] == "tool_call" && value["id"] == id && value["name"] == name {
				input := value["input"]
				if text, ok := input.(string); ok {
					if json.Unmarshal([]byte(text), &input) != nil {
						return false
					}
				}
				return reflect.DeepEqual(input, arguments)
			}
			for _, child := range value {
				if visit(child) {
					return true
				}
			}
		case []any:
			for _, child := range value {
				if visit(child) {
					return true
				}
			}
		}
		return false
	}
	return visit(root)
}
func payloadQuery(row *RunRecord) string {
	var p ChatPayload
	_ = json.Unmarshal(row.Payload, &p)
	return p.Query
}
func (s *Service) toolEventBus(row *RunRecord, epoch int64) *event.EventBus {
	bus := event.NewEventBus()
	for _, kind := range []event.EventType{event.EventAgentToolCall, event.EventAgentProgress, event.EventToolApprovalRequired, event.EventToolApprovalResolved, event.EventMCPOAuthRequired, event.EventMCPOAuthResolved} {
		bus.On(kind, func(ctx context.Context, e event.Event) error {
			data, err := json.Marshal(e)
			if err != nil {
				return err
			}
			return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
				current, err := lockRun(tx, row.ID)
				if err != nil {
					return err
				}
				if err = owned(current, epoch); err != nil {
					return err
				}
				if err = outbox(tx, current, StreamEvent{Type: "bus", Data: data}); err != nil {
					return err
				}
				return tx.Save(current).Error
			})
		})
	}
	return bus
}
func decodeBusEvent(body json.RawMessage) (event.Event, error) {
	var e event.Event
	if err := json.Unmarshal(body, &e); err != nil {
		return e, err
	}
	raw, err := json.Marshal(e.Data)
	if err != nil {
		return e, err
	}
	var target any
	switch e.Type {
	case event.EventAgentToolCall:
		target = &event.AgentToolCallData{}
	case event.EventAgentToolResult:
		target = &event.AgentToolResultData{}
	case event.EventAgentReferences:
		target = &event.AgentReferencesData{}
	case event.EventAgentProgress:
		target = &event.AgentProgressData{}
	case event.EventToolApprovalRequired:
		target = &event.ToolApprovalRequiredData{}
	case event.EventToolApprovalResolved:
		target = &event.ToolApprovalResolvedData{}
	case event.EventMCPOAuthRequired:
		target = &event.MCPOAuthRequiredData{}
	case event.EventMCPOAuthResolved:
		target = &event.MCPOAuthResolvedData{}
	default:
		return e, nil
	}
	if err = json.Unmarshal(raw, target); err != nil {
		return e, err
	}
	e.Data = reflect.ValueOf(target).Elem().Interface()
	return e, nil
}
func runSteps(db *gorm.DB, id string) ([]types.AgentStep, error) {
	var calls []ToolReceipt
	if err := db.Where("run_id = ?", id).Order("created_at, call_id").Find(&calls).Error; err != nil {
		return nil, err
	}
	steps := make([]types.AgentStep, 0, len(calls))
	for i, call := range calls {
		var args map[string]any
		var result ToolCallResponse
		if err := json.Unmarshal(call.Arguments, &args); err != nil {
			return nil, err
		}
		if len(call.Response) > 0 {
			if err := json.Unmarshal(call.Response, &result); err != nil {
				return nil, err
			}
		}
		steps = append(steps, types.AgentStep{Iteration: i, ToolCalls: []types.ToolCall{{ID: call.CallID, Name: call.Name, Args: args, Result: &types.ToolResult{Success: result.Success, Output: result.Output, Error: result.Error, Data: result.Data}}}})
	}
	return steps, nil
}
