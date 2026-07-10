package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
	"github.com/Tencent/WeKnora/internal/types/interfaces"
)

// knowledgeLookupStub embeds the large production interface and overrides only
// the lookup used by this focused identifier-kind regression test.
type knowledgeLookupStub struct {
	interfaces.KnowledgeService
}

func (knowledgeLookupStub) GetKnowledgeByIDOnly(context.Context, string) (*types.Knowledge, error) {
	return nil, errors.New("knowledge not found")
}

func TestGetDocumentInfoTreatsInScopeKnowledgeBaseIDAsScope(t *testing.T) {
	const knowledgeBaseID = "kb-in-scope"
	tool := NewGetDocumentInfoTool(
		knowledgeLookupStub{},
		nil,
		types.SearchTargets{{KnowledgeBaseID: knowledgeBaseID}},
	)

	result, err := tool.Execute(context.Background(), json.RawMessage(`{"knowledge_ids":["kb-in-scope"]}`))
	if err != nil {
		t.Fatalf("Execute() error = %v, want nil", err)
	}
	if result == nil || !result.Success {
		t.Fatalf("Execute() result = %#v, want successful scope acknowledgement", result)
	}
	if !strings.Contains(result.Output, "accessible knowledge base, not a document ID") {
		t.Fatalf("result output = %q, want KB scope guidance", result.Output)
	}
	if got := result.Data["display_type"]; got != "knowledge_base_scope" {
		t.Fatalf("display_type = %v, want knowledge_base_scope", got)
	}
}
