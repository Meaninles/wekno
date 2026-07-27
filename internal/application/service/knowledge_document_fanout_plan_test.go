package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestBuildDocumentFanoutPlanUsesPlatformDerivativeDefaultWithoutKBOverride(t *testing.T) {
	t.Parallel()

	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		FileName:             "matrix.csv",
		ProcessingGeneration: "generation-1",
	}
	plan, err := buildDocumentFanoutPlan(
		context.Background(),
		knowledge,
		&types.KnowledgeBase{ID: "kb-1", EmbeddingModelID: "embedding-1"},
		ProcessChunksOptions{},
		nil,
	)
	if err != nil {
		t.Fatalf("buildDocumentFanoutPlan() error = %v", err)
	}
	if plan.DataTable == nil {
		t.Fatal("DataTable is nil, want durable work that resolves the platform derivative default at execution")
	}
	if plan.DataTable.SummaryModel != "" || plan.DataTable.EmbeddingModel != "embedding-1" {
		t.Fatalf("DataTable = %#v, want empty derivative override plus embedding model", plan.DataTable)
	}
}

func TestBuildDocumentFanoutPlanPersistsCompleteTableEnrichment(t *testing.T) {
	t.Parallel()

	knowledge := &types.Knowledge{
		ID:                   "knowledge-1",
		TenantID:             42,
		KnowledgeBaseID:      "kb-1",
		FileName:             "matrix.xlsx",
		ProcessingGeneration: "generation-1",
	}
	plan, err := buildDocumentFanoutPlan(
		context.Background(),
		knowledge,
		&types.KnowledgeBase{
			ID:                "kb-1",
			SummaryModelID:    "conversation-model",
			DerivativeModelID: "derivative-1",
			EmbeddingModelID:  "embedding-1",
		},
		ProcessChunksOptions{},
		nil,
	)
	if err != nil {
		t.Fatalf("buildDocumentFanoutPlan() error = %v", err)
	}
	if plan.DataTable == nil {
		t.Fatal("DataTable is nil, want durable table enrichment")
	}
	if plan.DataTable.SummaryModel != "derivative-1" || plan.DataTable.EmbeddingModel != "embedding-1" {
		t.Fatalf("DataTable = %#v", plan.DataTable)
	}
}
