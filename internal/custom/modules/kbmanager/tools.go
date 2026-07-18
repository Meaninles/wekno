package kbmanager

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	agenttools "github.com/Tencent/WeKnora/internal/agent/tools"
	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/utils"
)

type ToolScope struct {
	AgentID       string
	AgentTenantID uint64
	SessionID     string
	Runtime       *types.KnowledgeManagementRuntimeScope
}

type ListDocumentsTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type AddDocumentTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type ReplaceDocumentTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type DeleteDocumentTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type MutationStatusTool struct {
	agenttools.BaseTool
	service *Service
	scope   ToolScope
}

type MutationStatusRequest struct {
	OperationID string `json:"operation_id" jsonschema:"operation_id returned by kb_add_document, kb_replace_document, or kb_delete_document"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"optional long-poll for up to 30 seconds, only when the user explicitly asks to wait for processing"`
}

func NewListDocumentsTool(service *Service, scope ToolScope) *ListDocumentsTool {
	return &ListDocumentsTool{
		BaseTool: agenttools.NewBaseTool(
			ToolListDocuments,
			"List document-level metadata inside the exact effective knowledge-management scope. Use before replace/delete to get stable document IDs, hashes, timestamps, parse status, metadata and tags. A selected document limits results to that document; a selected KB permits that KB only.",
			utils.GenerateSchema[ListDocumentsRequest](),
		),
		service: service,
		scope:   scope,
	}
}

func NewAddDocumentTool(service *Service, scope ToolScope) *AddDocumentTool {
	return &AddDocumentTool{
		BaseTool: agenttools.NewBaseTool(
			ToolAddDocument,
			"Add one whole document to WeKnora and start native parsing in the background. The source MUST be a current-run create_artifact file_token or input file id; URL and filesystem-path ingestion are forbidden. Standalone add requires whole-KB turn scope. Once the tool confirms the document was added, report that immediate outcome without waiting for parsing; do not claim parsing or enrichment completed.",
			utils.GenerateSchema[AddDocumentRequest](),
		),
		service: service,
		scope:   scope,
	}
}

func NewReplaceDocumentTool(service *Service, scope ToolScope) *ReplaceDocumentTool {
	return &ReplaceDocumentTool{
		BaseTool: agenttools.NewBaseTool(
			ToolReplaceDocument,
			"Write the replacement document to the old document's knowledge base. This tool ONLY adds the new document and NEVER deletes the old one. Modify is a composite of add+delete permission. After this tool confirms the new document was added, immediately call kb_delete_document for the inspected old document without waiting for parsing. If add fails, never delete the old document; if delete fails, report that both documents remain.",
			utils.GenerateSchema[ReplaceDocumentRequest](),
		),
		service: service,
		scope:   scope,
	}
}

func NewDeleteDocumentTool(service *Service, scope ToolScope) *DeleteDocumentTool {
	return &DeleteDocumentTool{
		BaseTool: agenttools.NewBaseTool(
			ToolDeleteDocument,
			"Delete one whole document within the exact current-turn scope using WeKnora's native cleanup, including chunks/indexes, document-scoped graph data and Wiki source references. Inspect with kb_list_documents first and pass expected_file_hash when available. Never simulate deletion.",
			utils.GenerateSchema[DeleteDocumentRequest](),
		),
		service: service,
		scope:   scope,
	}
}

func NewMutationStatusTool(service *Service, scope ToolScope) *MutationStatusTool {
	return &MutationStatusTool{
		BaseTool: agenttools.NewBaseTool(
			ToolMutationStatus,
			"Get durable add/replace/delete operation status when the user explicitly asks to inspect or wait for background processing. Do not call this automatically after add or replace. A pending operation is not parsed/completed even though its new document may already have been added.",
			utils.GenerateSchema[MutationStatusRequest](),
		),
		service: service,
		scope:   scope,
	}
}

func (t *ListDocumentsTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input ListDocumentsRequest
	if err := json.Unmarshal(args, &input); err != nil {
		return failedResult(err)
	}
	result, err := t.service.ListDocuments(ctx, t.scope, input)
	if err != nil {
		return failedResult(err)
	}
	return jsonResult("Document inventory", result)
}

func (t *AddDocumentTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input AddDocumentRequest
	if err := json.Unmarshal(args, &input); err != nil {
		return failedResult(err)
	}
	operation, err := t.service.AddDocument(ctx, t.scope, input)
	if err != nil {
		return failedOperationResult(operation, err)
	}
	return operationResult(operation)
}

func (t *ReplaceDocumentTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input ReplaceDocumentRequest
	if err := json.Unmarshal(args, &input); err != nil {
		return failedResult(err)
	}
	operation, err := t.service.ReplaceDocument(ctx, t.scope, input)
	if err != nil {
		return failedOperationResult(operation, err)
	}
	return operationResult(operation)
}

func (t *DeleteDocumentTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input DeleteDocumentRequest
	if err := json.Unmarshal(args, &input); err != nil {
		return failedResult(err)
	}
	operation, err := t.service.DeleteDocument(ctx, t.scope, input)
	if err != nil {
		return failedOperationResult(operation, err)
	}
	return operationResult(operation)
}

func (t *MutationStatusTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input MutationStatusRequest
	if err := json.Unmarshal(args, &input); err != nil {
		return failedResult(err)
	}
	waitSeconds := input.WaitSeconds
	if waitSeconds < 0 {
		waitSeconds = 0
	}
	if waitSeconds > 30 {
		waitSeconds = 30
	}
	operation, err := t.service.GetOperation(ctx, t.scope, strings.TrimSpace(input.OperationID), time.Duration(waitSeconds)*time.Second)
	if err != nil {
		return failedResult(err)
	}
	return operationResult(operation)
}

func failedResult(err error) (*types.ToolResult, error) {
	return &types.ToolResult{Success: false, Error: err.Error()}, err
}

func failedOperationResult(operation *Operation, err error) (*types.ToolResult, error) {
	if operation == nil {
		return failedResult(err)
	}
	return &types.ToolResult{Success: false, Error: err.Error(), Data: operationView(operation)}, err
}

func jsonResult(label string, value any) (*types.ToolResult, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return failedResult(err)
	}
	data := map[string]interface{}{}
	marshaled, _ := json.Marshal(value)
	_ = json.Unmarshal(marshaled, &data)
	return &types.ToolResult{Success: true, Output: fmt.Sprintf("%s:\n%s", label, string(encoded)), Data: data}, nil
}

func operationResult(operation *Operation) (*types.ToolResult, error) {
	view := operationView(operation)
	encoded, _ := json.MarshalIndent(view, "", "  ")
	data := view
	return &types.ToolResult{
		Success: true,
		Output:  "Knowledge mutation operation:\n" + string(encoded),
		Data:    data,
	}, nil
}

func operationView(operation *Operation) map[string]interface{} {
	if operation == nil {
		return map[string]interface{}{}
	}
	return map[string]interface{}{
		"operation_id":      operation.ID,
		"type":              operation.Type,
		"state":             operation.State,
		"terminal":          operation.Terminal(),
		"knowledge_base_id": operation.KnowledgeBaseID,
		"old_knowledge_id":  operation.OldKnowledgeID,
		"new_knowledge_id":  operation.NewKnowledgeID,
		"file_name":         operation.FileName,
		"result_message":    operation.ResultMessage,
		"error_message":     operation.ErrorMessage,
		"created_at":        operation.CreatedAt,
		"updated_at":        operation.UpdatedAt,
		"completed_at":      operation.CompletedAt,
	}
}
