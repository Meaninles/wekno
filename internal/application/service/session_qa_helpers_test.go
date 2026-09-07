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

func TestResolveKnowledgeBasesNonePreservesOnlyValidatedSessionUploads(t *testing.T) {
	req := &types.QARequest{
		CustomAgent:      &types.CustomAgent{Config: types.CustomAgentConfig{KBSelectionMode: "none"}},
		KnowledgeBaseIDs: []string{"unselected-kb"}, KnowledgeIDs: []string{"untrusted-document", "upload"},
		SessionUploadKnowledgeIDs: []string{"upload", "historical-upload"},
	}
	kbs, files := (&sessionService{}).resolveKnowledgeBases(context.Background(), req)
	if len(kbs) != 0 || len(files) != 2 || files[0] != "upload" || files[1] != "historical-upload" {
		t.Fatalf("unexpected resolved scope: %v, %v", kbs, files)
	}
}
