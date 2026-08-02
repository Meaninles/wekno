package sourcerefs

import (
	"reflect"
	"testing"

	"github.com/Tencent/WeKnora/internal/types"
)

func TestNarrowDocumentSearchTargets_ExplicitFileOverridesOnlyItsWholeKB(t *testing.T) {
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-a", TenantID: 7},
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-b", TenantID: 8},
	}

	got, covered := narrowDocumentSearchTargets(targets, map[string][]string{
		"kb-a": {"doc-2", "doc-1", "doc-2"},
	})

	if !covered["kb-a"] {
		t.Fatal("expected explicit kb-a scope to be covered")
	}
	if got[0].Type != types.SearchTargetTypeKnowledge ||
		!reflect.DeepEqual(got[0].KnowledgeIDs, []string{"doc-2", "doc-1"}) {
		t.Fatalf("kb-a target was not narrowed to explicit files: %#v", got[0])
	}
	if got[1].Type != types.SearchTargetTypeKnowledgeBase || got[1].KnowledgeBaseID != "kb-b" {
		t.Fatalf("non-overlapping kb-b target changed: %#v", got[1])
	}
	if len(targets[0].KnowledgeIDs) != 0 || targets[0].Type != types.SearchTargetTypeKnowledgeBase {
		t.Fatalf("input target was mutated: %#v", targets[0])
	}
}

func TestNarrowDocumentSearchTargets_PreservesTagAndMergesExplicitFileTarget(t *testing.T) {
	targets := types.SearchTargets{
		{Type: types.SearchTargetTypeKnowledgeBase, KnowledgeBaseID: "kb-a", TenantID: 7, TagIDs: []string{"tag-1"}},
		{Type: types.SearchTargetTypeKnowledge, KnowledgeBaseID: "kb-a", TenantID: 7, KnowledgeIDs: []string{"doc-0"}},
	}

	got, covered := narrowDocumentSearchTargets(targets, map[string][]string{"kb-a": {"doc-1"}})

	if !covered["kb-a"] {
		t.Fatal("expected tag target to authorize the same-KB explicit file metadata")
	}
	if got[0].Type != types.SearchTargetTypeKnowledgeBase ||
		!reflect.DeepEqual(got[0].TagIDs, []string{"tag-1"}) || len(got[0].KnowledgeIDs) != 0 {
		t.Fatalf("tag scope should remain independent: %#v", got[0])
	}
	if got[1].Type != types.SearchTargetTypeKnowledge ||
		!reflect.DeepEqual(got[1].KnowledgeIDs, []string{"doc-0", "doc-1"}) {
		t.Fatalf("explicit file scope should be merged without changing tag scope: %#v", got[1])
	}
}
