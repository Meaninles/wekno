package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
	"github.com/Tencent/WeKnora/internal/utils"
)

var dataSchemaTool = BaseTool{
	name:        ToolDataSchema,
	description: "Use this tool to get the schema information of a CSV or Excel file loaded into DuckDB. It returns the table name, columns, and row count.",
	schema:      utils.GenerateSchema[DataSchemaInput](),
}

var tableSchemaTool = BaseTool{
	name:        ToolTableSchema,
	description: "Use this tool to inspect the schema of a CSV/Excel table from a knowledge-base file or a current-turn uploaded table attachment before writing DuckDB SQL.",
	schema:      utils.GenerateSchema[DataSchemaInput](),
}

type DataSchemaInput struct {
	KnowledgeID string `json:"knowledge_id" jsonschema:"id of the knowledge to query"`
}

type DataSchemaTool struct {
	BaseTool
	knowledgeService interfaces.KnowledgeService
	chunkRepo        interfaces.ChunkRepository
	targetChunkTypes []types.ChunkType
	runtimeLoader    *TableAnalysisTool
}

func NewDataSchemaTool(knowledgeService interfaces.KnowledgeService, chunkRepo interfaces.ChunkRepository, targetChunkTypes ...types.ChunkType) *DataSchemaTool {
	return newDataSchemaTool(dataSchemaTool, knowledgeService, chunkRepo, nil, targetChunkTypes...)
}

func NewTableSchemaTool(knowledgeService interfaces.KnowledgeService, chunkRepo interfaces.ChunkRepository, runtimeLoader *TableAnalysisTool, targetChunkTypes ...types.ChunkType) *DataSchemaTool {
	return newDataSchemaTool(tableSchemaTool, knowledgeService, chunkRepo, runtimeLoader, targetChunkTypes...)
}

func newDataSchemaTool(base BaseTool, knowledgeService interfaces.KnowledgeService, chunkRepo interfaces.ChunkRepository, runtimeLoader *TableAnalysisTool, targetChunkTypes ...types.ChunkType) *DataSchemaTool {
	if len(targetChunkTypes) == 0 {
		targetChunkTypes = []types.ChunkType{types.ChunkTypeTableSummary, types.ChunkTypeTableColumn}
	}
	return &DataSchemaTool{
		BaseTool:         base,
		knowledgeService: knowledgeService,
		chunkRepo:        chunkRepo,
		targetChunkTypes: targetChunkTypes,
		runtimeLoader:    runtimeLoader,
	}
}

func (t *DataSchemaTool) Cleanup(ctx context.Context) {
	if t.runtimeLoader != nil {
		t.runtimeLoader.Cleanup(ctx)
	}
}

// Execute executes the tool logic
func (t *DataSchemaTool) Execute(ctx context.Context, args json.RawMessage) (*types.ToolResult, error) {
	var input DataSchemaInput
	if err := json.Unmarshal(args, &input); err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to parse input args: %v", err),
		}, err
	}

	if t.runtimeLoader != nil {
		schema, err := t.runtimeLoader.LoadFromKnowledgeID(ctx, input.KnowledgeID)
		if err != nil {
			return &types.ToolResult{
				Success: false,
				Error:   fmt.Sprintf("Failed to load table source '%s': %v", input.KnowledgeID, err),
			}, err
		}
		output := schema.Description()
		return &types.ToolResult{
			Success: true,
			Output:  output,
			Data: map[string]interface{}{
				"summary": output,
				"schema":  schema,
			},
		}, nil
	}

	if _, ok := types.ParseRuntimeAttachmentID(input.KnowledgeID); ok {
		err := fmt.Errorf("uploaded table attachments are not available to this schema tool")
		return &types.ToolResult{Success: false, Error: err.Error()}, err
	}

	// Get knowledge to get TenantID (use IDOnly to support cross-tenant shared KB)
	knowledge, err := t.knowledgeService.GetKnowledgeByIDOnly(ctx, input.KnowledgeID)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to get knowledge '%s': %v", input.KnowledgeID, err),
		}, err
	}

	// Get chunks for the knowledge ID using ChunkRepository
	// We only need table summary and column chunks
	chunkTypes := t.targetChunkTypes
	page := &types.Pagination{
		Page:     1,
		PageSize: 100, // Should be enough for schema chunks
	}

	chunks, _, err := t.chunkRepo.ListPagedChunksByKnowledgeID(
		ctx,
		knowledge.TenantID,
		input.KnowledgeID,
		page,
		chunkTypes,
		"", // tagID
		"", // keyword
		"", // searchField
		"", // sortOrder
		"", // knowledgeType
	)
	if err != nil {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("Failed to list chunks for knowledge ID '%s': %v", input.KnowledgeID, err),
		}, err
	}

	var summaryContent, columnContent string
	for _, chunk := range chunks {
		if chunk.ChunkType == types.ChunkTypeTableSummary {
			summaryContent = chunk.Content
		} else if chunk.ChunkType == types.ChunkTypeTableColumn {
			columnContent = chunk.Content
		}
	}

	if summaryContent == "" || columnContent == "" {
		return &types.ToolResult{
			Success: false,
			Error:   fmt.Sprintf("No table schema information found for knowledge ID '%s'", input.KnowledgeID),
		}, fmt.Errorf("no schema info found")
	}

	output := fmt.Sprintf("%s\n\n%s", summaryContent, columnContent)

	return &types.ToolResult{
		Success: true,
		Output:  output,
		Data: map[string]interface{}{
			"summary": summaryContent,
			"columns": columnContent,
		},
	}, nil
}
