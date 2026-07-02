package service

import (
	"context"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestResolveKnowledgeBases_KBSelectionNoneIgnoresExplicitTargets(t *testing.T) {
	svc := &sessionService{}
	req := &types.QARequest{
		KnowledgeBaseIDs: []string{"kb-1"},
		KnowledgeIDs:     []string{"doc-1"},
		TagScopes: []types.TagScope{{
			KnowledgeBaseID: "kb-1",
			TagIDs:          []string{"tag-1"},
		}},
		CustomAgent: &types.CustomAgent{
			Config: types.CustomAgentConfig{
				KBSelectionMode: "none",
			},
		},
	}

	kbIDs, knowledgeIDs := svc.resolveKnowledgeBases(context.Background(), req)

	if len(kbIDs) != 0 {
		t.Fatalf("expected KB targets to be ignored, got %v", kbIDs)
	}
	if len(knowledgeIDs) != 0 {
		t.Fatalf("expected knowledge targets to be ignored, got %v", knowledgeIDs)
	}
	if len(req.TagScopes) != 0 {
		t.Fatalf("expected tag scopes to be cleared, got %v", req.TagScopes)
	}
}
